package walkman

import "io/fs"

type walkItem2 struct {
	PathOff   uint32 // Offset into worker's path arena
	PathLen   uint16 // Length of the path
	Depth     uint16 // Current depth
	Ino       uint64 // Directory inode (for cycle detection)
	ParentIno uint64 // Immediate parent inode
}

type WalkResult2 struct {
	Dir     string
	Entries []fs.DirEntry
	Errs    []DirErr
}

type workerState2 struct {
	// raw buf for the syscall
	buf [256]byte
	//

	// storage for all paths this worker computed
	pathStore stringStore
	// scratch buffer for spawning child items
	spawnBuf []walkItem
}

// the fs.DirEntry equivilant
// TODO: should i strive for dir entry compatibility?
type Entry struct {
	path     string // stored in the string store
	fileType uint8  // DT_DIR, DT_REG, DT_LNK
	ino      uint64
}
// TODO: can we optimise paths somehow? storing the who path for each file is kinda
// redundent but a centralised storage, a tree etc would require syncronisation

func (e Entry) Name() string               { } // TODO: return only the leaf }
func (e Entry) IsDir() bool                { }
func (e Entry) Type() fs.FileMode          { }
func (e Entry) Info() (fs.FileInfo, error) { /* lazy stat */ }

type DirBatch struct { // the walkResult
	Dir     string  // stored in the string store
	Entries []Entry // Flat slice of entries
}



