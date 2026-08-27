// Package walkman
package walkman

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	wsp "github.com/PAKIWASI/workstealpool"
)

// Sentinel errors reported via WalkResult.Err
var (
	errSymlinkCycle = errors.New("walkman: symlink cycle")
	errNoDevInoInfo = errors.New("walkman: no dev/ino info available")
)

type walkConf struct {
	followLinks bool // off by default
	maxDepth    uint32 // 0 means unlimited
	skipSet     map[string]struct{}
}

// DirErr is one error encountered while producing a WalkResult.
// Name identifies which file/dir caused the err while
// WalkResult.Dir is the directory being listed
type DirErr struct {
	Name string
	Err  error
}

// WalkResult is one directory's outcome.
//
// Ret and Err are not mutually exclusive: a directory can list
// successfully (Ret populated) while individual entries inside it still
// had problems (e.g. one dangling symlink, one detected cycle), each
// recorded as its own ItemErr in Err alongside the otherwise-complete Ret.
// Ret is nil only when the directory itself couldn't be read at all, in
// which case Err holds exactly that one failure.
type WalkResult struct {
	Dir     string
	Entries []fs.DirEntry
	Errs    []DirErr
}

// walkItem is the actual work-stealing pool item. parent is only non-nil
// when followLinks is on, so plain walks pay zero heap allocation overhead.
type walkItem struct {
	depth  uint32
	path   string
	parent *walkItem
}

// dirKey identifies a directory by device+inode, stable across the
// different paths that can reach the same directory (e.g. through a symlink).
type dirKey struct {
	dev, ino uint64
}

// containsTarget walks the parent chain from item up to root,
// Lstat'ing each one fresh and comparing it against k. Returns true (a
// cycle) at the first match. Only called once per followed symlink.
//
// A failed Lstat on an ancestor (raced away, permissions) just means that
// ancestor is skipped rather than erroring the whole walk.
func (item *walkItem) containsTarget(k dirKey) bool {
	for n := item; n != nil; n = n.parent {
		nk, err := statKey(n.path)
		if err != nil {
			continue
		}
		if nk == k {
			return true
		}
	}
	return false
}

// statKey Lstat's path and returns its dirKey. Only called when followLinks is on,
// and only against a resolved symlink target and the ancestors being checked against it.
func statKey(path string) (dirKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return dirKey{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return dirKey{}, errNoDevInoInfo
	}
	return dirKey{dev: uint64(st.Dev), ino: st.Ino}, nil
}

// PoolConfig exposes the underlying worker-pool knobs
type PoolConfig struct {
	PoolSize         int
	InitialWorkerCap int
	ResultBuffSize   int
}

// DefaultPoolConfig matches PoolSize to GOMAXPROCS. Per the workstealpool
// README's own benchmarks: speedup on a CPU-bound divide-and-conquer
// workload tracks physical/logical core count and then plateaus right at
// GOMAXPROCS, with a slight regression going meaningfully past it
// (oversubscription). InitialWorkerCap/ResultBuffSize matter far less
// (a few hundred µs across their whole sweep). 32/64 here just matches
// the fixed baseline used across their other sweeps, not a walking-specific finding
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		PoolSize:         runtime.GOMAXPROCS(0),
		InitialWorkerCap: 32, // TODO: experiment with these numbers
		ResultBuffSize:   64,
	}
}

type Walkman struct {
	conf walkConf
	pool *wsp.WorkerPool[walkItem, WalkResult]
}

// NewWalkman builds a Walkman with GOMAXPROCS-based pool sizing.
// Use NewWalkmanWithConfig for explicit pool sizing.
func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) *Walkman {
	return NewWalkmanWithConfig(followLinks, maxDepth, skipList, DefaultPoolConfig())
}

// NewWalkmanWithConfig is NewWalkman but with explicit pool sizing, for
// callers who've measured what suits their workload rather than accepting the defaults.
func NewWalkmanWithConfig(
	followLinks bool,
	maxDepth uint32,
	skipList []string,
	pc PoolConfig,
) *Walkman {

	skipSet := make(map[string]struct{}, len(skipList))
	for _, v := range skipList {
		skipSet[v] = struct{}{}
	}

	w := &Walkman{
		conf: walkConf{
			followLinks: followLinks,
			maxDepth:    maxDepth,
			skipSet:     skipSet,
		},
	}

	// The pool is bound to one Task at construction. Which one we hand it
	// is the only thing that differs between plain and symlink-following
	// walks. same item type, same pool, no per-entry branch.
	execute := w.visit
	if followLinks {
		execute = w.visitSym
	}

	w.pool = wsp.NewWorkerPool(
		context.Background(),
		pc.PoolSize,
		pc.InitialWorkerCap,
		pc.ResultBuffSize,
		execute,
	)

	return w
}

// readDir opens name and reads every entry in it, unsorted. Deliberately
// using the File.ReadDir(-1) form rather than the package-level os.ReadDir,
// which sorts by filename. That sort is wasted work here since results
// are consumed by directory, not in a global sorted order anyway.
func readDir(name string) ([]fs.DirEntry, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return f.ReadDir(-1)
}

