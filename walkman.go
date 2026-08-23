// Package walkman
package walkman

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	wsp "github.com/PAKIWASI/workstealpool"
)

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
	ret []fs.DirEntry
	err error
}

type ctxSkipList struct{}

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

func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) Walkman {

	return Walkman{
		conf: walkConf{
			followLinks: followLinks,
			maxDepth:    maxDepth,
			skipList:    skipList,
		},
		// TODO: set pool size etc based on GOMAXPROCS and other stuff after seeing the
		// performance differences in the benchmarks
		pool: wsp.NewWorkerPool(context.WithValue(context.Background(), ctxSkipList, skipList), 8, 8, 0,
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
				
				for i := range dirs {
					if slices.Contains(ctx.Value(ctxSkipList), dirs[i].Name()) {

					}
					if dirs[i].IsDir() {
						spawn(filepath.Join(item, dirs[i].Name()))
					}
				}
				filepath.WalkDir(root string, fn fs.WalkDirFunc)
				// TODO: skipping dirs?
				return &WalkResult{ret: dirs, err: nil}, nil
			}),
	}
}

func (w *Walkman) Walk(root string) {

}

func main() {

}
