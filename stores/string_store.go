package stores

import (
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// maxSpins is the number of consecutive failed CAS attempts ensureCap
// will make before yielding to the scheduler instead of immediately retrying
const maxSpins = 50

// stringStoreNodeSize is the fixed capacity, in bytes, of each
// StringStoreNode's backing buffer.
const stringStoreNodeSize = 1016

// StringStoreNode is a single fixed-size arena chunk used by StringStore.
// Strings are packed into buf sequentially, off tracks how many bytes
// have been claimed so far and is updated atomically so multiple
// goroutines can claim disjoint ranges concurrently.
type StringStoreNode struct {
	buf [stringStoreNodeSize]byte
	off atomic.Uint64
}

// StringStore is a lock-free, append-only arena allocator for strings.
// It packs stored strings into fixed-size StringStoreNode buffers linked
// through tail, avoiding a per-string heap allocation for anything that
// fits within StringStoreNodeSize. It is safe for concurrent use.
type StringStore struct {
	tail atomic.Pointer[StringStoreNode]
}

// NewStringStore returns a StringStore ready for use, backed by a single empty node.
func NewStringStore() *StringStore {
	store := &StringStore{}
	store.tail.Store(&StringStoreNode{})
	return store
}

// ensureCap atomically claims cap contiguous bytes from the store's
// current node, growing to a new node if the current one doesn't have
// enough room left. Strings larger than StringStoreNodeSize are
// allocated on their own instead of going through the arena, since they
// could never fit in a single node.
//
// The returned slice is only ever handed to a single caller, so callers
// may write into it without further synchronization.
func (ss *StringStore) ensureCap(cap uint64) []byte {
	if cap > stringStoreNodeSize {
		return make([]byte, cap)
	}

	spins := 0
	for {
		node := ss.tail.Load()
		nodeOff := node.off.Load()
		if stringStoreNodeSize-nodeOff < cap {
			// Not enough room left in this node, swap in a fresh one and
			// claim from that instead.
			newNode := &StringStoreNode{}
			if !ss.tail.CompareAndSwap(node, newNode) {
				// someone else made a new node, try again
				spins = spinOrYield(spins)
				continue
			}
			node = newNode
			nodeOff = 0
		}
		// claim the range
		if !node.off.CompareAndSwap(nodeOff, nodeOff+cap) {
			// someone else took it, try again
			spins = spinOrYield(spins)
			continue
		}
		// we have the range, write the result
		return node.buf[nodeOff : nodeOff+cap]
	}
}

// spinOrYield counts a failed CAS attempt and, once maxSpins is hit,
// yields the current goroutine to the scheduler and resets the count.
// It returns the (possibly reset) spin count for the caller to keep track of.
func spinOrYield(spins int) int {
	spins++
	if spins >= maxSpins {
		runtime.Gosched()
		return 0
	}
	return spins
}

// StoreBytes copies buf into the arena and returns the stored copy as a
// string. The returned string aliases arena memory rather than being a
// fresh allocation, so repeated calls avoid the usual per-string allocation cost.
func (ss *StringStore) StoreBytes(buf []byte) string {
	l := len(buf)
	back := ss.ensureCap(uint64(l))
	copy(back, buf)
	return unsafe.String(unsafe.SliceData(back), l)
}

// StoreString copies str into the arena and returns the stored copy.
// It behaves like StoreBytes but takes a string directly.
func (ss *StringStore) StoreString(str string) string {
	l := len(str)
	back := ss.ensureCap(uint64(l))
	copy(back, str)
	return unsafe.String(unsafe.SliceData(back), l)
}

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

