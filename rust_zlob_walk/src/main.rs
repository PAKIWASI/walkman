// zlob-walk-cli: a thin CLI wrapper around zlob's `walk::WalkBuilder`
// (github.com/dmtrKovalenko/zlob), built to be directly comparable to the
// Go `walkman` package and to rust_ignore_parallel's ignore-parallel-cli —
// same flags, same output format, so it drops straight into the existing
// bench_harness.sh / perf_bench.sh / run_all.sh harness as a third tool.
//
// zlob's walker defaults to WalkFlags::RECOMMENDED (gitignore respected,
// hidden entries skipped) — ripgrep-style defaults, same story as `ignore`
// crate's own WalkBuilder defaults. That's not a fair comparison against
// walkman (which has no such filtering), so this always passes
// WalkFlags::empty() (plus FOLLOW_SYMLINKS when requested) to get the raw,
// unfiltered tree — same as ignore-parallel-cli explicitly disabling every
// ripgrep-style filter.
//
// Mirrors walkman / ignore-parallel-cli semantics:
//   - skip list: entries whose *name* matches are pruned at every depth,
//     via `extra_ignore` with bare (slash-less) gitignore-style patterns
//     — a bare pattern matches the basename at any depth, same effect as
//     walkman's skipList.
//   - max-depth: 0 = unlimited, like walkman's/ignore-parallel-cli's
//   - follow-links: off by default
//   - the walk root itself is never reported as an entry (zlob's own
//     depth convention: direct children of the root are depth 1), so
//     unlike ignore-parallel-cli there's no explicit depth==0 skip needed
//   - errors: zlob's `run()` visitor receives a plain WalkEntry, not a
//     Result — by default it silently skips directories it can't read
//     (WalkFlags::ABORT_ON_ERROR would change that to a hard stop) and
//     does not surface a per-entry error to the Rust callback. So unlike
//     ignore-parallel-cli, `errors` in the summary line is always 0 by
//     construction, not because none occurred — don't read anything into
//     it being zero.
//
// NOTE ON API SURFACE: the exact `WalkEntryKind` variant names below
// (Dir / File / Symlink) are my best read of zlob 1.6.5's docs.rs page,
// but I could not get a fully authoritative fetch of that specific enum
// page while writing this. If `cargo build` complains about the match
// arms, run `cargo doc -p zlob --open` (or grep the vendored source under
// `~/.cargo/registry/src/.../zlob-*/src/walk.rs`) and fix the variant
// names — everything else here is confirmed against the real API.
//
// Usage:
//   zlob-walk-cli [OPTIONS] [ROOT]
//
// Options:
//   --max-depth N      0 = unlimited (default: 0)
//   --follow-links     follow symlinks (default: off)
//   --skip a,b,c       comma-separated names to prune (like walkman's skipList)
//   --print            print every entry (like main.go does) instead of just counting
//   --bench N          repeat the walk N times, report min/avg/max wall time
//   --quiet            suppress the summary line (useful with --bench)
//   --workers N        thread count, 0 = one-per-CPU (default: 0),
//                      matches walkman's --workers flag for bench_harness.sh
//
// Examples:
//   zlob-walk-cli --workers 8 /path             # 8-thread parallel walk
//   zlob-walk-cli --skip .git,node_modules /p   # prune like walkman's skipList
//   zlob-walk-cli --bench 10 --workers 4 /path  # 10 timed runs, min/avg/max

use std::env;
use std::io::{self, Write};
use std::process::ExitCode;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use zlob::walk::{WalkBuilder, WalkEntryKind, WalkFlags, WalkState};

