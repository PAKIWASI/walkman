// Package stores gives..
package stores

import (
	"os"
	"unsafe"
)

// defaultStringNodeSize is the number of bytes each stringNode holds.
// Tune to your typical path/name length distribution.
const defaultStringNodeSize = 1024

// stringNode is one fixed-capacity chunk of a StringStore's arena.
type stringNode struct {
	buf [defaultStringNodeSize]byte
}

// StringStore is a single-writer, append-only arena allocator for strings.
// It packs stored strings into fixed-size byte buffers, avoiding a
// per-string heap allocation for anything that fits within one node.
//
// Unlike GenericStore, this is NOT safe for concurrent use
// It's meant to be owned by exactly one worker for the lifetime of a walk
type StringStore struct {
	tail *stringNode
	off  int // how many of tail's defaultStringNodeSize bytes are filled
}

// NewStringStore returns a StringStore ready for use, backed by a single empty node.
func NewStringStore() *StringStore {
	return &StringStore{tail: &stringNode{}}
}

// ensureCap claims n contiguous bytes from the current node, starting a
// fresh node if there isn't enough room left. A request larger than one
// node's capacity gets its own dedicated allocation rather than being
// split across nodes.
func (ss *StringStore) ensureCap(n uint64) []byte {
	if n > defaultStringNodeSize {
		return make([]byte, n)
	}
	if uint64(defaultStringNodeSize-ss.off) < n {
		// Not enough room left in this node: start a fresh one, wasting
		// whatever's left of the old node's tail. Bounded, one-time
		// waste per boundary - same trade-off entryArena makes.
		ss.tail = &stringNode{}
		ss.off = 0
	}
	back := ss.tail.buf[ss.off : ss.off+int(n)]
	ss.off += int(n)
	return back
}

// StoreBytes copies buf into the arena and returns the stored copy as a
// string. The returned string aliases arena memory rather than being a
// fresh allocation, so repeated calls avoid the usual per-string allocation cost.
func (ss *StringStore) StoreBytes(buf []byte) string {
	back := ss.ensureCap(uint64(len(buf)))
	copy(back, buf)
	return unsafe.String(unsafe.SliceData(back), len(buf))
}

// StoreString copies str into the arena and returns the stored copy.
// It behaves like StoreBytes but takes a string directly.
func (ss *StringStore) StoreString(str string) string {
	back := ss.ensureCap(uint64(len(str)))
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
	back := ss.ensureCap(uint64(len(str)) + 1) // +1 reserved NUL terminator
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

	back := ss.ensureCap(uint64(total))

	copy(back[:plen], parent)
	if sep == 1 {
		back[plen] = os.PathSeparator
	}
	copy(back[plen+sep:], child)

	return unsafe.String(unsafe.SliceData(back), total)
}

// StorePathZ , same reasoning as StoreStringZ
func (ss *StringStore) StorePathZ(parent, child string) string {
	plen := len(parent)
	sep := 0
	if parent[plen-1] != os.PathSeparator {
		sep++
	}
	total := plen + sep + len(child)

	back := ss.ensureCap(uint64(total) + 1) // +1 reserved NUL terminator

	copy(back[:plen], parent)
	if sep == 1 {
		back[plen] = os.PathSeparator
	}
	copy(back[plen+sep:total], child)
	// back[total] is left as zero by the arena's fresh node memory.

	return unsafe.String(unsafe.SliceData(back), total)
}

// NewStringZ returns a NUL-terminated string backed by its own dedicated
// allocation rather than an arena. For one-off callers that don't hold a
// per-worker StringStore of their own - e.g. Walk's root path, built once
// on the caller's goroutine before any worker exists to own it.
func NewStringZ(s string) string {
	buf := make([]byte, len(s)+1)
	copy(buf, s)
	// buf[len(s)] is left as zero by make.
	return unsafe.String(unsafe.SliceData(buf), len(s))
}
