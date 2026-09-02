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
- [Channels, not callbacks](#channels-not-callbacks)
- [CLI tools](#cli-tools)
- [Benchmarks](#benchmarks)
- [Testing](#testing)
- [Roadmap](#roadmap)
- [LLM Usage](#llm-usage)

## Features

- Concurrent traversal via a work-stealing pool, worker count configurable (defaults to `GOMAXPROCS`)
- Streaming results through a channel, one `WalkResult` per directory — pull-based like an iterator, not a callback
- Per-directory error reporting that doesn't abort unrelated work
- Skip entries by name (prunes matching directories), optional max depth, optional symlink following
- Benchmark suite vs Rust's parallel `ignore::WalkParallel` and [fastwalk](https://github.com/charlievieth/fastwalk), on the linux kernel source tree

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

## Channels, not callbacks

`walkman` is **channel-based**, no callbacks. `Walk` returns `<-chan WalkResult` immediately, and you consume it with an ordinary `for range`. It behaves like a pull iterator over the tree rather than a push callback:

```go
for result := range w.Walk(root) {
    // runs on your goroutine, at your pace
}
```

This has a few consequences:

- **No shared-state synchronization in your code.** A callback that's invoked concurrently from N workers has to protect anything it touches (counters, buffers, output) with atomics or a mutex, even for something as simple as counting files. A channel consumer is single-threaded by construction, increment a plain `int`, no `sync/atomic` required.
- **Natural backpressure.** Workers block on a full result channel (bounded by `ResultBuffSize`) until you drain it, so a slow consumer throttles the walk instead of the walker racing ahead and burning memory queuing up callback results you haven't gotten to yet.
- **Composability with `select`.** Because it's a channel, it drops straight into `select` alongside a `context.Done()`, a timeout, or another channel — cancel a walk mid-traversal without threading a `context.Context` check into every callback invocation.
- **Stop early for free.** `break` out of the `for range` and the pool stops feeding you more results; a callback-based walker needs you to return a sentinel error (like `filepath.SkipAll`) from inside the callback to achieve the same thing.


## CLI tools

A Go CLI (`main/`) that provides example usage of `walkman`.

```bash
go build -o build/walkman ./walkman
./build/main --workers 8 --skip .git,node_modules ~/projects/proj
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

`walkman` vs Rust's parallel `ignore::WalkParallel` (the ripgrep walker) vs `fastwalk`, walking the **Linux kernel source** (`linux-7.2.2`, 6,203 dirs / 94,757 files / 99 symlinks). Tested on my laptop: **Intel i5-1135G7 (4C/8T)**, Artix Linux (kernel 7.1.9), via `hyperfine --min-runs 10 --warmup 5`.

**Mean wall-clock (ms), no symlinks:**

| Workers | walkman | ignore-parallel | fastwalk |
| ---: | ---: | ---: | ---: |
| 1 | **72** | 101 | 76 |
| 2 | **44** | 54 | 49 |
| 4 | 29 | **26** | 35 |
| 8 | **22** | 26 | 26 |

**Mean wall-clock (ms), following symlinks:**

| Workers | walkman | ignore-parallel | fastwalk |
| ---: | ---: | ---: | ---: |
| 1 | **81** | 105 | 81 |
| 2 | 51 | **50** | 54 |
| 4 | 31 | **29** | 38 |
| 8 | **24** | 29 | 33 |


### Conclusion

`walkman` *seems* faster on my machine, both on my fs root and on the linux source. BUT I've seen varying results on different machines/cpus, running the same tests, on the same linux source.
I'm not claiming my tool is "faster than ignore or fastwalk", It is in the ballpark though.

## Testing

```bash
go test ./...              # full suite
go test -bench=. -benchmem ./...
```

### What's wrong with `go test -race`

Running `go test -race` on high-concurrency configurations (such as `TestWalk_ConsistentAcrossPoolSizes` under heavy oversubscription with tiny buffers) may occasionally report a data race inside the underlying `workstealpool` library.
This is a known, benign artifact of `workstealpool`'s unboxed Chase-Lev circular ring buffer: when the ring buffer wraps around, ThreadSanitizer flags the physical slot reuse as a race on unboxed struct values (`walkItem`), even though the slot is only reused after the thief has already finished with it.
`walkman`'s internal data structures (including path arena storage, cycle detection sets, and error lists) are completely race-free. For a full technical explanation of the lock-free deque mechanics, see [workstealpool/README.md](https://github.com/PAKIWASI/workstealpool#known-limitation-benign-race-under--race).


## Roadmap

- Deterministic/sorted and breadth-first output (layered on top of the base walker)
- `fs.FS`-based traversal

## LLM Usage

All walkman and workstealpool code is my own. LLM was used to write the rust ignore cli wrapper for benchmarking.
Also used for basic tests (with a lot of modifications afterwards), analysing benchmark output and populating the README's benchmark section.
