package walkman

import (
	"syscall"
	"unsafe"
)

const (
	getdentsBufSize  = 32 * 1024 // 32 KB per worker
	sysGetdents64    = syscall.SYS_GETDENTS64
	direntNameOffset = 19 // uint64(8) + int64(8) + uint16(2) + uint8(1) = 19, no padding on this ABI
)

type linuxDirent64 struct {
	Ino    uint64  // inode number
	Off    int64   // offset to next entry in the buf
	Reclen uint16  // total size of this record
	Type   uint8   // file type hint
	Name   [1]byte // NULL terminated name, variable length
	// (we get a variable length c array and Name slice will point to it)
}

// readDirRaw reads directory entries directly via SYS_GETDENTS64 into worker's scratch buffer
func readDirRaw(
	dirPath string, // the directory to read
	buf []byte, // per worker scratch buf
	onEntry func(name []byte, dType uint8, ino uint64) error, // storage func (closes on persistant container?)
) error {
	// Open directory with O_DIRECTORY and O_CLOEXEC
	fd, err := syscall.Open(dirPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
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

			// find the position of the name string within the record
			// it should be at offset 19 (no padding)
			namePtr := unsafe.Add(unsafe.Pointer(dent), direntNameOffset)
			maxLen := int(reclen) - direntNameOffset

			nameBytes := unsafe.Slice((*byte)(namePtr), maxLen) // make a slice header that point to those maxLen bytes
			nameLen := 0
			for nameLen < maxLen && nameBytes[nameLen] != 0 { // find the len of the string by looking for the NULL terminator
				nameLen++
			}
			name := nameBytes[:nameLen] // make a slice that points to the actual string (without NULL)

			// Filter "." and ".." entries in-place
			if !(len(name) == 1 && name[0] == '.') &&
				!(len(name) == 2 && name[0] == '.' && name[1] == '.') {
				// save the info we need somewhere (the file name, the type hint and the inode)
				if err := onEntry(name, dent.Type, dent.Ino); err != nil {
					return err
				}
			}

			pos += reclen // increment to the next record
		}
	}
	return nil
}
