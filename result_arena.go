package walkman

import "slices"

const resArenaMinCapEntries = 64
const resArenaMinCapDirErrs = 4

// each worker gets this to store the actual entries and errors
// the DirBatch result from the channel contains entry and err slices
// backed by these arrays
type resultArena struct {
	entries []Entry
	dirErrs []DirErr
}

func newResultArena(entriesCap, dirErrCap int) resultArena {
	if entriesCap <= 0 {
		entriesCap = resArenaMinCapEntries
	}
	if dirErrCap <= 0 {
		dirErrCap = resArenaMinCapDirErrs
	}

	return resultArena{
		entries: make([]Entry, entriesCap),
		dirErrs: make([]DirErr, dirErrCap),
	}
}

func (ra *resultArena) getEntryMark() (off int) {
	return len(ra.entries) - 1
}

func (ra *resultArena) getDirErrMark() (off int) {
	return len(ra.dirErrs) - 1
}

// func (pa *pathArena) store[T string | []byte](name T) (uint32, uint32) {
// }

func (ra *resultArena) storeEntry(e Entry) {
	l := len(ra.entries)
	if l >= cap(ra.entries) {
		slices.Grow(ra.entries, 2*l)
	}

	ra.entries = append(ra.entries, e)
}

func (ra *resultArena) storeDirErr(derr DirErr) {
	l := len(ra.entries)
	if l >= cap(ra.entries) {
		slices.Grow(ra.entries, 2*l)
	}

	ra.dirErrs = append(ra.dirErrs, derr)
}

// return a slice entries[off:len(entry)-1]
func (ra *resultArena) sliceEntry(off int) []Entry {
	return ra.entries[off : len(ra.entries)-1]
}

func (ra *resultArena) sliceDirErr(off int) []DirErr {
	return ra.dirErrs[off : len(ra.dirErrs)-1]
}
