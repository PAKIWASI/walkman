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

	wsp "github.com/PAKIWASI/workstealpool"
)

// Sentinel errors reported via WalkResult.Errs
var (
	ErrSymlinkCycle    = errors.New("walkman: symlink cycle")
	ErrNoDevInoInfo    = errors.New("walkman: no dev/ino info available")
	ErrDanglingSymlink = errors.New("walkman: dangling/unresolved symlink")
)



// walkItem is the input to each worker's Task function.
// Each worker gets one of these, does the work on it, and then spawns
// more work (if needed) by creating another walkItem.
type walkItem struct {
	path     stringID
	Depth    uint16      // depth of this entry
	Ino      uint64      // this directory's own inode (free from getdents64/d_ino)
	Ancestor ancestorRef // zero value when followLinks is off
	// when followLinks is on: locates the ancestorEntry,
	// which has the ino, dev(from readDirRaw) pair and a link to it's parent
}

// WalkResult is one directory's outcome.
//
// Entries and Errs are not mutually exclusive: a directory can list
// successfully (Entries populated) while individual entries inside it still
// had problems (e.g. one dangling symlink, one detected cycle), each
// recorded as its own DirErr in Errs alongside the otherwise complete Entries.
// Entries is nil only when the directory itself couldn't be read at all, in
// which case Errs holds exactly that one failure.
type WalkResult struct {
	dir     stringID
	entries []Entry
	errs    []DirErr
}


// DirErr is one error encountered while producing a WalkResult.
// Name identifies which file/dir caused the error, while
// WalkResult.Dir is the directory being listed.
type DirErr struct {
	name stringID
	err  error
}

func (derr DirErr) Name() string {}

func (derr DirErr) Err() error {}


// Entry / DirBatch: per-batch, offset-based, not persistent
//
// A DirBatch is built once by one worker from one
// readDirRaw call and sent immediately to the external consumer,
// it is never re-queued, never stolen, and nothing about it needs to outlive that
// one send. So its Names buffer is fresh, small, and scoped to that single
// batch, built inline as entries are read from the kernel buffer, and freed
// by the GC once the caller is done with it

// Entry is the fs.DirEntry-shaped, zero-alloc equivalent for one directory entry
type Entry struct {
	name stringID
	typ  uint8 // DT_DIR, DT_REG, DT_LNK, DT_UNKNOWN, ...
	ino  uint64
}

// Ino returns the entry's inode number, as reported by getdents64
func (e Entry) Ino() uint64 { return e.ino }

// IsDir reports whether the entry is a directory, per d_type. It does NOT
// fall back to a stat call for DT_UNKNOWN, callers that need certainty
// should stat explicitly
func (e Entry) IsDir() bool { return e.typ == dtDir }

// FileMode maps d_type to the corresponding fs.FileMode bits. DT_UNKNOWN
// (and anything else this table doesn't recognize) deliberately maps to fs.ModeIrregular
func (e Entry) FileMode() fs.FileMode {
	switch e.typ {
	case dtDir:
		return fs.ModeDir
	case dtLnk:
		return fs.ModeSymlink
	case dtReg:
		return 0
	default:
		return fs.ModeIrregular
	}
}

// DirBatch is one directory's result: the full path, the entries and any errors
// Dir, Entries, and Errs are exported: the consumer directly needs all three
type DirBatch struct {
	dir     stringID  // this directory's full path
	Entries []Entry // flat slice of entries
	Errs    []DirErr // nil if no errors in this directory
}

// Name resolves e's leaf name against this batch's own names buffer. Takes
// the batch as a receiver (rather than Entry holding a back-pointer)
// specifically so Entry itself can stay a small, flat value with no pointer
// fields (§4.4, §8.1).
func (b *DirBatch) Name(e Entry) string {
	return unsafe.String(&b.names[e.nameOffset], e.nameLen)
}

// entryView adapts an Entry + its owning DirBatch to satisfy fs.DirEntry.
// Entry alone can't implement fs.DirEntry directly: fs.DirEntry's methods
// take no extra arguments, but resolving a name or lazily stat-ing requires
// the owning batch's names buffer and Dir path. entryView stays unexported —
// it's a mechanism, not part of the public type surface — and is only ever
// produced through DirBatch.DirEntry below.
type entryView struct {
	e Entry
	b *DirBatch
}

