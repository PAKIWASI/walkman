package stores

import (
	"sync/atomic"
)

// defaultGenericNodeSize is the number of elements each GenericNode holds.
const defaultGenericNodeSize = 1016

// GenericNode is a single fixed-capacity arena chunk. off tracks how many
// elements have been claimed so far (not necessarily written yet) and
// is updated atomically so multiple goroutines can claim disjoint
// ranges concurrently.
type GenericNode[T any] struct {
	buf [defaultGenericNodeSize]T
	off atomic.Uint64
}

// GenericStore is a lock-free, append-only arena allocator generic over T.
// It packs appended values into fixed-size node buffers linked through
// tail, avoiding a per-value heap allocation for anything that fits
// within a node. It is safe for concurrent use.
//
// T should be a basic type (int, float64, bool, ...) or a simple
// struct: a plain data holder with no embedded synchronization
// primitives (sync.Mutex, channels, ...) and, ideally, no pointers
// back into the store itself. Struct values are copied with normal Go
// assignment semantics, so slice/map/pointer fields are shallow-copied
type GenericStore[T any] struct {
	tail atomic.Pointer[GenericNode[T]]
}

// NewStore returns a Store whose nodes each hold defaultGenericNodeSize
// elements of T.
func NewStore[T any]() *GenericStore[T] {
	s := &GenericStore[T]{}
	s.tail.Store(&GenericNode[T]{})
	return s
}

// ensureCap atomically claims count contiguous slots from the store's
// current node, growing to a new node if the current one doesn't have
// enough room left. A request larger than the store's node size is
// allocated on its own instead of going through the arena
func (s *GenericStore[T]) ensureCap(cap uint64) []T {
	if cap > defaultGenericNodeSize {
		return make([]T, cap)
	}

	spins := 0
	for {
		n := s.tail.Load()
		nOff := n.off.Load()
		if defaultGenericNodeSize-nOff < cap {
			// Not enough room left in this node, swap in a fresh one and
			// claim from that instead.
			newNode := &GenericNode[T]{}
			if !s.tail.CompareAndSwap(n, newNode) {
				// someone else made a new node, try again
				spins = spinOrYield(spins)
				continue
			}
			n = newNode
			nOff = 0
		}
		// claim the range
		if !n.off.CompareAndSwap(nOff, nOff+cap) {
			// someone else took it, try again
			spins = spinOrYield(spins)
			continue
		}
		// we have the range, write the result
		return n.buf[nOff : nOff+cap]
	}
}

// Append stores a single value in the arena and returns a pointer to
// its slot. The pointer stays valid for the lifetime of the Store.
func (s *GenericStore[T]) Append(v T) *T {
	slot := s.ensureCap(1)
	slot[0] = v
	return &slot[0]
}

// AppendSlice copies items into the arena as a contiguous run and
// returns the stored copy. The returned slice aliases arena memory
// rather than being copied again, so it's cheap even for large batches.
func (s *GenericStore[T]) AppendSlice(items []T) []T {
	slot := s.ensureCap(uint64(len(items)))
	copy(slot, items)
	return slot
}
