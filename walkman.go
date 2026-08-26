// Package walkman
package walkman

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"

	wsp "github.com/PAKIWASI/workstealpool"
)

// TODO:
// LATER:
// 1. sorting, bfs — the pool delivers WalkResults in true completion
//    order (whichever worker/thief finishes a directory first), not path
//    order. Getting a sorted or breadth-first view means buffering results
//    from the channel and re-ordering on top, as a separate layer — the
//    pool itself can't give you that for free without giving up stealing.
// 2. iterator support — once Go's range-over-func (iter.Seq) is in play
//    here, Walk could return an iterator instead of a channel. Same
//    ordering caveat as above applies.
// 3. pipelines?


type walkConf struct {
	followLinks bool   // off by default
	trackStats  bool   // off by default: every stat is an atomic.Add
	maxDepth    uint32 // 0 means unlimited
	skipSet     map[string]struct{}
}

// walkStats is written concurrently from every worker's Task invocation when trackStats is enabled
type walkStats struct {
	files           atomic.Uint32
	dirs            atomic.Uint32
	links           atomic.Uint32
	skipped         atomic.Uint32
	maxDepthReached atomic.Uint32
}

type WalkResult struct {
	Dir string
	Ret []fs.DirEntry
	Err error
}

// walkItem is the actual work-stealing pool item. ancestors is only ever
// non-nil when followLinks is on (visitSym populates it; visit ignores it
// and always spawns with it left at its zero value), so plain walks pay
// only for the width of one extra pointer field
type walkItem struct {
	depth     uint32
	path      string
	ancestors *ancestorNode
}

// dirKey identifies a directory by device+inode, stable across the
// different paths that can reach the same directory (e.g. through a symlink).
type dirKey struct {
	dev, ino uint64
}

// ancestorNode is a chain of every directory above the current walkItem in
// the walk's current path, root-to-here. Only built/consulted when
// followLinks is on; nil (and untouched) otherwise.
type ancestorNode struct {
	key    dirKey
	parent *ancestorNode
}

func (a *ancestorNode) contains(k dirKey) bool {
	for n := a; n != nil; n = n.parent {
		if n.key == k {
			return true
		}
	}
	return false
}

// statKey Lstat's path and returns its dirKey. Only called when followLinks
// is on: once per plain directory (to extend the ancestor chain in case a
// symlink further down points back to it) and once per resolved symlink
// target (to check it against that chain).
func statKey(path string) (dirKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return dirKey{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return dirKey{}, fmt.Errorf("walkman: no dev/ino info for %s", path)
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
// (a few hundred µs across their whole sweep) — 32/64 here just matches
// the fixed baseline used across their other sweeps, not a walking-specific finding
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		PoolSize:         runtime.GOMAXPROCS(0),
		InitialWorkerCap: 32, // TODO: experiment with these numbers
		ResultBuffSize:   64,
	}
}

type Walkman struct {
	conf  walkConf
	stats walkStats
	pool  *wsp.WorkerPool[walkItem, WalkResult]
}

// NewWalkman builds a Walkman with GOMAXPROCS-based pool sizing and stats
// tracking off. Use NewWalkmanWithConfig for explicit pool sizing or to turn stats on.
func NewWalkman(followLinks bool, maxDepth uint32, skipList []string) *Walkman {
	return NewWalkmanWithConfig(followLinks, maxDepth, skipList, false, DefaultPoolConfig())
}