func (v entryView) Name() string      { return v.b.Name(v.e) }
func (v entryView) IsDir() bool       { return v.e.IsDir() }
func (v entryView) Type() fs.FileMode { return v.e.FileMode() }

func (v entryView) Info() (fs.FileInfo, error) {
	// Lazy stat: fs.DirEntry.Info() is the one method that can't be answered
	// from d_type/d_ino alone. Only paid by callers that actually need full
	// fs.FileInfo (size, mtime, mode bits beyond type), and only per call,
	// not per entry read. Lstat, not Stat: fs.DirEntry.Info() is documented
	// to describe a symlink itself, not follow it.
	return os.Lstat(filepath.Join(v.b.Dir, v.b.Name(v.e)))
}

var _ fs.DirEntry = entryView{}

// DirEntry answered: DirBatch.Entries — plain []Entry — is what a consumer
// gets by default. That's the zero-alloc type this whole rewrite exists to
// produce, and it's what visit2/visitSym2 build directly with no conversion
// step. entryView/fs.DirEntry is an *opt-in*, per-entry escape hatch for
// interop with code that specifically wants a real fs.DirEntry (e.g. a
// helper written against the standard library's shape), reached via:
//
//	de := batch.DirEntry(entry) // fs.DirEntry
//
// This is deliberately a one-at-a-time method, not a bulk
// "func (b *DirBatch) AsDirEntries() []fs.DirEntry" — converting the whole
// batch would box every Entry into an interface value, which is exactly the
// per-entry heap allocation §2/§4 exist to eliminate. Keeping the conversion
// per-call makes that cost visible and opt-in at each call site instead of
// silently reintroduced for every consumer, including the ones that never
// needed fs.DirEntry in the first place.
func (b *DirBatch) DirEntry(e Entry) fs.DirEntry {
	return entryView{e: e, b: b}
}


