// ignore-parallel-cli: a thin CLI wrapper around the `ignore` crate's
// WalkBuilder::build_parallel(), built to be directly comparable to the Go
// `walkman` package (github.com/PAKIWASI/walkman) and to rust_walkdir's
// walkdir-cli (the sequential reference point).
//
// `ignore` is ripgrep's directory-walking crate: unlike `walkdir`, it can
// spread the walk across multiple OS threads via WalkParallel, which makes
// it the more relevant comparison for a concurrent walker like walkman —
// walkdir-cli only ever tells you "how fast is single-threaded," this tells
// you "how fast is *another* multi-threaded walker."
//
// By default `ignore` is built for tools like ripgrep: it filters out
// hidden files and anything matched by .gitignore/.ignore/git excludes.
// That's not a fair comparison against walkman (which has no such
// filtering), so every one of those filters is explicitly turned off below
// — this walks the raw filesystem tree, same as walkdir-cli and walkman.
//
// Mirrors walkman semantics:
//   - skip list: entries whose *name* matches are pruned (subtree skipped
//     for dirs, via WalkState::Skip from the visitor)
//   - max-depth: 0 = unlimited, like walkdir-cli's/walkman's
//   - follow-links: off by default
//   - root entry (depth 0) excluded from counts, like walkdir-cli's
//     min_depth(1)
//   - errors (permission denied, broken symlink, etc.) are counted, not
//     fatal
//
// Usage:
//   ignore-parallel-cli [OPTIONS] [ROOT]
//
// Options:
//   --max-depth N      0 = unlimited (default: 0)
//   --follow-links     follow symlinks (default: off)
//   --skip a,b,c       comma-separated names to prune (like walkman's skipList)
//   --print            print every entry (like main.go does) instead of just counting
//   --bench N          repeat the walk N times, report min/avg/max wall time
//   --quiet            suppress the summary line (useful with --bench)
//   --workers N        thread count, 0 = available_parallelism (default: 0),
//                      matches walkman's --workers flag for bench_harness.sh
//
// Examples:
//   ignore-parallel-cli --workers 8 /path             # 8-thread parallel walk
//   ignore-parallel-cli --skip .git,node_modules /p   # prune like walkman's skipList
//   ignore-parallel-cli --bench 10 --workers 4 /path  # 10 timed runs, min/avg/max

use ignore::{DirEntry, Error, ParallelVisitor, ParallelVisitorBuilder, WalkBuilder, WalkState};
use std::env;
use std::io::{self, Write};
use std::process::ExitCode;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

struct Opts {
    root: String,
    max_depth: usize, // 0 = unlimited
    follow_links: bool,
    skip: Vec<String>,
    print: bool,
    bench: Option<u32>,
    quiet: bool,
    workers: usize, // 0 = available_parallelism
}

fn parse_args() -> Result<Opts, String> {
    let mut root = ".".to_string();
    let mut max_depth = 0usize;
    let mut follow_links = false;
    let mut skip = Vec::new();
    let mut print = false;
    let mut bench = None;
    let mut quiet = false;
    let mut workers = 0usize;

    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--max-depth" => {
                let v = args.next().ok_or("--max-depth needs a value")?;
                max_depth = v.parse().map_err(|_| "--max-depth must be a non-negative integer")?;
            }
            "--follow-links" => follow_links = true,
            "--skip" => {
                let v = args.next().ok_or("--skip needs a value")?;
                skip = v.split(',').filter(|s| !s.is_empty()).map(String::from).collect();
            }
            "--print" => print = true,
            "--bench" => {
                let v = args.next().ok_or("--bench needs a value")?;
                bench = Some(v.parse().map_err(|_| "--bench must be a positive integer")?);
            }
            "--quiet" => quiet = true,
            "--workers" => {
                let v = args.next().ok_or("--workers needs a value")?;
                workers = v.parse().map_err(|_| "--workers must be a non-negative integer")?;
            }
            "-h" | "--help" => {
                print_help();
                std::process::exit(0);
            }
            other if !other.starts_with('-') => root = other.to_string(),
            other => return Err(format!("unknown flag: {other}")),
        }
    }

    Ok(Opts { root, max_depth, follow_links, skip, print, bench, quiet, workers })
}

