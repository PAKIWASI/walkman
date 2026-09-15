package walkman

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
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
func drain(t *testing.T, w *Walkman, root string) ([]DirBatch, error) {
	t.Helper()

	var results []DirBatch
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

// countEntries sums direct file/dir entries across every DirBatch's Entries
// (present whenever the directory itself was readable, regardless of any
// per-entry errors alongside it) and every DirErr across every result,
// the same convention BenchmarkWalk_* uses: a directory's entries are
// counted once, from its own listing, not re-derived from recursing into
// it again.
func countEntries(results []DirBatch) (files, dirs int, errs int) {
	for _, r := range results {
		errs += len(r.Errs)
		for _, e := range r.Entries {
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
// actually visited (i.e. got its own DirBatch for), independent of
// delivery order.
func walkedDirs(results []DirBatch) []string {
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
	return slices.Contains(ss, s)
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
	if len(results[0].Errs) != 0 {
		t.Fatalf("results[0].Err = %v, want empty", results[0].Errs)
	}
	if len(results[0].Entries) != 0 {
		t.Fatalf("results[0].Entries = %v, want empty", results[0].Entries)
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
	// must get exactly one DirBatch.
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
		for _, e := range r.Entries {
			if e.Name() == "skipme" {
				t.Fatalf("skipped dir %q still present in root's Entries", e.Name())
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

	// maxDepth=2: root (depth 1) is walked, and its direct child dirs
	// (depth 2, i.e. "d1") are walked, but "d1"'s subdirectories (depth
	// 3, "d2") are not recursed into - though "d2" still shows up as an
	// entry inside d1's own listing.
	w := NewWalkman(false, 2, nil)
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
		for _, e := range r.Entries {
			if e.Name() == "d2" && e.IsDir() {
				found = true
			}
		}
		if !found {
			t.Fatalf("d1's Entries = %v, want it to contain entry d2", r.Entries)
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

	w := NewWalkmanWithConfig(false, 0, nil, PoolConfig{PoolSize: 2, InitialWorkerCap: 4, ResultBuffSize: 4})
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	for _, r := range results {
		if r.Dir == link {
			t.Fatalf("followLinks=false but walked into symlink %s", link)
		}
	}
}

func TestWalk_FollowLinks_True_Descends(t *testing.T) {
	root := buildTree(t, []string{
		"empty/",
	})
	skipIfNoSymlinkSupport(t, root)

	// The target lives outside root's own tree, so it's reachable *only*
	// by following the symlink — that makes it unambiguous that descending
	// into it required actually resolving and walking the link, rather
	// than being reached some other way.
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "inside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	link := filepath.Join(root, "link_to_target")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	// walkman reports the symlink's own path here, not a canonicalized
	// target path - matching walkdir's own documented contract ("the
	// yielded DirEntry represents the target... while the path corresponds
	// to the link"). It's not resolved via filepath.EvalSymlinks.
	var found *DirBatch
	for i := range results {
		if results[i].Dir == link {
			found = &results[i]
		}
	}
	if found == nil {
		t.Fatalf("followLinks=true but never walked into %s (the symlink's own path); visited %v", link, walkedDirs(results))
	}

	// Path equality alone doesn't prove the right directory was actually
	// read - confirm its contents came through too.
	gotInside := false
	for _, e := range found.Entries {
		if e.Name() == "inside.txt" {
			gotInside = true
		}
	}
	if !gotInside {
		t.Fatalf("walked into %s but didn't read the target's contents (missing inside.txt); entries=%v", link, found.Entries)
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

	if len(results[0].Errs) != 1 {
		t.Fatalf("root result Errs = %v, want exactly 1 (the dangling symlink)", results[0].Errs)
	}
	if got := results[0].Errs[0]; got.Name != "dangling" || !errors.Is(got.Err, ErrDanglingSymlink) {
		t.Fatalf("root result Errs[0] = %+v, want {Name: dangling, Err: ErrDanglingSymlink}", got)
	}

	// A dangling symlink is reported via Errs, not left in Entries.
	for _, e := range results[0].Entries {
		if e.Name() == "dangling" {
			t.Fatalf("dangling symlink found in Entries, want it removed since it's reported in Errs")
		}
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
	if len(results[0].Errs) == 0 {
		t.Fatal("results[0].Err = empty, want a not-exist error")
	}
	if !errors.Is(results[0].Errs[0].Err, fs.ErrNotExist) {
		t.Fatalf("results[0].Err[0].Err = %v, want fs.ErrNotExist", results[0].Errs[0].Err)
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
	if len(results) != 1 || len(results[0].Errs) == 0 {
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

	var lockedResult *DirBatch
	okCount := 0
	for i, r := range results {
		if r.Dir == locked {
			lockedResult = &results[i]
		} else if len(r.Errs) == 0 {
			okCount++
		}
	}

	if lockedResult == nil {
		t.Fatal("never got a result for the locked directory")
	}
	if len(lockedResult.Errs) == 0 {
		t.Fatal("locked directory result Err = empty, want a permission error")
	}
	if !errors.Is(lockedResult.Errs[0].Err, fs.ErrPermission) {
		t.Fatalf("locked directory Err = %v, want fs.ErrPermission", lockedResult.Errs[0].Err)
	}

	// The rest of the tree (root, ok/, also_ok/) should still have been
	// walked normally despite the one failure.
	if okCount < 3 {
		t.Fatalf("only %d successful results alongside the failure, want >= 3 (root, ok/, also_ok/)", okCount)
	}
}

// ---------------------------------------------------------------------
// Consumer counting from results
// ---------------------------------------------------------------------

func TestWalk_ConsumerCounting_Accurate(t *testing.T) {
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
	w := NewWalkmanWithConfig(false, 0, []string{"skipme"}, pc)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	files, dirs, errs := countEntries(results)
	if errs != 0 {
		t.Fatalf("errs = %d, want 0", errs)
	}
	if files != 3 {
		t.Errorf("files = %d, want 3", files)
	}
	if dirs != 2 {
		t.Errorf("dirs = %d, want 2", dirs)
	}
}

func TestWalk_FollowLinks_ConsumerCounts(t *testing.T) {
	root := buildTree(t, []string{
		"f1.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "nested.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := os.Symlink(filepath.Join(root, "f1.txt"), filepath.Join(root, "link_to_file")); err != nil {
		t.Fatalf("Symlink (file): %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link_to_dir")); err != nil {
		t.Fatalf("Symlink (dir): %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "does_not_exist"), filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("Symlink (dangling): %v", err)
	}

	pc := PoolConfig{PoolSize: 4, InitialWorkerCap: 8, ResultBuffSize: 8}
	w := NewWalkmanWithConfig(true, 0, nil, pc)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	files, dirs, errs := countEntries(results)
	// "dangling" is a real symlink but resolves to nothing, so it's
	// reported as a DirErr rather than counted as a file or dir.
	if errs != 1 {
		t.Fatalf("errs = %d, want 1 (the dangling symlink)", errs)
	}
	if files < 2 {
		t.Errorf("files = %d, want >= 2", files)
	}
	if dirs < 1 {
		t.Errorf("dirs = %d, want >= 1", dirs)
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
	for i := range 6 {
		for j := range 6 {
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
		t.Run("", func(t *testing.T) {
			w := NewWalkmanWithConfig(false, 0, nil, pc)
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
	for i := range 4 {
		for j := range 4 {
			spec = append(spec, filepath.Join("d"+itoa(i), "leaf"+itoa(j)+".txt"))
		}
	}
	root := buildTree(t, spec)
	wantFiles, wantDirs := sequentialCounts(t, root, nil)

	const trials = 30
	for trial := range trials {
		pc := PoolConfig{PoolSize: 6, InitialWorkerCap: 4, ResultBuffSize: 4}
		w := NewWalkmanWithConfig(false, 0, nil, pc)

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

	for trial := range 20 {
		w := NewWalkmanWithConfig(false, 0, nil, pc)
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
	if got := DefaultPoolConfig().PoolSize; got != runtime.GOMAXPROCS(0) {
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

// wantCycleErr fails the test unless: Wait() came back nil (a symlink cycle
// is a per-entry error, not a fatal one - it must not abort the walk or show
// up on Wait, same as a permission-denied readDir or a dangling symlink),
// and at least one drained DirBatch carries an Err mentioning "cycle".
// (A tree can legitimately trip cycle detection more than once - e.g. two
// symlinks pointing at each other get caught independently, once from each
// side - so this only asserts presence, not count.)
func wantCycleErr(t *testing.T, results []DirBatch, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Wait() = %v, want nil (symlink cycle is a per-entry error)", err)
	}
	if countCycleErrs(results) == 0 {
		t.Fatal("got 0 results with a cycle error, want at least 1")
	}
}

func countCycleErrs(results []DirBatch) int {
	var n int
	for _, r := range results {
		for _, ie := range r.Errs {
			if strings.Contains(ie.Err.Error(), "cycle") {
				n++
			}
		}
	}
	return n
}

// TestWalk_SymlinkCycle_SelfReference covers a symlink that points at the
// directory it lives in, the tightest possible cycle.
func TestWalk_SymlinkCycle_SelfReference(t *testing.T) {
	root := buildTree(t, []string{
		"sub/",
	})
	skipIfNoSymlinkSupport(t, root)

	sub := filepath.Join(root, "sub")
	link := filepath.Join(sub, "self")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	wantCycleErr(t, results, err)
}

// TestWalk_SymlinkCycle_ThroughAnotherSymlink is the case the original
// ancestors-only-on-symlink-hop design already handled: a symlink points
// back to a directory that was itself reached earlier in this same path by
// following a different symlink.
func TestWalk_SymlinkCycle_ThroughAnotherSymlink(t *testing.T) {
	root := buildTree(t, []string{
		"a/",
		"b/",
	})
	skipIfNoSymlinkSupport(t, root)

	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")

	// a/into_b -> b, b/back_to_a -> a
	if err := os.Symlink(b, filepath.Join(a, "into_b")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(a, filepath.Join(b, "back_to_a")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	wantCycleErr(t, results, err)
}

// TestWalk_SymlinkCycle_ThroughPlainAncestor is the gap the fix closes: a
// symlink points back to an ancestor that was reached by ordinary
// directory descent, never through a symlink. Previously the ancestor
// chain only grew on symlink hops, so this cycle went undetected and the
// walk would have spun forever (or until something else stopped it).
func TestWalk_SymlinkCycle_ThroughPlainAncestor(t *testing.T) {
	root := buildTree(t, []string{
		"sub/deeper/",
	})
	skipIfNoSymlinkSupport(t, root)

	// sub/deeper/back_to_sub -> sub (reached only by plain descent, no
	// symlink involved in getting there)
	sub := filepath.Join(root, "sub")
	deeper := filepath.Join(sub, "deeper")
	link := filepath.Join(deeper, "back_to_sub")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	wantCycleErr(t, results, err)
}

// TestWalk_SymlinkCycle_BackToRoot checks the root-seeding fix: a symlink
// deep in the tree pointing straight back at the walk's own root, which is
// never itself the target of a spawn (so its key has to be seeded before
// the walk starts, not picked up along the way).
func TestWalk_SymlinkCycle_BackToRoot(t *testing.T) {
	root := buildTree(t, []string{
		"sub/deeper/",
	})
	skipIfNoSymlinkSupport(t, root)

	link := filepath.Join(root, "sub", "deeper", "back_to_root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	wantCycleErr(t, results, err)
}

// TestWalk_SymlinkCycle_False_NeverChecked confirms cycle detection is
// entirely opt-in: with followLinks off, the same tree that would trigger
// TestWalk_SymlinkCycle_SelfReference above just walks fine (the symlink
// itself is counted and skipped, never followed).
func TestWalk_SymlinkCycle_False_NeverChecked(t *testing.T) {
	root := buildTree(t, []string{
		"sub/",
	})
	skipIfNoSymlinkSupport(t, root)

	sub := filepath.Join(root, "sub")
	if err := os.Symlink(sub, filepath.Join(sub, "self")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(false, 0, nil)
	_, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil (followLinks=false never resolves the cycle-forming symlink)", err)
	}
}

// TestWalk_Symlink_SiblingDirsNoFalseCycle guards against an overly broad
// ancestor comparison: two unrelated directories linked to each other
// side-by-side (not nested) should walk fine, not be flagged as a cycle.
// c/link_to_d -> d and d/link_to_c -> c, but c and d are siblings, not one
// inside the other, so following both links in sequence never revisits a
// (dev, ino) pair already on the current path's ancestor chain.
func TestWalk_Symlink_SiblingDirsNoFalseCycle(t *testing.T) {
	root := buildTree(t, []string{
		"c/",
		"d/",
		"c/file.txt",
		"d/file.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	c := filepath.Join(root, "c")
	d := filepath.Join(root, "d")
	if err := os.Symlink(d, filepath.Join(c, "link_to_d")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 3, nil) // cap depth so c -> d -> c (via file only, no back-link) can't recurse forever regardless
	_, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil for a non-cyclic cross-link", err)
	}
}

// TestWalk_SymlinkToFile_NotTreatedAsCycleCandidate ensures a symlink to a
// regular file (not a directory) is just counted as a link/file and never
// enters the cycle-checking path at all.
func TestWalk_SymlinkToFile_NotTreatedAsCycleCandidate(t *testing.T) {
	root := buildTree(t, []string{
		"target.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	link := filepath.Join(root, "link_to_file")
	if err := os.Symlink(filepath.Join(root, "target.txt"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (just root, symlink-to-file never spawns a child item)", len(results))
	}
}

// TestWalk_SymlinkCycle_ErrorIsFirstAndOnly checks that this single
// self-referencing symlink - only one cycle-forming context in the whole
// tree - is reported exactly once, not duplicated, and (since it's a
// per-entry error, not fatal) doesn't trip the pool's "first error wins,
// then shuts down" contract or show up on Wait at all.
func TestWalk_SymlinkCycle_ErrorIsFirstAndOnly(t *testing.T) {
	root := buildTree(t, []string{
		"sub/",
	})
	skipIfNoSymlinkSupport(t, root)

	sub := filepath.Join(root, "sub")
	if err := os.Symlink(sub, filepath.Join(sub, "self")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil (symlink cycle is a per-entry error)", err)
	}
	if n := countCycleErrs(results); n != 1 {
		t.Fatalf("got %d results with a cycle error, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------
// Shared assertion/helper used by the readDirRaw and walk-level skip tests
// ---------------------------------------------------------------------

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

// skipSetOf builds the skip set shape readDirRaw takes: a plain
// name-keyed membership set, matched against each entry's basename at
// every depth (a match prunes the whole subtree for directories).
func skipSetOf(names ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(names))
	for _, n := range names {
		s[n] = struct{}{}
	}
	return s
}

// ---------------------------------------------------------------------
// Integration-level regression: large mixed skip/keep fanout.
//
// A real directory read can't force a specific getdents64 order the way a
// direct readDirRaw test can, but with enough entries and a good fraction
// skipped, a skip-filtering regression shows up often enough across
// sub-tests that it fails reliably rather than needing exact ordering
// control.
// ---------------------------------------------------------------------

func TestWalk_SkipList_LargeMixedFanout(t *testing.T) {
	const total = 400
	skipNames := make(map[string]struct{})
	var spec []string
	for i := range total {
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
		for _, e := range r.Entries {
			if _, skipped := skipNames[e.Name()]; skipped {
				t.Fatalf("skipped entry %q still present in Entries for %s", e.Name(), r.Dir)
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
		if len(r.Entries) != wantRootEntries {
			t.Fatalf("root Entries has %d entries, want %d (total=%d skipped=%d)",
				len(r.Entries), wantRootEntries, total, len(skipNames))
		}
	}
}

// TestWalk_SkipList_AgreesWithSequentialCount cross-checks Walkman results
// against sequentialCounts (which uses filepath.WalkDir's SkipDir) on a large fanout tree.
func TestWalk_SkipList_AgreesWithSequentialCount(t *testing.T) {
	const total = 200
	var spec []string
	var skipList []string
	for i := range total {
		n := "d" + itoa(i)
		spec = append(spec, n+"/", filepath.Join(n, "f.txt"))
		if i%2 == 0 {
			skipList = append(skipList, n)
		}
	}
	root := buildTree(t, spec)

	wantFiles, wantDirs := sequentialCounts(t, root, skipList)

	w := NewWalkmanWithConfig(false, 0, skipList, DefaultPoolConfig())
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
}

// ---------------------------------------------------------------------
// Edge Cases & Additional Coverage
// ---------------------------------------------------------------------

func TestWalk_MaxDepth_One(t *testing.T) {
	root := buildTree(t, []string{
		"root_file.txt",
		"sub1/child1.txt",
		"sub2/child2.txt",
		"sub2/sub3/child3.txt",
	})

	// maxDepth=1: only the root itself is visited; its direct children (root_file.txt, sub1, sub2)
	// are in Entries, but neither sub1 nor sub2 is descended into.
	w := NewWalkman(false, 1, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	visited := walkedDirs(results)
	if len(visited) != 1 || visited[0] != root {
		t.Fatalf("visited %v, want exactly [%q]", visited, root)
	}

	files, dirs, errs := countEntries(results)
	if errs != 0 || files != 1 || dirs != 2 {
		t.Fatalf("files=%d dirs=%d errs=%d, want files=1 dirs=2 errs=0", files, dirs, errs)
	}
}

func TestWalk_MaxDepth_WithFollowLinks(t *testing.T) {
	root := buildTree(t, []string{
		"real_dir/",
		"real_dir/file.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "outside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	link := filepath.Join(root, "sym_to_target")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// maxDepth=1 with followLinks=true: root is visited, symlink target is inspected,
	// but because depth=1 is max, neither real_dir nor sym_to_target is descended into.
	w := NewWalkman(true, 1, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	visited := walkedDirs(results)
	if len(visited) != 1 || visited[0] != root {
		t.Fatalf("visited %v, want exactly [%q]", visited, root)
	}
}

func TestWalk_SkipList_FiltersRegularFiles(t *testing.T) {
	root := buildTree(t, []string{
		"keep.txt",
		"ignore.log",
		"temp.tmp",
		"sub/keep2.txt",
		"sub/ignore.log",
	})

	w := NewWalkman(false, 0, []string{"ignore.log", "temp.tmp"})
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	for _, r := range results {
		for _, e := range r.Entries {
			if e.Name() == "ignore.log" || e.Name() == "temp.tmp" {
				t.Fatalf("skipped file %q found in Entries for %s", e.Name(), r.Dir)
			}
		}
	}

	files, dirs, errs := countEntries(results)
	if errs != 0 || files != 2 || dirs != 1 {
		t.Fatalf("files=%d dirs=%d errs=%d, want files=2 dirs=1 errs=0", files, dirs, errs)
	}
}

func TestWalk_MultipleBrokenSymlinks_AllReported(t *testing.T) {
	root := buildTree(t, []string{"ok.txt"})
	skipIfNoSymlinkSupport(t, root)

	link1 := filepath.Join(root, "broken1")
	link2 := filepath.Join(root, "broken2")
	link3 := filepath.Join(root, "broken3")

	if err := os.Symlink(filepath.Join(root, "missing1"), link1); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "missing2"), link2); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "missing3"), link3); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	if len(results[0].Errs) != 3 {
		t.Fatalf("got %d Errs, want 3", len(results[0].Errs))
	}

	for _, de := range results[0].Errs {
		if !errors.Is(de.Err, ErrDanglingSymlink) {
			t.Errorf("Err = %v, want ErrDanglingSymlink for %s", de.Err, de.Name)
		}
	}
}

func TestWalk_ChainedSymlinks(t *testing.T) {
	root := buildTree(t, nil)
	skipIfNoSymlinkSupport(t, root)

	targetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(targetDir, "final.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// link1 -> link2 -> targetDir
	link2 := filepath.Join(root, "link2")
	link1 := filepath.Join(root, "link1")
	if err := os.Symlink(targetDir, link2); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(link2, link1); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// broken_link1 -> broken_link2 -> nonexistent
	broken2 := filepath.Join(root, "broken2")
	broken1 := filepath.Join(root, "broken1")
	if err := os.Symlink(filepath.Join(root, "does_not_exist"), broken2); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(broken2, broken1); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	w := NewWalkman(true, 0, nil)
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	// Verify broken chain is reported in Errs
	foundBroken1, foundBroken2 := false, false
	for _, r := range results {
		for _, de := range r.Errs {
			if de.Name == "broken1" && errors.Is(de.Err, ErrDanglingSymlink) {
				foundBroken1 = true
			}
			if de.Name == "broken2" && errors.Is(de.Err, ErrDanglingSymlink) {
				foundBroken2 = true
			}
		}
	}
	if !foundBroken1 || !foundBroken2 {
		t.Errorf("broken symlinks not properly reported: broken1=%v broken2=%v", foundBroken1, foundBroken2)
	}
}

func TestWalk_LargeFlatDirectory(t *testing.T) {
	root := t.TempDir()
	const count = 3000

	for i := range count {
		f := filepath.Join(root, "file"+itoa(i)+".txt")
		if err := os.WriteFile(f, []byte("a"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	w := NewWalkmanWithConfig(false, 0, nil, PoolConfig{PoolSize: 4, InitialWorkerCap: 32, ResultBuffSize: 16})
	results, err := drain(t, w, root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	files, dirs, errs := countEntries(results)
	if errs != 0 || dirs != 0 || files != count {
		t.Fatalf("files=%d dirs=%d errs=%d, want files=%d dirs=0 errs=0", files, dirs, errs, count)
	}
}


// ---------------------------------------------------------------------
// followLinks mode: fast path, drop isolation, entry semantics
// ---------------------------------------------------------------------

// TestWalk_FollowLinks_NoSymlinks_MatchesPlainWalk pins the one thing the
// followLinks fast path can silently break: a tree with no symlinks at all has
// to produce exactly the same walk as followLinks=false. That path skips the
// symlink resolution pass for every directory (sawLink is false throughout), so
// a bug in it - a lost entry, a spurious write-back, a missing spawn - would
// show up as a visit-set or count mismatch here and nowhere else.
func TestWalk_FollowLinks_NoSymlinks_MatchesPlainWalk(t *testing.T) {
	root := buildTree(t, []string{
		"root.txt",
		"a/",
		"a/a1.txt",
		"a/suba/",
		"a/suba/deep.txt",
		"b/",
		"b/b1.txt",
		"c/", // empty dir, so the empty-batch path is covered in this mode too
	})

	plain, err := drain(t, NewWalkman(false, 0, nil), root)
	if err != nil {
		t.Fatalf("followLinks=false: Wait() = %v, want nil", err)
	}
	followed, err := drain(t, NewWalkman(true, 0, nil), root)
	if err != nil {
		t.Fatalf("followLinks=true: Wait() = %v, want nil", err)
	}

	plainDirs, followedDirs := walkedDirs(plain), walkedDirs(followed)
	if len(plainDirs) != len(followedDirs) {
		t.Fatalf("visited %d dirs with followLinks=false, %d with true\nplain:    %v\nfollowed: %v",
			len(plainDirs), len(followedDirs), plainDirs, followedDirs)
	}
	for i := range plainDirs {
		if plainDirs[i] != followedDirs[i] {
			t.Fatalf("visited dirs differ:\nplain:    %v\nfollowed: %v", plainDirs, followedDirs)
		}
	}

	pFiles, pDirs, pErrs := countEntries(plain)
	fFiles, fDirs, fErrs := countEntries(followed)
	if pFiles != fFiles || pDirs != fDirs || pErrs != fErrs {
		t.Fatalf("counts differ: plain files=%d dirs=%d errs=%d, followLinks files=%d dirs=%d errs=%d",
			pFiles, pDirs, pErrs, fFiles, fDirs, fErrs)
	}
	if fErrs != 0 {
		t.Fatalf("followLinks walk of a link-free tree reported %d errors, want 0", fErrs)
	}
}

// TestWalk_FollowLinks_KeepsGoodEntriesAlongsideErrors is the drop-isolation
// check: a dangling link is reported in Errs and dropped from Entries, but its
// siblings - including the directory that still has to be walked - have to come
// through the same in-place compaction untouched. Asserting the surviving set
// exactly (not "at least these") is what would catch an off-by-one in the
// compaction indices.
func TestWalk_FollowLinks_KeepsGoodEntriesAlongsideErrors(t *testing.T) {
	root := buildTree(t, []string{
		"ok.txt",
		"sub/inner.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	results, err := drain(t, NewWalkman(true, 0, nil), root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	var rootBatch *DirBatch
	for i := range results {
		if results[i].Dir == root {
			rootBatch = &results[i]
		}
	}
	if rootBatch == nil {
		t.Fatalf("no result for root; visited %v", walkedDirs(results))
	}

	if len(rootBatch.Errs) != 1 {
		t.Fatalf("root Errs = %+v, want exactly the one dangling link", rootBatch.Errs)
	}
	if !errors.Is(rootBatch.Errs[0].Err, ErrDanglingSymlink) {
		t.Errorf("root Errs[0].Err = %v, want ErrDanglingSymlink", rootBatch.Errs[0].Err)
	}

	var got []string
	for _, e := range rootBatch.Entries {
		got = append(got, e.Name())
	}
	assertSameSet(t, got, []string{"ok.txt", "sub"})

	// the surviving directory is still walked, and holds its own entry
	walkedSub := false
	for _, r := range results {
		if r.Dir != filepath.Join(root, "sub") {
			continue
		}
		walkedSub = true
		if len(r.Entries) != 1 || r.Entries[0].Name() != "inner.txt" {
			t.Errorf("sub Entries = %v, want exactly [inner.txt]", r.Entries)
		}
		if len(r.Errs) != 0 {
			t.Errorf("sub Errs = %+v, want none", r.Errs)
		}
	}
	if !walkedSub {
		t.Errorf("directory sub was never walked; visited %v", walkedDirs(results))
	}
}

// TestWalk_FollowLinks_EntriesReportResolvedType: how a followed link looks to
// a consumer. The entry keeps the link own name and path - that is what makes
// it reportable, and walkable, at the link location - while its type describes
// the target: IsDir true (so it is spawnable) for a link to a directory, a
// regular file for a link to a file.
func TestWalk_FollowLinks_EntriesReportResolvedType(t *testing.T) {
	root := buildTree(t, []string{
		"f.txt",
		"sub/inner.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "link_to_dir")); err != nil {
		t.Fatalf("Symlink (dir): %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "f.txt"), filepath.Join(root, "link_to_file")); err != nil {
		t.Fatalf("Symlink (file): %v", err)
	}

	results, err := drain(t, NewWalkman(true, 0, nil), root)
	if err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	var rootBatch *DirBatch
	for i := range results {
		if results[i].Dir == root {
			rootBatch = &results[i]
		}
	}
	if rootBatch == nil {
		t.Fatal("no result for root")
	}

	byName := make(map[string]Entry, len(rootBatch.Entries))
	for _, e := range rootBatch.Entries {
		byName[e.Name()] = e
	}

	toDir, ok := byName["link_to_dir"]
	if !ok {
		t.Fatalf("link_to_dir missing from root Entries: %v", rootBatch.Entries)
	}
	if !toDir.IsDir() {
		t.Error("link_to_dir.IsDir() = false, want true (its target is a directory)")
	}
	if toDir.Type() != fs.ModeDir {
		t.Errorf("link_to_dir.Type() = %v, want %v", toDir.Type(), fs.ModeDir)
	}

	toFile, ok := byName["link_to_file"]
	if !ok {
		t.Fatalf("link_to_file missing from root Entries: %v", rootBatch.Entries)
	}
	if toFile.IsDir() {
		t.Error("link_to_file.IsDir() = true, want false")
	}
	if toFile.Type() != 0 {
		t.Errorf("link_to_file.Type() = %v, want 0 (regular file)", toFile.Type())
	}

	// a followed directory is walked at the link own path, not the target's
	walkedLink := false
	for _, r := range results {
		if r.Dir == filepath.Join(root, "link_to_dir") {
			walkedLink = true
		}
	}
	if !walkedLink {
		t.Errorf("link_to_dir was not walked at its own path; visited %v", walkedDirs(results))
	}
}
