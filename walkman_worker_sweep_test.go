// Drop this alongside walkman_bench_test.go. It reuses walkParallel from
// there and adds:
//
//   1. A worker-count sweep as sub-benchmarks (b.Run), so `go test -bench`
//      gives you the whole scaling curve in one invocation instead of one
//      hardcoded PoolConfig.
//   2. WALKMAN_BENCH_ROOT support inherited from benchRoot() in the
//      existing file — point this at a build_tree.sh output, not "/",
//      per the non-determinism problem in bench.log.
//   3. benchstat-compatible output: `go test -bench=WorkerSweep -count=10`
//      piped through `benchstat` gives you confidence intervals and
//      flags noisy results automatically, instead of eyeballing an
//      avg/min/max triple from a hand-rolled loop.
//
// Usage:
//   ./build_tree.sh --shape wide --root /tmp/tree_wide --seed 42
//   WALKMAN_BENCH_ROOT=/tmp/tree_wide go test -bench=WorkerSweep -benchmem -count=10 ./... | tee new.txt
//   benchstat new.txt
//
// To compare two versions of walkman (e.g. before/after a workstealpool
// change), run this against both and: benchstat old.txt new.txt

package walkman

import (
	"runtime"
	"testing"
)

func BenchmarkWorkerSweep(b *testing.B) {
	root := benchRoot()

	workerCounts := []int{1, 2, 4, 8, runtime.GOMAXPROCS(0)}
	seen := map[int]bool{}

	for _, n := range workerCounts {
		if seen[n] {
			continue // GOMAXPROCS(0) may duplicate one of the fixed values on small machines
		}
		seen[n] = true

		b.Run(workersLabel(n), func(b *testing.B) {
			pc := PoolConfig{PoolSize: n, InitialWorkerCap: 32, ResultBuffSize: 64}
			var files, dirs int
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				files, dirs = walkParallel(b, root, benchSkip, pc)
			}
			b.StopTimer()
			b.ReportMetric(float64(files), "files")
			b.ReportMetric(float64(dirs), "dirs")
		})
	}
}

func workersLabel(n int) string {
	switch n {
	case 1:
		return "workers=1_(fair_vs_rust_sequential)"
	default:
		return "workers=" + itoa(n)
	}
}
