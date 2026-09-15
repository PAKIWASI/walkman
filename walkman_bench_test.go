package walkman

// ---------------------------------------------------------------------
// Benchmark tuning tests for the walkman worker-pool walker.
//
// These benchmarks sweep the three PoolConfig parameters (PoolSize,
// InitialWorkerCap, ResultBuffSize) plus the followLinks flag to help
// find the optimal tuning for a given workload.
//
// Usage:
//
//   # PoolSize sweep on the real Linux kernel source tree:
//   go test -run='^$' -bench=BenchmarkWalk_PoolSweep -tree=/path/to/linux-source -benchtime=3s
//
//   # Buffer config grid:
//   go test -run='^$' -bench=BenchmarkWalk_BufferSweep -tree=/path/to/linux-source -benchtime=3s
//
//   # Tuning report (finds the fastest config):
//   go test -run=TestWalk_TuningReport -v -tree=/path/to/linux-source
//
// If -tree is not provided, a synthetic tree is generated so benchmarks
// are always runnable (though results may differ from a real tree).
// ---------------------------------------------------------------------

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// Flag & tree setup
// ---------------------------------------------------------------------

// benchTreeFlag is the -tree flag: set it to a directory tree to walk
// (e.g. the Linux kernel source). If empty, a synthetic tree is built.
var benchTreeFlag = flag.String("tree", "", "path to a directory tree to walk (e.g. /path/to/linux-source)")

var (
	cachedBenchRoot  string
	getBenchRootOnce sync.Once
	getBenchRootErr  error
)

// getBenchRoot returns the tree root for benchmarks.
// If -tree is set and is a directory, uses it directly.
// Otherwise, lazily builds a synthetic tree (cached for the process).
func getBenchRoot() (string, error) {
	getBenchRootOnce.Do(func() {
		if *benchTreeFlag != "" {
			info, err := os.Stat(*benchTreeFlag)
			if err != nil {
				getBenchRootErr = fmt.Errorf("tree path %q: %w", *benchTreeFlag, err)
				return
			}
			if !info.IsDir() {
				getBenchRootErr = fmt.Errorf("tree path %q is not a directory", *benchTreeFlag)
				return
			}
			cachedBenchRoot = *benchTreeFlag
			return
		}
		// Build a synthetic tree under a temp dir.
		dir, err := os.MkdirTemp("", "walkman-bench-*")
		if err != nil {
			getBenchRootErr = fmt.Errorf("creating temp dir: %w", err)
			return
		}
		if err := buildBenchmarkTree(dir); err != nil {
			getBenchRootErr = fmt.Errorf("building synthetic tree: %w", err)
			return
		}
		cachedBenchRoot = dir
	})
	return cachedBenchRoot, getBenchRootErr
}

// buildBenchmarkTree creates a synthetic directory tree under the given
// root that resembles a medium-sized source repository:
//
//  - 10 top-level source packages, each with 5 sub-packages of 10 files
//  - docs/, scripts/, tests/, config/, build/ top-level dirs
//  - Root-level files (README.md, LICENSE, etc.)
//  - A handful of symlinks (best-effort, silently skipped if unsupported)
//
// Total: ~63 dirs, ~500 files, ~7 symlinks.
func buildBenchmarkTree(root string) error {
	pkgs := []string{
		"core", "io", "net", "util", "data",
		"parse", "render", "query", "auth", "cache",
	}

	for _, pkg := range pkgs {
		pkgDir := filepath.Join(root, "src", pkg)
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			return err
		}
		for sub := 1; sub <= 5; sub++ {
			subDir := filepath.Join(pkgDir, fmt.Sprintf("sub%d", sub))
			if err := os.MkdirAll(subDir, 0o755); err != nil {
				return err
			}
			for f := 1; f <= 10; f++ {
				fname := filepath.Join(subDir, fmt.Sprintf("file%d.go", f))
				if err := os.WriteFile(fname, []byte("package "+pkg), 0o644); err != nil {
					return err
				}
			}
		}
	}

	// Top-level directories
	for _, d := range []string{"docs", "scripts", "tests", "config", "build"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
	}

	// Docs files
	for _, docSub := range []string{"manual", "api", "design"} {
		docDir := filepath.Join(root, "docs", docSub)
		if err := os.MkdirAll(docDir, 0o755); err != nil {
			return err
		}
		for f := 1; f <= 5; f++ {
			if err := os.WriteFile(
				filepath.Join(docDir, fmt.Sprintf("doc%d.md", f)),
				[]byte("# Document "+fmt.Sprintf("%d", f)),
				0o644,
			); err != nil {
				return err
			}
		}
	}

	// Root-level files
	for _, fname := range []string{"README.md", "LICENSE", "Makefile", "go.mod", "main.go"} {
		if err := os.WriteFile(
			filepath.Join(root, fname),
			[]byte("root content"),
			0o644,
		); err != nil {
			return err
		}
	}

	addSymlinks(root, pkgs)
	return nil
}

