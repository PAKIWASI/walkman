package walkman

import (
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// fakeDirEntry: a minimal fs.DirEntry with a controllable Name(), so
// filterSkipped can be tested against exact, deterministic orderings.
// Real directory reads (readDir/ReadDir(-1)) do NOT guarantee any
// particular order, which is exactly why the swap-delete bug below
// couldn't be pinned down through an integration test against a real
// tempdir tree — the entry order there is filesystem-dependent, not
// something a test can force.
// ---------------------------------------------------------------------

type fakeDirEntry struct {
	name  string
	isDir bool
}

func (f fakeDirEntry) Name() string { return f.name }
func (f fakeDirEntry) IsDir() bool  { return f.isDir }
func (f fakeDirEntry) Type() fs.FileMode {
	if f.isDir {
		return fs.ModeDir
	}
	return 0
}
func (f fakeDirEntry) Info() (fs.FileInfo, error) { return fakeFileInfo(f), nil }

type fakeFileInfo fakeDirEntry

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return fakeDirEntry(f).Type() }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.isDir }
func (f fakeFileInfo) Sys() any           { return nil }

func entries(names ...string) []fs.DirEntry {
	out := make([]fs.DirEntry, len(names))
	for i, n := range names {
		out[i] = fakeDirEntry{name: n}
	}
	return out
}

func names(dirs []fs.DirEntry) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Name()
	}
	return out
}

// assertSameSet fails unless got and want contain exactly the same
// elements (any order, no duplicates counted twice).
func assertSameSet(t *testing.T, got, want []string) {
	t.Helper()

	gotCount := make(map[string]int, len(got))
	for _, g := range got {
		gotCount[g]++
	}
	wantCount := make(map[string]int, len(want))
	for _, w := range want {
		wantCount[w]++
	}

	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for k, c := range wantCount {
		if gotCount[k] != c {
			t.Fatalf("got %v, want %v (mismatch on %q: got %d, want %d)", got, want, k, gotCount[k], c)
		}
	}
}

// ---------------------------------------------------------------------
// filterSkipped: direct unit tests, order-controlled
// ---------------------------------------------------------------------

func TestFilterSkipped_NoSkipSet_ReturnsUnchanged(t *testing.T) {
	in := entries("a", "b", "c")
	out := filterSkipped(in, nil)
	assertSameSet(t, names(out), []string{"a", "b", "c"})
}

func TestFilterSkipped_EmptyInput(t *testing.T) {
	out := filterSkipped(entries(), skipSetOf("a"))
	if len(out) != 0 {
		t.Fatalf("got %v, want empty", names(out))
	}
}

func TestFilterSkipped_NoneMatch(t *testing.T) {
	in := entries("a", "b", "c")
	out := filterSkipped(in, skipSetOf("zzz"))
	assertSameSet(t, names(out), []string{"a", "b", "c"})
}

func TestFilterSkipped_AllMatch(t *testing.T) {
	in := entries("a", "b", "c")
	out := filterSkipped(in, skipSetOf("a", "b", "c"))
	if len(out) != 0 {
		t.Fatalf("got %v, want empty", names(out))
	}
}

func TestFilterSkipped_SingleMatch_Head(t *testing.T) {
	in := entries("skip", "keep1", "keep2")
	out := filterSkipped(in, skipSetOf("skip"))
	assertSameSet(t, names(out), []string{"keep1", "keep2"})
}

func TestFilterSkipped_SingleMatch_Tail(t *testing.T) {
	in := entries("keep1", "keep2", "skip")
	out := filterSkipped(in, skipSetOf("skip"))
	assertSameSet(t, names(out), []string{"keep1", "keep2"})
}

func TestFilterSkipped_SingleMatch_Middle(t *testing.T) {
	in := entries("keep1", "skip", "keep2")
	out := filterSkipped(in, skipSetOf("skip"))
	assertSameSet(t, names(out), []string{"keep1", "keep2"})
}

