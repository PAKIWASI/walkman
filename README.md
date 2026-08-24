# Walkman

Walkman is a concurrent filesystem walker for Go built around a work stealing worker pool.

It walks directories in parallel, streams one result per visited directory, and leaves result ordering to task completion order.

```go
w := walkman.NewWalkman(
    false,       // follow symlinks
    0,           // max depth, 0 = unlimited
    []string{},  // names to skip
)

results := w.Walk("/path/to/root")

for result := range results {
    if result.Err != nil {
        // handle directory-specific error
        continue
    }

    for _, entry := range result.Ret {
        // process entry
    }
}

if err := w.Wait(); err != nil {
    // terminal pool error
}
```

## Features

• Concurrent directory traversal using a work stealing pool

• Streaming per-directory results through a channel

• Configurable worker pool size (Defaults to GOMAXPROCS workers)

• Configurable worker capacity and result buffering

• Skip entries by name, with pruning at every depth

• Maximum traversal depth

• Optional symbolic link following

• Recoverable per-directory errors

• Optional traversal statistics


Walkman treats directory errors as results from the affected directory instead of terminating the entire traversal.

The default configuration keeps statistics disabled because each statistic requires an atomic operation per entry.

## Performance

Benchmarks were run on:

11th Gen Intel Core i5-1135G7 @ 2.40 GHz
Linux amd64

Walkman uses multiple workers by default, while the Rust walkdir comparison uses sequential traversal.

### Synthetic tree

3,110 files and 1,554 directories:

| Implementation | Workers |  Average |
| -------------- | ------: | -------: |
| Walkman        |       1 | 12.08 ms |
| Walkman        | default |  4.12 ms |
| Rust walkdir   |       1 |  5.51 ms |

With one worker, Walkman takes about 2.2x the time of the Rust implementation.

With its default worker pool, Walkman takes about 25% less time.

A larger synthetic tree with 39,062 files and 19,530 directories produced:

| Implementation |     Time |
| -------------- | -------: |
| Walkman        | 66.17 ms |
| Rust walkdir   | 98.72 ms |

Walkman was about 1.49x faster on this workload.

### Worker scaling

A wide synthetic tree containing 11,304 files and 1,884 directories was benchmarked 10 times per worker count.

| Workers |  Average | Speedup |
| ------: | -------: | ------: |
|       1 | 19.81 ms |   1.00x |
|       2 | 12.69 ms |   1.56x |
|       4 |  4.81 ms |   4.12x |
|       8 |  3.44 ms |   5.77x |

The scaling remains strong through 8 workers.

Allocation overhead stays nearly flat:

| Workers | Bytes/op | Allocs/op |
| ------: | -------: | --------: |
|       1 | ~2.06 MB |   ~47,448 |
|       2 | ~2.07 MB |   ~47,464 |
|       4 | ~2.08 MB |   ~47,510 |
|       8 | ~2.09 MB |   ~47,583 |

The small increase in allocations comes from increased parallel traversal rather than a large per-worker overhead.

### Real filesystem

On `/usr`:

Walkman: 122.27 ms
Rust walkdir: 402.99 ms

Walkman was about 3.30x faster.

On `/`:

Walkman: 343.21 ms
Rust walkdir: 925.68 ms

Walkman was about 2.70x faster.

The `/` run produced different entry counts between Walkman and walkdir, despite identical error counts. The traversal semantics around links and filesystem behavior still need investigation before treating this as a strict correctness and performance comparison.

## Walkman vs walkdir

| Feature                     | Walkman  | Rust walkdir  |
| --------------------------- | -------- | ------------- |
| Concurrent traversal        | Yes      | No            |
| Work stealing               | Yes      | No            |
| Streaming results           | Yes      | Iterator      |
| Per-directory results       | Yes      | Yes           |
| Skip/prune                  | Yes      | Yes           |
| Maximum depth               | Yes      | Yes           |
| Minimum depth               | No       | Yes           |
| Symlink following           | Yes      | Yes           |
| Symlink cycle detection     | No       | Yes           |
| Root symlink control        | No       | Yes           |
| Same filesystem restriction | No       | Yes           |
| Sorting                     | No       | Yes           |
| Contents-first traversal    | No       | Yes           |
| Maximum open directories    | No       | Yes           |
| Atomic statistics           | Optional | No equivalent |
| Configurable worker pool    | Yes      | No            |

Walkman deliberately optimizes for concurrent traversal and streaming. It does not attempt to reproduce every semantic and ordering feature of walkdir yet.

## Tests

The test suite covers:

• Empty and flat directories

• Nested traversal compared with filepath.WalkDir

• Skip lists and subtree pruning

• Maximum depth

• Unlimited depth

• Symlink handling with and without following links

• Broken symlinks

• Missing roots

• Regular-file roots

• Permission errors

• Statistics

• Statistics disabled by default

• Consistency across pool sizes

• Repeated runs for flakiness

• Oversubscribed worker pools

• GOMAXPROCS-based default configuration

## TODO

• Symlink cycle detection using filesystem identity

• Deterministic sorting

• Breadth-first traversal

• Minimum depth

• Same-filesystem traversal

• Root symlink control

• Maximum open directory limit

• Contents-first traversal

• Iterator support with Go range-over-func

• Additional traversal pipelines and processing APIs

• Improve compatibility with walkdir semantics where useful
