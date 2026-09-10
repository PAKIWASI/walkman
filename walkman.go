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
	depth    uint32      // depth of this entry
	ancestor ancestorRef // zero value when followLinks is off
	// when followLinks is on: locates the ancestorEntry,
	// which has the ino, dev(from readDirRaw) pair and a link to it's parent
}

// DirErr is one error encountered while producing a WalkResult.
// Name identifies which file/dir caused the error, while
// WalkResult.Dir is the directory being listed.
type DirErr struct {
	name stringID
	err  error
}

func (derr DirErr) Name() string { return derr.name.string() }

func (derr DirErr) Err() error { return derr.err }

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
	parentDir stringID
	name      stringID
	ino       uint64
	typ       uint8 // DT_DIR, DT_REG, DT_LNK, DT_UNKNOWN, ...
}

func (e Entry) Name() string { return e.name.string() }

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
	return os.Lstat(filepath.Join(e.parentDir.string(), e.Name()))
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
type DirBatch struct {
	dir     stringID // this directory's full path
	Entries []Entry  // flat slice of entries
	Errs    []DirErr // nil if no errors in this directory
}

func (b *DirBatch) Dir() string { return b.dir.string() }

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
		InitialWorkerCap: 64,
		ResultBuffSize:   256, // TODO: is this enough? i dont want this to block. ever.
		// TODO: see what config do the bench scripts use
	}
}

type workerState struct {
	// raw buf for the getdents64 syscall. Sized to getdentsBufSize (32 KB, matching readdir_linux.go)
	buf [getdentsBufSize]byte
	// append-only storage for all directory paths this worker computes
	// Persistent for the worker's lifetime
	paths pathArena
	// per-worker ancestor chain storage for symlink-cycle detection
	// Unused, and never grown when followLinks is off
	ancestors ancestorArena

	results resultArena
	// scratch buffer for spawning child items
	spawnBuf []walkItem
}

type Walkman struct {
	conf walkConf
	pool *wsp.WorkerPool[walkItem, DirBatch]
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
			w.workers[i].ancestors = newAncestorArena(0)
		}
	}
	for i := range w.workers {
		w.workers[i].paths = newStringArena(0)
		w.workers[i].results = newResultArena(0, 0)
		w.workers[i].spawnBuf = make([]walkItem, 8)
	}

	// The pool is bound to one Task at construction. Which one we hand it
	// is the only thing that differs between plain and symlink-following
	// walks. same item type, same pool, no per-entry branch.
	execute := w.visit
	// if followLinks {
	// 	execute = w.visitSym
	// }

	w.pool = wsp.NewWorkerPool(
		context.Background(),
		pc.PoolSize,
		pc.InitialWorkerCap,
		pc.ResultBuffSize,
		execute,
	)

	return w
}


