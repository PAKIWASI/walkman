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
- [Comparison: walkman vs ignore](#comparison-walkman-vs-ignore)
- [Testing](#testing)
- [Limitations / roadmap](#limitations--roadmap)
- [Project layout](#project-layout)

## Features

- Concurrent traversal via a work-stealing pool, worker count configurable (defaults to `GOMAXPROCS`)
- Streaming results through a channel, one `WalkResult` per directory
- Per-directory error reporting that doesn't abort unrelated work
- Skip entries by name (prunes matching directories), optional max depth, optional symlink following
- Zero-overhead core walker (metrics and counts computed directly in the consumer loop with zero atomics)
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
        for _, e := range result.Errs {
            log.Printf("%s: %s: %v", result.Dir, e.Name, e.Err)
        }
        for _, entry := range result.Entries {
            fmt.Printf("%s/%s\n", result.Dir, entry.Name())
        }
    }

    if err := w.Wait(); err != nil {
        log.Fatal(err)
    }
}
```

> **Drain before you wait.** `Walk`'s channel stays open until all queued work completes — consume it fully, then call `Wait()` for the terminal pool error. Per-entry errors live inside `WalkResult.Errs` and don't abort the rest of the traversal — a directory can list successfully (`Entries` populated) while individual entries inside it (a dangling symlink, a detected cycle) still show up as their own `DirErr`.

## API

### `NewWalkman`

```go
func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) *Walkman
```

Defaults: `PoolSize = GOMAXPROCS`, `InitialWorkerCap = 32`, `ResultBuffSize = 64`.

### `NewWalkmanWithConfig`

```go
func NewWalkmanWithConfig(followLinks bool, maxDepth uint32, skipList []string, pc PoolConfig) *Walkman

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
    Dir     string
    Entries []fs.DirEntry // this directory's direct entries
    Errs    []DirErr      // zero or more per-entry problems from this directory
}

type DirErr struct {
    Name string // the entry that caused the error
    Err  error
}
```

Child directories are scheduled as separate tasks, not included in `Entries`. `Entries` and `Errs` aren't mutually exclusive: a directory can list successfully while individual entries inside it (a dangling symlink, a detected cycle) are reported as their own `DirErr` alongside the otherwise-complete `Entries`. `Entries` is nil only when the directory itself couldn't be opened at all, in which case `Errs` holds exactly that one failure.

### `Wait`

```go
func (w *Walkman) Wait() error
```

Blocks until the pool finishes; returns the first fatal pool-level error.

## Traversal semantics

| Aspect | Behavior |
| --- | --- |
| **Ordering** | Completion-ordered, not path-sorted. Layer sorting/BFS on top of the stream if you need it. |
| **Skip list** | Matches entry names at every depth (e.g. `.git`, `node_modules`), a match prunes the whole subtree. |
| **Max depth** | `0` = unlimited; otherwise the walker won't descend past that depth. |
| **Symlinks** | Not followed by default. When enabled, resolves via `os.Stat` and descends if the target is a directory. A detected cycle is reported as a `DirErr` (`errSymlinkCycle`) on the offending entry. |
| **Errors** | A directory that fails to open reports its error as the sole `DirErr` in its own `WalkResult`, other workers keep going. |

## How it works

Each task is intentionally small:

```go
type walkItem struct {
    path  string
    depth uint32
    // only used when --follow-links is enabled
    parent *ancestor
}
```

For every directory: open it, read entries with `File.ReadDir(-1)`, drop skipped names, classify entries, optionally resolve symlinks, emit a `WalkResult`, and push child directories back onto the pool.

The pool itself lives in [`github.com/PAKIWASI/workstealpool`](https://github.com/PAKIWASI/workstealpool).

## CLI tools

A Go CLI (`main/`) and a Rust reference CLI share matching options for comparison:

- `rust_ignore_parallel/`, wrapping the [`ignore`](https://crates.io/crates/ignore) crate's `WalkBuilder::build_parallel()` (the walker ripgrep uses): a multi-threaded walker, and the relevant comparison for `walkman` since both are concurrent. All of `ignore`'s ripgrep-style filtering (`.gitignore`, hidden files, git excludes) is explicitly disabled so it walks the raw tree, same as `walkman`.

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
cd rust_ignore_parallel && cargo build --release    # produces build/ignore-parallel-cli
```

