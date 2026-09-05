package walkman

// noParent marks the root of a chain
const noParent = ^uint16(0)

// ancestorRef: two plain integers
// Resolved by indexing into workers[ownerID].ancestors
type ancestorRef struct {
	ownerID uint16 // which worker's ancestorArena this points into
	idx     uint32 // index into that arena
}

// ancestorEntry is one link in an ancestor chain:
// a directory's dev+ino plus a reference to its own parent link.
type ancestorEntry struct {
	ino, dev uint64
	parent   ancestorRef
}

// ancestorArena: per-worker, append-only chain storage for symlink-cycle
// detection under work stealing amortized append, indices stable across growth
type ancestorArena struct {
	entries []ancestorEntry
}

// pushAncestor links a new ancestor node onto workerID's own arena and
// returns a ref to it. Called once per directory spawn in followLinks mode
// no heap allocation beyond the arena's own amortized growth.
// The caller passes the spawning directory's own dev/ino
// and the parent's ref (which may point into a different worker's arena if
// that ancestor was itself produced by a different worker). Nothing about
// an existing chain is ever touched, only a new leaf is added.
func (w *Walkman2) pushAncestor(workerID int, ino, dev uint64, parent ancestorRef) ancestorRef {
	arena := &w.workers[workerID].ancestors
	idx := uint32(len(arena.entries))
	arena.entries = append(arena.entries, ancestorEntry{ino: ino, dev: dev, parent: parent})
	return ancestorRef{ownerID: uint16(workerID), idx: idx}
}

// hasCycle walks the ancestor chain starting at ref, comparing (dev, ino)
// pairs. O(depth) integer comparisons, zero syscalls, only ever called when
// followLinks is on and the current entry is actually a symlink pointing at a directory
func (w *Walkman2) hasCycle(ref ancestorRef, targetIno, targetDev uint64) bool {
	for ref.ownerID != noParent {
		e := w.workers[ref.ownerID].ancestors.entries[ref.idx]
		if e.ino == targetIno && e.dev == targetDev {
			return true
		}
		ref = e.parent
	}
	return false
}