func (w *Walkman) visit(
	_ context.Context,
	workerID int,
	item walkItem,
	res chan<- DirBatch,
	spawn func(...walkItem),
) error {
	path := item.path.string()
	worker := &w.workers[workerID]
	mark := worker.results.getEntryMark()
	err := readDirRaw(path, worker.buf[:], nil, w.conf.skipSet,
		func(name []byte, dType uint8, ino uint64) error {
			worker.results.storeEntry(
				Entry{
					parentDir: item.path,
					name:      worker.paths.storeByte(name),
					ino:       ino,
					typ:       dType,
				})
			return nil
		})
	if err != nil {
		// the directory itself couldn't be read. One DirErr, not a pool-wide abort.
		res <- DirBatch{dir: item.path, Errs: []DirErr{{name: item.path, err: err}}}
		return nil
	}

	entries := worker.results.sliceEntry(mark)
	res <- DirBatch{dir: item.path, Entries: entries}

	if w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth {
		return nil
	}

	spawnBuf := worker.spawnBuf[:0]

	for i := range entries {
		if entries[i].Type().Type().IsDir() {
			// allocate the full path to the subdir and store the stringID
			spawnBuf = append(spawnBuf, walkItem{
				path:  worker.paths.joinAndStorePath(item.path, entries[i].name),
				depth: item.depth + 1,
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
// func (w *Walkman) visitSym(
// 	_ context.Context,
// 	workerID int,
// 	item walkItem,
// 	res chan<- DirBatch,
// 	spawn func(...walkItem),
// ) error {
// 	path := item.leaf.path
// 	dirs, err := readDir(path)
// 	if err != nil {
// 		res <- DirBatch{Dir: path, Errs: []DirErr{{Name: path, Err: err}}}
// 		return nil
// 	}
//
// 	before := len(dirs)
// 	if len(w.conf.skipSet) != 0 && before != 0 {
// 		dirs = filterSkipped(dirs, w.conf.skipSet)
// 	}
//
// 	result := DirBatch{Dir: path}
//
// 	worker := &w.workers[workerID]
// 	ancestorsResolved := false
//
// 	hasCycle := func(k dirKey) bool {
// 		if !ancestorsResolved {
// 			ancestorsResolved = true
// 			worker.dirBuf = worker.dirBuf[:0]
// 			for n := item.leaf; n != nil; n = n.parent {
// 				nk, err := statKey(n.path)
// 				if err != nil {
// 					continue
// 				}
// 				worker.dirBuf = append(worker.dirBuf, nk)
// 			}
// 		}
// 		return slices.Contains(worker.dirBuf, k)
// 	}
//
// 	swapDel := func(i int) []fs.DirEntry {
// 		n := len(dirs)
// 		dirs[i] = dirs[n-1]
// 		return dirs[:n-1]
// 	}
//
// 	atMaxDepth := w.conf.maxDepth != 0 && item.depth+1 > w.conf.maxDepth
// 	spawnBuf := worker.spawnBuf[:0]
//
// 	for i := 0; i < len(dirs); {
// 		entry := dirs[i]
// 		childPath := w.newPath(workerID, path, entry.Name())
//
// 		mode := entry.Type()
// 		isDir := mode.IsDir()
// 		isSymlink := mode&fs.ModeSymlink != 0
//
// 		if isSymlink {
// 			info, err := os.Stat(childPath)
// 			if err != nil {
// 				result.Errs = append(result.Errs, DirErr{Name: entry.Name(), Err: ErrDanglingSymlink})
// 				dirs = swapDel(i)
// 				continue
// 			}
// 			if !info.IsDir() {
// 				dirs[i] = fs.FileInfoToDirEntry(info)
// 				i++
// 				continue
// 			}
// 			st, ok := info.Sys().(*syscall.Stat_t)
// 			if !ok {
// 				i++
// 				continue
// 			}
// 			k := dirKey{dev: uint64(st.Dev), ino: st.Ino}
// 			if hasCycle(k) {
// 				result.Errs = append(result.Errs, DirErr{Name: entry.Name(), Err: ErrSymlinkCycle})
// 				dirs = swapDel(i)
// 				continue
// 			}
// 			dirs[i] = fs.FileInfoToDirEntry(info)
// 			isDir = true
// 		}
//
// 		if isDir && !atMaxDepth {
// 			spawnBuf = append(spawnBuf, walkItem{
// 				depth: item.depth + 1,
// 				leaf:  &pathNode{path: childPath, parent: item.leaf},
// 			})
// 		}
// 		i++
// 	}
//
// 	result.Entries = dirs
// 	res <- result
//
// 	if len(spawnBuf) != 0 {
// 		spawn(spawnBuf...)
// 	}
//
// 	return nil
// }

// Walk starts walking root and returns a channel of per-directory results.
// The channel closes once every worker has finished (no work left, or a
// fatal error occurred). Call Wait after draining the channel to get the
// terminal error, if any.
func (w *Walkman) Walk(root string) <-chan DirBatch {
	// Clean once, here, so every child path built during the walk (via
	// join, not filepath.Join) can assume its parent is already clean
	// without re-running filepath.Clean per entry.
	root = filepath.Clean(root)

	// TODO: no followlinks handling for now
	w.pool.Submit(walkItem{
		path: w.workers[0].paths.storeString(root),
		depth: 1,
	})
	return w.pool.Run()
}

// Wait blocks until the walk has fully finished and returns the first
// fatal error encountered (nil on normal completion). Call it after
// draining the channel returned by Walk
func (w *Walkman) Wait() error {
	return w.pool.Wait()
}