fn print_help() {
    println!(
        "ignore-parallel-cli - ignore::WalkParallel wrapper, comparable to Go's walkman\n\n\
         USAGE:\n    ignore-parallel-cli [OPTIONS] [ROOT]\n\n\
         OPTIONS:\n\
         \x20   --max-depth N     0 = unlimited (default: 0)\n\
         \x20   --follow-links    follow symlinks (default: off)\n\
         \x20   --skip a,b,c      comma-separated names to prune\n\
         \x20   --print           print every entry\n\
         \x20   --bench N         repeat N times, report timing stats\n\
         \x20   --quiet           suppress summary line\n\
         \x20   --workers N       thread count, 0 = available_parallelism"
    );
}

#[derive(Default)]
struct Counts {
    files: u64,
    dirs: u64,
    links: u64,
    errors: u64,
}

struct AtomicCounts {
    files: AtomicU64,
    dirs: AtomicU64,
    links: AtomicU64,
    errors: AtomicU64,
}

impl AtomicCounts {
    fn new() -> Self {
        AtomicCounts {
            files: AtomicU64::new(0),
            dirs: AtomicU64::new(0),
            links: AtomicU64::new(0),
            errors: AtomicU64::new(0),
        }
    }

    fn snapshot(&self) -> Counts {
        Counts {
            files: self.files.load(Ordering::Relaxed),
            dirs: self.dirs.load(Ordering::Relaxed),
            links: self.links.load(Ordering::Relaxed),
            errors: self.errors.load(Ordering::Relaxed),
        }
    }
}

// Per-thread visitor. `ignore` hands one of these to each worker thread via
// the builder below, so counting only needs shared atomics — no locking on
// the hot path. Printing (rare, opt-in) goes through a shared, locked
// stdout so output isn't interleaved mid-line across threads.
struct Visitor {
    counts: Arc<AtomicCounts>,
    skip: Arc<Vec<String>>,
    print: bool,
    stdout: Option<Arc<Mutex<io::Stdout>>>,
}

impl ParallelVisitor for Visitor {
    fn visit(&mut self, entry: Result<DirEntry, Error>) -> WalkState {
        let entry = match entry {
            Ok(e) => e,
            Err(err) => {
                self.counts.errors.fetch_add(1, Ordering::Relaxed);
                if self.print {
                    eprintln!("error: {err}");
                }
                return WalkState::Continue;
            }
        };

        // depth 0 is the root itself — excluded from counts
        if entry.depth() == 0 {
            return WalkState::Continue;
        }

        // Mirror walkman's skipList: prune by bare name, applied at every
        // depth. Returning Skip here stops descent into this entry if it's
        // a directory; the entry itself is not counted or printed.
        if let Some(name) = entry.file_name().to_str() {
            if self.skip.iter().any(|s| s == name) {
                return WalkState::Skip;
            }
        }

        let ft = entry.file_type();
        let (kind, is_dir, is_link) = match &ft {
            Some(t) if t.is_dir() => ("d", true, false),
            Some(t) if t.is_symlink() => ("l", false, true),
            _ => ("f", false, false),
        };

        if is_dir {
            self.counts.dirs.fetch_add(1, Ordering::Relaxed);
        } else if is_link {
            self.counts.links.fetch_add(1, Ordering::Relaxed);
        } else {
            self.counts.files.fetch_add(1, Ordering::Relaxed);
        }

        if self.print {
            if let Some(stdout) = &self.stdout {
                let mut out = stdout.lock().unwrap();
                let _ = writeln!(out, "{}  {}", kind, entry.path().display());
            }
        }

        WalkState::Continue
    }
}

