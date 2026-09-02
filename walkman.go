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
	"syscall"

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
	skipSet     map[string]struct{} // directories/files to skip from the result
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
// recorded as its own ItemErr in Err alongside the otherwise complete Ret.
// Ret is nil only when the directory itself couldn't be read at all, in
// which case Err holds exactly that one failure.
type WalkResult struct {
	Dir     string
	Entries []fs.DirEntry
	Errs    []DirErr
}

// walkItem is the input to each worker's Task function
// each worker get's one of these and do the work on it and then spawn
// more work (if needed) by creating another walkItem
type walkItem struct {
	depth uint32
	leaf  *pathNode
}

// pathNode is a linked list that goes from any walkItem (anything in the filesystem tree)
// to it's parent node and also stores the full path (starting from root) upto that node
// used as path storage as well as to detect symlinks (Lstat each path as we go up the chain)
type pathNode struct {
	path   string
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

// PoolConfig exposes the underlying workerpool knobs
type PoolConfig struct {
	PoolSize         int
	InitialWorkerCap int
	ResultBuffSize   int
}

// DefaultPoolConfig matches PoolSize to GOMAXPROCS. Per the workstealpool
// README's own benchmarks: speedup on a CPU-bound divide-and-conquer
// workload tracks physical/logical core count and then plateaus right at
// GOMAXPROCS, with a slight regression going meaningfully past it (oversubscription).
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		PoolSize:         runtime.GOMAXPROCS(0),
		InitialWorkerCap: 32,
		ResultBuffSize:   128,
	}
}

type WorkerState struct {
	// scratch space for symlink detection
	dirBuf    []dirKey
	// Storage for all paths this worker computed
	pathStore StringStore
	// each worker buffers walkItems for each subdir
	// then does one spawn() with the slice.
	// Only ever touched by the "spawn" branch of visitDir/visitSymDir,
	// which builds it, calls spawn(), and returns without recursing —
	// never held live across an inline-recursive call, which would
	// alias and corrupt it (see visitDir).
	spawnBuf []walkItem
}

// inlineCutoff/maxInlineChain default: see plan.md 1.1. Below inlineCutoff
// child directories, a worker walks them synchronously instead of paying
// spawn overhead (deque push + pending atomic + wakeup broadcast) for
// work that's cheaper to just do inline. maxInlineChain caps consecutive
// inline levels so a long, narrow subtree (e.g. a single-child chain)
// can't monopolize a worker and hide from stealing indefinitely.
const (
	defaultInlineCutoff   = 2
	defaultMaxInlineChain = 32
)

