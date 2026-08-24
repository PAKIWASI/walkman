package walkman

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// Test tree helpers
// ---------------------------------------------------------------------

// treeSpec is a tiny declarative way to build a directory tree for a
// test: keys are slash-separated paths relative to the tree root, and a
// trailing "/" marks a directory (created even if it ends up empty).
// Anything else is created as a regular file with a few bytes of content.
func buildTree(t *testing.T, spec []string) string {
	t.Helper()

	root := t.TempDir()

	for _, rel := range spec {
		isDir := len(rel) > 0 && rel[len(rel)-1] == '/'
		clean := filepath.Clean(rel)
		full := filepath.Join(root, clean)

		if isDir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", full, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", full, err)
		}
	}

	return root
}

// drain runs w.Walk(root) to completion and returns every result plus the
// terminal error from Wait. It also enforces a hard timeout so a
// termination bug (lost wakeup, deadlock) fails the test instead of
// hanging the whole run.
func drain(t *testing.T, w *Walkman, root string) ([]WalkResult, error) {
	t.Helper()

	var results []WalkResult
	done := make(chan struct{})

	go func() {
		defer close(done)
		for r := range w.Walk(root) {
			results = append(results, r)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Walk did not complete within 10s (possible deadlock/lost wakeup)")
	}

	return results, w.Wait()
}

// countEntries sums direct file/dir entries across every successful
// WalkResult, the same convention BenchmarkWalk_* uses: a directory's
// entries are counted once, from its own listing, not re-derived from
// recursing into it again.
func countEntries(results []WalkResult) (files, dirs int, errs int) {
	for _, r := range results {
		if r.Err != nil {
			errs++
			continue
		}
		for _, e := range r.Ret {
			if e.IsDir() {
				dirs++
			} else {
				files++
			}
		}
	}
	return
}

// walkedDirs returns the sorted set of directories a Walkman walk
// actually visited (i.e. got its own WalkResult for), independent of
// delivery order.
func walkedDirs(results []WalkResult) []string {
	dirs := make([]string, 0, len(results))
	for _, r := range results {
		dirs = append(dirs, r.Dir)
	}
	sort.Strings(dirs)
	return dirs
}

// sequentialCounts is the filepath.WalkDir baseline used to check
// Walkman's counts, following the exact same "swallow per-entry errors,
// don't count the root itself" convention as Walkman.visit.
func sequentialCounts(t *testing.T, root string, skip []string) (files, dirs int) {
	t.Helper()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			if contains(skip, d.Name()) {
				return filepath.SkipDir
			}
			dirs++
			return nil
		}
		files++
		return nil
	})
	if err != nil {
		t.Fatalf("filepath.WalkDir(%q): %v", root, err)
	}
	return
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Basic correctness
// ---------------------------------------------------------------------

func TestWalk_EmptyDirectory(t *testing.T) {
	root := t.TempDir()

	w := NewWalkman(false, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (just the root)", len(results))
	}
	if results[0].Dir != root {
		t.Fatalf("results[0].Dir = %q, want %q", results[0].Dir, root)
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}
	if len(results[0].Ret) != 0 {
		t.Fatalf("results[0].Ret = %v, want empty", results[0].Ret)
	}
}

