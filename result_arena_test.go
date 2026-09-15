package walkman

import (
	"errors"
	"testing"
)

// entryArena/resultArena are what let a worker hand out per-directory Entry and
// DirErr slices without a per-entry allocation: values are copied once into
// chunked, append-only storage, and the slice the consumer receives points
// straight into it. These tests pin the two properties callers depend on -
// the arena owns its copies, and different batches never alias - plus the
// DirErr mark/slice protocol visitSym uses.

func entriesOf(names ...string) []Entry {
	out := make([]Entry, len(names))
	for i, n := range names {
		out[i] = Entry{name: n, typ: dtReg, ino: uint64(i + 1)}
	}
	return out
}

// TestEntryArena_StoreEntriesCopies: the caller's scratch buffer is reused for
// the next directory, so everything still readable from a returned slice must
// be arena memory, never scratch memory.
func TestEntryArena_StoreEntriesCopies(t *testing.T) {
	ra := newResultArena(0)

	scratch := entriesOf("a", "b", "c")
	got := ra.storeEntries(scratch)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}

	for i := range scratch { // simulate the next directory reusing scratch
		scratch[i] = Entry{name: "clobbered", typ: dtDir}
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Name() != want {
			t.Errorf("stored entry %d = %q after scratch reuse, want %q", i, got[i].Name(), want)
		}
		if got[i].typ != dtReg || got[i].ino != uint64(i+1) {
			t.Errorf("stored entry %d = {typ:%d ino:%d}, want {typ:%d ino:%d}",
				i, got[i].typ, got[i].ino, dtReg, i+1)
		}
	}
}

// TestEntryArena_EmptyStoresNothing: an empty directory reserves nothing and
// gets a nil slice back, which is what makes DirBatch.Entries nil for it.
func TestEntryArena_EmptyStoresNothing(t *testing.T) {
	ra := newResultArena(0)

	if got := ra.storeEntries(nil); got != nil {
		t.Errorf("storeEntries(nil) = %v, want nil", got)
	}
	if got := ra.storeEntries(entriesOf()); got != nil {
		t.Errorf("storeEntries(empty) = %v, want nil", got)
	}
}

// TestEntryArena_BatchesDoNotAlias: two directories must never share storage,
// or a consumer still holding one batch would watch its own slice mutate when
// the next batch lands.
func TestEntryArena_BatchesDoNotAlias(t *testing.T) {
	ra := newResultArena(0)

	first := ra.storeEntries(entriesOf("first", "first2"))
	second := ra.storeEntries(entriesOf("second"))

	for i := range second {
		for j := range first {
			if &second[i] == &first[j] {
				t.Fatalf("second batch element %d aliases first batch element %d", i, j)
			}
		}
	}
	if first[0].Name() != "first" || second[0].Name() != "second" {
		t.Errorf("batches = %q / %q, want first / second", first[0].Name(), second[0].Name())
	}
}

// TestEntryArena_OversizedBatchOwnAllocation: a directory larger than one node
// gets a dedicated allocation sized exactly to the batch. The alternative -
// splitting it across nodes - would break the contiguity the design exists
// for.
func TestEntryArena_OversizedBatchOwnAllocation(t *testing.T) {
	ra := newResultArena(0)

	n := entryNodeSize + 1
	names := make([]string, n)
	for i := range names {
		names[i] = "big"
	}

	got := ra.storeEntries(entriesOf(names...))
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}
	if cap(got) != len(got) {
		t.Errorf("cap(got) = %d, want %d (a dedicated allocation, not a slice of a node)", cap(got), len(got))
	}
}

// TestEntryArena_NodeBoundaryStartsFreshNode: when a batch doesn't fit in what
// is left of a node, the tail of that node is abandoned and the batch starts a
// fresh one - bounded waste, and no overlap with the batch before it.
func TestEntryArena_NodeBoundaryStartsFreshNode(t *testing.T) {
	ra := newResultArena(0)

	filler := make([]string, entryNodeSize-1)
	for i := range filler {
		filler[i] = "fill"
	}
	first := ra.storeEntries(entriesOf(filler...))
	second := ra.storeEntries(entriesOf("x", "y"))

	for i := range second {
		for j := range first {
			if &second[i] == &first[j] {
				t.Fatalf("post-boundary batch element %d aliases the previous node's element %d", i, j)
			}
		}
	}
	if first[0].Name() != "fill" || second[0].Name() != "x" {
		t.Errorf("first/second = %q / %q, want fill / x", first[0].Name(), second[0].Name())
	}
}

// TestResultArena_DirErrMarkAndSlice mirrors how visitSym uses the error
// accumulator: mark where this directory starts, append whatever its entries
// produce, then publish exactly the slice from the mark on. Anything already
// published for an earlier directory must stay readable.
func TestResultArena_DirErrMarkAndSlice(t *testing.T) {
	ra := newResultArena(0)

	ra.storeDirErr(DirErr{Name: "older", Err: ErrSymlinkCycle})
	published := ra.sliceDirErr(0)
	if len(published) != 1 || published[0].Name != "older" {
		t.Fatalf("published = %+v, want one err named older", published)
	}

	off := ra.getDirErrMark()
	ra.storeDirErr(DirErr{Name: "new1", Err: ErrDanglingSymlink})
	ra.storeDirErr(DirErr{Name: "new2", Err: ErrDanglingSymlink})
	got := ra.sliceDirErr(off)

	if len(got) != 2 {
		t.Fatalf("len(sliceDirErr(off)) = %d, want 2", len(got))
	}
	if got[0].Name != "new1" || got[1].Name != "new2" {
		t.Errorf("sliceDirErr(off) = %+v, want new1 then new2", got)
	}
	if !errors.Is(got[0].Err, ErrDanglingSymlink) {
		t.Errorf("got[0].Err = %v, want ErrDanglingSymlink", got[0].Err)
	}
	if len(published) != 1 || published[0].Name != "older" {
		t.Errorf("previously published slice changed: %+v", published)
	}
}

// TestResultArena_DirErrsGrowPastInitialCap: the accumulator starts with room
// for resArenaMinCapDirErrs and must grow (and move) once one directory reports
// more errors than that.
func TestResultArena_DirErrsGrowPastInitialCap(t *testing.T) {
	const n = resArenaMinCapDirErrs * 3
	ra := newResultArena(0)

	off := ra.getDirErrMark()
	for i := range n {
		ra.storeDirErr(DirErr{Name: "e" + itoa(i), Err: ErrDanglingSymlink})
	}

	got := ra.sliceDirErr(off)
	if len(got) != n {
		t.Fatalf("len = %d, want %d", len(got), n)
	}
	for i := range n {
		if want := "e" + itoa(i); got[i].Name != want {
			t.Fatalf("got[%d].Name = %q, want %q", i, got[i].Name, want)
		}
	}
}

// TestNewResultArena_DefaultCap: a non-positive dirErrCap falls back to the
// minimum, so newResultArena(0) - what every production worker gets - is always
// usable.
func TestNewResultArena_DefaultCap(t *testing.T) {
	for _, req := range []int{0, -1} {
		ra := newResultArena(req)
		if ra.entries == nil {
			t.Fatalf("newResultArena(%d).entries = nil", req)
		}
		if got := cap(ra.dirErrs); got < resArenaMinCapDirErrs {
			t.Errorf("newResultArena(%d) dirErr cap = %d, want >= %d", req, got, resArenaMinCapDirErrs)
		}
	}
}