`ignore-parallel-cli` takes `--workers N` (thread count, `0` = `available_parallelism`), matching `walkman`'s flag so it sweeps the same way in `bench_harness.sh`.


## Benchmarks

`walkman` (Go) vs Rust's parallel `ignore::WalkParallel` (the ripgrep walker), across three synthetic tree shapes, Intel i5-1135G7 (4C/8T). Rust's *sequential* `walkdir` crate isn't in the repo anymore — walkman beat it decisively and consistently in every early run, which stopped being interesting once `ignore::WalkParallel` was in the mix as the real competitor.

| Shape | Dirs | Files | What it stresses |
| --- | ---: | ---: | --- |
| `wide` | 1,884 | 11,304 | shallow, high branching — lots of independent, stealable work |
| `deep` | 16,382 | 32,764 | deep, low branching — long dependency chains, little to steal |
| `mixed` | 19,530 | 78,120 | moderate depth/branching — closer to a real source tree |

There's also a symlink-focused sweep (`run_all_sym.sh`) that builds the same three shapes with 200 symlinks scattered in (including deliberate cycles) and benchmarks both CLIs with `--follow-links` on, to exercise the cycle-detection path rather than just the plain walk.

**Wall-clock mean, ms (`hyperfine`, full process per run, `--min-runs 10 --warmup 5`):**

*No symlinks:*

| Workers | `wide` walkman | `wide` ignore | `deep` walkman | `deep` ignore | `mixed` walkman | `mixed` ignore |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | **15.0** | 22.4 | 88.9 | **81.4** | 114.0 | **103.6** |
| 4 | **5.9** | 9.9 | 31.7 | **24.3** | 41.4 | **29.3** |
| 8 | **8.6** | 9.4 | **24.7** | 27.2 | 31.6 | **27.5** |

*With `--follow-links` (200 symlinks/tree, cycles included):*

| Workers | `wide` walkman | `wide` ignore | `deep` walkman | `deep` ignore | `mixed` walkman | `mixed` ignore |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | **19.6** | 22.0 | 106.9 | **88.0** | 139.6 | **109.3** |
| 4 | **7.8** | 12.6 | 38.5 | **25.1** | 45.6 | **31.4** |
| 8 | **10.6** | 11.1 | **29.4** | 23.3 | 34.2 | **27.6** |

Takeaways:
- **`wide`** — walkman wins outright at every worker count, symlinks or not.
- **`deep`/`mixed`** — `ignore` is ahead at 1–4 workers; on `deep` walkman catches up and wins by 8 workers, on `mixed` `ignore` stays ahead throughout.
- **Known anomaly:** on `wide` with symlinks, walkman gets *slower* going from 4→8 workers (7.8ms → 10.6ms), reproduced across two independent runs with tight variance (not noise). No such regression on the no-symlinks `wide` sweep. Likely contention in the cycle-detection path under 8-way concurrency on a shallow tree; not yet root-caused.

**Allocation pressure (`perf stat`, workers=1, mean of 10 runs, no symlinks):**

| Shape | walkman page-faults | ignore page-faults | walkman cache-misses | ignore cache-misses |
| --- | ---: | ---: | ---: | ---: |
| `wide` | 169 | 111 | 248K | 126K |
| `deep` | 401 | 111 | 671K | 452K |
| `mixed` | 491 | 112 | 1.04M | 776K |

walkman faults and cache-misses more than `ignore` at every shape, and the gap widens with tree size. Likely explanation for `ignore` pulling ahead as trees get deeper/wider: whatever allocation `walkman` does per directory doesn't stay flat.

Reproduce with:

```bash
cd test
./run_all.sh \
  --walkman          ../build/main \
  --ignore-parallel  ../build/ignore-parallel-cli \
  --workers          "1,4,$(nproc)" \
  --runs             10 \
  --out-dir          bench_results

./run_all_sym.sh \
  --walkman          ../build/main \
  --ignore-parallel  ../build/ignore-parallel-cli \
  --workers          "1,4,$(nproc)" \
  --runs             10 \
  --links            200 \
  --out-dir          bench_results_symlinks
```