func TestWalk_FlatDirectory(t *testing.T) {
	root := buildTree(t, []string{"a.txt", "b.txt", "c.txt"})

	w := NewWalkman(false, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	files, dirs, errs := countEntries(results)
	if files != 3 || dirs != 0 || errs != 0 {
		t.Fatalf("files=%d dirs=%d errs=%d, want files=3 dirs=0 errs=0", files, dirs, errs)
	}
}

func TestWalk_NestedTree_MatchesFilepathWalkDir(t *testing.T) {
	root := buildTree(t, []string{
		"root1.txt",
		"root2.txt",
		"a/",
		"a/a1.txt",
		"a/a2.txt",
		"a/suba/",
		"a/suba/deep1.txt",
		"a/suba/deep2.txt",
		"a/subb/",
		"b/",
		"b/b1.txt",
		"c/", // empty dir
	})

	wantFiles, wantDirs := sequentialCounts(t, root, nil)

	w := NewWalkman(false, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	gotFiles, gotDirs, errs := countEntries(results)
	if errs != 0 {
		t.Fatalf("got %d error results, want 0", errs)
	}
	if gotFiles != wantFiles || gotDirs != wantDirs {
		t.Fatalf("got files=%d dirs=%d, want files=%d dirs=%d", gotFiles, gotDirs, wantFiles, wantDirs)
	}

	// Every directory in the tree (including empty ones and the root)
	// must get exactly one WalkResult.
	wantDirsVisited := []string{
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "suba"),
		filepath.Join(root, "a", "subb"),
		filepath.Join(root, "b"),
		filepath.Join(root, "c"),
	}
	sort.Strings(wantDirsVisited)

	gotDirsVisited := walkedDirs(results)
	if len(gotDirsVisited) != len(wantDirsVisited) {
		t.Fatalf("visited %d dirs, want %d\ngot:  %v\nwant: %v",
			len(gotDirsVisited), len(wantDirsVisited), gotDirsVisited, wantDirsVisited)
	}
	for i := range wantDirsVisited {
		if gotDirsVisited[i] != wantDirsVisited[i] {
			t.Fatalf("visited dirs = %v, want %v", gotDirsVisited, wantDirsVisited)
		}
	}
}

// ---------------------------------------------------------------------
// SkipList
// ---------------------------------------------------------------------

func TestWalk_SkipList_PrunesSubtree(t *testing.T) {
	root := buildTree(t, []string{
		"keep/",
		"keep/k.txt",
		"skipme/",
		"skipme/hidden1.txt",
		"skipme/hidden2.txt",
		"skipme/nested/",
		"skipme/nested/deep.txt",
	})

	w := NewWalkman(false, 0, []string{"skipme"})
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	for _, r := range results {
		if r.Dir == filepath.Join(root, "skipme") || r.Dir == filepath.Join(root, "skipme", "nested") {
			t.Fatalf("walked into skipped subtree: %s", r.Dir)
		}
	}

	// "skipme" itself must not appear as an entry of the root either.
	for _, r := range results {
		if r.Dir != root {
			continue
		}
		for _, e := range r.Ret {
			if e.Name() == "skipme" {
				t.Fatalf("skipped dir %q still present in root's Ret", e.Name())
			}
		}
	}
}

func TestWalk_SkipList_AppliesAtEveryDepth(t *testing.T) {
	root := buildTree(t, []string{
		"a/skipme/x.txt",
		"b/c/skipme/y.txt",
		"a/keep.txt",
	})

	w := NewWalkman(false, 0, []string{"skipme"})
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	for _, r := range results {
		if filepath.Base(r.Dir) == "skipme" {
			t.Fatalf("walked into skipped dir at depth: %s", r.Dir)
		}
	}
}

// ---------------------------------------------------------------------
// MaxDepth
// ---------------------------------------------------------------------

func TestWalk_MaxDepth_StopsDescending(t *testing.T) {
	root := buildTree(t, []string{
		"d1/d2/d3/leaf.txt",
		"d1/shallow.txt",
	})

	// maxDepth=1: root (depth 0) is walked, and its direct child dirs
	// (depth 1, i.e. "d1") are walked, but "d1"'s subdirectories (depth
	// 2, "d2") are not recursed into - though "d2" still shows up as an
	// entry inside d1's own listing.
	w := NewWalkman(false, 1, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	visited := walkedDirs(results)
	want := []string{root, filepath.Join(root, "d1")}
	sort.Strings(want)

	if len(visited) != len(want) {
		t.Fatalf("visited %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("visited %v, want %v", visited, want)
		}
	}

	// d1's own listing should still contain d2, even though we never
	// walk into it.
	for _, r := range results {
		if r.Dir != filepath.Join(root, "d1") {
			continue
		}
		found := false
		for _, e := range r.Ret {
			if e.Name() == "d2" && e.IsDir() {
				found = true
			}
		}
		if !found {
			t.Fatalf("d1's Ret = %v, want it to contain entry d2", r.Ret)
		}
	}
}

func TestWalk_MaxDepth_Zero_IsUnlimited(t *testing.T) {
	root := buildTree(t, []string{
		"a/b/c/d/e/leaf.txt",
	})

	w := NewWalkman(false, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	visited := walkedDirs(results)
	want := []string{
		root,
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "b"),
		filepath.Join(root, "a", "b", "c"),
		filepath.Join(root, "a", "b", "c", "d"),
		filepath.Join(root, "a", "b", "c", "d", "e"),
	}
	sort.Strings(want)

	if len(visited) != len(want) {
		t.Fatalf("visited %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("visited %v, want %v", visited, want)
		}
	}
}

// ---------------------------------------------------------------------
// Symlinks
// ---------------------------------------------------------------------

func skipIfNoSymlinkSupport(t *testing.T, root string) {
	t.Helper()
	target := filepath.Join(root, "__symlink_probe_target")
	link := filepath.Join(root, "__symlink_probe_link")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported on this filesystem/platform: %v", err)
	}
	os.Remove(target)
	os.Remove(link)
}

func TestWalk_FollowLinks_False_DoesNotDescend(t *testing.T) {
	root := buildTree(t, []string{
		"real/",
		"real/inside.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	link := filepath.Join(root, "link_to_real")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkmanWithConfig(false, 0, nil, true, PoolConfig{PoolSize: 2, InitialWorkerCap: 4, ResultBuffSize: 4})
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	for _, r := range results {
		if r.Dir == link {
			t.Fatalf("followLinks=false but walked into symlink %s", link)
		}
	}

	_, _, links, _, _ := w.Stats()
	if links != 1 {
		t.Fatalf("stats.links = %d, want 1", links)
	}
}

func TestWalk_FollowLinks_True_Descends(t *testing.T) {
	root := buildTree(t, []string{
		"real/",
		"real/inside.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	link := filepath.Join(root, "link_to_real")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	found := false
	for _, r := range results {
		if r.Dir == link {
			found = true
		}
	}
	if !found {
		t.Fatalf("followLinks=true but never walked into symlink %s; visited %v", link, walkedDirs(results))
	}
}

func TestWalk_BrokenSymlink_DoesNotCrash(t *testing.T) {
	root := buildTree(t, nil)
	skipIfNoSymlinkSupport(t, root)

	link := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "does_not_exist"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil) // followLinks=true is the path that stats the target
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (just root)", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("root result Err = %v, want nil", results[0].Err)
	}

	foundLink := false
	for _, e := range results[0].Ret {
		if e.Name() == "dangling" {
			foundLink = true
		}
	}
	if !foundLink {
		t.Fatalf("dangling symlink missing from root's Ret entirely")
	}
}

// ---------------------------------------------------------------------
// Errors: recoverable per-item failures must not abort the whole walk
// ---------------------------------------------------------------------

func TestWalk_NonexistentRoot_ReportsErrorNotFatal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	w := NewWalkman(false, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil (a per-item error, not fatal)", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("results[0].Err = nil, want a not-exist error")
	}
	if !errors.Is(results[0].Err, fs.ErrNotExist) {
		t.Fatalf("results[0].Err = %v, want fs.ErrNotExist", results[0].Err)
	}
}

func TestWalk_RootIsRegularFile_ReportsError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not_a_dir.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	w := NewWalkman(false, 0, nil)
	results, err := drain(t, w, file)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want a single error result", results)
	}
}

