package walkman

import (
	"os"
	"slices"
	"unsafe"
)

const (
	stringMinimumCap = 1024
	pathMinimuCap = 16
)

// stringStore implements a container that stores string efficiently in the heap
// It grows automatically
type stringStore struct {
	buf []byte
	off int
}

type stringID struct {
	off uint32
	len uint32
}

// NewPathStorage initilises and returns a new PathStorage object.
// cap controls the inital capacity of the undrelying buffer,
// passing cap <= 0 sets the buffer capacity to minimumCap (1024)
func NewPathStorage(cap int) stringStore {
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

// Store stores the input string in it's own storage and returns an obj
// that can be used to retrieve the string
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

// StorePath normalises and joins parent and child strings as "parent/child"
// and returns the resulting string id
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

type pathNodeStore struct {
	// all pathNode objects
	buf []pathNodeID
}

type pathNodeID struct {
	path string
	store *pathNode
}

func newPathNodeStore(cap int) pathNodeStore {
	ps := pathNodeStore{}
	if cap <= 0 {
		cap = pathMinimuCap
	}
	ps.buf = make([]pathNodeID, cap)
	return ps
}

func (ps *pathNodeStore) store(path string, parentIdx uint32) pathNodeID {
	pn := pathNodeID{
		path: path,
		idx: uint32(len(ps.buf)),
		parentIdx: parentIdx,
	}
	ps.buf = append(ps.buf, pn)
	return pn
}

func (ps *pathNodeStore) retrieve(idx uint32) pathNodeID {
	return ps.buf[int(idx)]
}


