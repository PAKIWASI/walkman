package walkman

import "slices"

const resArenaMinCapDirErrs = 4

// entryNodeSize is the fixed element count of each entryNode chunk.
// Tuned for Entry (parentDir, name string; ino uint64; typ uint8 ~48 bytes)
const entryNodeSize = 256

// entryNode is one fixed-capacity chunk of the entries arena.
// Once a node is no longer the tail, the arena never looks at it again; it's kept
// alive only by whatever caller-held Entry slices still point into it.
type entryNode struct {
	buf [entryNodeSize]Entry
}

// entryArena is a single-writer, append-only, chunked store for
// Entry values, one per worker.
//
// Callers reserve one directory's entire set of entries in a single
// storeEntries call, so each directory's entries land contiguously
// within one node (or, for a directory bigger than entryNodeSize, in
// a dedicated one-off allocation) instead of being
// scattered piecemeal the way per-entry appends into a growable slice
// would scatter them across repeated reallocations.
//
// Because nodes are never resized or discarded, growth here costs
// nothing beyond linking in a new node: no copy of previously stored
// entries, and no abandoned backing arrays for the GC to scan.
type entryArena struct {
	tail *entryNode
	off  int // how many of tail's entryNodeSize slots are filled
}

func newEntryArena() *entryArena {
	return &entryArena{tail: &entryNode{}}
}

// storeEntries copies items into the arena as a single contiguous
// run and returns the stored (arena-backed) slice. items is a
// per-worker scratch buffer the caller reuses across directories
// the arena always makes its own copy, so the caller is free to
// reset/reuse items immediately after this call returns.
//
// A request larger than one node's capacity gets its own dedicated
// allocation rather than being split across nodes
func (a *entryArena) storeEntries(items []Entry) []Entry {
	n := len(items)
	if n == 0 {
		return nil
	}
	if n > entryNodeSize {
		dst := make([]Entry, n)
		copy(dst, items)
		return dst
	}
	if entryNodeSize-a.off < n {
		// Not enough room left in this node: start a fresh one and
		// claim from that instead, wasting whatever's left of the
		// old node's tail. Bounded, one-time waste per boundary.
		a.tail = &entryNode{}
		a.off = 0
	}
	dst := a.tail.buf[a.off : a.off+n]
	copy(dst, items)
	a.off += n
	return dst
}

// each worker gets this to store the actual entries and errors
// the DirBatch result from the channel contains entry and err slices
// backed by these arrays
type resultArena struct {
	entries *entryArena
	dirErrs []DirErr
}

func newResultArena(dirErrCap int) resultArena {
	if dirErrCap <= 0 {
		dirErrCap = resArenaMinCapDirErrs
	}

	return resultArena{
		entries: newEntryArena(),
		dirErrs: make([]DirErr, 0, dirErrCap),
	}
}

func (ra *resultArena) getDirErrMark() (off int) {
	return len(ra.dirErrs)
}

// storeEntries reserves and copies a whole directory's worth of entries in one shot
func (ra *resultArena) storeEntries(items []Entry) []Entry {
	return ra.entries.storeEntries(items)
}

func (ra *resultArena) storeDirErr(derr DirErr) {
	if len(ra.dirErrs) >= cap(ra.dirErrs) {
		ra.dirErrs = slices.Grow(ra.dirErrs, len(ra.dirErrs))
	}
	ra.dirErrs = append(ra.dirErrs, derr)
}

func (ra *resultArena) sliceDirErr(off int) []DirErr {
	return ra.dirErrs[off:len(ra.dirErrs)]
}


