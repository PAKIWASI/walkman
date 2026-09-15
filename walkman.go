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

	"github.com/PAKIWASI/walkman/stores"
	wsp "github.com/PAKIWASI/workstealpool"
)

// Sentinel errors reported via DirBatch.Errs
var (
	ErrSymlinkCycle    = errors.New("walkman: symlink cycle")
	ErrNoDevInoInfo    = errors.New("walkman: no dev/ino info available")
	ErrDanglingSymlink = errors.New("walkman: dangling/unresolved symlink")
)

// walkItem is the input to each worker's Task function.
// Each worker gets one of these, does the work on it, and then spawns
// more work (if needed) by creating another walkItem.
type walkItem struct {
	// path MUST be NUL-terminated arena memory (via StringStore's
	// StorePathZ or StoreStringZ). readDirRaw opens it via a raw
	// openat straight against this string's backing bytes
	path  string
	depth uint32 // depth of this entry
	// ancestor is the link in the walk's shared ancestor chain describing
	// this item's own directory (ino/dev of the directory, plus its own
	// parent link). nil when followLinks is off
	ancestor *ancestorEntry
}

// DirErr is one error encountered while producing a DirBatch.
// Name identifies which file/dir caused the error, while
// DirBatch.Dir is the directory being listed.
type DirErr struct {
	Name string
	Err  error
}

// Entry is the fs.DirEntry-shaped, zero-alloc equivalent for one directory entry
type Entry struct {
	parentDir string
	name      string
	ino       uint64
	typ       uint8 // DT_DIR, DT_REG, DT_LNK, DT_UNKNOWN, ...
}

func (e Entry) Name() string { return e.name }

// Ino returns the entry's inode number, as reported by getdents64
func (e Entry) Ino() uint64 { return e.ino }

// IsDir reports whether the entry is a directory, per d_type. It does NOT
// fall back to a stat call for DT_UNKNOWN, callers that need certainty should stat explicitly
func (e Entry) IsDir() bool { return e.typ == dtDir }

func (e Entry) Type() fs.FileMode { return e.FileMode() }

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

// lazy entry info
func (e Entry) Info() (fs.FileInfo, error) {
	return os.Lstat(filepath.Join(e.parentDir, e.name))
}

var _ fs.DirEntry = Entry{}

// DirBatch is one directory's result: the full path, the entries and any errors
//
// Entries and Errs are not mutually exclusive: a directory can list
// successfully (Entries populated) while individual entries inside it still
// had problems (e.g. one dangling symlink, one detected cycle), each
// recorded as its own DirErr in Errs alongside the otherwise complete Entries.
// Entries is nil only when the directory itself couldn't be read at all, in
// which case Errs holds exactly that one failure.
//
// A DirBatch is built once by one worker from one readDirRaw call, never
// re-queued and never stolen. Its Entries/Errs slices point into that worker's
// append-only arenas, which are never reused, so a batch a consumer is still
// holding is never overwritten by the next directory that worker handles -
// the slices stay valid, and unchanged, for as long as the consumer keeps them.
type DirBatch struct {
	Dir     string   // this directory's full path
	Entries []Entry  // flat slice of entries
	Errs    []DirErr // empty if this directory had no errors
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

// DefaultPoolConfig sizes PoolSize to runtime.GOMAXPROCS(0)
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		PoolSize:         runtime.GOMAXPROCS(0),
		InitialWorkerCap: 64,
		ResultBuffSize:   256, // TODO: is this enough? i dont want this to block. ever.
		// TODO: see what config do the bench scripts use
	}
}

type workerState struct {
	// raw buf for the getdents64 syscall, sized to getdentsBufSize (readdir_linux.go)
	buf [getdentsBufSize]byte

	results resultArena
	// scratch buffer for spawning child items
	spawnBuf []walkItem
	// scratch buffer for accumulating one directory's entries before
	// they're flushed into results.entries
	entryScratch []Entry
}