// addSymlinks creates a few symlinks in the synthetic tree.
// Errors are silently ignored (e.g. on Windows without developer mode).
func addSymlinks(root string, pkgs []string) {
	// Symlink to a real file within the tree
	target := filepath.Join(root, "src", pkgs[0], "sub1", "file1.go")
	os.Symlink(target, filepath.Join(root, "src", pkgs[0], "latest.go"))

	// Symlink to a directory
	targetDir := filepath.Join(root, "src", pkgs[0])
	os.Symlink(targetDir, filepath.Join(root, "src", "core_latest"))

	// Top-level file symlinks
	for i := 1; i <= 5; i++ {
		pkg := pkgs[i%len(pkgs)]
		target := filepath.Join(root, "src", pkg, fmt.Sprintf("sub%d", i), fmt.Sprintf("file%d.go", i))
		os.Symlink(target, filepath.Join(root, fmt.Sprintf("link%d", i)))
	}
}

// hasSymlinks reports whether root contains any symlinks.
func hasSymlinks(root string) (bool, error) {
	symlinkFound := false
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			symlinkFound = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return symlinkFound, nil
}

// ---------------------------------------------------------------------
// Walk helpers & sweep value definitions
// ---------------------------------------------------------------------

// drainBenchmark runs a full walk using the given config and consumes
// all results to prevent backpressure from skewing timing.
// Returns the total number of entries visited.
func drainBenchmark(b *testing.B, w *Walkman, root string) uint64 {
	b.Helper()
	var total uint64
	for batch := range w.Walk(root) {
		total += uint64(len(batch.Entries))
	}
	if err := w.Wait(); err != nil {
		b.Logf("Wait: %v", err)
	}
	return total
}

// poolSizesSweep is the set of worker counts to benchmark.
// Built once from GOMAXPROCS to always include the local core count.
var poolSizesSweep = buildPoolSizeSet()

func buildPoolSizeSet() []int {
	mc := runtime.GOMAXPROCS(0)
	seen := make(map[int]bool)
	var sizes []int
	for _, s := range []int{1, 2, 4, 8, 16, 32, 64, mc, mc * 2} {
		if s > 0 && !seen[s] {
			sizes = append(sizes, s)
			seen[s] = true
		}
	}
	sort.Ints(sizes)
	return sizes
}

// bufferSweep is the set of (InitialWorkerCap, ResultBuffSize) pairs to sweep.
var bufferSweep = []struct {
	InitialWorkerCap int
	ResultBuffSize   int
}{
	{16, 64},
	{16, 256},
	{16, 1024},
	{64, 64},
	{64, 256},
	{64, 1024},
	{256, 256},
	{256, 1024},
	{1024, 256},
	{1024, 1024},
}

// ---------------------------------------------------------------------
// Benchmarks: PoolSize sweep
// ---------------------------------------------------------------------

