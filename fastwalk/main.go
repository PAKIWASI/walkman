// Package main provides a CLI wrapper around the fastwalk package
// (github.com/charlievieth/fastwalk) built to be directly comparable to
// the Go `walkman` package and the Rust `ignore` wrapper. Same flags, same
// semantics, same output shape, so timing numbers between the binaries
// are actually measuring the same thing.
//
//	fastwalk-cli [OPTIONS] [ROOT]
//
//	--max-depth N     0 = unlimited (default: 0)
//	--follow-links    follow symlinks (default: off)
//	--skip a,b,c      comma-separated names to prune
//	--print           print every entry
//	--bench N         repeat N times, report timing stats
//	--quiet           suppress summary line
//	--workers N       pool size (0 = fastwalk default, matches DefaultNumWorkers())
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charlievieth/fastwalk"
)

func main() {
	maxDepth := flag.Uint("max-depth", 0, "0 = unlimited")
	followLinks := flag.Bool("follow-links", false, "follow symlinks")
	skipStr := flag.String("skip", "", "comma-separated names to prune")
	print := flag.Bool("print", false, "print every entry")
	bench := flag.Uint("bench", 0, "repeat N times, report timing stats (0 = single run)")
	quiet := flag.Bool("quiet", false, "suppress summary line")
	workers := flag.Int("workers", 0, "pool size (0 = fastwalk default)")
	flag.Parse()

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	skipSet := make(map[string]struct{})
	if *skipStr != "" {
		for _, s := range strings.Split(*skipStr, ",") {
			if s != "" {
				skipSet[s] = struct{}{}
			}
		}
	}

	cleanRoot := filepath.Clean(root)

	runOnce := func() (files, dirs, links, errs uint64, elapsed time.Duration) {
		var (
			fCount atomic.Uint64
			dCount atomic.Uint64
			lCount atomic.Uint64
			eCount atomic.Uint64
			outMu  sync.Mutex
		)

		conf := fastwalk.Config{
			Follow:     *followLinks,
			MaxDepth:   int(*maxDepth),
			NumWorkers: *workers,
		}

		walkFn := func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				eCount.Add(1)
				if *print {
					outMu.Lock()
					fmt.Fprintf(os.Stderr, "error: %s %v\n", path, err)
					outMu.Unlock()
				}
				// Continue walking despite errors to mirror walkman / ignore semantics
				return nil
			}

			if de == nil {
				return nil
			}

			// Exclude the root entry (depth 0) itself from counts/print,
			// matching walkman / ignore-parallel-cli semantics.
			if depth := fastwalk.DirEntryDepth(de); depth == 0 || path == cleanRoot || path == root {
				return nil
			}

			name := de.Name()
			if len(skipSet) > 0 {
				if _, skip := skipSet[name]; skip {
					if de.IsDir() {
						return fastwalk.SkipDir
					}
					return nil
				}
			}

			t := de.Type()
			kind := "f"
			switch {
			case t&fs.ModeSymlink != 0:
				kind = "l"
				lCount.Add(1)
			case t.IsDir():
				kind = "d"
				dCount.Add(1)
			default:
				kind = "f"
				fCount.Add(1)
			}

			if *print {
				outMu.Lock()
				fmt.Printf("%s  %s\n", kind, path)
				outMu.Unlock()
			}

			return nil
		}

		start := time.Now()
		if err := fastwalk.Walk(&conf, root, walkFn); err != nil {
			fmt.Fprintf(os.Stderr, "walk failed: %v\n", err)
			os.Exit(1)
		}
		elapsed = time.Since(start)

		return fCount.Load(), dCount.Load(), lCount.Load(), eCount.Load(), elapsed
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
