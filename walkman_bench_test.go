package walkman

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unsafe"
)

// benchRoot returns the tree to walk in benchmarks. Defaults to "/" per
// the intended comparison (whole-OS walk), but overridable via
// WALKMAN_BENCH_ROOT — walking all of "/" on every -count/-benchtime
// repetition is slow and somewhat filesystem/permission-dependent, so for
// CI or quick iteration point this at a smaller subtree instead, e.g.:
//
//	WALKMAN_BENCH_ROOT=/usr go test -bench=Walk -benchtime=3x ./...
func benchRoot() string {
	if r := os.Getenv("WALKMAN_BENCH_ROOT"); r != "" {
		return r
	}
	return "/"
}

// benchSkip mirrors what a real caller would pass — skip nothing by
// default, since the point of the comparison is raw walk throughput, not
// how well skipping is implemented.
var benchSkip []string

// walkDirSequential is the Go-stdlib baseline: filepath.WalkDir, counting
// files/dirs while swallowing per-entry errors exactly like Walkman's
// visit does (a permission-denied subdirectory doesn't abort the walk),
// so the two are doing equivalent work.
func walkDirSequential(root string, skip []string) (files, dirs int) {
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Same policy as Walkman.visit: a per-item failure is data,
			// not a reason to stop. WalkDir would otherwise abort on the
			// first EACCES it hits.
			return nil
		}
		if path == root {
			return nil // root itself isn't "found", matching Walkman's Dir/Ret split
		}
		if d.IsDir() {
			if slices.Contains(skip, d.Name()) {
				return filepath.SkipDir
			}
			dirs++
			return nil
		}
		files++
		return nil
	})
	return
}

// walkParallel drains a full Walkman walk and returns the same counts as
// walkDirSequential, for apples-to-apples comparison.
func walkParallel(b *testing.B, root string, skip []string, pc PoolConfig) (files, dirs int) {
	b.Helper()
	w := NewWalkmanWithConfig(false, 0, skip, false, pc)
	for r := range w.Walk(root) {
		if r.Err != nil {
			continue // same policy as the sequential baseline above
		}
		for _, e := range r.Ret {
			if e.IsDir() {
				dirs++
			} else {
				files++
			}
		}
	}
	if err := w.Wait(); err != nil {
		b.Fatalf("Wait() = %v", err)
	}
	return files, dirs
}

// BenchmarkWalk_Sequential is the baseline every other benchmark in this
// file should be compared against: Go's own filepath.WalkDir, single
// goroutine, no stealing, no atomics.
func BenchmarkWalk_Sequential(b *testing.B) {
	root := benchRoot()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		walkDirSequential(root, benchSkip)
	}
}

// BenchmarkWalk_Parallel is Walkman at its default (GOMAXPROCS-sized)
// pool config — the number to compare directly against
// BenchmarkWalk_Sequential above.
func BenchmarkWalk_Parallel(b *testing.B) {
	root := benchRoot()
	pc := DefaultPoolConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		walkParallel(b, root, benchSkip, pc)
	}
}

// BenchmarkWalk_PoolSize sweeps worker count at fixed
// cap/buffer, mirroring BenchmarkCountPrimes_PoolSize in workstealpool —
// the question is whether "match GOMAXPROCS" (defaultPoolConfig's
// assumption) actually holds for a syscall-bound workload like directory
// walking, or whether the plateau/oversubscription point sits somewhere
// else entirely versus the CPU-bound prime-counting benchmark it was
// borrowed from.
func BenchmarkWalk_PoolSize(b *testing.B) {
	root := benchRoot()
	for _, poolSize := range []int{1, 2, 4, 8, 16, 32} {
		pc := PoolConfig{PoolSize: poolSize, InitialWorkerCap: 32, ResultBuffSize: 64}
		b.Run(fmt.Sprintf("workers=%d", poolSize), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				walkParallel(b, root, benchSkip, pc)
			}
		})
	}
}

// BenchmarkWalk_InitialWorkerCap sweeps each worker deque's starting
// capacity at fixed pool size, isolating resizeCopy cost from directories
// with wide fan-out (e.g. many top-level dirs under "/").
func BenchmarkWalk_InitialWorkerCap(b *testing.B) {
	root := benchRoot()
	for _, cap := range []int{2, 8, 32, 128, 512} {
		pc := PoolConfig{PoolSize: 8, InitialWorkerCap: cap, ResultBuffSize: 64}
		b.Run(fmt.Sprintf("cap=%d", cap), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				walkParallel(b, root, benchSkip, pc)
			}
		})
	}
}