Or run one shape/harness at a time with `build_tree.sh` + `bench_harness.sh` (hyperfine) / `perf_bench.sh` (perf counters). See each script's `--help`.

## Comparison: walkman vs ignore

| Aspect | `walkman` (Go) | `ignore::WalkParallel` (Rust) |
| --- | --- | --- |
| **Delivery Model** | **Directory-batched streaming** (`<-chan WalkResult`) | **Entry-level parallel visitor** (`ParallelVisitor` callback) |
| **Consumer Execution** | Single-goroutine stream consumption | Concurrent in-thread callbacks across worker threads |
| **Directory Context** | Complete `[]fs.DirEntry` slice per directory | Dispersed individual file/dir entries |
| **Memory / Allocations** | Per-directory slice allocations & channel buffers | In-place buffer reuse, minimal heap churn |
| **Best-fit Workloads** | Wide trees, directory-level indexing/aggregation | Deep trees, file-level filtering/grep searching |

### Strengths & Weaknesses

#### `walkman`
- **Strengths:**
  - **Directory-level autonomy:** Caller receives all entries of a directory together, making folder-level aggregation, custom batch filtering, and batch I/O simple without cross-thread coordination.
  - **Wide-tree throughput:** Work-stealing pool keeps all CPU cores saturated on wide, shallow trees, consistently beating `ignore` across worker counts.
  - **Clean consumer consumption:** Channel-based consumption eliminates mutex and atomic contention in consumer code.
- **Weaknesses:**
  - **Allocation & channel overhead:** Streaming per-directory slices across channels increases page faults and cache misses on deep/narrow trees.
  - **Completion-ordered:** Results arrive non-deterministically as directory tasks complete.

#### `ignore`
- **Strengths:**
  - **Minimal memory pressure:** Reuses thread-local buffers, resulting in fewer page faults and lower cache misses on deep and mixed trees.
  - **Optimized for item filtering:** Built for ripgrep-style search where individual files are filtered independently inside worker threads.
- **Weaknesses:**
  - **No directory batching:** Does not expose whole directory contents at once; grouping entries by directory requires user-managed thread synchronization.
  - **Thread-safety burden:** Aggregation or counting in caller code must be synchronized across worker threads (e.g. via atomics or mutexes).

## Testing

```bash
go test ./...              # full suite
go test -race ./...        # race detector
go test -bench=. -benchmem ./...
```

Covers empty/flat directories, equivalence with `filepath.WalkDir`, skip-list pruning, max-depth, symlinks (following, broken links), permission errors, counts accuracy, consistency across worker counts, and worker parking/wake/termination behavior.

## Limitations / roadmap

Deliberately deferred so far:

- Deterministic/sorted or breadth-first output
- `iter.Seq`-based iteration, `FilterEntry`-style filtering, `ContentsFirst` ordering
- A cap on simultaneously open directories
- `fs.FS`-based traversal

Ordering/scheduling policy is kept out of the core walker on purpose: sorted or BFS output needs buffering, which belongs above the streaming core, not inside it.

## Project layout

```text
walkman/
├── walkman.go                      # core concurrent walker
├── walkman_test.go                 # correctness + concurrency tests
├── walkman_bench_test.go           # benchmark suite
├── walkman_worker_sweep_test.go    # WorkerSweep benchmark (benchstat-friendly)
├── main/main.go                    # Go CLI / benchmark wrapper
├── rust_ignore_parallel/src/main.rs # comparable Rust ignore::WalkParallel wrapper
├── build/                          # built binaries (main, ignore-parallel-cli)
├── test/
│   ├── build_tree.sh               # synthetic tree generator (wide/deep/mixed)
│   ├── bench_harness.sh            # hyperfine wall-clock sweep
│   ├── perf_bench.sh               # perf-counter sweep
│   ├── run_all.sh                  # runs both harnesses across all three shapes
│   ├── run_all_sym.sh              # symlink-focused counterpart (--follow-links, cycles)
│   ├── bench_results/              # recorded CSVs + run_manifest.txt (no symlinks)
│   └── bench_results_symlinks/     # recorded CSVs + run_manifest.txt (--follow-links)
└── Roadmap.md                      # feature roadmap / implementation notes
```

## License

[MIT](LICENSE)
