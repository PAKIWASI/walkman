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
- Benchmark suite vs `filepath.WalkDir` and Rust's parallel `ignore::WalkParallel` (the ripgrep walker), plus a synthetic tree generator (wide/deep/mixed). Rust's sequential `walkdir` crate was also benchmarked early on — walkman won decisively and consistently — and has since been dropped from the ongoing suite; see [Benchmarks](#benchmarks).

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

Same deterministic wide tree (**1,884 dirs, 11,304 files**), Intel i5-1135G7, measured two ways.

> **Note on Rust's sequential `walkdir` crate:** it was included in early benchmark runs as an initial sanity baseline. walkman beat it decisively and consistently across every tree shape and worker count tested — see the historical numbers in the collapsed section below. Since it's single-threaded by design, "walkman beats a sequential walker" isn't an interesting result to keep re-measuring, so `walkdir-cli` has been removed from the benchmark suite, test harnesses, and build tooling. The comparison that actually matters, and the only one still tracked going forward, is walkman vs Rust's **parallel** walker, `ignore::WalkParallel`.

<details>
<summary>Historical walkdir-cli numbers (no longer run; kept for reference)</summary>

| Walker | Workers | Mean |
| --- | ---: | ---: |
| Rust `walkdir` (sequential) | seq | 17.4 ms |
| `walkman` | 4 | 5.2 ms |

walkman was ~3.3x faster than sequential `walkdir` at its best worker count, on this tree. Full historical breakdown available in prior commits' README revisions and `test/*.log`.

</details>

**CLI (`hyperfine`, full process per run):**

| Walker | Workers | Mean |
| --- | ---: | ---: |
| Rust `ignore::WalkParallel` | 1 | 19.7 ms |
| Rust `ignore::WalkParallel` | 2 | 17.7 ms |
| Rust `ignore::WalkParallel` | 4 | 20.2 ms |
| Rust `ignore::WalkParallel` | 8 | 21.3 ms |
| `walkman` | 1 | 12.1 ms |
| `walkman` | 2 | 7.3 ms |
| `walkman` | 4 | **5.2 ms** |
| `walkman` | 8 | 7.6 ms |

**In-process (`go test -bench=WorkerSweep -count=10`):**

| Workers | Mean |
| --- | ---: |
| 1 | 11.9 ms |
| 2 | 6.9 ms |
| 4 | 4.2 ms |
| 8 | **3.2 ms** |

CLI timing plateaus/regresses past 4 workers (process overhead dominates); the in-process pool keeps scaling to 8. `ignore::WalkParallel` doesn't scale on this tree at all — its wall-clock is flat-to-worse from 1→8 workers while its own CPU time climbs steadily (9 → 20 → 40 → 57 ms user), the signature of fixed per-call thread-pool spin-up/teardown cost that a tree this size can't amortize.

Reproduce with:

```bash
./test/build_tree.sh --shape wide --root /tmp/tree_wide --seed 42
./test/bench_harness.sh \
  --walkman          ./build/main \
  --ignore-parallel  ./build/ignore-parallel-cli \
  --tree             /tmp/tree_wide \
  --workers          "1,2,4,8" \
  --out              test/results_wide.csv
WALKMAN_BENCH_ROOT=/tmp/tree_wide go test -bench=WorkerSweep -benchmem -count=10 ./...
```

`ignore-parallel-cli` build pin (see `rust_ignore_parallel/Cargo.toml`): built with `rustc`/`cargo` 1.75.0, `ignore = "=0.4.16"`, `globset` pinned to `0.4.16` — newer releases of both require the `edition2024` Cargo feature (~1.85+ toolchain). `--release` profile, `opt-level = 3`, `lto = true`.

Raw logs are in `test/*.log`.

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
├── build/build_tree.sh             # simple synthetic-tree generator
├── test/                           # benchmark harness + recorded logs
└── help.md                         # project notes / implementation plan
```

## License

[MIT](LICENSE)
