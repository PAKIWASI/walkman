package walkman

import (
	"os"
	"slices"
	"unsafe"
)

const (
	stringMinimumCap = 1024
)

// stringStore implements a container that stores strings efficiently in the heap.
// It grows automatically as needed.
type stringStore struct {
	buf []byte
	off int
}

type stringID struct {
	off uint32
	len uint32
}

// newStringStore initializes and returns a new stringStore.
// cap controls the initial capacity of the underlying buffer;
// passing cap <= 0 sets the buffer capacity to stringMinimumCap (1024).
func newStringStore(cap int) stringStore {
	ps := stringStore{}
	if cap <= 0 {
		cap = stringMinimumCap
	}
	ps.buf = make([]byte, cap)
	return ps
}

func (ss *stringStore) retrieve(id stringID) string {
	return unsafe.String(unsafe.SliceData(ss.buf[id.off:id.off+id.len]), id.len)
}

// store stores the input string in its own storage and returns a stringID
// that can be used to retrieve the string.
func (ss *stringStore) store(str string) stringID {
	s := len(str)
	c := cap(ss.buf)
	id := stringID{off: uint32(ss.off), len: uint32(s)}
	if s > 0 {
		if ss.off+s > len(ss.buf) {
			if ss.off+s >= c {
				ss.buf = slices.Grow(ss.buf, 2*c+s)
			}
			ss.buf = ss.buf[:ss.off+s]
		}

		copy(ss.buf[ss.off:ss.off+s], str)
		ss.off += s
	}

	return id
}

// storePath normalizes and joins parent and child strings with the OS path separator
// and returns the resulting stringID.
func (ss *stringStore) storePath(parent, child string) stringID {
	plen := len(parent)
	clen := len(child)
	sep := 0
	if parent[plen-1] != os.PathSeparator {
		sep++
	}
	total := plen + sep + clen

	c := cap(ss.buf)
	if ss.off+total > len(ss.buf) {
		if ss.off+total >= c {
			ss.buf = slices.Grow(ss.buf, 2*c+total)
		}
		ss.buf = ss.buf[:ss.off+total]
	}

	id := stringID{off: uint32(ss.off), len: uint32(total)}

	copy(ss.buf[ss.off:ss.off+plen], parent)
	if sep == 1 {
		ss.buf[ss.off+plen] = os.PathSeparator
	}
	copy(ss.buf[ss.off+plen+sep:ss.off+total], child)
	ss.off += total

	return id
}