type Walkman struct {
	conf walkConf
	pool *wsp.WorkerPool[walkItem, DirBatch]
	// per-worker state keyed by workerID
	workers []workerState
	// append-only storage for every directory path computed during the
	// walk, shared across all workers. Lock-free/CAS-based (stores.StringStore),
	// since every worker now writes into the same instance.
	paths *stores.StringStore
	// shared ancestor-chain storage for symlink-cycle detection, one
	// instance for the whole pool. nil when followLinks is off.
	ancestors *stores.GenericStore[ancestorEntry]
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

	w.paths = stores.NewStringStore()
	if w.conf.followLinks {
		w.ancestors = newAncestorStore()
	}

	w.workers = make([]workerState, pc.PoolSize)
	for i := range w.workers {
		w.workers[i].results = newResultArena(0)
		w.workers[i].spawnBuf = make([]walkItem, 8)
		w.workers[i].entryScratch = make([]Entry, 0, entryNodeSize)
	}

	// The pool is bound to one Task at construction. Which one we hand it
	// is the only thing that differs between plain and symlink-following
	// walks. same item type, same pool, no per-entry branch.
	execute := w.visit
	if w.conf.followLinks {
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

// readEntries reads one directory into the calling worker's entry scratch and
// returns it, along with whether the listing contained a symlink at all.
//
// The returned slice is worker scratch that the next directory reuses, so the
// caller must copy it into the result arena (storeEntries does) and must not
// hold on to it past that point.
//
// sawLink is what lets followLinks mode skip symlink handling entirely for the
// overwhelming majority of directories, which contain no links at all. It
// comes off the d_type the kernel already handed us.
func (w *Walkman) readEntries(worker *workerState, item walkItem) (entries []Entry, sawLink bool, err error) {
	scratch := worker.entryScratch[:0]
	err = readDirRaw(item.path, worker.buf[:], w.conf.skipSet,
		func(name []byte, dType uint8, ino uint64) error {
			scratch = append(scratch, Entry{
				parentDir: item.path,
				name:      w.paths.StoreBytes(name),
				ino:       ino,
				typ:       dType,
			})
			sawLink = sawLink || dType == dtLnk
			return nil
		})
	worker.entryScratch = scratch // keep the grown cap for the next directory
	return scratch, sawLink, err
}

func (w *Walkman) visit(
	_ context.Context,
	workerID int,
	item walkItem,
	res chan<- DirBatch,
	spawn func(...walkItem),
) error {
	worker := &w.workers[workerID]

	// visit never follows links, so the sawLink signal is not its business.
	scratch, _, err := w.readEntries(worker, item)
	if err != nil {
		// the directory itself couldn't be read. One DirErr, not a pool-wide abort.
		res <- DirBatch{Dir: item.path, Errs: []DirErr{{Name: item.path, Err: err}}}
		return nil
	}

	// One reservation for the whole directory: lands contiguously in a
	// single arena node (or its own allocation if it's bigger than one
	// node) instead of scattering across repeated slice regrowths.
	entries := worker.results.storeEntries(scratch)
	res <- DirBatch{Dir: item.path, Entries: entries}

	if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
		return nil
	}

	spawnBuf := worker.spawnBuf[:0]

	for i := range entries {
		if entries[i].Type().Type().IsDir() {
			// allocate the full path to the subdir and store it in the arena
			spawnBuf = append(spawnBuf, walkItem{
				path:  w.paths.StorePathZ(item.path, entries[i].name),
				depth: item.depth + 1,
			})
		}
	}

	if len(spawnBuf) != 0 {
		spawn(spawnBuf...)
	}

	return nil
}

// visitSym is visit's counterpart for followLinks. Same item type, same pool,
// same batching/arena plumbing. It differs only in what it does with one
// directory's entries before publishing them:
//
//   - every symlink entry is resolved with one os.Stat. That is the only
//     syscall this mode adds, and only for directories that actually contain a
//     DT_LNK entry, so both the plain-walk path and the link-free-directory
//     path are untouched,
//   - a symlink that resolves to a directory is reported as that directory
//     (the entry keeps the link's own name, but its type becomes DT_DIR so
//     IsDir is true) and is then walked under the link's own path
//   - a symlink that resolves to anything else stays an ordinary entry,
//     reporting the target's type,
//   - a symlink that doesn't resolve at all (missing target, or a chain the
//     kernel refuses to follow: ELOOP) is reported as ErrDanglingSymlink and
//     dropped from the batch,
//   - a symlink that resolves to a directory already present in this
//     directory's ancestor chain is reported as ErrSymlinkCycle and dropped
//     as well.
//
// Cycle detection is pure (dev, ino) comparison against item.ancestor, the
// walk's shared ancestor chain (ancestors.go): O(depth) integer comparisons,
// zero syscalls.
func (w *Walkman) visitSym(
	_ context.Context,
	workerID int,
	item walkItem,
	res chan<- DirBatch,
	spawn func(...walkItem),
) error {
	worker := &w.workers[workerID]

	scratch, sawLink, err := w.readEntries(worker, item)
	if err != nil {
		// the directory itself couldn't be read. One DirErr, not a pool-wide abort.
		res <- DirBatch{Dir: item.path, Errs: []DirErr{{Name: item.path, Err: err}}}
		return nil
	}

	errsOff := worker.results.getDirErrMark()

	atMaxDepth := w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth
	spawnBuf := worker.spawnBuf[:0]

	// One pass over the listing that both resolves symlinks and builds the spawn list
	keep := 0
	for i := range scratch {
		e := scratch[i]

		if sawLink && e.typ == dtLnk {
			// The link's own path: what gets reported and, when the target
			// is a directory, what gets walked
			childPath := w.paths.StorePathZ(item.path, e.name)

			info, statErr := os.Stat(childPath)
			if statErr != nil {
				// Unresolvable. This entry leads nowhere walkable.
				worker.results.storeDirErr(DirErr{Name: e.name, Err: ErrDanglingSymlink})
				continue
			}

			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				// No dev/ino means no cycle check is possible, so the entry
				// stays unresolved and reported rather than walked unguarded.
				worker.results.storeDirErr(DirErr{Name: e.name, Err: ErrNoDevInoInfo})
				scratch[keep] = e
				keep++
				continue
			}

			if !info.IsDir() {
				// Symlink to a file/fifo/socket/device: not a traversal
				// candidate, just an entry whose type describes the target now.
				// Mutating the copy means this write-back is not optional, even when keep == i.
				e.typ = dTypeFromInfo(info)
				scratch[keep] = e
				keep++
				continue
			}

			if w.hasCycle(item.ancestor, st.Ino, uint64(st.Dev)) {
				worker.results.storeDirErr(DirErr{Name: e.name, Err: ErrSymlinkCycle})
				continue
			}

			// The target's identity, not the link's, is what a further symlink
			// pointing back into this path has to be compared against, and it
			// is also the device the target's own children belong to.
			e.typ = dtDir
			if !atMaxDepth {
				spawnBuf = append(spawnBuf, walkItem{
					path:     childPath,
					depth:    item.depth + 1,
					// dev:      uint64(st.Dev),
					ancestor: w.pushAncestor(workerID, st.Ino, uint64(st.Dev), item.ancestor),
				})
			}
			scratch[keep] = e
			keep++
			continue
		}

		if e.typ == dtDir && !atMaxDepth {
			spawnBuf = append(spawnBuf, walkItem{
				path:     w.paths.StorePathZ(item.path, e.name),
				depth:    item.depth + 1,
				// dev:      item.dev,
				ancestor: w.pushAncestor(workerID, e.ino, item.ancestor.dev, item.ancestor),
			})
		}
		if keep != i {
			// Only reachable after a drop above
			scratch[keep] = e
		}
		keep++
	}
	scratch = scratch[:keep]

	// One reservation for the whole resolved directory, and one batch carrying
	// both its entries and its DirErrs (empty when this directory had none).
	entries := worker.results.storeEntries(scratch)
	res <- DirBatch{
		Dir:     item.path,
		Entries: entries,
		Errs:    worker.results.sliceDirErr(errsOff),
	}

	if len(spawnBuf) != 0 {
		spawn(spawnBuf...)
	}

	return nil
}

