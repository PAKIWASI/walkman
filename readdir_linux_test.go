package walkman

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PAKIWASI/walkman/stores"
)

// readDirRaw's dirPath must be arena-backed and NUL-terminated: openDirZ hands
// its backing bytes straight to openat (see readdir_linux.go). A plain Go
// string from t.TempDir() would make the kernel read past the end of it looking
// for a terminator, so every path in this file goes through the same
// StringStore API the walker itself uses.
//
// NOTE: readDirRaw also indexes buf[0] unconditionally, so an empty scratch
// buffer panics instead of returning an error. Production always passes
// worker.buf[:] (getdentsBufSize bytes); these tests always pass a real buffer.
func pathZ(dir string) string {
	return stores.NewStringStore().StoreStringZ(dir)
}

type rawEntry struct {
	dType uint8
	ino   uint64
}

// listRaw reads dir through readDirRaw with the given scratch buffer and skip
// set, and returns what onEntry saw, keyed by name.
func listRaw(t *testing.T, dir string, buf []byte, skip map[string]struct{}) map[string]rawEntry {
	t.Helper()

	got := make(map[string]rawEntry)
	err := readDirRaw(pathZ(dir), buf, skip, func(name []byte, dType uint8, ino uint64) error {
		got[string(name)] = rawEntry{dType: dType, ino: ino}
		return nil
	})
	if err != nil {
		t.Fatalf("readDirRaw(%q) = %v, want nil", dir, err)
	}
	return got
}

func rawNames(got map[string]rawEntry) []string {
	out := make([]string, 0, len(got))
	for n := range got {
		out = append(out, n)
	}
	return out
}

// statIno reads path's inode number straight from the kernel, for
// cross-checking what getdents64 reported.
func statIno(t *testing.T, path string) uint64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Stat(%q).Sys() = %T, want *syscall.Stat_t", path, info.Sys())
	}
	return st.Ino
}

func noopEntry([]byte, uint8, uint64) error { return nil }

// TestReadDirRaw_BasicListing checks the raw reader against a tree whose
// contents are known exactly: every entry shows up once, "." and ".." are
// filtered (before onEntry ever sees them), and both the type hint and the
// inode come from the kernel dirent rather than from an extra lookup each.
func TestReadDirRaw_BasicListing(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	file := filepath.Join(root, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	skipIfNoSymlinkSupport(t, root)
	if err := os.Symlink(file, filepath.Join(root, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	got := listRaw(t, root, make([]byte, getdentsBufSize), nil)
	assertSameSet(t, rawNames(got), []string{"a.txt", "sub", "link"})

	if e := got["a.txt"]; e.dType != dtReg {
		t.Errorf("a.txt d_type = %d, want %d (dtReg)", e.dType, dtReg)
	}
	if e := got["sub"]; e.dType != dtDir {
		t.Errorf("sub d_type = %d, want %d (dtDir)", e.dType, dtDir)
	}
	if e := got["link"]; e.dType != dtLnk {
		t.Errorf("link d_type = %d, want %d (dtLnk)", e.dType, dtLnk)
	}

	// The dirent's inode must be the kernel's, not a made-up value. Checked on
	// the plain entries only: a symlink's dirent describes the link itself, so
	// verifying that one would need an lstat, and this test is about the
	// reader, not about symlink semantics.
	for _, name := range []string{"a.txt", "sub"} {
		wantIno := statIno(t, filepath.Join(root, name))
		if got[name].ino != wantIno {
			t.Errorf("%s ino = %d, want %d", name, got[name].ino, wantIno)
		}
	}
}

// TestReadDirRaw_SkipFilter pins the skip set to the reader itself: a skipped
// name must never reach onEntry, so there is nothing for the caller to filter
// afterwards (and nothing to allocate for it either - the lookup is done on a
// zero-copy view of the kernel's own name bytes).
func TestReadDirRaw_SkipFilter(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"keep1", "keep2", "skip1", "skip2"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "skipme"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	skip := skipSetOf("skip1", "skip2", "skipme")
	got := listRaw(t, root, make([]byte, getdentsBufSize), skip)
	assertSameSet(t, rawNames(got), []string{"keep1", "keep2"})

	// Same tree, no skip set: everything is there, so the result above really
	// was the filter and not a reading bug.
	all := listRaw(t, root, make([]byte, getdentsBufSize), nil)
	assertSameSet(t, rawNames(all), []string{"keep1", "keep2", "skip1", "skip2", "skipme"})
}

// TestReadDirRaw_SkipSpansGetdentsBatches drives the reader with a deliberately
// tiny scratch buffer so one directory needs many SYS_GETDENTS64 rounds: a name
// skipped in one round must not reappear (or vanish) in the next. This is the
// class of bug the old swap-delete filter could hide, and the reason skipping
// moved into the parse loop.
func TestReadDirRaw_SkipSpansGetdentsBatches(t *testing.T) {
	root := t.TempDir()
	const total = 300

	var want []string
	skip := make(map[string]struct{})
	for i := range total {
		name := "f" + itoa(i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if i%3 == 0 {
			skip[name] = struct{}{}
		} else {
			want = append(want, name)
		}
	}

	const tiny = 512 // a handful of dirent records per round, ~20+ rounds for 300 entries
	got := listRaw(t, root, make([]byte, tiny), skip)
	assertSameSet(t, rawNames(got), want)
}

// TestReadDirRaw_Errors: every failure of the directory open itself comes back
// as a bare errno for the caller to wrap in a per-directory DirErr, never as a
// panic or a partial listing.
func TestReadDirRaw_Errors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not_a_dir.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Run("missing path", func(t *testing.T) {
		missing := filepath.Join(root, "does-not-exist")
		err := readDirRaw(pathZ(missing), make([]byte, getdentsBufSize), nil, noopEntry)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("readDirRaw(%q) = %v, want fs.ErrNotExist", missing, err)
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		// O_DIRECTORY turns "this is a regular file" into ENOTDIR up front,
		// instead of handing back an fd we'd have to reject ourselves
		err := readDirRaw(pathZ(file), make([]byte, getdentsBufSize), nil, noopEntry)
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("readDirRaw(%q) = %v, want ENOTDIR", file, err)
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: permission bits don't deny access, can't exercise EACCES")
		}
		locked := filepath.Join(root, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		t.Cleanup(func() { os.Chmod(locked, 0o755) }) // let TempDir cleanup succeed

		err := readDirRaw(pathZ(locked), make([]byte, getdentsBufSize), nil, noopEntry)
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("readDirRaw(%q) = %v, want fs.ErrPermission", locked, err)
		}
	})
}

// TestReadDirRaw_OnEntryErrorAborts: the callback's error is a stop signal,
// handed back verbatim and without reading any further entries - that is what
// lets a caller (or a future bounded-memory mode) cut a listing short.
func TestReadDirRaw_OnEntryErrorAborts(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	sentinel := errors.New("stop here")
	seen := 0
	err := readDirRaw(pathZ(root), make([]byte, getdentsBufSize), nil,
		func([]byte, uint8, uint64) error {
			seen++
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Errorf("readDirRaw = %v, want the onEntry error back verbatim", err)
	}
	if seen != 1 {
		t.Errorf("onEntry called %d times, want 1 (the reader must stop at the first error)", seen)
	}
}