func TestWalk_PermissionDenied_IsRecoverable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't deny access, can't exercise EACCES")
	}

	root := buildTree(t, []string{
		"ok/ok.txt",
		"locked/secret.txt",
		"also_ok/fine.txt",
	})

	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) }) // let TempDir cleanup succeed

	w := NewWalkman(false, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil (one bad subdir shouldn't cancel the pool)", err)
	}

	var lockedResult *WalkResult
	okCount := 0
	for i, r := range results {
		if r.Dir == locked {
			lockedResult = &results[i]
		} else if r.Err == nil {
			okCount++
		}
	}

	if lockedResult == nil {
		t.Fatal("never got a result for the locked directory")
	}
	if lockedResult.Err == nil {
		t.Fatal("locked directory result Err = nil, want a permission error")
	}
	if !errors.Is(lockedResult.Err, fs.ErrPermission) {
		t.Fatalf("locked directory Err = %v, want fs.ErrPermission", lockedResult.Err)
	}

	// The rest of the tree (root, ok/, also_ok/) should still have been
	// walked normally despite the one failure.
	if okCount < 3 {
		t.Fatalf("only %d successful results alongside the failure, want >= 3 (root, ok/, also_ok/)", okCount)
	}
}

// ---------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------

func TestWalk_Stats_AccurateCounts(t *testing.T) {
	root := buildTree(t, []string{
		"f1.txt",
		"f2.txt",
		"sub1/",
		"sub1/f3.txt",
		"sub2/",
		"skipme/",
		"skipme/whatever.txt",
	})

	pc := PoolConfig{PoolSize: 4, InitialWorkerCap: 8, ResultBuffSize: 8}
	w := NewWalkmanWithConfig(false, 0, []string{"skipme"}, true, pc)
	_, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	files, dirs, links, skipped, maxDepthReached := w.Stats()

	// Files: f1.txt, f2.txt (root) + f3.txt (sub1) = 3.
	if files != 3 {
		t.Errorf("stats.files = %d, want 3", files)
	}
	// Dirs: sub1, sub2 (skipme is filtered before the dirs counter, per
	// visit()'s ordering: skip-filtering happens before the entry loop).
	if dirs != 2 {
		t.Errorf("stats.dirs = %d, want 2", dirs)
	}
	if links != 0 {
		t.Errorf("stats.links = %d, want 0", links)
	}
	if skipped != 1 {
		t.Errorf("stats.skipped = %d, want 1", skipped)
	}
	if maxDepthReached != 0 {
		t.Errorf("stats.maxDepthReached = %d, want 0", maxDepthReached)
	}
}

