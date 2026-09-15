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

- Concurrent traversal via a work-stealing pool, worker count configurable (defaults to `runtime.GOMAXPROCS(0)`, i.e. logical/SMT threads)
- Streaming results through a channel, one `DirBatch` per directory — pull-based like an iterator, not a callback
- Per-directory error reporting that doesn't abort unrelated work
- Skip entries by name (prunes matching directories), optional max depth, optional symlink following
- `Entry` implements `fs.DirEntry` (zero-allocation equivalent), with inode numbers from `getdents64`
- Benchmark suite vs Rust's parallel `ignore::WalkParallel`, `fastwalk`, and the `zlob` crate, on the linux kernel source tree

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

> **Drain before you wait.** `Walk`'s channel stays open until all queued work completes, consume it fully then call `Wait()` for the terminal pool error. Each directory produces one `DirBatch`; per-entry errors live inside `DirBatch.Errs` and don't abort the rest of the traversal. A directory can list successfully (`Entries` populated) while individual entries inside it (a dangling symlink, a detected cycle) still show up as their own `DirErr`.

## API

### `NewWalkman`

```go
func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) *Walkman
```

Defaults: `PoolSize = runtime.GOMAXPROCS(0)` (logical/SMT threads), `InitialWorkerCap = 64`, `ResultBuffSize = 256`.

### `NewWalkmanWithConfig`

```go
func NewWalkmanWithConfig(followLinks bool, maxDepth uint32, skipList []string, pc PoolConfig) *Walkman

type PoolConfig struct {
    PoolSize         int // number of workers
    InitialWorkerCap int // initial local-queue capacity per worker
    ResultBuffSize   int // result-channel buffer size
}
```

`DefaultPoolConfig` sizes `PoolSize` to `runtime.GOMAXPROCS(0)` (logical/SMT threads); walkman's workload is syscall- and allocation-heavy, not CPU-bound, and the bench harness showed wall time regressing once workers outnumbered the machine's physical cores — callers who've measured a better number can still set `PoolSize` explicitly.

### `Walk`

```go
func (w *Walkman) Walk(root string) <-chan DirBatch

type DirBatch struct {
    Dir     string   // this directory's full path
    Entries []Entry  // flat slice of this directory's direct entries
    Errs    []DirErr // zero or more per-entry problems from this directory
}

type DirErr struct {
    Name string // the entry that caused the error
    Err  error
}

type Entry struct {
    // Path strings inside Entry are arena-backed and stable for the
    // lifetime of the DirBatch they came from.
}

func (e Entry) Name() string
func (e Entry) IsDir() bool            // per d_type; no stat fallback for DT_UNKNOWN
func (e Entry) Type() fs.FileMode      // maps d_type -> ModeDir / ModeSymlink / 0 / ModeIrregular
func (e Entry) FileMode() fs.FileMode  // same as Type
func (e Entry) Info() (fs.FileInfo, error) // calls os.Lstat on demand
func (e Entry) Ino() uint64            // inode number from getdents64
```

`Entry` implements `fs.DirEntry`, so it drops into any API that wants one.

Child directories are scheduled as separate tasks, not included in `Entries`. `Entries` and `Errs` aren't mutually exclusive: a directory can list successfully while individual entries inside it (a dangling symlink, a detected cycle) are reported as their own `DirErr` alongside the otherwise-complete `Entries`. `Entries` is nil only when the directory itself couldn't be opened at all, in which case `Errs` holds exactly that one failure.

A `DirBatch` is built once by one worker from one `readdir`/`getdents64` call, never re-queued and never stolen. Its `Entries`/`Errs` slices point into that worker's append-only arena, which is never reused, so a batch a consumer is still holding is never overwritten by the next directory that worker handles — the slices stay valid, and unchanged, for as long as the consumer keeps them.

### Sentinel errors

```go
var (
    ErrSymlinkCycle    = errors.New("walkman: symlink cycle")
    ErrDanglingSymlink = errors.New("walkman: dangling/unresolved symlink")
    ErrNoDevInoInfo    = errors.New("walkman: no dev/ino info available")
)
```