// BenchmarkWalk_ResultBuffSize sweeps the results channel buffer at fixed
// pool size/cap — how much the drain loop (walkParallel's range over
// Walk()) becomes the bottleneck versus workers blocking on a full
// channel.
func BenchmarkWalk_ResultBuffSize(b *testing.B) {
	root := benchRoot()
	for _, buf := range []int{0, 1, 16, 64, 256, 1024} {
		pc := PoolConfig{PoolSize: 8, InitialWorkerCap: 32, ResultBuffSize: buf}
		b.Run(fmt.Sprintf("buf=%d", buf), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				walkParallel(b, root, benchSkip, pc)
			}
		})
	}
}

// BenchmarkWalk_TrackStats isolates the atomic.Add cost trackStats adds
// per entry — this is the number that justifies (or doesn't) keeping it
// opt-in and off by default.
func BenchmarkWalk_TrackStats(b *testing.B) {
	root := benchRoot()
	pc := DefaultPoolConfig()

	b.Run("off", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w := NewWalkmanWithConfig(false, 0, benchSkip, false, pc)
			for range w.Walk(root) {
			}
			w.Wait()
		}
	})

	b.Run("on", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			w := NewWalkmanWithConfig(false, 0, benchSkip, true, pc)
			for range w.Walk(root) {
			}
			w.Wait()
		}
	})
}

// buildSyntheticTree builds a deterministic, depth x breadth directory
// tree once (outside the timed loop) and returns its root. Unlike
// benchRoot()'s default of walking "/", this gives every run - on any
// machine, any permissions, any real-world filesystem clutter - the
// exact same shape and file count, so results are comparable across
// commits/machines instead of being an artifact of whatever happens to
// be mounted at "/" that day.
func buildSyntheticTree(b *testing.B, depth, breadth int) string {
	b.Helper()
	root := b.TempDir()

	var build func(dir string, level int)
	build = func(dir string, level int) {
		// A couple of files at every level so leaves aren't the only
		// thing being counted.
		for f := range 2 {
			p := filepath.Join(dir, fmt.Sprintf("file%d.txt", f))
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				b.Fatalf("WriteFile(%q): %v", p, err)
			}
		}
		if level >= depth {
			return
		}
		for i := range breadth {
			sub := filepath.Join(dir, fmt.Sprintf("d%d", i))
			if err := os.Mkdir(sub, 0o755); err != nil {
				b.Fatalf("Mkdir(%q): %v", sub, err)
			}
			build(sub, level+1)
		}
	}
	build(root, 0)

	return root
}

// BenchmarkWalk_Synthetic is the deterministic counterpart to
// BenchmarkWalk_Sequential/_Parallel above: same sequential-vs-pool
// comparison, but on a fixed-shape tree (depth 4, breadth 6 -> 1,554
// directories, 3,108 files) instead of whatever "/" happens to contain.
// Prefer this pair when comparing numbers across machines or commits;
// prefer the "/" benchmarks when you specifically want to know real-world
// walk throughput on this machine's actual filesystem.
func BenchmarkWalk_Synthetic(b *testing.B) {
	const depth, breadth = 4, 6
	root := buildSyntheticTree(b, depth, breadth)

	b.Run("Sequential", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			walkDirSequential(root, benchSkip)
		}
	})

	b.Run("Parallel", func(b *testing.B) {
		pc := DefaultPoolConfig()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			walkParallel(b, root, benchSkip, pc)
		}
	})
}

// BenchmarkWalk_Synthetic_PoolSize is BenchmarkWalk_PoolSize's
// deterministic counterpart - the same "does stealing pay for itself as
// worker count climbs" question, but reproducible across machines.
func BenchmarkWalk_Synthetic_PoolSize(b *testing.B) {
	const depth, breadth = 4, 6
	root := buildSyntheticTree(b, depth, breadth)

	for _, poolSize := range []int{1, 2, 4, 8, 16, 32} {
		pc := PoolConfig{PoolSize: poolSize, InitialWorkerCap: 32, ResultBuffSize: 64}
		b.Run(fmt.Sprintf("workers=%d", poolSize), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				walkParallel(b, root, benchSkip, pc)
			}
		})
	}
}

// TestWalkItemSize documents the actual, measured cost of adding the
// ancestors field to walkItem for both modes, rather than an eyeballed
// claim. It's a test (always run, always visible), not a benchmark, since
// unsafe.Sizeof is a compile-time constant — there's nothing to time.
func TestWalkItemSize(t *testing.T) {
	t.Logf("unsafe.Sizeof(walkItem{}) = %d bytes", unsafe.Sizeof(walkItem{}))
	t.Logf("unsafe.Sizeof(ancestorNode{}) = %d bytes (never allocated unless followLinks is on)", unsafe.Sizeof(ancestorNode{}))
}

