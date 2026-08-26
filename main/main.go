// walkman-bench: a CLI wrapper around the walkman package built to be
// directly comparable to walkdir-cli (the Rust `walkdir` wrapper). Same
// flags, same semantics, same output shape, so timing numbers between the
// two binaries are actually measuring the same thing.
//
// Usage mirrors walkdir-cli exactly:
//
//	walkman-bench [OPTIONS] [ROOT]
//
//	--max-depth N     0 = unlimited (default: 0)
//	--follow-links    follow symlinks (default: off)
//	--skip a,b,c      comma-separated names to prune
//	--print           print every entry
//	--bench N         repeat N times, report timing stats
//	--quiet           suppress summary line
//	--workers N       pool size (0 = GOMAXPROCS default, matches defaultPoolConfig)

// TODO: symlink cycle detection is not present right now, do that to test that with walkdir aswell
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"time"

	walkman "github.com/PAKIWASI/walkman"
)

func main() {
	maxDepth := flag.Uint("max-depth", 0, "0 = unlimited")
	followLinks := flag.Bool("follow-links", false, "follow symlinks")
	skipStr := flag.String("skip", "", "comma-separated names to prune")
	print := flag.Bool("print", false, "print every entry")
	bench := flag.Uint("bench", 0, "repeat N times, report timing stats (0 = single run)")
	quiet := flag.Bool("quiet", false, "suppress summary line")
	workers := flag.Int("workers", 0, "pool size (0 = GOMAXPROCS default)")
	flag.Parse()

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	var skip []string
	if *skipStr != "" {
		skip = strings.Split(*skipStr, ",")
	}

	pc := walkman.PoolConfig{
		PoolSize:         *workers,
		InitialWorkerCap: 64,
		ResultBuffSize:   128,
	}
	if pc.PoolSize == 0 {
		pc.PoolSize = runtime.GOMAXPROCS(0)
	}

	runOnce := func() (files, dirs, links, errs uint64, elapsed time.Duration) {
		w := walkman.NewWalkmanWithConfig(*followLinks, uint32(*maxDepth), skip, true, pc)
		start := time.Now()
		for r := range w.Walk(root) {
			if r.Err != nil {
				errs++
				if *print {
					fmt.Fprintf(os.Stderr, "error: %s %v\n", r.Dir, r.Err)
				}
				continue
			}
			if !*print {
				continue
			}
			for _, e := range r.Ret {
				t := e.Type()
				kind := "f"
				switch {
				case t&fs.ModeSymlink != 0 && *followLinks:
					full := r.Dir + "/" + e.Name()
					info, err := os.Stat(full) // Stat follows the symlink
					switch {
					case err != nil:
						continue
					case info.IsDir():
						kind = "d"
					}
				case t&fs.ModeSymlink != 0:
					kind = "l"
				case t.IsDir():
					kind = "d"
				}
				fmt.Printf("%s  %s\n", kind, r.Dir+"/"+e.Name())
			}
		}
		elapsed = time.Since(start)
		if err := w.Wait(); err != nil {
			fmt.Fprintf(os.Stderr, "walk failed: %v\n", err)
			os.Exit(1)
		}
		f, d, l, _, _ := w.Stats()
		files, dirs, links = uint64(f), uint64(d), uint64(l)
		return
	}

	if *bench > 0 {
		n := int(*bench)
		durations := make([]time.Duration, 0, n)
		var lastFiles, lastDirs, lastLinks, lastErrs uint64
		var total time.Duration
		min := time.Duration(1<<63 - 1)
		var max time.Duration
		for i := 0; i < n; i++ {
			f, d, l, e, elapsed := runOnce()
			lastFiles, lastDirs, lastLinks, lastErrs = f, d, l, e
			durations = append(durations, elapsed)
			total += elapsed
			if elapsed < min {
				min = elapsed
			}
			if elapsed > max {
				max = elapsed
			}
		}
		avg := total / time.Duration(n)
		if !*quiet {
			fmt.Printf("files=%d dirs=%d links=%d errors=%d\n", lastFiles, lastDirs, lastLinks, lastErrs)
		}
		fmt.Printf("bench: n=%d avg=%v min=%v max=%v avg_ns_op=%d\n", n, avg, min, max, avg.Nanoseconds())
		return
	}

	files, dirs, links, errs, elapsed := runOnce()
	if !*quiet {
		fmt.Printf("files=%d dirs=%d links=%d errors=%d\n", files, dirs, links, errs)
		fmt.Printf("elapsed=%v\n", elapsed)
	}
}