// NewWalkmanWithConfig is NewWalkman but with every knob explicit: whether
// to track walkStats (costs an atomic.Add per entry when on), and pool
// sizing, for callers who've measured what suits their workload rather
// than accepting the defaults.
func NewWalkmanWithConfig(
	followLinks bool,
	maxDepth uint32,
	skipList []string,
	trackStats bool,
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
			trackStats:  trackStats,
		},
	}

	// The pool is bound to one Task at construction. Which one we hand it
	// is the only thing that differs between plain and symlink-following
	// walks — same item type, same pool, no per-entry branch.
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
// name (never containing a separator). Deliberately not filepath.Join:
// that's variadic (a slice alloc) and re-runs filepath.Clean on the
// result, which is wasted work once the parent is already clean
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
// called concurrently, from any worker in the pool for different items—
// so it must not touch anything on Walkman that isn't safe for that
// (conf is read-only after construction; stats is all atomics).
func (w *Walkman) visit(
	_ context.Context,
	item walkItem,
	spawn func(walkItem),
) (*WalkResult, error) {
	dirs, err := readDir(item.path)
	if err != nil {
		// A permission-denied (or similar) directory is a fact about that
		// one item, not a reason to kill every other worker in the pool.
		return &WalkResult{Dir: item.path, Err: err}, nil
	}

	before := len(dirs)

	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}
	skippedCount := uint32(before - len(dirs))

	// Local, non-atomic counters accumulated across every entry in this
	// directory; the atomics (one per counter, not per entry) only get
	// touched once, after the loop, and only if trackStats is on at all.
	var filesCount, dirsCount, linksCount, maxDepthCount uint32

	for i := range dirs {
		entry := dirs[i]

		mode := entry.Type()
		isDir := mode.IsDir()
		isSymlink := mode&fs.ModeSymlink != 0

		// this Task never follows symlinks: visitSym is used instead when
		// followLinks is on (see NewWalkmanWithConfig)
		if isSymlink {
			linksCount++
			continue
		}

		if !isDir {
			filesCount++
			continue
		}

		dirsCount++

		if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
			maxDepthCount++
			continue
		}

		spawn(walkItem{
			path:  join(item.path, entry.Name()),
			depth: item.depth + 1,
		})
	}

	if w.conf.trackStats {
		// Zero-valued counters still cost an atomic fence for nothing, so
		// only touch the ones that actually moved this directory.
		if skippedCount != 0 {
			w.stats.skipped.Add(skippedCount)
		}
		if filesCount != 0 {
			w.stats.files.Add(filesCount)
		}
		if dirsCount != 0 {
			w.stats.dirs.Add(dirsCount)
		}
		if linksCount != 0 {
			w.stats.links.Add(linksCount)
		}
		if maxDepthCount != 0 {
			w.stats.maxDepthReached.Add(maxDepthCount)
		}
	}

	return &WalkResult{Dir: item.path, Ret: dirs, Err: nil}, nil
}