func TestWalk_Stats_OffByDefault(t *testing.T) {
	root := buildTree(t, []string{"f1.txt", "f2.txt"})

	w := NewWalkman(false, 0, nil) // trackStats defaults to false
	if _, err := drain(t, w, root); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	files, dirs, links, skipped, maxDepthReached := w.Stats()
	if files != 0 || dirs != 0 || links != 0 || skipped != 0 || maxDepthReached != 0 {
		t.Fatalf("stats = (%d,%d,%d,%d,%d), want all zero with tracking off",
			files, dirs, links, skipped, maxDepthReached)
	}
}

// ---------------------------------------------------------------------
// Concurrency: pool-size sweep, race-heavy repeats, oversubscription
// ---------------------------------------------------------------------

// TestWalk_ConsistentAcrossPoolSizes checks the walk's file/dir counts are
// identical regardless of pool shape, from a single well-behaved worker up
// through heavy oversubscription with tiny buffers.
//
// NOTE(-race): the oversubscribed/tiny-buffer config here drives the same
// kind of concurrent PushBottom/Steal traffic through a struct-typed T
// (walkItem) that workstealpool's own README documents as the trigger for
// its known benign race between PushBottom's array write and Steal's
// array read (see workstealpool's deque.go and README, "Known limitation:
// benign race under -race"). An occasional -race report here, with no
// accompanying count-mismatch failure, is that documented condition, not
// a walkman bug - see workstealpool's TestCountPrimesParallel_Repeated
// for the same caveat on the upstream side.
func TestWalk_ConsistentAcrossPoolSizes(t *testing.T) {
	// A reasonably wide+deep synthetic tree so there's real stealing
	// pressure, not just a couple of directories.
	var spec []string
	for i := 0; i < 6; i++ {
		for j := 0; j < 6; j++ {
			spec = append(spec, filepath.Join(
				"d"+itoa(i), "d"+itoa(j), "leaf"+itoa(j)+".txt",
			))
		}
	}
	root := buildTree(t, spec)

	wantFiles, wantDirs := sequentialCounts(t, root, nil)

	configs := []PoolConfig{
		{PoolSize: 1, InitialWorkerCap: 4, ResultBuffSize: 1},
		{PoolSize: 2, InitialWorkerCap: 4, ResultBuffSize: 4},
		{PoolSize: 4, InitialWorkerCap: 8, ResultBuffSize: 16},
		{PoolSize: 8, InitialWorkerCap: 8, ResultBuffSize: 64},
		{PoolSize: 32, InitialWorkerCap: 2, ResultBuffSize: 1}, // oversubscribed, tiny buffers
	}

	for _, pc := range configs {
		pc := pc
		t.Run("", func(t *testing.T) {
			w := NewWalkmanWithConfig(false, 0, nil, false, pc)
			results, err := drain(t, w, root)
			if err != nil {
				t.Fatalf("Wait() = %v, want nil", err)
			}

			gotFiles, gotDirs, errs := countEntries(results)
			if errs != 0 {
				t.Fatalf("cfg=%+v: got %d error results, want 0", pc, errs)
			}
			if gotFiles != wantFiles || gotDirs != wantDirs {
				t.Fatalf("cfg=%+v: got files=%d dirs=%d, want files=%d dirs=%d",
					pc, gotFiles, gotDirs, wantFiles, wantDirs)
			}
		})
	}
}

