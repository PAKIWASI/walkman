package walkman

import (
	"os"
	"slices"
	"unsafe"
)

const (
	minimumCap = 1024
)

// StringStore implements a container that stores string efficiently in the heap
// It grows automatically
type StringStore struct {
	buf []byte
	off int
}

type StringID struct {
	store *StringStore
	off   int
	len   int
}

// read-only view of the byte buffer. Strings are immutable so it's all good
func (s *StringID) String() string {
	return unsafe.String(unsafe.SliceData(s.store.buf[s.off:s.off+s.len]), s.len)
}

// NewPathStorage initilises and returns a new PathStorage object.
// cap controls the inital capacity of the undrelying buffer,
// passing cap <= 0 sets the buffer capacity to minimumCap (1024)
func NewPathStorage(cap int) StringStore {
	ps := StringStore{}
	if cap <= 0 {
		cap = minimumCap
	}
	ps.buf = make([]byte, cap)
	return ps
}

// Store stores the input string in it's own storage and returns an obj
// that can be used to retrieve the string
func (p *StringStore) Store(str string) StringID {
	s := len(str)
	c := cap(p.buf)
	id := StringID{store: p, off: p.off, len: s}
	if s > 0 {
		if p.off+s >= c {
			p.buf = slices.Grow(p.buf, 2*c+s)
		}

		copy(p.buf[p.off:p.off+s], str)
		p.off += s
	}

	return id
}

// StorePath normalises and joins parent and child strings as "parent/child"
// and returns the resulting string id
func (p *StringStore) StorePath(parent, child string) StringID {
	plen := len(parent)
	clen := len(child)
	sep := 0
	if parent[plen-1] != os.PathSeparator {
		sep++
	}
	total := plen + sep + clen

	c := cap(p.buf)
	if p.off+total >= c {
		p.buf = slices.Grow(p.buf, 2*c+total)
	}

	id := StringID{store: p, off: p.off, len: total}

	copy(p.buf[p.off:p.off+plen], parent)
	if sep == 1 {
		p.buf[p.off+plen] = os.PathSeparator
	}
	copy(p.buf[p.off+plen+sep:p.off+total], child)
	p.off += total

	return id
}