// BenchmarkWalk_PoolSweep measures how walk throughput scales with
// the number of worker goroutines (PoolSize), holding all other
// config at defaults. This is the primary tuning knob — more workers
// help up to a point, after which contention and overhead dominate.
//
// Run with:
//   go test -run='^$' -bench=BenchmarkWalk_PoolSweep -tree=/path -benchtime=3s
func BenchmarkWalk_PoolSweep(b *testing.B) {
	root, err := getBenchRoot()
	if err != nil {
		b.Fatalf("getBenchRoot: %v", err)
	}

	for _, ps := range poolSizesSweep {
		pc := PoolConfig{
			PoolSize:         ps,
			InitialWorkerCap: 64,
			ResultBuffSize:   256,
		}
		b.Run(fmt.Sprintf("pool=%d", ps), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				w := NewWalkmanWithConfig(false, 0, nil, pc)
				total := drainBenchmark(b, w, root)
				if total == 0 {
					b.Fatal("walk returned zero entries")
				}
			}
		})
	}
}

// BenchmarkWalk_PoolSweep_FollowLinks is the same sweep as
// BenchmarkWalk_PoolSweep but with followLinks=true. Skipped if
// the tree contains no symlinks.
func BenchmarkWalk_PoolSweep_FollowLinks(b *testing.B) {
	root, err := getBenchRoot()
	if err != nil {
		b.Fatalf("getBenchRoot: %v", err)
	}
	hasSyms, err := hasSymlinks(root)
	if err != nil {
		b.Fatalf("hasSymlinks: %v", err)
	}
	if !hasSyms {
		b.Skip("tree has no symlinks; followLinks=true benchmark is not meaningful")
	}

	for _, ps := range poolSizesSweep {
		pc := PoolConfig{
			PoolSize:         ps,
			InitialWorkerCap: 64,
			ResultBuffSize:   256,
		}
		b.Run(fmt.Sprintf("pool=%d_follow=true", ps), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				w := NewWalkmanWithConfig(true, 0, nil, pc)
				total := drainBenchmark(b, w, root)
				if total == 0 {
					b.Fatal("walk returned zero entries")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------
// Benchmark: followLinks impact
// ---------------------------------------------------------------------

// BenchmarkWalk_FollowLinksImpact directly compares followLinks=true
// vs false at several pool sizes to quantify the symlink-following
// overhead. Skipped if the tree contains no symlinks.
func BenchmarkWalk_FollowLinksImpact(b *testing.B) {
	root, err := getBenchRoot()
	if err != nil {
		b.Fatalf("getBenchRoot: %v", err)
	}
	hasSyms, err := hasSymlinks(root)
	if err != nil {
		b.Fatalf("hasSymlinks: %v", err)
	}
	if !hasSyms {
		b.Skip("tree has no symlinks")
	}

	mc := runtime.GOMAXPROCS(0)
	impactSizes := []int{1, 4, 8, 16}

	for _, ps := range impactSizes {
		if ps > mc {
			continue
		}
		pc := PoolConfig{
			PoolSize:         ps,
			InitialWorkerCap: 64,
			ResultBuffSize:   256,
		}
		for _, fl := range []bool{false, true} {
			label := "nofollow"
			if fl {
				label = "follow"
			}
			b.Run(fmt.Sprintf("pool=%d_%s", ps, label), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					w := NewWalkmanWithConfig(fl, 0, nil, pc)
					total := drainBenchmark(b, w, root)
					if total == 0 {
						b.Fatal("walk returned zero entries")
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------------
// Benchmark: Buffer config sweep
// ---------------------------------------------------------------------

// BenchmarkWalk_BufferSweep measures the effect of InitialWorkerCap
// (local queue depth per worker) and ResultBuffSize (result channel
// buffer) on walk performance, holding PoolSize at GOMAXPROCS.
//
// InitialWorkerCap controls how many directory paths a worker can hold
// in its local work queue before stealing is needed. ResultBuffSize
// controls the buffer between worker producers and the consumer that
// receives DirBatch results.
func BenchmarkWalk_BufferSweep(b *testing.B) {
	root, err := getBenchRoot()
	if err != nil {
		b.Fatalf("getBenchRoot: %v", err)
	}

	pc := runtime.GOMAXPROCS(0)

	for _, bc := range bufferSweep {
		pcfg := PoolConfig{
			PoolSize:         pc,
			InitialWorkerCap: bc.InitialWorkerCap,
			ResultBuffSize:   bc.ResultBuffSize,
		}
		b.Run(fmt.Sprintf("iq=%d_rb=%d", bc.InitialWorkerCap, bc.ResultBuffSize), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				w := NewWalkmanWithConfig(false, 0, nil, pcfg)
				total := drainBenchmark(b, w, root)
				if total == 0 {
					b.Fatal("walk returned zero entries")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------
// Test: Tuning report (finds best config)
// ---------------------------------------------------------------------

// tuningConfig is a single configuration candidate for the tuning report.
type tuningConfig struct {
	Name             string
	PoolSize         int
	InitialWorkerCap int
	ResultBuffSize   int
	FollowLinks      bool
}

// tuningResult is the measured result for one config.
type tuningResult struct {
	Config  tuningConfig
	Time    time.Duration
	Allocs  int64
	Bytes   int64
	Entries uint64
}

// TestWalk_TuningReport runs a focused set of configurations against
// the benchmark tree (real or synthetic), measures each, and prints
// a report identifying the fastest configuration for this machine.
//
// This is a test (not a benchmark) because it needs to programmatically
// compare results. Each config is run a fixed number of times to get
// quick, comparable numbers.
//
// Usage:
//   go test -run=TestWalk_TuningReport -v -timeout=300s
//   go test -run=TestWalk_TuningReport -v -timeout=300s -tree=/path/to/linux
func TestWalk_TuningReport(t *testing.T) {
	root, err := getBenchRoot()
	if err != nil {
		t.Fatalf("getBenchRoot: %v", err)
	}
	t.Logf("benchmark tree: %s (size: %s, %s)",
		root, treeSizeHuman(root), dirCountHuman(root))

	hasSyms, _ := hasSymlinks(root)
	if hasSyms {
		t.Log("tree contains symlinks; followLinks variants will be tested")
	} else {
		t.Log("tree has no symlinks; skipping followLinks=true configs")
	}

	configs := buildTuningConfigs(hasSyms)
	const iters = 5

			results := make([]tuningResult, 0, len(configs))

	for _, cfg := range configs {
		pc := PoolConfig{
			PoolSize:         cfg.PoolSize,
			InitialWorkerCap: cfg.InitialWorkerCap,
			ResultBuffSize:   cfg.ResultBuffSize,
		}

		var times []time.Duration
		var entryCount uint64
		var lastAllocs, lastBytes int64

		for i := 0; i < iters; i++ {
			w := NewWalkmanWithConfig(cfg.FollowLinks, 0, nil, pc)

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			start := time.Now()
			for batch := range w.Walk(root) {
				entryCount += uint64(len(batch.Entries))
			}
			elapsed := time.Since(start)
			if err := w.Wait(); err != nil {
				t.Logf("  config %s: Wait error: %v", cfg.Name, err)
			}

			runtime.ReadMemStats(&after)
			times = append(times, elapsed)
			lastAllocs = int64(after.Mallocs - before.Mallocs)
			lastBytes = int64(after.TotalAlloc - before.TotalAlloc)
		}

		r := tuningResult{
			Config:  cfg,
			Time:    medianDuration(times),
			Allocs:  lastAllocs,
			Bytes:   lastBytes,
			Entries: entryCount / uint64(iters),
		}
		results = append(results, r)

		t.Logf("  %-30s: %s  (entries=%d, allocs=%d, %s)",
			cfg.Name,
			formatDuration(r.Time),
			r.Entries,
			r.Allocs,
			humanBytes(r.Bytes),
		)
	}

	// Sort by median wall time (fastest first).
	sort.Slice(results, func(i, j int) bool {
		return results[i].Time < results[j].Time
	})

	best := results[0]
	t.Logf("")
	t.Logf("=== Tuning Report ===")
	t.Logf("Tree:           %s", root)
	t.Logf("Tree size:      %s", treeSizeHuman(root))
	t.Logf("Entries walked: %d", best.Entries)
	t.Logf("Iterations:     %d per config", iters)
	t.Logf("Configs tested: %d", len(configs))
	t.Logf("")
	t.Logf("Top 5 fastest configurations:")
	for i := 0; i < 5 && i < len(results); i++ {
		r := results[i]
		t.Logf("  %2d. %-30s  %s  (allocs=%d, %s)",
			i+1,
			r.Config.Name,
			formatDuration(r.Time),
			r.Allocs,
			humanBytes(r.Bytes),
		)
	}
	t.Logf("")
	t.Logf("Winner: %s — %s", best.Config.Name, formatDuration(best.Time))
	t.Logf("  PoolSize=%d, InitialWorkerCap=%d, ResultBuffSize=%d, FollowLinks=%v",
		best.Config.PoolSize, best.Config.InitialWorkerCap,
		best.Config.ResultBuffSize, best.Config.FollowLinks)
}

// ---------------------------------------------------------------------
// Config generation for tuning report
// ---------------------------------------------------------------------

// buildTuningConfigs generates the set of configurations to test in
// TestWalk_TuningReport. It includes:
//
//   - PoolSize sweep at defaults (1, 2, 4, 8, 16, GOMAXPROCS)
//   - A representative subset of buffer configs at GOMAXPROCS
//   - followLinks variants at key pool sizes (only if tree has symlinks)
func buildTuningConfigs(hasSymlinks bool) []tuningConfig {
	mc := runtime.GOMAXPROCS(0)
	var configs []tuningConfig

	// PoolSize sweep at defaults
	for _, ps := range []int{1, 2, 4, 8, 16, mc} {
		if ps <= 0 {
			continue
		}
		name := fmt.Sprintf("pool=%d_defbuf", ps)
		if ps == mc {
			name = "pool=GOMAXPROCS_defbuf"
		}
		configs = append(configs, tuningConfig{
			Name:             name,
			PoolSize:         ps,
			InitialWorkerCap: 64,
			ResultBuffSize:   256,
		})
	}

	// Buffer config variants at GOMAXPROCS (subset for quick turnaround)
	for _, bc := range bufferSweep {
		if len(configs) >= 15 {
			break
		}
		name := fmt.Sprintf("iq=%d_rb=%d", bc.InitialWorkerCap, bc.ResultBuffSize)
		configs = append(configs, tuningConfig{
			Name:             name,
			PoolSize:         mc,
			InitialWorkerCap: bc.InitialWorkerCap,
			ResultBuffSize:   bc.ResultBuffSize,
		})
	}

	// followLinks variants at key pool sizes
	if hasSymlinks {
		for _, ps := range []int{1, 8, mc} {
			if ps > mc {
				continue
			}
			configs = append(configs, tuningConfig{
				Name:             fmt.Sprintf("pool=%d_follow=true", ps),
				PoolSize:         ps,
				InitialWorkerCap: 64,
				ResultBuffSize:   256,
				FollowLinks:      true,
			})
		}
	}

	return configs
}

// ---------------------------------------------------------------------
// Utility functions
// ---------------------------------------------------------------------

// medianDuration returns the median of a slice of durations.
func medianDuration(durs []time.Duration) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// formatDuration formats a duration for human-readable logging.
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return d.String()
	}
	return d.Round(time.Millisecond).String()
}

// humanBytes formats a byte count into a human-readable string.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), suffix[exp])
}

// treeSizeHuman returns the approximate total size of all files under root.
func treeSizeHuman(root string) string {
	var total int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return humanBytes(total)
}

// dirCountHuman returns the number of directories and files under root.
func dirCountHuman(root string) string {
	var dirs, files int
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs++
		} else {
			files++
		}
		return nil
	})
	return fmt.Sprintf("%d dirs, %d files", dirs, files)
}