// join builds a child path from an already-clean parent and a bare entry
// name (never containing a separator). Deliberately not filepath.Join (variadic)
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	if dir[len(dir)-1] == os.PathSeparator {
		return dir + name
	}
	return dir + string(os.PathSeparator) + name
}

// filterSkipped removes, in place and without preserving order, every
// entry whose Name() is in skip. It's a swap-delete
func filterSkipped(dirs []fs.DirEntry, skip map[string]struct{}) []fs.DirEntry {
	n := len(dirs)
	for i := 0; i < n; {
		if _, present := skip[dirs[i].Name()]; present {
			n--
			dirs[i] = dirs[n]
			continue // re-check index i against the newly swapped-in entry
		}
		i++
	}
	return dirs[:n]
}

// visit is the Task run for every directory the walk encounters. It is
// called concurrently, from any worker in the pool for different items,
// so it must not touch anything on Walkman that isn't safe for that
// (conf is read-only after construction).
func (w *Walkman) visit(
	_ context.Context,
	item walkItem,
	spawn func(walkItem),
) (WalkResult, bool, error) {
	dirs, err := readDir(item.path)
	if err != nil {
		// A permission-denied (or similar) directory is a fact about that
		// one item, not a reason to kill every other worker in the pool.
		// Ret stays nil: the directory itself couldn't be read at all, so
		// there's nothing else this result can carry.
		return WalkResult{Dir: item.path, Errs: []DirErr{{Name: item.path, Err: err}}}, true, nil
	}

	before := len(dirs)

	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}

	for i := range dirs {
		entry := dirs[i]

		mode := entry.Type()
		isDir := mode.IsDir()
		isSymlink := mode&fs.ModeSymlink != 0

		// this Task never follows symlinks
		if isSymlink {
			continue
		}

		if !isDir {
			continue
		}

		if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
			continue
		}

		spawn(walkItem{
			path:  join(item.path, entry.Name()),
			depth: item.depth + 1,
		})
	}

	return WalkResult{Dir: item.path, Entries: dirs}, true, nil
}

// visitSym is visit's counterpart for followLinks: same item type, same
// pool, only used when the Walkman was built with followLinks on.
// It additionally resolves symlinked directories and walks into them,
// guarding against cycles via item.parent, the chain
// of directories from root down to here,
// regardless of whether each hop was a plain directory or a followed symlink.
func (w *Walkman) visitSym(
	_ context.Context,
	item walkItem,
	spawn func(walkItem),
) (WalkResult, bool, error) {
	dirs, err := readDir(item.path)
	if err != nil {
		return WalkResult{Dir: item.path, Errs: []DirErr{{Name: item.path, Err: err}}}, true, nil
	}

	before := len(dirs)

	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}

	// Err field is nil until the first problem, which is the common case
	result := WalkResult{Dir: item.path, Entries: dirs}

	for i := range dirs {
		entry := dirs[i]

		childPath := join(item.path, entry.Name())

		mode := entry.Type()
		isDir := mode.IsDir()
		isSymlink := mode&fs.ModeSymlink != 0

		if isSymlink {
			// followLinks is on, so a symlink is classified by what it
			// resolves to. One os.Stat resolves the whole chain
			info, err := os.Stat(childPath)
			if err != nil {
				// Dangling/unresolvable symlink is skipped.
				continue
			}

			if !info.IsDir() {
				// Resolves to a non-directory (file, device, etc)
				continue
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			k := dirKey{dev: uint64(st.Dev), ino: st.Ino}

			// Cycle detected: record it against this one entry and skip
			// descending into it, but keep processing the rest of dirs.
			if item.containsTarget(k) {
				result.Errs = append(result.Errs, DirErr{Name: entry.Name(), Err: errSymlinkCycle})
				continue
			}

			// A symlink-to-directory entry never has fs.ModeDir set on its
			// own DirEntry. Force isDir now that we've resolved it.
			isDir = true
		}

		if !isDir {
			continue
		}

		if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
			continue
		}

		spawn(walkItem{
			path:   childPath,
			depth:  item.depth + 1,
			parent: &item,
		})
	}

	return result, true, nil
}

// Walk starts walking root and returns a channel of per-directory results.
// The channel closes once every worker has finished (no work left, or a
// fatal error occurred). Call Wait after draining the channel to get the
// terminal error, if any.
func (w *Walkman) Walk(root string) <-chan WalkResult {
	// Clean once, here, so every child path built during the walk (via
	// join, not filepath.Join) can assume its parent is already clean
	// without re-running filepath.Clean per entry.
	root = filepath.Clean(root)

	w.pool.Submit(walkItem{path: root, depth: 0})
	return w.pool.Run()
}

// Wait blocks until the walk has fully finished and returns the first
// fatal error encountered (nil on normal completion). Call it after
// draining the channel returned by Walk
func (w *Walkman) Wait() error {
	return w.pool.Wait()
}
