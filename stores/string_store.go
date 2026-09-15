package stores

import (
	"os"
	"unsafe"
)

// TODO: make the string store per worker?


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

// StoreStringZ behaves exactly like StoreString, except
// it reserves one extra trailing byte in the arena that
// is never included in the returned string's length, giving callers a
// NUL-terminated C-string view they can pass straight to a raw
// syscall via unsafe.Pointer(unsafe.StringData(s)) with zero extra
// allocation. Using this for any path that will be opened via a raw openat rather than syscall.Open
func (ss *StringStore) StoreStringZ(str string) string {
	back := ss.store.ensureCap(uint64(len(str)) + 1) // +1 reserved NUL terminator
	copy(back, str)
	// back[len(str)] is left as zero by the arena's fresh node memory.
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

// same reasoning as StoreStringZ
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