`ErrSymlinkCycle` and `ErrDanglingSymlink` are reported via `DirBatch.Errs` when symlink following is on. `ErrNoDevInoInfo` is a portability guard surfaced when a directory's `stat` can't produce dev/ino (e.g. a platform where `getdents64`'s `d_type` isn't reliable and `Stat` can't fill dev/ino for the ancestor chain) — on Linux it is not reachable.

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
| **Symlinks** | Not followed by default. When enabled, resolves each symlink via `os.Stat` and descends if the target is a directory. A detected cycle is reported as a `DirErr` (`walkman.ErrSymlinkCycle`) on the offending entry and dropped; a dangling symlink is reported as `walkman.ErrDanglingSymlink` and dropped. The entry keeps the link's own name and path but reports the **target's** type (so a link-to-dir is `IsDir()==true` and walked at the link's own path). Cycle detection compares `(dev, ino)` against a shared ancestor chain, so it survives work stealing. |
| **Errors** | A directory that fails to open reports its error as the sole `DirErr` in its own `DirBatch`, other workers keep going. |

## How it works

Each task is intentionally small. The leaf carries the full path to the directory, its depth, the device number of the directory it walks, and a pointer into the walk's shared ancestor chain (used for cycle detection when symlinks are enabled):

```go
type walkItem struct {
    path    string   // NUL-terminated arena memory (StringStore)
    depth   uint32
    dev     uint64   // device of this directory; zero only if the root stat failed
    ancestor *ancestorEntry // this directory's own link in the shared chain; nil when followLinks is off
}

type ancestorEntry struct {
    ino, dev uint64
    parent   *ancestorEntry // nil marks the chain root (the walk's starting directory)
}
```

Path strings are efficiently stored in per-worker heap arena buffers (`stores.StringStore`) to minimize heap allocations during traversal; the shared ancestor chain lives in one `stores.GenericStore[ancestorEntry]`, lock-free and safe for concurrent append/read across workers.

For every directory: open it, read entries with `getdents64`, drop skipped names, classify entries by `d_type`, optionally resolve `DT_LNK` entries via `os.Stat`, emit a `DirBatch`, and push child directories back onto the pool. Plain and symlink-following walks use the same pool, the same item type, and the same per-directory read path — only the per-entry task function differs (`visit` vs `visitSym`), bound once at construction when `followLinks` is set.

The work-stealing pool itself lives in [`github.com/PAKIWASI/workstealpool`](https://github.com/PAKIWASI/workstealpool).

## Channels, not callbacks

`walkman` is **channel-based**, no callbacks. `Walk` returns `<-chan DirBatch` immediately, and you consume it with an ordinary `for range`. It behaves like a pull iterator over the tree rather than a push callback:

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
--workers N        worker-pool size; 0 = runtime.GOMAXPROCS(0)
```

```bash
cd rust_ignore_parallel && cargo build --release    # produces build/ignore-parallel-cli
cd rust_zlob_walk         && cargo build --release  # produces build/zlob-walk-cli
```

The Rust CLIs are thin wrappers around `ignore::WalkParallel` and `zlob::walk::WalkBuilder` respectively, built to be directly comparable to the Go `walkman` package — same flags, same output format, same unfiltered semantics (gitignore/hidden/ignore-case all explicitly disabled), so they drop straight into the benchmark harness in `test/`.


## Benchmarks

`walkman` vs Rust's parallel `ignore::WalkParallel` (the ripgrep walker) vs `fastwalk` vs the `zlob` crate's walker, walking the **Linux kernel source** (`linux-7.2.2`, 6,203 dirs / 94,757 files / 99 symlinks). Harness: `test/run_all.sh` (hyperfine + perf counters, 10 runs / 5 warmup, worker sweeps via `--workers`).

### Ryzen 7530U (6C/12T) — 2026-09-15

Host: Linux 7.2.4-arch1-2, AMD Ryzen 5 7530U with Radeon Graphics. Binaries built from the working tree at run time. `DefaultPoolConfig` sizes `PoolSize` to `runtime.GOMAXPROCS(0)`, which is 12 on this box. This pass re-ran `test/run_all.sh` (20 runs / 5 warmup, workers 1/2/4/8/10/12) and `test/run_all_sym.sh` (10 runs / 5 warmup, workers 1/2/4/6/8/10/12) with all three CLIs built, so `zlob-walk` is included in the plain-walk sweep below; it still has no follow-links mode, so it's absent from the symlinks sweep.

Numbers below are the **median** of the per-run `perf stat` wall-clock samples.

**Median wall-clock (ms), no symlinks followed:**

| Workers | walkman | ignore-parallel | zlob-walk |
| ---: | ---: | ---: | ---: |
| 1 | **83** | 96 | 65 |
| 2 | 40 | 51 | **35** |
| 4 | 22 | 28 | **19** |
| 8 | 15 | 20 | **12** |
| 10 | 13 | 18 | **11** |
| 12 | 13 | 18 | **10** |

**Median wall-clock (ms), following symlinks:**

| Workers | walkman | ignore-parallel |
| ---: | ---: | ---: |
| 1 | 89 | **98** |
| 2 | 48 | **56** |
| 4 | 28 | **30** |
| 6 | 22 | **23** |
| 8 | **19** | 22 |
| 10 | **19** | 20 |
| 12 | **16** | 35 |


### Intel i5-1135G7 (4C/8T) — 2026-09-15

Host: Linux 7.2.2-artix1-1.1, 11th Gen Intel(R) Core(TM) i5-1135G7 @ 2.40GHz. `hyperfine --min-runs 10 --warmup 5`, `test/run_all.sh`, worker sweep 1/2/4/8. This pass's binary set was `walkman`, `ignore-parallel`, and `zlob-walk` — `fastwalk` wasn't built for this run, and `zlob-walk` has no follow-links mode, so this pass covers plain-walk only. The follow-links/`fastwalk` table below is carried over from an earlier pass on the same machine and was not re-measured here.

**Mean wall-clock (ms), no symlinks — hyperfine:**

| Workers | walkman | ignore-parallel | zlob-walk |
| ---: | ---: | ---: | ---: |
| 1 | **55** | 95 | 64 |
| 2 | 28 | 45 | **25** |
| 4 | 16 | 30 | **14** |
| 8 | **14** | 31 | 15 |

**Mean wall-clock (ms), following symlinks, hyperfine (earlier pass, kernel 7.1.9, includes `fastwalk`; not re-measured in the 2026-09-15 run above):**

| Workers | walkman | ignore-parallel | fastwalk |
| ---: | ---: | ---: | ---: |
| 1 | 81 | 105 | 81 |
| 2 | 51 | **50** | 54 |
| 4 | 31 | **29** | 38 |
| 8 | **24** | 29 | 33 |

### Which is faster

`walkman` beats the rust (`ignore`) and go (`fastwalk`) implementations, but is slower than the zig (`zlob`) implementation.
I used up all of my C wizardry (just througing arena/chunked storage at the problem lol) and this is as fast as I can make it, for now.


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
- `zlob` like globbing (but we don't have simd).

## LLM Usage

All walkman and workstealpool code is my own. LLM was used to help write the rust cli wrappers for benchmarking.
Also used for basic tests (with a lot of modifications afterwards), analysing benchmark output and populating the README's benchmark section.