// TestWalk_RepeatedRuns_NoFlakiness hammers the same tree many times to
// surface any intermittent termination/stealing bug rather than trusting
// one lucky pass. Intended to be run with -race.
//
// NOTE(-race): also capable of triggering workstealpool's documented
// benign PushBottom/Steal race (see the note on
// TestWalk_ConsistentAcrossPoolSizes above) - a lone -race report here
// with counts still correct is that known condition, not a new bug.
func TestWalk_RepeatedRuns_NoFlakiness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repeated-run stress test in -short mode")
	}

	var spec []string
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			spec = append(spec, filepath.Join("d"+itoa(i), "leaf"+itoa(j)+".txt"))
		}
	}
	root := buildTree(t, spec)
	wantFiles, wantDirs := sequentialCounts(t, root, nil)

	const trials = 30
	for trial := 0; trial < trials; trial++ {
		pc := PoolConfig{PoolSize: 6, InitialWorkerCap: 4, ResultBuffSize: 4}
		w := NewWalkmanWithConfig(false, 0, nil, false, pc)

		results, err := drain(t, w, root)
		if err != nil {
			t.Fatalf("trial %d: Wait() = %v, want nil", trial, err)
		}
		gotFiles, gotDirs, errs := countEntries(results)
		if errs != 0 || gotFiles != wantFiles || gotDirs != wantDirs {
			t.Fatalf("trial %d: got files=%d dirs=%d errs=%d, want files=%d dirs=%d errs=0",
				trial, gotFiles, gotDirs, errs, wantFiles, wantDirs)
		}
	}
}

// TestWalk_OversubscribedParksAndWakesCleanly is the walkman-level analog
// of workstealpool's TestWorkerPool_ParkingUnderLightLoad: far more
// workers than there is work, so most of them should park at least once,
// and a lost-wakeup bug would hang this instead of failing loudly.
func TestWalk_OversubscribedParksAndWakesCleanly(t *testing.T) {
	root := buildTree(t, []string{"a/x.txt", "b/y.txt", "c/z.txt"})

	pc := PoolConfig{PoolSize: 64, InitialWorkerCap: 4, ResultBuffSize: 4}

	for trial := 0; trial < 20; trial++ {
		w := NewWalkmanWithConfig(false, 0, nil, false, pc)
		if _, err := drain(t, w, root); err != nil {
			t.Fatalf("trial %d: Wait() = %v, want nil", trial, err)
		}
	}
}

func TestNewWalkman_DefaultsToGOMAXPROCS(t *testing.T) {
	root := t.TempDir()
	w := NewWalkman(false, 0, nil)
	if _, err := drain(t, w, root); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}
	// Not directly observable from the exported API, so this just
	// documents/protects the constructor's intended default via
	// defaultPoolConfig() rather than reaching into unexported state.
	if got := defaultPoolConfig().PoolSize; got != runtime.GOMAXPROCS(0) {
		t.Fatalf("defaultPoolConfig().PoolSize = %d, want GOMAXPROCS = %d", got, runtime.GOMAXPROCS(0))
	}
}

// itoa avoids pulling in strconv just for tiny loop-index formatting in
// synthetic tree specs above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
