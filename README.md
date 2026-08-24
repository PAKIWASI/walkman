# walkman

**A concurrent filesystem walker for Go, powered by work stealing.**

`walkman` walks a directory tree by turning each directory into an independent task and executing those tasks on a configurable work-stealing pool. Instead of recursively processing one branch at a time, workers can steal pending directories from one another, keeping available CPU workers busy when the filesystem exposes enough parallel work.

> **Status:** experimental / actively evolving. The core walker, concurrency behavior, tests, and benchmark harness are in place, but the API and feature set are not yet considered stable.

[![Go Reference](https://pkg.go.dev/badge/github.com/PAKIWASI/walkman.svg)](https://pkg.go.dev/github.com/PAKIWASI/walkman)

---

## Why walkman?

The standard-library `filepath.WalkDir` is intentionally simple and sequential. That is a good default, but a large directory tree can contain thousands of independent directories whose metadata can be read concurrently.
`walkman` takes a different approach:
Each directory is a task. A worker processes that directory, emits its entries, and spawns child directories back into the pool. When a worker runs out of local work, it can steal work from another worker's queue.
This makes `walkman` particularly interesting for **wide trees** with lots of independent directories to process.

---

## Features

- Concurrent directory traversal using a work-stealing pool
- Configurable worker count
- Defaults to `GOMAXPROCS`
- Streaming results through a channel, one `WalkResult` per directory
- Per-directory error reporting without aborting unrelated work
- Skip entries by name, pruning matching directories
- Optional maximum traversal depth
- Optional symlink following
- Optional atomic traversal statistics
- Reusable pool configuration for workload-specific tuning
- Tests covering correctness, concurrency, error handling, symlinks, depth limits, and termination behavior
- Benchmark suite comparing:
  - `walkman` vs Go's `filepath.WalkDir`
  - different worker counts
  - pool capacity and result-buffer settings
  - stats enabled vs disabled
  - `walkman` vs Rust's `walkdir` crate through a matching CLI wrapper
- Reproducible synthetic-tree benchmark tooling with wide/deep/mixed shapes

---

## Installation

```bash
go get github.com/PAKIWASI/walkman
```

Import it in your program:

```go
import "github.com/PAKIWASI/walkman"
```

---

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

### Important: drain `Walk` before `Wait`

`Walk` returns a channel that remains open until all queued work has completed. Consume the channel first, then call `Wait()` for the terminal pool error:

```go
for result := range w.Walk(root) {
    // consume results
}

if err := w.Wait(); err != nil {
    // fatal pool-level error
}
```

A directory-specific filesystem error is returned inside its `WalkResult` and does **not** automatically terminate the entire traversal.

---

## API

### `NewWalkman`

```go
func NewWalkman(
    followLinks bool,
    maxDepth uint32,
    skipList []string,
) *Walkman
```

Creates a walker using the default pool configuration.

Defaults:

- `PoolSize = GOMAXPROCS`
- `InitialWorkerCap = 32`
- `ResultBuffSize = 64`
- statistics disabled

### `NewWalkmanWithConfig`

```go
func NewWalkmanWithConfig(
    followLinks bool,
    maxDepth uint32,
    skipList []string,
    trackStats bool,
    pc PoolConfig,
) *Walkman
```

Use this when you want explicit control over the underlying work-stealing pool.

```go
type PoolConfig struct {
    PoolSize         int
    InitialWorkerCap int
    ResultBuffSize   int
}
```

`PoolSize` controls the number of workers. The other two values control the initial local-worker capacity and result-channel buffering.

### `Walk`

```go
func (w *Walkman) Walk(root string) <-chan WalkResult
```

Starts a traversal and returns a streaming result channel.

Each result describes one processed directory:

```go
type WalkResult struct {
    Dir string
    Ret []fs.DirEntry
    Err error
}
```

`Ret` contains that directory's direct entries. Child directories are submitted as separate tasks.

### `Wait`

```go
func (w *Walkman) Wait() error
```

Waits for the worker pool to finish and returns the first fatal pool-level error.

### `Stats`

```go
func (w *Walkman) Stats() (
    files, dirs, links, skipped, maxDepthReached uint32,
)
```

Returns a point-in-time snapshot of traversal statistics. Statistics are zero unless `trackStats` was enabled.

Stats are maintained with atomics because directory tasks execute concurrently.

---

## Traversal semantics

### Ordering

Results are **completion ordered**, not globally sorted.

The worker pool may finish directories in whichever order workers complete them, so this is expected:

```text
/
├── a/
├── b/
└── c/
```

Possible result order:

```text
/
a/
c/
b/
```

`walkman` deliberately does not buffer the entire traversal just to impose pathname order. If you need sorted or breadth-first output, consume the stream and add that policy as a separate layer.

### Skip list

`skipList` matches entry names at every depth.

For example:

```go
w := walkman.NewWalkman(false, 0, []string{".git", "node_modules"})
```

A matching directory is pruned, so its subtree is never scheduled.

### Maximum depth

`maxDepth == 0` means unlimited depth.

A non-zero value prevents the walker from descending past that depth.

### Symlinks

Symlinks are not followed by default.

When `followLinks` is enabled, `walkman` uses `os.Stat` to resolve the link target and descends when that target is a directory.

**Current limitation:** there is no global symlink-cycle detection yet. A symlink that points back into an ancestor can therefore cause unbounded traversal. Only enable link following on trees where this is acceptable.

### Errors

Errors opening individual directories are returned in that directory's `WalkResult`:

```go
if result.Err != nil {
    log.Printf("cannot read %s: %v", result.Dir, result.Err)
    continue
}
```

This keeps a single inaccessible directory from stopping unrelated workers from continuing.

Broken symlinks are treated as non-recursable links when link following is enabled.

---

## How it works

The core task is intentionally small:

```go
type walkItem struct {
    path  string
    depth uint32
}
```

For each directory task, `walkman`:

1. opens the directory and reads its entries with `File.ReadDir(-1)`;
2. removes skipped names;
3. classifies entries from their `DirEntry` type;
4. optionally resolves symlink targets;
5. emits the directory's entries as a `WalkResult`;
6. submits child directories back to the work-stealing pool.

The implementation also avoids a few unnecessary costs in the hot path:

- `File.ReadDir(-1)` is used instead of `os.ReadDir`, avoiding the latter's filename sort;
- paths are cleaned once at the root and child paths are constructed with a small `join` helper instead of repeatedly calling `filepath.Join`;
- statistics are disabled by default so normal walks do not pay the atomic-counter overhead.

The pool itself is provided by [`github.com/PAKIWASI/workstealpool`](https://github.com/PAKIWASI/workstealpool).

---

## CLI / benchmarking tools

The repository includes a small Go CLI under `main/` and a Rust CLI wrapper under `rust_walkdir/`.

### Build the Go CLI

```bash
go build -o build/main ./main
```

Run it:

```bash
./build/main /path/to/tree
```

Options:

```text
--max-depth N      0 = unlimited
--follow-links     follow symlinks
--skip a,b,c       comma-separated names to prune
--print            print every entry
--bench N          repeat N times and report timing
--quiet            suppress the summary line
--workers N        worker-pool size; 0 = GOMAXPROCS
```

Example:

```bash
./build/main --workers 8 --skip .git,node_modules /home/me/project
```

### Build the Rust reference CLI

```bash
cd rust_walkdir
cargo build --release
```

The resulting `walkdir-cli` is a thin wrapper around the Rust [`walkdir`](https://crates.io/crates/walkdir) crate with matching traversal options so the two implementations can be compared on the same tree.

---

## Benchmarks

Benchmarking filesystem walkers is surprisingly easy to get wrong. A live filesystem changes, page cache state changes between runs, and a worker pool can trade additional total CPU time for lower wall-clock time.

The repository therefore includes a reproducible benchmark harness under `test/`:

```text
test/
├── build_tree.sh       # deterministic synthetic tree generator
├── bench_harness.sh    # hyperfine-based comparison harness
└── results_*.csv       # recorded benchmark output
```

Synthetic trees can be generated with different shapes:

```bash
./test/build_tree.sh --shape wide  --root /tmp/tree_wide  --seed 42
./test/build_tree.sh --shape deep  --root /tmp/tree_deep  --seed 42
./test/build_tree.sh --shape mixed --root /tmp/tree_mixed --seed 42
```

The shapes matter because parallelism depends on how much independent directory work exists:

| Shape | Characteristics | Expected parallelism |
| --- | --- | --- |
| `wide` | shallow, high branching factor | High |
| `deep` | long, narrow chains | Lower |
| `mixed` | moderate depth and branching | More representative |

### Recorded wide-tree comparison

One recorded benchmark used a deterministic wide tree containing **1,884 directories and 11,304 files** on an Intel 11th Gen Core i5-1135G7 system.

The stored results were:

| Walker | Workers | Mean wall time |
| --- | ---: | ---: |
| Rust `walkdir` | sequential | 15.88 ms |
| `walkman` | 1 | 12.04 ms |
| `walkman` | 2 | 7.29 ms |
| `walkman` | 4 | 4.66 ms |
| `walkman` | 8 | 7.49 ms |

That particular run shows a useful property of the project: **more workers are not automatically better**. On that tree and machine, 4 workers were faster than 8.

Another recorded run of the worker sweep showed the same general scaling pattern: large gains from 1 → 2 → 4 workers, followed by a plateau as available parallel work and machine resources became the limiting factors.

These numbers are **examples from the repository's benchmark logs, not universal performance guarantees**. Filesystem, OS, storage medium, page-cache state, CPU topology, tree shape, and worker count all affect the result.

### Re-run the comparison

The benchmark harness uses `hyperfine`, warm-up runs, multiple measurements, and a deterministic tree:

```bash
./test/bench_harness.sh \
  --walkman   ./build/main \
  --walkdir   ./build/walkdir-cli \
  --tree      /tmp/tree_wide \
  --workers   "1,2,4,8" \
  --out       test/results_wide.csv
```

It can also record user/system CPU time to make the wall-time vs total-CPU trade-off visible.

---

## Testing

Run the test suite with:

```bash
go test ./...
```

Run the race detector:

```bash
go test -race ./...
```

Run benchmarks:

```bash
go test -bench=. -benchmem ./...
```

The test suite covers, among other cases:

- empty and flat directories;
- equivalence with `filepath.WalkDir` on nested trees;
- skip-list pruning;
- maximum-depth handling;
- symlink behavior with following enabled and disabled;
- broken symlinks;
- nonexistent roots;
- permission-denied directories;
- statistics accuracy;
- consistency across worker counts;
- repeated runs for flakiness;
- worker parking/waking and termination behavior;
- default worker sizing from `GOMAXPROCS`.

---

## Current limitations / roadmap

The project intentionally leaves several policy decisions out of the core walker for now.

Planned or explicitly deferred work includes:

- deterministic sorting of results;
- breadth-first traversal;
- iterator support using Go's `iter.Seq`;
- richer filtering / `FilterEntry`-style behavior;
- `ContentsFirst` ordering;
- configurable limits on simultaneously open directories;
- global symlink-cycle detection;
- `fs.FS`-based traversal and tests;
- broader benchmark coverage across small, wide, deep, and latency-injected trees;
- further tooling / lint / race-detector cleanup as the API settles.

The most important design constraint is that ordering and scheduling are separate concerns: preserving aggressive work stealing while also guaranteeing globally sorted or BFS output requires buffering and therefore belongs above the core stream.

---

## Project layout

```text
walkman/
├── walkman.go                 # core concurrent walker
├── walkman_test.go            # correctness + concurrency tests
├── walkman_bench_test.go      # benchmark suite
├── walkman_worker_sweep_test.go
├── main/
│   └── main.go                # Go benchmark/CLI wrapper
├── rust_walkdir/
│   ├── Cargo.toml
│   └── src/main.rs            # comparable Rust walkdir wrapper
├── build/
│   └── build_tree.sh          # simple synthetic-tree generator
├── test/
│   ├── build_tree.sh          # deterministic benchmark tree generator
│   ├── bench_harness.sh       # benchmark automation
│   └── results_*.csv          # recorded results
└── help.md                    # project notes / implementation plan
```