type walkConf struct {
	followLinks bool                // off by default
	maxDepth    uint32              // 0 means unlimited
	skipSet     map[string]struct{} // directories/files to skip from the result
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

type workerState struct {
	// raw buf for the getdents64 syscall. Sized to getdentsBufSize (32 KB, matching readdir_linux.go)
	buf [getdentsBufSize]byte

	// append-only storage for all directory paths this worker computes
	// Persistent for the worker's lifetime
	pathStore pathArena

	// per-worker ancestor chain storage for symlink-cycle detection
	// Unused, and never grown when followLinks is off
	ancestors ancestorArena

	// scratch buffer for spawning child items
	spawnBuf []walkItem2
}

type Walkman struct {
	conf walkConf
	pool *wsp.WorkerPool[walkItem, WalkResult]
	// per-worker state keyed by workerID
	workers []workerState
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

	w.workers = make([]workerState, pc.PoolSize)
	if w.conf.followLinks {
		for i := range w.workers {
			w.workers[i].dirBuf = make([]dirKey, 16)
		}
	}
	for i := range w.workers {
		w.workers[i].pathStore = newStringStore(0)
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
// entry whose Name() is in skip. It is an in-place swap-delete.
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

// newPath stores a path in this worker's storage and returns a string slice referencing it.
func (w *Walkman) newPath(workerID int, parent, child string) string {
	pathStore := &w.workers[workerID].pathStore
	return pathStore.retrieve(pathStore.storePath(parent, child))
}

// visit is the Task run for every directory the walk encounters. It is
// called concurrently, from any worker in the pool for different items,
// so it must not touch anything on Walkman that isn't safe for that
// (conf is read-only after construction).
func (w *Walkman) visit(
	_ context.Context,
	workerID int,
	item walkItem,
	res chan<- WalkResult,
	spawn func(...walkItem),
) error {
	path := item.leaf.path
	dirs, err := readDir(path)
	if err != nil {
		// A permission-denied (or similar) directory is a fact about that
		// one item, not a reason to kill every other worker in the pool.
		// Entries stays nil: the directory itself couldn't be read at all, so
		// there's nothing else this result can carry.
		res <- WalkResult{Dir: path, Errs: []DirErr{{Name: path, Err: err}}}
		return nil
	}

	before := len(dirs)
	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}

	// Send to channel. We don't care if it's dirs, files or symlinks;
	// this task doesn't follow symlinks.
	res <- WalkResult{Dir: path, Entries: dirs}

	// We have reached max depth, don't spawn child dirs, stop here.
	if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
		return nil
	}

	// Spawn child directories.
	spawnBuf := w.workers[workerID].spawnBuf[:0]

	for i := range dirs {
		if dirs[i].Type().IsDir() {
			spawnBuf = append(spawnBuf, walkItem{
				depth: item.depth + 1,
				leaf: &pathNode{
					path:   w.newPath(workerID, path, dirs[i].Name()),
					parent: item.leaf,
				},
			})
		}
	}
	if len(spawnBuf) != 0 {
		spawn(spawnBuf...)
	}

	return nil
}

// visitSym is visit's counterpart for followLinks: same item type, same
// pool, only used when the Walkman was built with followLinks on.
// It additionally resolves symlinked directories and walks into them,
// guarding against cycles via item.leaf.parent, the chain
// of directories from root down to here,
// regardless of whether each hop was a plain directory or a followed symlink.
func (w *Walkman) visitSym(
	_ context.Context,
	workerID int,
	item walkItem,
	res chan<- WalkResult,
	spawn func(...walkItem),
) error {
	path := item.leaf.path
	dirs, err := readDir(path)
	if err != nil {
		res <- WalkResult{Dir: path, Errs: []DirErr{{Name: path, Err: err}}}
		return nil
	}

	before := len(dirs)
	if len(w.conf.skipSet) != 0 && before != 0 {
		dirs = filterSkipped(dirs, w.conf.skipSet)
	}

	result := WalkResult{Dir: path}

	worker := &w.workers[workerID]
	ancestorsResolved := false

	hasCycle := func(k dirKey) bool {
		if !ancestorsResolved {
			ancestorsResolved = true
			worker.dirBuf = worker.dirBuf[:0]
			for n := item.leaf; n != nil; n = n.parent {
				nk, err := statKey(n.path)
				if err != nil {
					continue
				}
				worker.dirBuf = append(worker.dirBuf, nk)
			}
		}
		return slices.Contains(worker.dirBuf, k)
	}

	swapDel := func(i int) []fs.DirEntry {
		n := len(dirs)
		dirs[i] = dirs[n-1]
		return dirs[:n-1]
	}

	atMaxDepth := w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth
	spawnBuf := worker.spawnBuf[:0]

	for i := 0; i < len(dirs); {
		entry := dirs[i]
		childPath := w.newPath(workerID, path, entry.Name())

		mode := entry.Type()
		isDir := mode.IsDir()
		isSymlink := mode&fs.ModeSymlink != 0

		if isSymlink {
			info, err := os.Stat(childPath)
			if err != nil {
				result.Errs = append(result.Errs, DirErr{Name: entry.Name(), Err: ErrDanglingSymlink})
				dirs = swapDel(i)
				continue
			}
			if !info.IsDir() {
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
			if hasCycle(k) {
				result.Errs = append(result.Errs, DirErr{Name: entry.Name(), Err: ErrSymlinkCycle})
				dirs = swapDel(i)
				continue
			}
			dirs[i] = fs.FileInfoToDirEntry(info)
			isDir = true
		}

		if isDir && !atMaxDepth {
			spawnBuf = append(spawnBuf, walkItem{
				depth: item.depth + 1,
				leaf:  &pathNode{path: childPath, parent: item.leaf},
			})
		}
		i++
	}

	result.Entries = dirs
	res <- result

	if len(spawnBuf) != 0 {
		spawn(spawnBuf...)
	}

	return nil
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
	w.pool.Submit(walkItem{depth: 1, leaf: &pathNode{path: root}})
	return w.pool.Run()
}

// Wait blocks until the walk has fully finished and returns the first
// fatal error encountered (nil on normal completion). Call it after
// draining the channel returned by Walk
func (w *Walkman) Wait() error {
	return w.pool.Wait()
}