struct Opts {
    root: String,
    max_depth: usize, // 0 = unlimited
    follow_links: bool,
    skip: Vec<String>,
    print: bool,
    bench: Option<u32>,
    quiet: bool,
    workers: usize, // 0 = one-per-CPU
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
        "zlob-walk-cli - zlob::walk::WalkBuilder wrapper, comparable to Go's walkman\n\n\
         USAGE:\n    zlob-walk-cli [OPTIONS] [ROOT]\n\n\
         OPTIONS:\n\
         \x20   --max-depth N     0 = unlimited (default: 0)\n\
         \x20   --follow-links    follow symlinks (default: off)\n\
         \x20   --skip a,b,c      comma-separated names to prune\n\
         \x20   --print           print every entry\n\
         \x20   --bench N         repeat N times, report timing stats\n\
         \x20   --quiet           suppress summary line\n\
         \x20   --workers N       thread count, 0 = one-per-CPU"
    );
}

#[derive(Default)]
struct Counts {
    files: u64,
    dirs: u64,
    links: u64,
    errors: u64, // always 0 here — see NOTE at top of file
}

struct AtomicCounts {
    files: AtomicU64,
    dirs: AtomicU64,
    links: AtomicU64,
}

impl AtomicCounts {
    fn new() -> Self {
        AtomicCounts {
            files: AtomicU64::new(0),
            dirs: AtomicU64::new(0),
            links: AtomicU64::new(0),
        }
    }

    fn snapshot(&self) -> Counts {
        Counts {
            files: self.files.load(Ordering::Relaxed),
            dirs: self.dirs.load(Ordering::Relaxed),
            links: self.links.load(Ordering::Relaxed),
            errors: 0,
        }
    }
}

fn run_walk(opts: &Opts) -> Counts {
    let mut builder = WalkBuilder::new(&opts.root).expect("invalid root path");

    // Raw traversal: strip zlob's ripgrep-style RECOMMENDED defaults so
    // this walks the same unfiltered tree walkman and ignore-parallel-cli
    // do. FOLLOW_SYMLINKS is the only bit we ever add back in.
    let mut flags = WalkFlags::empty();
    if opts.follow_links {
        flags |= WalkFlags::FOLLOW_SYMLINKS;
    }
    builder.options(flags);
    builder.threads(opts.workers); // 0 = let zlob pick (one per CPU)

    if opts.max_depth != 0 {
        builder.max_depth(Some(opts.max_depth));
    }

    if !opts.skip.is_empty() {
        // Bare (slash-less) gitignore-style patterns match the basename
        // at any depth — the same prune-by-name behavior as walkman's
        // skipList / ignore-parallel-cli's --skip.
        builder.extra_ignore(&opts.skip).expect("invalid --skip pattern");
    }

    let counts = Arc::new(AtomicCounts::new());
    let print = opts.print;
    let stdout = if print { Some(Arc::new(Mutex::new(io::stdout()))) } else { None };

    let counts_cb = Arc::clone(&counts);
    let stdout_cb = stdout.clone();
    builder
        .run(move |entry| {
            let kind_char = match entry.kind() {
                WalkEntryKind::Dir => {
                    counts_cb.dirs.fetch_add(1, Ordering::Relaxed);
                    "d"
                }
                WalkEntryKind::Symlink => {
                    counts_cb.links.fetch_add(1, Ordering::Relaxed);
                    "l"
                }
                _ => {
                    counts_cb.files.fetch_add(1, Ordering::Relaxed);
                    "f"
                }
            };

            if print {
                if let Some(stdout) = &stdout_cb {
                    let mut out = stdout.lock().unwrap();
                    let _ = writeln!(out, "{}  {}", kind_char, entry.path().display());
                }
            }

            WalkState::Continue
        })
        .expect("walk failed");

    Arc::try_unwrap(counts).map(|c| c.snapshot()).unwrap_or_else(|c| c.snapshot())
}

// Rust ignores SIGPIPE by default, which turns "closed the read end early"
// (e.g. `zlob-walk-cli / | head`) into an ugly panic instead of a quiet
// exit, like every other Unix CLI. Restore the default disposition on
// startup — same fix as ignore-parallel-cli.
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