// visitSym is visit's counterpart for followLinks: same item type, same
// pool, only used when the Walkman was built with followLinks on (see
// NewWalkmanWithConfig). It additionally resolves symlinked directories and
// walks into them, guarding against cycles via item.ancestors — the chain
// of dirKeys (dev+ino) for every directory from root down to here,
// regardless of whether each hop was a plain directory or a followed
// symlink. That "regardless" matters: a symlink can point back to an
// ancestor that was reached by ordinary descent, not just one reached
// through another symlink, so the chain has to cover both or cycles through
// a plain ancestor go undetected.
func (w *Walkman) visitSym(
	_ context.Context,
	item walkItem,
	spawn func(walkItem),
) (*WalkResult, error) {
	dirs, err := readDir(item.path)
	if err != nil {
		return &WalkResult{Dir: item.path, Err: err}, nil
	}

	before := len(dirs)

	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}
	skippedCount := uint32(before - len(dirs))

	// Local, non-atomic counters accumulated across every entry in this
	// directory; the atomics (one per counter, not per entry) only get
	// touched once, after the loop, and only if trackStats is on at all.
	var filesCount, dirsCount, maxDepthCount uint32

	for i := range dirs {
		entry := dirs[i]

		childPath := join(item.path, entry.Name())
		childAncestors := item.ancestors

		mode := entry.Type()
		isDir := mode.IsDir()
		isSymlink := mode&fs.ModeSymlink != 0

		if isSymlink {
			// followLinks is on, so a symlink is classified by what it
			// resolves to, not tallied separately — this matches the
			// reference `ignore` crate's behavior (its DirEntry.file_type()
			// reports the resolved type once follow_links is set, so it
			// never reports is_symlink()==true and "links" stays 0).
			// Earlier this unconditionally bumped a linksCount *and* let a
			// resolved-to-directory symlink fall through to dirsCount++
			// below, double-counting that entry. Classifying explicitly
			// here, once, avoids that.
			resolvedSym, err := filepath.EvalSymlinks(childPath)
			if err != nil {
				// Dangling/unresolvable symlink is a fact about that one
				// entry, not a reason to fail the whole walk.
				continue
			}

			info, err := os.Lstat(resolvedSym)
			if err != nil {
				continue
			}
			if !info.IsDir() {
				// Resolves to a non-directory (file, device, etc.) — count
				// it as a file, same as `ignore` does when following links.
				filesCount++
				continue
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			k := dirKey{dev: uint64(st.Dev), ino: st.Ino}

			if item.ancestors.contains(k) {
				return nil, fmt.Errorf("walkman: symlink cycle: %s -> %s", childPath, resolvedSym)
			}

			// A symlink-to-directory entry never has fs.ModeDir set on its
			// own DirEntry (that's precisely what distinguishes it from a
			// plain directory at readdir time) — force isDir now that
			// we've resolved it, so it's treated as a directory below
			// instead of falling through to the file-count branch.
			isDir = true
			childPath = resolvedSym
			childAncestors = &ancestorNode{key: k, parent: item.ancestors}
		}

		if !isDir {
			filesCount++
			continue
		}

		dirsCount++

		if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
			maxDepthCount++
			continue
		}

		if !isSymlink {
			// Extend the chain with this plain directory's own key too, so
			// a symlink further down that points back to it is still
			// caught (see the doc comment above).
			if k, err := statKey(childPath); err == nil {
				childAncestors = &ancestorNode{key: k, parent: item.ancestors}
			}
			// If statKey fails here (e.g. raced away), we simply don't
			// extend the chain for this branch — same permissive handling
			// as a dangling symlink above, not a walk-ending error.
		}

		spawn(walkItem{
			path:      childPath,
			depth:     item.depth + 1,
			ancestors: childAncestors,
		})
	}

	if w.conf.trackStats {
		// Zero-valued counters still cost an atomic fence for nothing, so
		// only touch the ones that actually moved this directory.
		if skippedCount != 0 {
			w.stats.skipped.Add(skippedCount)
		}
		if filesCount != 0 {
			w.stats.files.Add(filesCount)
		}
		if dirsCount != 0 {
			w.stats.dirs.Add(dirsCount)
		}
		// links deliberately not touched here: with followLinks on, every
		// symlink is now folded into filesCount or dirsCount above by its
		// resolved type, matching `ignore`'s behavior of never reporting a
		// followed entry as a link. The links stat stays meaningful only
		// for the non-follow path (visit, above), where a symlink is never
		// resolved or descended into.
		if maxDepthCount != 0 {
			w.stats.maxDepthReached.Add(maxDepthCount)
		}
	}

	return &WalkResult{Dir: item.path, Ret: dirs, Err: nil}, nil
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

	item := walkItem{path: root, depth: 0}

	// Seed the ancestor chain with the root's own key so a symlink
	// anywhere in the tree that points straight back to root is still
	// caught, not just symlinks pointing at some deeper ancestor.
	if w.conf.followLinks {
		if k, err := statKey(root); err == nil {
			item.ancestors = &ancestorNode{key: k}
		}
	}

	w.pool.Submit(item)
	return w.pool.Run()
}

// Wait blocks until the walk has fully finished and returns the first
// fatal error encountered (nil on normal completion). Call it after
// draining the channel returned by Walk
func (w *Walkman) Wait() error {
	return w.pool.Wait()
}

// Stats returns a point-in-time snapshot of walk statistics. Always zero
// unless trackStats was enabled via NewWalkmanWithConfig. Safe to call
// while a walk is in progress, though the numbers will still be moving.
func (w *Walkman) Stats() (files, dirs, links, skipped, maxDepthReached uint32) {
	return w.stats.files.Load(),
		w.stats.dirs.Load(),
		w.stats.links.Load(),
		w.stats.skipped.Load(),
		w.stats.maxDepthReached.Load()
}
