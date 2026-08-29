// Package walkman implements a concurrent directory walker.
// It gives iterative control of each entry to the user via a channel
package walkman

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"unsafe"

	wsp "github.com/PAKIWASI/workstealpool"
)

// Sentinel errors reported via WalkResult.Err
var (
	errSymlinkCycle    = errors.New("walkman: symlink cycle")
	errNoDevInoInfo    = errors.New("walkman: no dev/ino info available")
	errDanglingSymlink = errors.New("walkman: dangling/unresolved symlink")
)

type walkConf struct {
	followLinks bool   // off by default
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

type walkItem struct {
	depth uint32
	leaf  *pathNode
}

type pathNode struct {
	name   string
	parent *pathNode
}

// dirKey identifies a directory by device+inode, stable across the
// different paths that can reach the same directory (e.g. through a symlink).
type dirKey struct {
	dev, ino uint64
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
		ResultBuffSize:   256,
	}
}

type WorkerState struct {
	dirBuf  []dirKey
	pathBuf []byte
	offsets []int
}

type Walkman struct {
	conf    walkConf
	pool    *wsp.WorkerPool[walkItem, WalkResult]
	workers []WorkerState
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

	w.workers = make([]WorkerState, pc.PoolSize)
	for i := range w.workers {
		w.workers[i].dirBuf = make([]dirKey, 16)
		w.workers[i].pathBuf = make([]byte, 256)
		w.workers[i].offsets = make([]int, 0, 16)
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

func joinLeaf(parent, leaf []byte) []byte {
	n := len(parent)
	if n == 0 {
		return leaf
	}
	l := len(leaf)
	if l == 0 {
		return parent
	}
	s := 0
	sep := byte(os.PathSeparator)
	if parent[n-1] != sep {
		s++
	}
	// grow and reslice
	parent = slices.Grow(parent, n+l+s)
	parent = parent[:n+l+s]
	if s == 1 {
		parent[n] = sep
	}
	copy(parent[n+s:n+l+s], leaf)
	return parent
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

// computePath materializes leaf's full path into buf (growing if needed).
// If offsets is non-nil, it also records, for each node from leaf up to
// root (same order as walking the leaf.parent chain, offsets[0] = leaf
// itself), the exclusive end index of that node's own path within the
// returned buf. Since every ancestor's path is a prefix of leaf's full
// path, buf[:offsets[i]] is the i-th ancestor's complete path with zero
// extra materialization - callers doing per-ancestor lookups (cycle
// detection) get every ancestor path as a slice of this one buffer.
func computePath(leaf *pathNode, buf []byte, offsets []int) ([]byte, []int) {
	reqSize, depth := 0, 0
	for n := leaf; n != nil; n = n.parent {
		reqSize += len(n.name)
		if n.parent != nil {
			reqSize++
		}
		depth++
	}
	buf = slices.Grow(buf, reqSize)
	buf = buf[:reqSize]

	if offsets != nil {
		offsets = slices.Grow(offsets[:0], depth)
		offsets = offsets[:depth]
	}

	pos := reqSize
	i := 0
	for n := leaf; n != nil; n = n.parent {
		if offsets != nil {
			offsets[i] = pos // end-of-path offset for n, captured before we consume its bytes
			i++
		}
		pos -= len(n.name)
		copy(buf[pos:], n.name)
		if n.parent != nil {
			pos--
			buf[pos] = os.PathSeparator
		}
	}
	return buf, offsets
}

// save because we only need a read-only view
func byteToStringNoCopy(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// save because we only need a read-only view
func stringTobyteNoCopy(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// visit is the Task run for every directory the walk encounters. It is
// called concurrently, from any worker in the pool for different items,
// so it must not touch anything on Walkman that isn't safe for that
// (conf is read-only after construction).
func (w *Walkman) visit(
	_ context.Context,
	workerID int,
	item walkItem,
	spawn func(walkItem),
) (WalkResult, bool, error) {

	worker := &w.workers[workerID]
	worker.pathBuf, _ = computePath(item.leaf, worker.pathBuf, nil)
	// copy made: this copy is sent as result
	pathStr := string(worker.pathBuf)
	// view into the byte buffer: for local calls to function that accept a string
	pathView := byteToStringNoCopy(worker.pathBuf)

	dirs, err := readDir(pathView)
	if err != nil {
		// A permission-denied (or similar) directory is a fact about that
		// one item, not a reason to kill every other worker in the pool.
		// Ret stays nil: the directory itself couldn't be read at all, so
		// there's nothing else this result can carry.
		return WalkResult{Dir: pathStr, Errs: []DirErr{{Name: pathStr, Err: err}}}, true, nil
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
			depth: item.depth + 1,
			leaf:  &pathNode{name: entry.Name(), parent: item.leaf},
		})
	}

	return WalkResult{Dir: pathStr, Entries: dirs}, true, nil
}

// visitSym is visit's counterpart for followLinks: same item type, same
// pool, only used when the Walkman was built with followLinks on.
// It additionally resolves symlinked directories and walks into them,
// guarding against cycles via item.parent, the chain
// of directories from root down to here,
// regardless of whether each hop was a plain directory or a followed symlink.
func (w *Walkman) visitSym(
	_ context.Context,
	workerID int,
	item walkItem,
	spawn func(walkItem),
) (WalkResult, bool, error) {

	worker := &w.workers[workerID]
	worker.pathBuf, worker.offsets = computePath(item.leaf, worker.pathBuf, worker.offsets)
	// copy made: this copy is sent as result
	pathStr := string(worker.pathBuf)
	// view into the byte buffer: for local calls to function that accept a string
	pathView := byteToStringNoCopy(worker.pathBuf)

	dirs, err := readDir(pathView)
	if err != nil {
		return WalkResult{Dir: pathStr, Errs: []DirErr{{Name: pathStr, Err: err}}}, true, nil
	}

	before := len(dirs)

	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}

	// Err field is nil until the first problem, which is the common case
	result := WalkResult{Dir: pathStr, Entries: dirs}

	// We do a syscall for dirkey only once per directory.
	// The rest of the symlinks just reuse those dirkey values
	ancestorsResolved := false

	hasCycle := func(k dirKey) bool {
		if !ancestorsResolved {
			ancestorsResolved = true
			worker.dirBuf = worker.dirBuf[:0]
			for _, end := range worker.offsets {
				nk, err := statKey(byteToStringNoCopy(worker.pathBuf[:end]))
				if err != nil {
					continue
				}
				worker.dirBuf = append(worker.dirBuf, nk)
			}
		}
		for _, ak := range worker.dirBuf {
			if ak == k {
				return true
			}
		}
		return false
	}

	// no new heap alloc, just shortening len
	swapDel := func(i int) []fs.DirEntry {
		n := len(dirs)
		dirs[i] = dirs[n-1]
		return dirs[:n-1]
	}

	// swapDel below shrinks dirs mid-loop (cycle case), which would walk the slice out of bounds.
	// i is only advanced when we don't delete, so a swapped-in entry at position i gets rechecked.
	for i := 0; i < len(dirs); {
		entry := dirs[i]

		// childPath := join(item.path, entry.Name())

		mode := entry.Type()
		isDir := mode.IsDir()
		isSymlink := mode&fs.ModeSymlink != 0

		if isSymlink {
			// followLinks is on, so a symlink is classified by what it
			// resolves to. One os.Stat resolves the whole chain
			leafBuf := joinLeaf(worker.pathBuf, stringTobyteNoCopy(entry.Name()))
			info, err := os.Stat(byteToStringNoCopy(leafBuf))
			if err != nil {
				// Dangling/unresolvable symlink. Report via the err slice so
				// users can easily distingish them
				result.Errs = append(
					result.Errs,
					DirErr{Name: entry.Name(), Err: errDanglingSymlink},
				)
				dirs = swapDel(i)
				continue
			}

			if !info.IsDir() {
				// Resolves to a non-directory (file, device, etc)
				// when symlink is a non-dir, we should resolve and report that file
				dirs[i] = fs.FileInfoToDirEntry(info)
				i++
				continue
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				i++
				continue
			}
			k := dirKey{dev: uint64(st.Dev), ino: st.Ino}

			// Cycle detected: record it against this one entry and skip
			// descending into it, but keep processing the rest of dirs.
			if hasCycle(k) {
				result.Errs = append(
					result.Errs,
					DirErr{Name: entry.Name(), Err: errSymlinkCycle},
				)
				dirs = swapDel(i)
				continue // recheck index i, now holding the swapped-in entry
			}

			// A symlink-to-directory entry never has fs.ModeDir set on its
			// own DirEntry (Lstat sees fs.ModeSymlink). Rewrite it to the
			// resolved, dir-typed DirEntry, same as the non-dir branch
			// above, so callers inspecting Entries see it as a directory.
			dirs[i] = fs.FileInfoToDirEntry(info)
			isDir = true
		}

		if !isDir {
			i++
			continue
		}

		if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
			i++
			continue
		}

		spawn(walkItem{
			depth: item.depth + 1,
			leaf:  &pathNode{name: entry.Name(), parent: item.leaf},
		})
		i++
	}

	// dirs may have been reassigned (shortened) by swapDel above; result
	// was built from the pre-loop dirs header, so refresh it here.
	result.Entries = dirs

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

	// parent is nil for the first walkItem
	w.pool.Submit(walkItem{depth: 0, leaf: &pathNode{name: root}})
	return w.pool.Run()
}

// Wait blocks until the walk has fully finished and returns the first
// fatal error encountered (nil on normal completion). Call it after
// draining the channel returned by Walk
func (w *Walkman) Wait() error {
	return w.pool.Wait()
}
