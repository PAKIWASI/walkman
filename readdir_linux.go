package walkman

import (
	"bytes"
	"syscall"
	"unsafe"
)

const (
	getdentsBufSize  = 128 * 1024 // 128 KB per worker
	sysGetdents64    = syscall.SYS_GETDENTS64
	direntNameOffset = 19 // uint64(8) + int64(8) + uint16(2) + uint8(1) = 19, no padding on this ABI

	// d_type values, per Linux's linux_dirent64 ABI
	dtUnknown uint8 = 0
	dtDir     uint8 = 4
	dtReg     uint8 = 8
	dtLnk     uint8 = 10

	sysOpenat = syscall.SYS_OPENAT
	// AT_FDCWD, per the Linux ABI
	atFDCWD = -0x64
)

// openDirZ opens pathZ via a raw openat syscall, the same way
// readDirRaw already reads entries via a raw SYS_GETDENTS64 instead of the stdlib wrapper.
//
// pathZ MUST be an arena-backed, NUL-terminated string, i.e. built
// via stores.StringStore's StorePathZ or StoreStringZ, never a plain
// Go string. syscall.Open's path parameter looks like an ordinary Go
// string, but on every single call it internally runs
// BytePtrFromString(path), which allocates a fresh []byte and copies
// the path into it just to produce a C-string pointer. Our paths are
// already NUL-terminated in arena memory by construction
func openDirZ(pathZ string, flags int) (int, error) {
	p := unsafe.Pointer(unsafe.StringData(pathZ))
	// the constant itself can't legally convert straight to uintptr, but a variable holding the same value can,
	// with the two's-complement wraparound the syscall ABI relies on.
	dirfd := atFDCWD
	r0, _, errno := syscall.Syscall6(
		uintptr(sysOpenat),
		uintptr(dirfd),
		uintptr(p),
		// O_LARGEFILE is 0 on amd64/arm64 but nonzero on some 32-bit
		// targets; syscall.Open ORs it in for exactly this reason, so
		// we match that here to keep behavior identical across arches.
		uintptr(flags|syscall.O_LARGEFILE),
		0, // mode: unused, we never pass O_CREAT
		0, 0,
	)
	if errno != 0 {
		return -1, errno
	}
	return int(r0), nil
}

type linuxDirent64 struct {
	Ino    uint64  // inode number
	Off    int64   // offset to next entry in the buf
	Reclen uint16  // total size of this record
	Type   uint8   // file type hint
	Name   [1]byte // NULL terminated name, variable length
	// (we get a variable length c array and Name slice will point to it)
}

// byteToString is a zero-copy view of buf as a string, used only for the
// transient skip-set lookup below. It doesn't outlive buf, and must never
// be retained; anything that needs to persist goes through the string
// store instead, which makes its own copy.
func byteToString(buf []byte) string {
	return unsafe.String(unsafe.SliceData(buf), len(buf))
}

// readDirRaw reads directory entries directly via SYS_GETDENTS64 into worker's scratch buffer.
func readDirRaw(
	dirPath string, // the directory to read; MUST be NUL-terminated (see openDirZ)
	buf []byte, // per worker scratch buf
	skip map[string]struct{},
	onEntry func(name []byte, dType uint8, ino uint64) error,
) error {
	// Open directory with O_DIRECTORY and O_CLOEXEC
	fd, err := openDirZ(dirPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	for {
		// this syscall places directory entries into the passed buf, as many that can fit
		// when we call it again(for{}), we get the next batch. n means how many bytes are written
		// to the buf. if n==0 then we go all directories, that's the EOF
		n, _, errno := syscall.Syscall(
			sysGetdents64,                    // syscall
			uintptr(fd),                      // file discriptro
			uintptr(unsafe.Pointer(&buf[0])), // pointer to our buf
			uintptr(len(buf)),                // buf size
		)
		if errno != 0 {
			return errno
		}
		if n == 0 {
			break // EOF
		}

		// Parse this batch of linux_dirent64 records from temp buffer into persistant storage

		// position into the temp buffer (we increment it with each record's total size at the end)
		var pos uintptr
		for pos < n {
			dent := (*linuxDirent64)(unsafe.Pointer(&buf[pos])) // I thought I escaped C
			reclen := uintptr(dent.Reclen)                      // total size of this linux_dirent64
			if reclen == 0 {
				break
			}

			pos += reclen // increment to the next record unconditionally

			// find the position of the name string within the record
			// it should be at offset 19 (no padding)
			namePtr := unsafe.Add(unsafe.Pointer(dent), direntNameOffset)
			maxLen := int(reclen) - direntNameOffset

			nameBytes := unsafe.Slice((*byte)(namePtr), maxLen) // make a slice header that point to those maxLen bytes
			nameLen := bytes.IndexByte(nameBytes, 0)            // NUL terminator
			if nameLen < 0 {
				nameLen = maxLen // defensive: ABI guarantees NUL-termination within reclen
			}
			name := nameBytes[:nameLen] // make a slice that points to the actual string (without NULL)

			// Filter "." and ".." entries in-place
			if (len(name) == 1 && name[0] == '.') ||
				(len(name) == 2 && name[0] == '.' && name[1] == '.') {
				continue
			}
			// filter what the user skipped explicitly
			if _, ok := skip[byteToString(name)]; ok {
				continue
			}

			// save the info we need somewhere (the file name, the type hint and the inode)
			if err := onEntry(name, dent.Type, dent.Ino); err != nil {
				return err
			}

		}
	}
	return nil
}
