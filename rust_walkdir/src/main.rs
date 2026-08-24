// walkdir-cli: a thin CLI wrapper around the `walkdir` crate, built to be
// directly comparable to the Go `walkman` package (github.com/PAKIWASI/walkman)
// and its benchmarks in walkman_bench_test.go.
//
// Mirrors walkman's semantics:
//   - skip list: entries whose *name* matches are pruned (subtree skipped for dirs)
//   - max-depth: 0 = unlimited, like walkman's maxDepth
//   - follow-links: off by default
//   - errors (permission denied, broken symlink, etc.) are counted, not fatal
//
// Usage:
//   walkdir-cli [OPTIONS] [ROOT]
//
// Options:
//   --max-depth N      0 = unlimited (default: 0)
//   --follow-links     follow symlinks (default: off)
//   --skip a,b,c       comma-separated names to prune (like walkman's skipList)
//   --print            print every entry (like main.go does) instead of just counting
//   --bench N          repeat the walk N times, report min/avg/max wall time (ns/op style)
//   --quiet            suppress the summary line (useful with --bench)
//
// Examples:
//   walkdir-cli /                              # count files/dirs under /, print summary
//   walkdir-cli --skip .git,node_modules /path # prune like walkman's skipList
//   walkdir-cli --bench 10 /path                # 10 timed runs, min/avg/max, comparable
//                                                # to `go test -bench=Walk_Sequential`

use std::env;
use std::process::ExitCode;
use std::time::{Duration, Instant};
use walkdir::WalkDir;

struct Opts {
    root: String,
    max_depth: usize, // 0 = unlimited
    follow_links: bool,
    skip: Vec<String>,
    print: bool,
    bench: Option<u32>,
    quiet: bool,
}

fn parse_args() -> Result<Opts, String> {
    let mut root = ".".to_string();
    let mut max_depth = 0usize;
    let mut follow_links = false;
    let mut skip = Vec::new();
    let mut print = false;
    let mut bench = None;
    let mut quiet = false;

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
            "-h" | "--help" => {
                print_help();
                std::process::exit(0);
            }
            other if !other.starts_with('-') => root = other.to_string(),
            other => return Err(format!("unknown flag: {other}")),
        }
    }

    Ok(Opts { root, max_depth, follow_links, skip, print, bench, quiet })
}

fn print_help() {
    println!(
        "walkdir-cli - Rust walkdir wrapper, comparable to Go's walkman\n\n\
         USAGE:\n    walkdir-cli [OPTIONS] [ROOT]\n\n\
         OPTIONS:\n\
         \x20   --max-depth N     0 = unlimited (default: 0)\n\
         \x20   --follow-links    follow symlinks (default: off)\n\
         \x20   --skip a,b,c      comma-separated names to prune\n\
         \x20   --print           print every entry\n\
         \x20   --bench N         repeat N times, report timing stats\n\
         \x20   --quiet           suppress summary line"
    );
}

struct Counts {
    files: u64,
    dirs: u64,
    links: u64,
    errors: u64,
}

fn run_walk(opts: &Opts) -> Counts {
    let mut counts = Counts { files: 0, dirs: 0, links: 0, errors: 0 };

    // min_depth(1): exclude the root itself from the count, matching
    // walkman's semantics (the root is never "found", only its children
    // are, via WalkResult.Ret) — see walkman_bench_test.go's
    // walkDirSequential, which applies the same `path == root` skip.
    let mut walker = WalkDir::new(&opts.root).follow_links(opts.follow_links).min_depth(1);
    if opts.max_depth != 0 {
        walker = walker.max_depth(opts.max_depth);
    }

    let skip = opts.skip.clone();

    let iter = walker.into_iter().filter_entry(move |e| {
        // Mirror walkman's skipList: prune by bare name, applied at every depth.
        if let Some(name) = e.file_name().to_str() {
            !skip.iter().any(|s| s == name)
        } else {
            true
        }
    });

    for entry in iter {
        match entry {
            Ok(e) => {
                let ft = e.file_type();
                if ft.is_dir() {
                    counts.dirs += 1;
                } else if ft.is_symlink() {
                    counts.links += 1;
                } else {
                    counts.files += 1;
                }
                if opts.print {
                    let kind = if ft.is_dir() { "d" } else if ft.is_symlink() { "l" } else { "f" };
                    println!("{}  {}", kind, e.path().display());
                }
            }
            Err(err) => {
                counts.errors += 1;
                if opts.print {
                    eprintln!("error: {err}");
                }
            }
        }
    }

    counts
}

// Rust ignores SIGPIPE by default, which turns "closed the read end early"
// (e.g. `walkdir-cli / | head`) into an ugly panic instead of a quiet exit,
// like every other Unix CLI. Restore the default disposition on startup.
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
        let mut last = Counts { files: 0, dirs: 0, links: 0, errors: 0 };
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