type Walkman struct {
	conf walkConf
	pool *wsp.WorkerPool[walkItem, WalkResult]
	// per-worker state keyed by workerID
	workers []WorkerState

	inlineCutoff   int
	maxInlineChain int
	// results produced by inline recursion (i.e. every directory visited
	// that wasn't itself submitted/spawned as a pool item) are sent here,
	// bypassing the pool's normal one-result-per-Task path entirely.
	// Walk() fans this in alongside the pool's own results channel.
	inlineOut chan WalkResult
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
		inlineCutoff:   defaultInlineCutoff,
		maxInlineChain: defaultMaxInlineChain,
		// buffered so a burst of inline results from one worker doesn't
		// immediately block on the fan-in goroutine; sized off ResultBuffSize
		// since it's the same kind of "how bursty can producers get" knob
		inlineOut: make(chan WalkResult, pc.ResultBuffSize),
	}

	w.workers = make([]WorkerState, pc.PoolSize)
	if !w.conf.followLinks {
		for i := range w.workers {
			w.workers[i].dirBuf = make([]dirKey, 16)
		}
	}
	for i := range w.workers {
		w.workers[i].pathStore = NewPathStorage(0)
		w.workers[i].spawnBuf = make([]walkItem, 8)
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

// Stores a path in this worker's storage and returns a slice to it
func (w *Walkman) newPath(workerID int, parent, child string) string {
	id := w.workers[workerID].pathStore.StorePath(parent, child)
	return id.String()
}

// visit is the Task run for every directory the walk encounters. It is
// called concurrently, from any worker in the pool for different items,
// so it must not touch anything on Walkman that isn't safe for that
// (conf is read-only after construction)
func (w *Walkman) visit(
	ctx context.Context,
	workerID int,
	item walkItem,
	spawn func(...walkItem),
) (WalkResult, bool, error) {
	return w.visitDir(ctx, workerID, item, spawn, 0), true, nil
}

// visitDir does the actual work for one directory, then either recurses
// inline (small fan-out, cheaper than spawn overhead) or spawns each
// child directory onto the pool (large fan-out, worth distributing).
// inlineChain counts consecutive inline levels above this call, so a
// long, narrow subtree still eventually surfaces to the pool instead of
// running unpreemptibly on one worker forever (see plan.md 1.1 "Risk").
func (w *Walkman) visitDir(
	ctx context.Context,
	workerID int,
	item walkItem,
	spawn func(...walkItem),
	inlineChain int,
) WalkResult {
	path := item.leaf.path
	dirs, err := readDir(path)
	if err != nil {
		// A permission-denied (or similar) directory is a fact about that
		// one item, not a reason to kill every other worker in the pool.
		// Ret stays nil: the directory itself couldn't be read at all, so
		// there's nothing else this result can carry.
		return WalkResult{Dir: path, Errs: []DirErr{{Name: path, Err: err}}}
	}

	before := len(dirs)
	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}

	result := WalkResult{Dir: path, Entries: dirs}

	// count child dirs first (cheap, no allocation) so we know whether to
	// take the inline path or the spawn path before doing any work
	descend := w.conf.maxDepth == 0 || item.depth+1 <= w.conf.maxDepth
	childDirCount := 0
	if descend {
		for i := range dirs {
			mode := dirs[i].Type()
			if mode.IsDir() && mode&fs.ModeSymlink == 0 {
				childDirCount++
			}
		}
	}

	if childDirCount == 0 {
		return result
	}

	if childDirCount <= w.inlineCutoff && inlineChain < w.maxInlineChain {
		// Below cutoff: walk children synchronously, right here, on this
		// worker, instead of paying spawn overhead. Each child's own
		// result is produced by recursing into visitDir (which may itself
		// inline further or spawn, depending on ITS OWN child count) and
		// forwarded to inlineOut manually, since this Task call only gets
		// to return one WalkResult (this directory's own).
		for i := range dirs {
			entry := dirs[i]
			mode := entry.Type()
			if mode&fs.ModeSymlink != 0 || !mode.IsDir() {
				continue
			}
			childItem := walkItem{
				depth: item.depth + 1,
				leaf:  &pathNode{path: w.newPath(workerID, path, entry.Name()), parent: item.leaf},
			}
			childResult := w.visitDir(ctx, workerID, childItem, spawn, inlineChain+1)
			select {
			case w.inlineOut <- childResult:
			case <-ctx.Done():
				return result
			}
		}
		return result
	}

	// childDirCount > cutoff: batch-spawn as before. spawnBuf is only
	// ever used here — built and handed to spawn() (which copies every
	// value out immediately) in the same call, never held across a
	// recursive call, so reusing it across Task invocations is safe.
	children := w.workers[workerID].spawnBuf[:0]
	for i := range dirs {
		entry := dirs[i]
		mode := entry.Type()
		if mode&fs.ModeSymlink != 0 || !mode.IsDir() {
			continue
		}
		children = append(children, walkItem{
			depth: item.depth + 1,
			leaf:  &pathNode{path: w.newPath(workerID, path, entry.Name()), parent: item.leaf},
		})
	}
	w.workers[workerID].spawnBuf = children
	spawn(children...)

	return result
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
	spawn func(...walkItem),
) (WalkResult, bool, error) {

	path := item.leaf.path
	dirs, err := readDir(path)
	if err != nil {
		return WalkResult{Dir: path, Errs: []DirErr{{Name: path, Err: err}}}, true, nil
	}

	before := len(dirs)
	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}

	// Err field is nil until the first problem, which is the common case
	result := WalkResult{Dir: path, Entries: dirs}

	// We do a syscall for dirkey only once per directory.
	// The rest of the symlinks just reuse those dirkey values
	// TODO: value vs pointer
	dirBuf := w.workers[workerID].dirBuf[:0]
	ancestorsResolved := false

	hasCycle := func(k dirKey) bool {
		if !ancestorsResolved {
			ancestorsResolved = true
			for n := item.leaf; n != nil; n = n.parent {
				nk, err := statKey(n.path)
				if err != nil {
					continue
				}
				dirBuf = append(dirBuf, nk)
			}
		}
		for _, ak := range dirBuf {
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

	spawnBuf := w.workers[workerID].spawnBuf[:0]

	// swapDel below shrinks dirs mid-loop (cycle case), which would walk the slice out of bounds.
	// i is only advanced when we don't delete, so a swapped-in entry at position i gets rechecked.
	for i := 0; i < len(dirs); {
		entry := dirs[i]

		childPath := w.newPath(workerID, path, entry.Name())

		mode := entry.Type()
		isDir := mode.IsDir()
		isSymlink := mode&fs.ModeSymlink != 0

		if isSymlink {
			// followLinks is on, so a symlink is classified by what it
			// resolves to. One os.Stat resolves the whole chain
			info, err := os.Stat(childPath)
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

		spawnBuf = append(spawnBuf, walkItem{
			depth: item.depth + 1,
			leaf: &pathNode{path: childPath, parent: item.leaf},
		})
		// spawn(walkItem{
		// 	depth: item.depth + 1,
		// 	leaf:  &pathNode{path: childPath, parent: item.leaf},
		// })
		i++
	}
	
	spawn(spawnBuf...)

	// dirs may have been reassigned (shortened) by swapDel above; result
	// was built from the pre-loop dirs header, so refresh it here.
	result.Entries = dirs

	return result, true, nil
}

// Walk starts walking root and returns a channel of per-directory results.
// The channel closes once every worker has finished (no work left, or a
// fatal error occurred). Call Wait after draining the channel to get the
// terminal error, if any.
//
// Results come from two producers that get fanned into the one channel
// returned here: the pool's own results (one per spawned/submitted item)
// and inlineOut (one per directory visited via inline recursion, see
// visitDir). It's safe to close inlineOut right after the pool's own
// channel closes: every inline send happens synchronously inside a Task
// call, which must return before that item's `pending` count decrements,
// which must happen before the pool can decide there's no work left
// anywhere and close its results channel — so by then every inline send
// for every submitted item has already completed.
func (w *Walkman) Walk(root string) <-chan WalkResult {
	// Clean once, here, so every child path built during the walk (via
	// join, not filepath.Join) can assume its parent is already clean
	// without re-running filepath.Clean per entry.
	root = filepath.Clean(root)

	// parent is nil for the first walkItem
	w.pool.Submit(walkItem{depth: 1, leaf: &pathNode{path: root}})
	poolResults := w.pool.Run()

	out := make(chan WalkResult, cap(w.inlineOut))
	go func() {
		defer close(out)
		inlineOut := w.inlineOut
		for poolResults != nil || inlineOut != nil {
			select {
			case r, ok := <-poolResults:
				if !ok {
					poolResults = nil
					continue
				}
				out <- r
			case r, ok := <-inlineOut:
				if !ok {
					inlineOut = nil
					continue
				}
				out <- r
			}
		}
	}()

	// Close inlineOut once every worker has exited (see doc comment above
	// for why this can't race a pending inline send).
	go func() {
		w.pool.Wait()
		close(w.inlineOut)
	}()

	return out
}

// Wait blocks until the walk has fully finished and returns the first
// fatal error encountered (nil on normal completion). Call it after
// draining the channel returned by Walk
func (w *Walkman) Wait() error {
	return w.pool.Wait()
}