struct VisitorBuilder {
    counts: Arc<AtomicCounts>,
    skip: Arc<Vec<String>>,
    print: bool,
    stdout: Option<Arc<Mutex<io::Stdout>>>,
}

impl<'s> ParallelVisitorBuilder<'s> for VisitorBuilder {
    fn build(&mut self) -> Box<dyn ParallelVisitor + 's> {
        Box::new(Visitor {
            counts: Arc::clone(&self.counts),
            skip: Arc::clone(&self.skip),
            print: self.print,
            stdout: self.stdout.clone(),
        })
    }
}

fn run_walk(opts: &Opts) -> Counts {
    let mut builder = WalkBuilder::new(&opts.root);
    builder
        // Disable every ripgrep-style filter individually (rather than
        // relying on a single "standard_filters" toggle) so this stays
        // correct across `ignore` versions: we want the raw tree, exactly
        // like walkdir-cli and walkman see it.
        .hidden(false)
        .parents(false)
        .ignore(false)
        .git_global(false)
        .git_ignore(false)
        .git_exclude(false)
        .require_git(false)
        .follow_links(opts.follow_links)
        .threads(opts.workers); // 0 = let `ignore` pick (available_parallelism)

    if opts.max_depth != 0 {
        builder.max_depth(Some(opts.max_depth));
    }

    let walker = builder.build_parallel();

    let counts = Arc::new(AtomicCounts::new());
    let skip = Arc::new(opts.skip.clone());
    let stdout = if opts.print { Some(Arc::new(Mutex::new(io::stdout()))) } else { None };

    let mut vb = VisitorBuilder {
        counts: Arc::clone(&counts),
        skip,
        print: opts.print,
        stdout,
    };

    walker.visit(&mut vb);

    Arc::try_unwrap(counts).map(|c| c.snapshot()).unwrap_or_else(|c| c.snapshot())
}

// Rust ignores SIGPIPE by default, which turns "closed the read end early"
// (e.g. `ignore-parallel-cli / | head`) into an ugly panic instead of a
// quiet exit, like every other Unix CLI. Restore the default disposition on startup
#[cfg(unix)]
fn restore_sigpipe() {
    unsafe {
        libc_signal(13 /* SIGPIPE */, 0 /* SIG_DFL */);
    }
}

#[cfg(unix)]
extern "C" {
    #[link_name = "signal"]
    fn libc_signal(signum: i32, handler: usize) -> usize;
}

#[cfg(not(unix))]
fn restore_sigpipe() {}

fn main() -> ExitCode {
    restore_sigpipe();

    let opts = match parse_args() {
        Ok(o) => o,
        Err(e) => {
            eprintln!("error: {e}");
            print_help();
            return ExitCode::FAILURE;
        }
    };

    if let Some(n) = opts.bench {
        let mut durations: Vec<Duration> = Vec::with_capacity(n as usize);
        let mut last = Counts::default();
        for _ in 0..n {
            let start = Instant::now();
            last = run_walk(&opts);
            durations.push(start.elapsed());
        }
        let total: Duration = durations.iter().sum();
        let avg = total / n;
        let min = durations.iter().min().unwrap();
        let max = durations.iter().max().unwrap();

        if !opts.quiet {
            println!(
                "files={} dirs={} links={} errors={}",
                last.files, last.dirs, last.links, last.errors
            );
        }
        println!(
            "bench: n={} avg={:?} min={:?} max={:?} avg_ns_op={}",
            n, avg, min, max, avg.as_nanos()
        );
        return ExitCode::SUCCESS;
    }

    let start = Instant::now();
    let counts = run_walk(&opts);
    let elapsed = start.elapsed();

    if !opts.quiet {
        println!(
            "files={} dirs={} links={} errors={}",
            counts.files, counts.dirs, counts.links, counts.errors
        );
        println!("elapsed={:?}", elapsed);
    }

    ExitCode::SUCCESS
}
