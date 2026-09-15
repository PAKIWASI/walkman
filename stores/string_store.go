package stores

import (
	"os"
	"unsafe"
)

// StringStore is a lock-free, append-only arena allocator for strings.
// It packs stored strings into fixed-size byte buffers, avoiding a
// per-string heap allocation for anything that fits within one node.
// It is safe for concurrent use.
type StringStore struct {
	store GenericStore[byte]
}

// NewStringStore returns a StringStore ready for use, backed by a single empty node.
func NewStringStore() *StringStore {
	ss := &StringStore{}
	ss.store.tail.Store(&GenericNode[byte]{})
	return ss
}

// StoreBytes copies buf into the arena and returns the stored copy as a
// string. The returned string aliases arena memory rather than being a
// fresh allocation, so repeated calls avoid the usual per-string allocation cost.
func (ss *StringStore) StoreBytes(buf []byte) string {
	back := ss.store.ensureCap(uint64(len(buf)))
	copy(back, buf)
	return unsafe.String(unsafe.SliceData(back), len(buf))
}

// StoreString copies str into the arena and returns the stored copy.
// It behaves like StoreBytes but takes a string directly.
func (ss *StringStore) StoreString(str string) string {
	back := ss.store.ensureCap(uint64(len(str)))
	copy(back, str)
	return unsafe.String(unsafe.SliceData(back), len(str))
}

// StorePath joins parent and child with an OS path separator (unless
// parent already ends with one) directly into the arena, returning the
// joined result as a single stored string.
func (ss *StringStore) StorePath(parent, child string) string {
	plen := len(parent)
	sep := 0
	if parent[plen-1] != os.PathSeparator {
		sep++
	}
	total := plen + sep + len(child)

	back := ss.store.ensureCap(uint64(total))

	copy(back[:plen], parent)
	if sep == 1 {
		back[plen] = os.PathSeparator
	}
	copy(back[plen+sep:], child)

	return unsafe.String(unsafe.SliceData(back), total)
}


// StorePathZ behaves exactly like StorePath, except it reserves one
// extra trailing byte in the arena that is never included in the
// returned string's length. Fresh arena node memory starts zeroed, and
// that reserved byte is never handed out to any other Store call (the
// node's claim offset moves past it), so it stays zero for the
// string's whole lifetime, giving callers that need a NUL-terminated
// C-string view (e.g. passing straight to a raw openat syscall) a
// pointer they can reuse via unsafe.Pointer(unsafe.StringData(s))
// with zero extra allocation, instead of paying for
// syscall.BytePtrFromString's copy on every call.
func (ss *StringStore) StorePathZ(parent, child string) string {
	plen := len(parent)
	sep := 0
	if parent[plen-1] != os.PathSeparator {
		sep++
	}
	total := plen + sep + len(child)
 
	back := ss.store.ensureCap(uint64(total) + 1) // +1 reserved NUL terminator
 
	copy(back[:plen], parent)
	if sep == 1 {
		back[plen] = os.PathSeparator
	}
	copy(back[plen+sep:total], child)
	// back[total] is left as zero by the arena's fresh node memory.
 
	return unsafe.String(unsafe.SliceData(back), total)
}