// TestFilterSkipped_HeadAndTailMatch_MiddleSurvives is the exact repro of
// the swap-delete bug: a match at index 0 gets overwritten with the last
// element, which is itself a match — and without re-examining index 0,
// that swapped-in match is never removed, while the untouched middle
// "keep" entry silently disappears when the slice is truncated.
//
// With [skipA, keep, skipB]:
//   - i=0: skipA matches -> dirs[0] = dirs[2] (skipB), len drops to 2
//   - i=1: keep does not match -> left alone
//   - if the loop does NOT recheck index 0, it never sees that dirs[0]
//     is now skipB (also a match), so skipB survives in the result and
//     "keep" is gone entirely.
func TestFilterSkipped_HeadAndTailMatch_MiddleSurvives(t *testing.T) {
	in := entries("skipA", "keep", "skipB")
	out := filterSkipped(in, skipSetOf("skipA", "skipB"))
	assertSameSet(t, names(out), []string{"keep"})
}

// TestFilterSkipped_ConsecutiveMatchesAtTail covers the case where the
// element swapped into a matched slot is *itself* freshly matched
// (rather than pre-existing), which only happens once you have 3+
// matches clustered near the end.
func TestFilterSkipped_ConsecutiveMatchesAtTail(t *testing.T) {
	in := entries("keep", "skip1", "skip2", "skip3")
	out := filterSkipped(in, skipSetOf("skip1", "skip2", "skip3"))
	assertSameSet(t, names(out), []string{"keep"})
}

// TestFilterSkipped_EveryAdjacentPairAtTail sweeps every 2-matches-out-of-N
// combination so any position-dependent variant of the swap-delete bug
// (not just head+tail) gets exercised, not just the one pattern above.
func TestFilterSkipped_EveryAdjacentPairAtTail(t *testing.T) {
	base := []string{"n0", "n1", "n2", "n3", "n4"}
	for skipI := 0; skipI < len(base); skipI++ {
		for skipJ := 0; skipJ < len(base); skipJ++ {
			if skipI == skipJ {
				continue
			}
			skip := skipSetOf(base[skipI], base[skipJ])
			var want []string
			for _, n := range base {
				if n != base[skipI] && n != base[skipJ] {
					want = append(want, n)
				}
			}
			in := entries(base...)
			out := filterSkipped(in, skip)
			if len(out) != len(want) {
				t.Fatalf("skip={%s,%s}: got %v, want set %v", base[skipI], base[skipJ], names(out), want)
			}
			assertSameSet(t, names(out), want)
		}
	}
}

func TestFilterSkipped_DuplicateNames_AllInstancesRemoved(t *testing.T) {
	// Not a realistic filesystem state (a real dir can't have two entries
	// with the same name), but filterSkipped shouldn't assume uniqueness
	// beyond what the caller guarantees - matching purely on skipSet
	// membership, so every occurrence goes.
	in := entries("skip", "keep", "skip", "skip")
	out := filterSkipped(in, skipSetOf("skip"))
	assertSameSet(t, names(out), []string{"keep"})
}

func skipSetOf(names ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(names))
	for _, n := range names {
		s[n] = struct{}{}
	}
	return s
}

// TestFilterSkipped_DoesNotAllocate guarantees the swap-delete is truly
// in-place: no new backing array, no per-call heap traffic. work is reset
// from backup via copy() before each timed call (copying into an
// already-sized destination doesn't itself allocate), so any non-zero
// AllocsPerRun result can only come from filterSkipped.
func TestFilterSkipped_DoesNotAllocate(t *testing.T) {
	orig := entries("a", "skip1", "b", "skip2", "c", "skip3", "d")
	backup := make([]fs.DirEntry, len(orig))
	copy(backup, orig)
	work := make([]fs.DirEntry, len(orig))
	skip := skipSetOf("skip1", "skip2", "skip3")

	allocs := testing.AllocsPerRun(1000, func() {
		copy(work, backup)
		_ = filterSkipped(work, skip)
	})

	if allocs != 0 {
		t.Fatalf("filterSkipped allocated %.1f times per call, want 0 (swap-delete must stay in-place)", allocs)
	}
}

