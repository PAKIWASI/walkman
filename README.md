# walkman

**A concurrent filesystem walker for Go, powered by work stealing.**

[![Go Reference](https://pkg.go.dev/badge/github.com/PAKIWASI/walkman.svg)](https://pkg.go.dev/github.com/PAKIWASI/walkman)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **Status:** experimental / actively evolving. Core walker, tests, and benchmarks are in place; API not yet stable.

`walkman` turns each directory into an independent task and runs those tasks on a configurable work-stealing pool. Idle workers steal pending directories from busier ones, so on **wide trees** with lots of independent directories, more CPU stays busy than a sequential walk allows.

## Contents

- [Features](#features)
- [Installation](#installation)
- [Basic usage](#basic-usage)
- [API](#api)
- [Traversal semantics](#traversal-semantics)
- [How it works](#how-it-works)
- [CLI tools](#cli-tools)
- [Benchmarks](#benchmarks)
- [Testing](#testing)
- [Limitations / roadmap](#limitations--roadmap)
- [Project layout](#project-layout)

## Features

- Concurrent traversal via a work-stealing pool, worker count configurable (defaults to `GOMAXPROCS`)
- Streaming results through a channel, one `WalkResult` per directory
- Per-directory error reporting that doesn't abort unrelated work
- Skip entries by name (prunes matching directories), optional max depth, optional symlink following
- Optional atomic traversal statistics
- Benchmark suite vs Rust's parallel `ignore::WalkParallel`, with a synthetic tree generator across wide/deep/mixed shapes — see [Benchmarks](#benchmarks)

## Installation

```bash
go get github.com/PAKIWASI/walkman
```

## Basic usage

```go
package main

import (
    "fmt"
    "log"

    "github.com/PAKIWASI/walkman"
)

func main() {
    w := walkman.NewWalkman(
        false, // follow symlinks
        0,     // max depth: 0 = unlimited
        nil,   // names to skip
    )

    for result := range w.Walk(".") {
        if result.Err != nil {
            log.Printf("%s: %v", result.Dir, result.Err)
            continue
        }
        for _, entry := range result.Ret {
            fmt.Printf("%s/%s\n", result.Dir, entry.Name())
        }
    }

    if err := w.Wait(); err != nil {
        log.Fatal(err)
    }
}
```

> **Drain before you wait.** `Walk`'s channel stays open until all queued work completes — consume it fully, then call `Wait()` for the terminal pool error. A per-directory error lives inside its `WalkResult` and doesn't abort the rest of the traversal.

## API

### `NewWalkman`

```go
func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) *Walkman
```

Defaults: `PoolSize = GOMAXPROCS`, `InitialWorkerCap = 32`, `ResultBuffSize = 64`, stats disabled.

### `NewWalkmanWithConfig`

```go
func NewWalkmanWithConfig(followLinks bool, maxDepth uint32, skipList []string, trackStats bool, pc PoolConfig) *Walkman

type PoolConfig struct {
    PoolSize         int // number of workers
    InitialWorkerCap int // initial local-queue capacity per worker
    ResultBuffSize   int // result-channel buffer size
}
```

### `Walk`

```go
func (w *Walkman) Walk(root string) <-chan WalkResult

type WalkResult struct {
    Dir string
    Ret []fs.DirEntry // this directory's direct entries
    Err error
}
```

Child directories are scheduled as separate tasks, not included in `Ret`.

### `Wait`

```go
func (w *Walkman) Wait() error
```

Blocks until the pool finishes; returns the first fatal pool-level error.

### `Stats`

```go
func (w *Walkman) Stats() (files, dirs, links, skipped, maxDepthReached uint32)
```

Point-in-time snapshot (atomic, since tasks run concurrently). Zero unless `trackStats` is enabled.

## Traversal semantics

| Aspect | Behavior |
| --- | --- |
| **Ordering** | Completion-ordered, not path-sorted. Layer sorting/BFS on top of the stream if you need it. |
| **Skip list** | Matches entry names at every depth (e.g. `.git`, `node_modules`); a match prunes the whole subtree. |
| **Max depth** | `0` = unlimited; otherwise the walker won't descend past that depth. |
| **Symlinks** | Not followed by default. When enabled, resolves via `os.Stat` and descends if the target is a directory. **No cycle detection yet.** |
| **Errors** | A directory that fails to open reports its error in its own `WalkResult`; other workers keep going. |

## How it works

Each task is intentionally small:

```go
type walkItem struct {
    path  string
    depth uint32
}
```

For every directory: open it, read entries with `File.ReadDir(-1)` (skips the sort `os.ReadDir` does), drop skipped names, classify entries, optionally resolve symlinks, emit a `WalkResult`, and push child directories back onto the pool. Stats stay off by default to avoid atomic-counter overhead on the common path.

The pool itself lives in [`github.com/PAKIWASI/workstealpool`](https://github.com/PAKIWASI/workstealpool).

## CLI tools

A Go CLI (`main/`) and two Rust reference CLIs share matching options for comparison:

- `rust_walkdir/`, wrapping the [`walkdir`](https://crates.io/crates/walkdir) crate — sequential, single-threaded baseline.
- `rust_ignore_parallel/`, wrapping the [`ignore`](https://crates.io/crates/ignore) crate's `WalkBuilder::build_parallel()` (the walker ripgrep itself uses) — a multi-threaded walker, so it's the more relevant comparison for `walkman` than the sequential one is. All of `ignore`'s ripgrep-style filtering (`.gitignore`, hidden files, git excludes) is explicitly disabled so it walks the raw tree, same as `walkdir-cli` and `walkman`.

```bash
go build -o build/main ./main
./build/main --workers 8 --skip .git,node_modules /home/me/project
```

```text
--max-depth N      0 = unlimited
--follow-links     follow symlinks
--skip a,b,c       comma-separated names to prune
--print            print every entry
--bench N          repeat N times and report timing
--quiet            suppress the summary line
--workers N        worker-pool size; 0 = GOMAXPROCS
```

```bash
cd rust_walkdir && cargo build --release            # produces build/walkdir-cli
cd rust_ignore_parallel && cargo build --release    # produces build/ignore-parallel-cli
```

`ignore-parallel-cli` takes the same flags as `walkdir-cli` plus `--workers N` (thread count, `0` = `available_parallelism`), matching `walkman`'s flag so it sweeps the same way in `bench_harness.sh`.


## Benchmarks

`walkman` (Go) vs Rust's parallel `ignore::WalkParallel` (the ripgrep walker), across three synthetic tree shapes, Intel i5-1135G7. Rust's *sequential* `walkdir` crate was dropped from the ongoing suite after early runs — walkman beat it decisively and consistently every time, which stopped being an interesting result to keep re-measuring once a real parallel competitor was in the mix.

| Shape | Dirs | Files | What it stresses |
| --- | ---: | ---: | --- |
| `wide` | 1,884 | 11,304 | shallow, high branching — lots of independent, stealable work |
| `mixed` | 19,530 | 78,120 | moderate depth/branching — closer to a real source tree |
| `deep` | 524,286 | 1,048,572 | deep, low branching — long dependency chains, little to steal |

**Wall-clock mean, ms (`hyperfine`, full process per run, `--min-runs 15 --warmup 5`):**

| Workers | `wide` walkman | `wide` ignore | `mixed` walkman | `mixed` ignore | `deep` walkman | `deep` ignore |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 12.5 | 20.7 | 112.0 | 104.9 | 2,869 | 2,184 |
| 2 | 7.2 | 13.9 | 67.6 | 57.2 | 1,709 | 1,138 |
| 4 | **5.1** | 9.6 | 41.3 | **29.2** | 1,039 | 625 |
| 8 | 7.9 | 9.2 | 32.8 | **28.1** | 829 | **461** |

Takeaways:
- **`wide`** — walkman wins outright at every worker count (best: 5.1ms at 4 workers vs 9.2ms for `ignore` at its best). Both regress past 4 workers here; the tree is too small to amortize more worker-pool overhead.
- **`mixed`** — walkman is ahead at 1 worker, `ignore` pulls ahead and stays ahead from 4 workers on.
- **`deep`** — `ignore` wins at every worker count, and the gap widens with more workers (461ms vs 829ms at 8).

**Allocation pressure (`perf stat`, workers=1, mean of 15 runs):**

| Shape | walkman page-faults | ignore page-faults | walkman cache-misses | ignore cache-misses |
| --- | ---: | ---: | ---: | ---: |
| `wide` | 162 | 106 | 22.3K | 5.9K |
| `mixed` | 483 | 107 | 88.6K | 14.8K |
| `deep` | 4,481 | 108 | 1.23M | 262.7K |

walkman faults and cache-misses noticeably more than `ignore` everywhere, and the gap grows with tree size (~40x more page-faults on `deep`). This is the likely explanation for why `ignore` pulls further ahead as trees get deeper: whatever allocation `walkman` does per directory doesn't stay flat.

> **Data-quality note:** `test/perf_bench.sh` had a regex bug where `task-clock` values ≥1000ms lost their leading digit(s) to perf's thousands-separator formatting (e.g. `2,924.90` parsed as `924.90`) — this only affected the `task_clock_ms` column on the `deep` shape and is now fixed, but the `deep` CPU-time-from-perf numbers in `test/bench_results/perf_deep.csv` predate the fix and can't be recovered after the fact (the lost digits aren't in the file). `context-switches` and `cpu-migrations` also read as a literal `0` across every single run in this environment, including 8-worker runs — almost certainly a sandbox/container restriction on those specific counters rather than genuine zero contention, so those two columns aren't trustworthy either. Wall-clock (`hyperfine`), page-faults, cache-misses, cycles, and instructions are unaffected by both issues. Re-run `test/run_all.sh` to get clean `deep` CPU-time numbers.

Reproduce with:

```bash
cd test
./run_all.sh \
  --walkman          ../build/main \
  --ignore-parallel  ../build/ignore-parallel-cli \
  --workers          "1,2,4,$(nproc)" \
  --runs             15 \
  --out-dir          bench_results
```

Or run one shape/harness at a time with `build_tree.sh` + `bench_harness.sh` (hyperfine) / `perf_bench.sh` (perf counters) — see each script's `--help`.

`ignore-parallel-cli` build pin: `rustc`/`cargo` 1.75.0, `ignore = "=0.4.16"`, `globset` pinned to `0.4.16` (newer releases require the `edition2024` Cargo feature, ~1.85+ toolchain), `--release` profile, `opt-level = 3`, `lto = true`.

## Testing

```bash
go test ./...              # full suite
go test -race ./...        # race detector
go test -bench=. -benchmem ./...
```

Covers empty/flat directories, equivalence with `filepath.WalkDir`, skip-list pruning, max-depth, symlinks (following, broken links), permission errors, stats accuracy, consistency across worker counts, and worker parking/wake/termination behavior.

## Limitations / roadmap

Deliberately deferred so far:

- Deterministic/sorted or breadth-first output
- `iter.Seq`-based iteration, `FilterEntry`-style filtering, `ContentsFirst` ordering
- A cap on simultaneously open directories
- Global symlink-cycle detection
- `fs.FS`-based traversal

Ordering/scheduling policy is kept out of the core walker on purpose: sorted or BFS output needs buffering, which belongs above the streaming core, not inside it.

## Project layout

```text
walkman/
├── walkman.go                      # core concurrent walker
├── walkman_test.go                 # correctness + concurrency tests
├── walkman_bench_test.go           # benchmark suite
├── walkman_worker_sweep_test.go    # WorkerSweep benchmark
├── main/main.go                    # Go CLI / benchmark wrapper
├── rust_walkdir/src/main.rs        # comparable Rust walkdir wrapper (sequential)
├── rust_ignore_parallel/src/main.rs # comparable Rust ignore::WalkParallel wrapper
├── build/                          # built binaries (main, walkdir-cli, ignore-parallel-cli)
├── test/
│   ├── build_tree.sh               # synthetic tree generator (wide/deep/mixed)
│   ├── bench_harness.sh            # hyperfine wall-clock sweep
│   ├── perf_bench.sh               # perf-counter sweep
│   ├── run_all.sh                  # runs both harnesses across all three shapes
│   └── bench_results/              # recorded CSVs + run_manifest.txt
└── Roadmap.md                      # feature roadmap / implementation notes
```

## License

[MIT](LICENSE)