// dTypeFromInfo maps a resolved os.Stat result back onto the d_type values
// Entry speaks. Anything with no d_type equivalent (fifo, socket, device
// node) becomes DT_UNKNOWN, which Entry.FileMode reports as fs.ModeIrregular:
// the only question ever asked of it is "is this a directory?", and false is
// the right answer for all of them.
func dTypeFromInfo(info fs.FileInfo) uint8 {
	switch mode := info.Mode(); {
	case mode.IsDir():
		return dtDir
	case mode&fs.ModeSymlink != 0:
		return dtLnk
	case mode.IsRegular():
		return dtReg
	default:
		return dtUnknown
	}
}

// Walk starts walking root and returns a channel of per-directory results.
// The channel closes once every worker has finished (no work left, or a
// fatal error occurred). Call Wait after draining the channel to get the
// terminal error, if any.
func (w *Walkman) Walk(root string) <-chan DirBatch {
	// Clean once, here, so every child path built during the walk (via
	// join, not filepath.Join) can assume its parent is already clean
	// without re-running filepath.Clean per entry.
	root = filepath.Clean(root)

	item := walkItem{
		path:  w.paths.StoreStringZ(root),
		depth: 1,
	}

	if w.conf.followLinks {
		// Seed the ancestor chain with the root's own identity
		if info, err := os.Stat(root); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				// item.dev = uint64(st.Dev)
				item.ancestor = w.pushAncestor(0, st.Ino, st.Dev, nil)
			}
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
