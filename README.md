# walkman

**A concurrent filesystem walker for Go, powered by work stealing.**

[![Go Reference](https://pkg.go.dev/badge/github.com/PAKIWASI/walkman.svg)](https://pkg.go.dev/github.com/PAKIWASI/walkman)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Work Stealing Pool](https://img.shields.io/badge/Pool-workstealpool-blue)](https://github.com/PAKIWASI/workstealpool)

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
- [Roadmap](#roadmap)
- [LLM Usage](#llm-usage)

## Features

- Concurrent traversal via a work-stealing pool, worker count configurable (defaults to `GOMAXPROCS`)
- Streaming results through a channel, one `WalkResult` per directory
- Per-directory error reporting that doesn't abort unrelated work
- Skip entries by name (prunes matching directories), optional max depth, optional symlink following
- Benchmark suite vs Rust's parallel `ignore::WalkParallel`, on the linux kernel source tree (or any other)

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

> **Drain before you wait.** `Walk`'s channel stays open until all queued work completes, consume it fully then call `Wait()` for the terminal pool error. Per-entry errors live inside `WalkResult.Errs` and don't abort the rest of the traversal. A directory can list successfully (`Entries` populated) while individual entries inside it (a dangling symlink, a detected cycle) still show up as their own `DirErr`.

## API

### `NewWalkman`

```go
func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) *Walkman
```

Defaults: `PoolSize = GOMAXPROCS`, `InitialWorkerCap = 32`, `ResultBuffSize = 128`.

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

Blocks until the pool finishes, returns the first fatal pool-level error.

## Traversal semantics

| Aspect | Behavior |
| --- | --- |
| **Ordering** | Completion-ordered, not path-sorted. Layer sorting/BFS on top of the stream if you need it. |
| **Skip list** | Matches entry names at every depth (e.g. `.git`, `node_modules`), a match prunes the whole subtree. |
| **Max depth** | `0` = unlimited; otherwise the walker won't descend past that depth. |
| **Symlinks** | Not followed by default. When enabled, resolves via `os.Stat` and descends if the target is a directory. A detected cycle is reported as a `DirErr` (`walkman.ErrSymlinkCycle`) on the offending entry. Dangling symlinks are reported as `walkman.ErrDanglingSymlink`. |
| **Errors** | A directory that fails to open reports its error as the sole `DirErr` in its own `WalkResult`, other workers keep going. |

## How it works

Each task is intentionally small. The leaf has the full path to the `walkItem` and a pointer to its direct ancestor (used for cycle detection if symlinks are enabled):

```go
type walkItem struct {
    depth uint32
    leaf  *pathNode
}

type pathNode struct {
    path   string
    parent *pathNode
}
```

Path strings are efficiently stored in per-worker heap arena buffers (`stringStore`) to minimize heap allocations during traversal.

For every directory: open it, read entries with `File.ReadDir(-1)`, drop skipped names, classify entries, optionally resolve symlinks, emit a `WalkResult`, and push child directories back onto the pool.

The work-stealing pool itself lives in [`github.com/PAKIWASI/workstealpool`](https://github.com/PAKIWASI/workstealpool).

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


## Benchmarks

`walkman` (Go) vs Rust's parallel `ignore::WalkParallel` (the ripgrep walker), walking a real-world tree instead of a synthetic one: the **Linux kernel source** (`linux-7.2.2`, 6,203 dirs / 94,757 files / 99 symlinks, no git history), on an Intel i5-1135G7 (4C/8T). Workers swept at 1/2/4/8, `hyperfine --min-runs 10 --warmup 5` for wall-clock, `perf stat` in parallel for scheduling/cache behavior. Run twice: once plain, once with `--follow-links` via `run_all.sh` / `run_all_sym.sh`.

**Wall-clock mean (`hyperfine`):**

| Workers | walkman | ignore | walkman (symlinks) | ignore (symlinks) |
| ---: | ---: | ---: | ---: | ---: |
| 1 | **72.8 ms** | 96.3 ms | **81.3 ms** | 108.6 ms |
| 2 | **41.8 ms** | 46.6 ms | **50.2 ms** | 49.3 ms |
| 4 | **27.1 ms** | 29.3 ms | **30.1 ms** | 29.3 ms |
| 8 | **21.7 ms** | 29.0 ms | **22.9 ms** | 27.7 ms |

walkman is faster at every worker count in both modes, with the biggest margins at 1 worker (~1.3x) and 8 workers (~1.3-1.4x); the 4-worker gap is the narrowest and per the `perf` runs below, it's the least statistically clean.

**Scaling (speedup relative to each tool's own 1-worker time, no symlinks):**

| Workers | walkman speedup | walkman efficiency | ignore speedup | ignore efficiency |
| ---: | ---: | ---: | ---: | ---: |
| 2 | 1.74x | 87% | 2.07x | 103% |
| 4 | 2.69x | 67% | 3.28x | 82% |
| 8 | 3.36x | 42% | 3.32x | 42% |

`ignore` scales more efficiently from 1→4 workers, but flatlines from 4→8 (3.28x → 3.32x, essentially no gain). `walkman` scales less efficiently early on but keeps improving through 8 workers, which is why it re-opens a lead there instead of hitting a wall.

**Scheduling & memory behavior (`perf stat`, mean of 10 runs, no symlinks):**

| Workers | walkman ctx-switches | ignore ctx-switches | walkman cache-misses | ignore cache-misses | walkman instructions | ignore instructions |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 6,400 | 2 | 711K | 450K | 437M | 463M |
| 2 | 3,162 | 18 | 746K | 463K | 406M | 466M |
| 4 | 1,624 | 58 | 816K | 510K | 397M | 470M |
| 8 | 604 | 106 | 793K | 495K | 403M | 475M |

Even at 1 worker, walkman racks up thousands of context switches (Go's goroutine/channel machinery runs regardless of worker count) against `ignore`'s near-zero — and pays for it in higher cache-misses at every worker count. It still wins on wall-clock because it executes fewer total instructions throughout: the core traversal path is leaner even though the concurrency scaffolding around it isn't free. CPU migrations tell a related story at 8 workers — walkman averages 85 vs `ignore`'s 3, i.e. Go's scheduler bounces goroutines across cores far more than `ignore`'s threads, which is the likely source of walkman's tapering (not zero) efficiency.

### Conclusion

`walkman` *seems* faster on my machine, both on my fs root and on the linux source. BUT I've seen varying results on different machines/cpus, running the same tests, on the same linux source.
I'm not claiming my tool is "faster than rust", It is in the ballpark though.

## Testing

```bash
go test ./...              # full suite
go test -bench=. -benchmem ./...
```

Covers empty/flat directories, equivalence with `filepath.WalkDir`, skip-list pruning, max-depth, symlinks (following, broken links), permission errors, counts accuracy, consistency across worker counts, and worker parking/wake/termination behavior.

### What's wrong with test -race

The race detector flags a harmless race condition in workstealpool's deque logic. Details in [workstealpool/README.md](https://github.com/PAKIWASI/workstealpool/blob/main/README.md)


## Roadmap

- Deterministic/sorted and breadth-first output (layered on top of the base walker)
- `fs.FS`-based traversal

## LLM Usage

All walkman and workstealpool code is my own. LLM was used to write the rust ignore cli wrapper for benchmarking.
Also used for basic tests (with a lot of modifications afterwards), analysing benchmark output and populating the README's benchmark section.