// TestFilterSkipped_SharesUnderlyingArray backs up the allocation
// guarantee with a structural check: reslicing (dirs[:n]) always keeps
// the same cap as the original slice, since cap only shrinks on a fresh
// allocation (append past capacity) - never on a plain reslice. A future
// change that swapped filterSkipped's reslice for e.g. append(out[:0],
// ...) or a fresh make() would still pass DoesNotAllocate in the trivial
// all-kept case but would show up here as a cap mismatch.
func TestFilterSkipped_SharesUnderlyingArray(t *testing.T) {
	in := entries("a", "skip", "b", "c")
	wantCap := cap(in)

	out := filterSkipped(in, skipSetOf("skip"))

	if cap(out) != wantCap {
		t.Fatalf("cap(out) = %d, want %d (result must reslice the same backing array, not allocate a new one)",
			cap(out), wantCap)
	}
}

// ---------------------------------------------------------------------
// Integration-level regression: large mixed skip/keep fanout.
//
// This can't force a specific readDir order the way the unit tests
// above do, but with enough entries and a good fraction skipped, the
// tail-cluster pattern shows up often enough across sub-tests that a
// regression here should fail reliably rather than needing exact
// ordering control.
// ---------------------------------------------------------------------

func TestWalk_SkipList_LargeMixedFanout(t *testing.T) {
	const total = 400
	skipNames := make(map[string]struct{})
	var spec []string
	for i := 0; i < total; i++ {
		n := "entry" + itoa(i)
		if i%3 == 0 { // every third one is a directory eligible for skipping
			spec = append(spec, n+"/")
			spec = append(spec, filepath.Join(n, "inner.txt"))
			if i%2 == 0 {
				skipNames[n] = struct{}{}
			}
		} else {
			spec = append(spec, n+".txt")
		}
	}
	root := buildTree(t, spec)

	var skipList []string
	for n := range skipNames {
		skipList = append(skipList, n)
	}

	w := NewWalkman(false, 0, skipList)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	for _, r := range results {
		base := filepath.Base(r.Dir)
		if _, skipped := skipNames[base]; skipped {
			t.Fatalf("walked into skipped subtree: %s", r.Dir)
		}
		for _, e := range r.Ret {
			if _, skipped := skipNames[e.Name()]; skipped {
				t.Fatalf("skipped entry %q still present in Ret for %s", e.Name(), r.Dir)
			}
		}
	}

	// Every non-skipped, non-inner file/dir in root's own listing should
	// still be there — total entries in root minus the skipped ones.
	for _, r := range results {
		if r.Dir != root {
			continue
		}
		wantRootEntries := total - len(skipNames)
		if len(r.Ret) != wantRootEntries {
			t.Fatalf("root Ret has %d entries, want %d (total=%d skipped=%d)",
				len(r.Ret), wantRootEntries, total, len(skipNames))
		}
	}
}

// TestWalk_SkipList_StatsAgreeWithPlainCount cross-checks the trackStats
// skipped counter against sequentialCounts (which uses filepath.WalkDir's
// SkipDir, an independent implementation) on the same large fanout tree,
// so a filterSkipped regression that drops or over-counts entries shows
// up as a stats mismatch too, not just a "wrong item present" failure.
func TestWalk_SkipList_StatsAgreeWithPlainCount(t *testing.T) {
	const total = 200
	var spec []string
	var skipList []string
	for i := 0; i < total; i++ {
		n := "d" + itoa(i)
		spec = append(spec, n+"/", filepath.Join(n, "f.txt"))
		if i%2 == 0 {
			skipList = append(skipList, n)
		}
	}
	root := buildTree(t, spec)

	wantFiles, wantDirs := sequentialCounts(t, root, skipList)

	w := NewWalkmanWithConfig(false, 0, skipList, true, DefaultPoolConfig())
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	gotFiles, gotDirs, errs := countEntries(results)
	if errs != 0 {
		t.Fatalf("got %d error results, want 0", errs)
	}
	if gotFiles != wantFiles || gotDirs != wantDirs {
		t.Fatalf("got files=%d dirs=%d, want files=%d dirs=%d (skipped=%d of %d)",
			gotFiles, gotDirs, wantFiles, wantDirs, len(skipList), total)
	}

	_, _, _, skipped, _ := w.Stats()
	if int(skipped) != len(skipList) {
		t.Fatalf("stats.skipped = %d, want %d", skipped, len(skipList))
	}
}
