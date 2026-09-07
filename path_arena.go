package walkman

import (
	"os"
	"slices"
	"unsafe"
)

const (
	pathArenaMinimumCap = 1024
)

// pathArena implements a container that stores strings efficiently in the heap.
// It grows automatically as needed.
type pathArena struct {
	buf []byte
	off int
}

// global id for a string with info about which
// worker's pathArena the string belongs to
type stringID struct {
	store   *pathArena
	PathLen uint32 // length of the path
	PathOff uint32 // offset into that worker's path arena
}

func (id stringID) string() string {
	return id.store.retrieve(id.PathOff, id.PathLen)
}

func byteToString(buf []byte) string {
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

// newStringArena initializes and returns a new stringStore.
// cap controls the initial capacity of the underlying buffer;
// passing cap <= 0 sets the buffer capacity to stringMinimumCap (1024).
func newStringArena(cap int) pathArena {
	ps := pathArena{}
	if cap <= 0 {
		cap = pathArenaMinimumCap
	}
	ps.buf = make([]byte, cap)
	return ps
}

func (pa *pathArena) retrieve(off, len uint32) string {
	return unsafe.String(unsafe.SliceData(pa.buf[off:off+len]), len)
}

// store stores the input string in its own storage and returns a stringID
// that can be used to retrieve the string.
func (pa *pathArena) store(name string) (uint32, uint32) {
	s := len(name)
	c := cap(pa.buf)
	retOff := uint32(pa.off)
	if s > 0 {
		if pa.off+s > len(pa.buf) {
			if pa.off+s >= c {
				pa.buf = slices.Grow(pa.buf, 2*c+s)
			}
			pa.buf = pa.buf[:pa.off+s]
		}

		copy(pa.buf[pa.off:pa.off+s], name)
		pa.off += s
	}

	return retOff, uint32(s)
}

// TODO: methods can have generics in gov1.27, idk wht's wrong
func (pa *pathArena) storeByte(name []byte) (uint32, uint32) {
	s := len(name)
	c := cap(pa.buf)
	retOff := uint32(pa.off)
	if s > 0 {
		if pa.off+s > len(pa.buf) {
			if pa.off+s >= c {
				pa.buf = slices.Grow(pa.buf, 2*c+s)
			}
			pa.buf = pa.buf[:pa.off+s]
		}
		copy(pa.buf[pa.off:pa.off+s], name)
		pa.off += s
	}

	return retOff, uint32(s)
}



// storePath normalizes and joins parent and child strings with the OS path separator
// and returns the resulting stringID.
func (pa *pathArena) storePath(parent, child string) (uint32, uint32) {
	plen := len(parent)
	clen := len(child)
	sep := 0
	if parent[plen-1] != os.PathSeparator {
		sep++
	}
	total := plen + sep + clen

	c := cap(pa.buf)
	if pa.off+total > len(pa.buf) {
		if pa.off+total >= c {
			pa.buf = slices.Grow(pa.buf, 2*c+total)
		}
		pa.buf = pa.buf[:pa.off+total]
	}

	retOff := uint32(pa.off)

	copy(pa.buf[pa.off:pa.off+plen], parent)
	if sep == 1 {
		pa.buf[pa.off+plen] = os.PathSeparator
	}
	copy(pa.buf[pa.off+plen+sep:pa.off+total], child)
	pa.off += total

	return retOff, uint32(total)
}
