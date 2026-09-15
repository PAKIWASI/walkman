package walkman

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Entry is the zero-allocation, fs.DirEntry-shaped value walkman hands out in
// place of a stdlib fs.DirEntry: one struct per dirent record, with nothing
// allocated per entry beyond the name bytes in the shared string store.
func TestEntry_NameInoPassthrough(t *testing.T) {
	e := Entry{parentDir: "/tmp", name: "thing.txt", ino: 42, typ: dtReg}
	if e.Name() != "thing.txt" {
		t.Errorf("Name() = %q, want %q", e.Name(), "thing.txt")
	}
	if e.Ino() != 42 {
		t.Errorf("Ino() = %d, want 42", e.Ino())
	}
}

// TestEntry_IsDirOnlyForDtDir: IsDir is the d_type hint and nothing else. It
// deliberately does not stat for DT_UNKNOWN - a caller that needs certainty
// stats the path itself, which is exactly what Entry.Info does.
func TestEntry_IsDirOnlyForDtDir(t *testing.T) {
	tests := []struct {
		name string
		typ  uint8
		want bool
	}{
		{"DT_DIR", dtDir, true},
		{"DT_REG", dtReg, false},
		{"DT_LNK", dtLnk, false},
		{"DT_UNKNOWN", dtUnknown, false},
		// no named const for these: fifo (1), char device (2), socket (12)
		{"fifo", 1, false},
		{"char device", 2, false},
		{"socket", 12, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Entry{typ: tt.typ}).IsDir(); got != tt.want {
				t.Errorf("Entry{typ: %d}.IsDir() = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
}

func TestEntry_FileModeMapping(t *testing.T) {
	tests := []struct {
		name string
		typ  uint8
		want fs.FileMode
	}{
		{"DT_DIR", dtDir, fs.ModeDir},
		{"DT_LNK", dtLnk, fs.ModeSymlink},
		{"DT_REG", dtReg, 0},
		{"DT_UNKNOWN", dtUnknown, fs.ModeIrregular},
		{"unknown d_type 99", 99, fs.ModeIrregular},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Entry{typ: tt.typ}
			if got := e.FileMode(); got != tt.want {
				t.Errorf("Entry{typ: %d}.FileMode() = %v, want %v", tt.typ, got, tt.want)
			}
			// Type() is the fs.DirEntry spelling of the same mapping
			if got := e.Type(); got != tt.want {
				t.Errorf("Entry{typ: %d}.Type() = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
}

// TestEntry_InfoIsLazyLstat: Info is the one method here that allocates and
// hits the filesystem, because Info is genuinely optional for a walk - a
// consumer that only needs names and types never pays for it.
func TestEntry_InfoIsLazyLstat(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := Entry{parentDir: root, name: "a.txt", typ: dtReg}.Info()
	if err != nil {
		t.Fatalf("Info() = %v, want nil", err)
	}
	if got.Name() != "a.txt" {
		t.Errorf("Info().Name() = %q, want %q", got.Name(), "a.txt")
	}
	if got.Size() != 5 {
		t.Errorf("Info().Size() = %d, want 5", got.Size())
	}
	if got.IsDir() {
		t.Error("Info().IsDir() = true, want false")
	}

	// A missing entry is an error, not a panic and not a zero value.
	if _, err := (Entry{parentDir: root, name: "gone.txt"}).Info(); err == nil {
		t.Error("Info() for a missing entry returned nil, want an error")
	}
}

// TestEntry_SatisfiesFSDirEntry backs the compile-time assertion in walkman.go
// (var _ fs.DirEntry = Entry{}) from the test side as well.
func TestEntry_SatisfiesFSDirEntry(t *testing.T) {
	var e fs.DirEntry = Entry{parentDir: "/", name: "x"}
	if e.Name() != "x" {
		t.Errorf("as fs.DirEntry: Name() = %q, want %q", e.Name(), "x")
	}
}

// TestDTypeFromInfo covers every branch of the os.Stat -> d_type mapping,
// including the two a walk cannot reach by following a symlink alone: Lstat is
// how a FileMode with ModeSymlink ever shows up (os.Stat has already followed
// it), and anything with no d_type of its own - fifo, socket, device node -
// has to fall through to DT_UNKNOWN rather than being mistaken for a file or a
// directory.
func TestDTypeFromInfo(t *testing.T) {
	root := t.TempDir()

	file := filepath.Join(root, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dir := filepath.Join(root, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(root, "l")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("symlinks not supported on this filesystem/platform: %v", err)
	}

	t.Run("regular file", func(t *testing.T) {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatalf("Stat(%q): %v", file, err)
		}
		if got := dTypeFromInfo(info); got != dtReg {
			t.Errorf("dTypeFromInfo(file) = %d, want %d (dtReg)", got, dtReg)
		}
	})

	t.Run("directory", func(t *testing.T) {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%q): %v", dir, err)
		}
		if got := dTypeFromInfo(info); got != dtDir {
			t.Errorf("dTypeFromInfo(dir) = %d, want %d (dtDir)", got, dtDir)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("Lstat(%q): %v", link, err)
		}
		if got := dTypeFromInfo(info); got != dtLnk {
			t.Errorf("dTypeFromInfo(lstat link) = %d, want %d (dtLnk)", got, dtLnk)
		}
	})

	t.Run("fifo falls through to unknown", func(t *testing.T) {
		fifo := filepath.Join(root, "p")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skipf("cannot create a fifo here: %v", err)
		}
		info, err := os.Stat(fifo)
		if err != nil {
			t.Fatalf("Stat(%q): %v", fifo, err)
		}
		if got := dTypeFromInfo(info); got != dtUnknown {
			t.Errorf("dTypeFromInfo(fifo) = %d, want %d (dtUnknown)", got, dtUnknown)
		}
		// and DT_UNKNOWN must not masquerade as a directory to a walker
		if (Entry{typ: dtUnknown}).IsDir() {
			t.Error("Entry{typ: dtUnknown}.IsDir() = true, want false")
		}
		if got := (Entry{typ: dtUnknown}).FileMode(); got != fs.ModeIrregular {
			t.Errorf("Entry{typ: dtUnknown}.FileMode() = %v, want %v", got, fs.ModeIrregular)
		}
	})
}
