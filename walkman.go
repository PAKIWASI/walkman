// Package walkman
package walkman

import (
	"context"
	"fmt"
	"io/fs"
	"os"

	wsp "github.com/PAKIWASI/workstealpool"
)

type walkConf struct {
	followLinks bool
	sortOutput  bool // our worker pool does not give us this, this will have to be another layer on top
	bfs         bool // this too
	maxDepth    uint32
	skipList    []string
}

type walkStats struct {
	files    uint32
	dirs     uint32
	links    uint32
	skipped  uint32
	maxDepth uint32
}

type WalkResult struct {
	d fs.DirEntry
	err error
}

type Walkman struct {
	conf  walkConf
	stats walkStats
	pool wsp.WorkerPool[string, WalkResult]
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

func NewWalkman() *Walkman {
	return &Walkman{
		pool: wsp.NewWorkerPool(context.Background(), 16, 8, 0,
			func(ctx context.Context, item string, spawn func(string)) (result *WalkResult, err error) {

			}),
	}
}

func (w *Walkman) Walk(root string) {

}





func main() {


}
