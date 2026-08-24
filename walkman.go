// Package walkman
package walkman

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	wsp "github.com/PAKIWASI/workstealpool"
)

// TODO:
// NOW:
// 1. max depth stuff
// 2. the TODOs scattered around
// LATER:
// 1. sorting, bfs
// 2. iterator support


type walkConf struct {
	followLinks bool
	// sortOutput  bool // our worker pool does not give us this, this will have to be another layer on top
	// bfs         bool // this too
	maxDepth uint32
	skipList []string
}

type walkStats struct {
	files           uint32
	dirs            uint32
	links           uint32
	skipped         uint32
	maxDepthReached uint32
}

type WalkResult struct {
	Dir string
	Ret []fs.DirEntry
	Err error
}

type Walkman struct {
	conf  walkConf
	stats walkStats
	pool  wsp.WorkerPool[string, WalkResult]
	depth uint32
}

func readDir(name string) ([]fs.DirEntry, error) {
	// The main path opener. works for both files and directories
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dirs, err := f.ReadDir(-1)
	return dirs, err
}

// TODO: provide another New func that takes in explicit values for pool config

func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) Walkman {

	return Walkman{
		conf: walkConf{
			followLinks: followLinks,
			maxDepth:    maxDepth,
			skipList:    skipList,
		},
		// TODO: set pool size etc based on GOMAXPROCS and other stuff after seeing the
		// performance differences in the benchmarks
		pool: wsp.NewWorkerPool(context.Background(), 8, 8, 0,
			// TODO: how can i (legally) put context to more use?
			func(ctx context.Context, item string, spawn func(string)) (result *WalkResult, err error) {
				f, err := os.Open(item)
				if err != nil {
					return nil, err
				}
				defer f.Close()

				dirs, err := f.ReadDir(-1)
				if err != nil {
					return nil, err
				}

				dirs = slices.DeleteFunc(dirs, func(d fs.DirEntry) bool {
					return slices.Contains(skipList, d.Name())
				})
				
				// TODO: recovrable erros like permission denied?

				for i := range dirs {
					if dirs[i].IsDir() {
						spawn(filepath.Join(item, dirs[i].Name()))
					}
				}

				return &WalkResult{Dir: item, Ret: dirs, Err: nil}, nil
			}),
	}
}

func (w *Walkman) Walk(root string) <-chan WalkResult {

	w.pool.Submit(root)
	ret := w.pool.Run()

	go func() {
		err := w.pool.Wait()
		fmt.Println(err)
	}()

	return ret
}

