package walkman

import "github.com/PAKIWASI/walkman/stores"

// ancestorEntry is one link in an ancestor chain: a directory's dev+ino
// plus a pointer to its own parent link. A nil parent marks the root of
// the chain (the walk's starting directory).
//
// Entries live in a per-worker stores.GenericStore[ancestorEntry], which
// is lock-free and safe for concurrent append/read, so a chain can be
// built by one worker and walked by another after work stealing without
// any extra synchronization here. GenericStore.Append hands back a
// pointer that's stable for the store's lifetime, so that pointer alone
// is enough to name a link — no separate (ownerID, idx) indirection is
// needed the way the old bespoke arena required.
type ancestorEntry struct {
	ino, dev uint64
	parent   *ancestorEntry
}

// pushAncestor links a new ancestor node onto workerID's own arena and
// returns a pointer to it. Called once per directory spawn in followLinks
// mode; no heap allocation beyond the arena's own amortized node growth.
// The caller passes the spawning directory's own dev/ino and the parent
// link (which may point into a different worker's arena if that ancestor
// was itself produced by a different worker, since the pointer is valid
// regardless of which store it came from). Nothing about an existing
// chain is ever touched, only a new leaf is added.
func (w *Walkman) pushAncestor(workerID int, ino, dev uint64, parent *ancestorEntry) *ancestorEntry {
	return w.workers[workerID].ancestors.Append(ancestorEntry{ino: ino, dev: dev, parent: parent})
}

// hasCycle walks the ancestor chain starting at ref, comparing (dev, ino)
// pairs. O(depth) integer comparisons, zero syscalls, only ever called
// when followLinks is on and the current entry is actually a symlink
// pointing at a directory.
func (w *Walkman) hasCycle(ref *ancestorEntry, targetIno, targetDev uint64) bool {
	for ref != nil {
		if ref.ino == targetIno && ref.dev == targetDev {
			return true
		}
		ref = ref.parent
	}
	return false
}

// newAncestorStore returns a GenericStore ready to hold one worker's
// ancestor chain entries.
func newAncestorStore() *stores.GenericStore[ancestorEntry] {
	return stores.NewStore[ancestorEntry]()
}