// buildSymlinkTree builds buildSyntheticTree's shape, then adds one
// symlink per directory pointing at a small target tree that lives
// entirely outside the walked root — real cross-links, not
// self-references, so followLinks=true does genuine extra directory
// descents (not just extra stats) without ever creating an actual cycle,
// the same way a well-formed real followLinks=true tree would.
func buildSymlinkTree(b *testing.B, depth, breadth int) string {
	b.Helper()
	root := buildSyntheticTree(b, depth, breadth)

	// A separate, unrelated tree the walked root has no other path to, so
	// every symlink resolves to the same place without ever pointing back
	// at anything already on the current descent path.
	target := b.TempDir()
	for f := range 2 {
		p := filepath.Join(target, fmt.Sprintf("leaf%d.txt", f))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			b.Fatalf("WriteFile(%q): %v", p, err)
		}
	}

	var addLinks func(dir string, level int)
	addLinks = func(dir string, level int) {
		link := filepath.Join(dir, "link_out")
		if err := os.Symlink(target, link); err != nil {
			b.Fatalf("Symlink(%q): %v", link, err)
		}
		if level >= depth {
			return
		}
		for i := range breadth {
			addLinks(filepath.Join(dir, fmt.Sprintf("d%d", i)), level+1)
		}
	}
	addLinks(root, 0)

	return root
}

// walkFollowLinks is walkParallel's followLinks=true counterpart.
func walkFollowLinks(b *testing.B, root string, pc PoolConfig) (files, dirs int) {
	b.Helper()
	w := NewWalkmanWithConfig(true, 0, nil, false, pc)
	for r := range w.Walk(root) {
		if r.Err != nil {
			continue
		}
		for _, e := range r.Ret {
			if e.IsDir() {
				dirs++
			} else {
				files++
			}
		}
	}
	if err := w.Wait(); err != nil {
		b.Fatalf("Wait() = %v", err)
	}
	return files, dirs
}

// BenchmarkWalk_FollowLinks_NoSymlinksPresent isolates the cost of turning
// followLinks on when the tree contains no symlinks at all — i.e. the cost
// of running visitSym instead of visit per directory. The struct-field
// cost (one extra pointer in walkItem) is effectively free either way; the
// real cost, if any, is the extra os.Lstat per plain directory that
// visitSym does to extend the ancestor chain for cycle detection. This is
// the number that answers "what does opting in cost me even on a tree with
// no symlinks to follow".
func BenchmarkWalk_FollowLinks_NoSymlinksPresent(b *testing.B) {
	const depth, breadth = 4, 6
	root := buildSyntheticTree(b, depth, breadth)
	pc := DefaultPoolConfig()

	b.Run("followLinks=false", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			walkParallel(b, root, nil, pc)
		}
	})

	b.Run("followLinks=true", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			walkFollowLinks(b, root, pc)
		}
	})
}

// BenchmarkWalk_FollowLinks_WithSymlinks is the realistic case: a tree
// that actually has symlinks to follow. followLinks=false here undercounts
// (it stops at each symlink, per its documented contract) so this isn't an
// apples-to-apples throughput comparison — it's the number for "what does
// a real followLinks=true walk, doing real extra descents and cycle
// checks, cost in absolute terms".
func BenchmarkWalk_FollowLinks_WithSymlinks(b *testing.B) {
	const depth, breadth = 3, 4
	root := buildSymlinkTree(b, depth, breadth)
	pc := DefaultPoolConfig()

	b.Run("followLinks=false", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			walkParallel(b, root, nil, pc)
		}
	})

	b.Run("followLinks=true", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			walkFollowLinks(b, root, pc)
		}
	})
}

// BenchmarkWalk_FollowLinks_PoolSize is BenchmarkWalk_PoolSize's
// followLinks=true counterpart: does stealing pay off the same way once
// every directory visit also carries an Lstat and ancestor-chain bookkeeping?
func BenchmarkWalk_FollowLinks_PoolSize(b *testing.B) {
	const depth, breadth = 3, 4
	root := buildSymlinkTree(b, depth, breadth)

	for _, poolSize := range []int{1, 2, 4, 8, 16, 32} {
		pc := PoolConfig{PoolSize: poolSize, InitialWorkerCap: 32, ResultBuffSize: 64}
		b.Run(fmt.Sprintf("workers=%d", poolSize), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				walkFollowLinks(b, root, pc)
			}
		})
	}
}
