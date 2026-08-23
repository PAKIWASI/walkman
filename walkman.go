// Package walkman
package walkman

import (
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

type Walkman struct {
	conf  walkConf
	stats walkStats
	wsp.LFdeque[string]
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




func main() {


}
