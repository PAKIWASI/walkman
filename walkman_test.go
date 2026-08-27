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

// countEntries sums direct file/dir entries across every WalkResult's Ret
// (present whenever the directory itself was readable, regardless of any
// per-entry errors alongside it) and every ItemErr across every result,
// the same convention BenchmarkWalk_* uses: a directory's entries are
// counted once, from its own listing, not re-derived from recursing into
// it again.
func countEntries(results []WalkResult) (files, dirs int, errs int) {
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
		t.Fatalf("results[0].Ret = %v, want empty", results[0].Entries)
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
		for _, e := range r.Entries {
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
		for _, e := range r.Entries {
			if e.Name() == "d2" && e.IsDir() {
				found = true
			}
		}
		if !found {
			t.Fatalf("d1's Ret = %v, want it to contain entry d2", r.Entries)
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
	var found *WalkResult
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
	if len(results[0].Errs) != 0 {
		t.Fatalf("root result Err = %v, want empty", results[0].Errs)
	}

	foundLink := false
	for _, e := range results[0].Entries {
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

	var lockedResult *WalkResult
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

// TestWalk_Stats_FollowLinks_ClassifiesByResolvedType locks in the fix for
// a double-counting bug: previously, visitSym bumped a links counter for
// every symlink unconditionally and then *also* bumped dirsCount for any
// symlink that resolved to a directory, counting that one entry twice.
// followLinks now classifies each symlink by what it resolves to (file or
// dir), matching the reference `ignore` crate's behavior of never
// reporting a followed entry as a link — so links stays 0 here, a
// symlink-to-dir counts only under dirs, and a symlink-to-file counts only
// under files.
func TestWalk_Stats_FollowLinks_ClassifiesByResolvedType(t *testing.T) {
	root := buildTree(t, []string{
		"f1.txt",
	})
	skipIfNoSymlinkSupport(t, root)

	// The symlinked directory's target lives outside root's own tree, so
	// it's reachable *only* through the symlink — if it lived inside root
	// too (e.g. a sibling "real_dir/"), it would legitimately get walked
	// twice (once directly, once via the symlink, per walkman's documented
	// per-path-not-per-inode traversal), which would muddy this count.
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
	w := NewWalkmanWithConfig(true, 0, nil, true, pc)
	if _, err := drain(t, w, root); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	files, dirs, links, _, _ := w.Stats()

	// Files: f1.txt + link_to_file (resolves to a file) + the resolved
	// target dir's nested.txt = 3. The dangling symlink resolves to
	// nothing and is skipped without incrementing any counter.
	if files != 3 {
		t.Errorf("stats.files = %d, want 3", files)
	}
	// Dirs: link_to_dir (resolves to a directory) = 1. Before the fix this
	// entry would ALSO have been counted under links, on top of being
	// counted here.
	if dirs != 1 {
		t.Errorf("stats.dirs = %d, want 1", dirs)
	}
	// links stays 0: followLinks classifies every resolvable symlink into
	// files or dirs above, matching `ignore`'s reported behavior.
	if links != 0 {
		t.Errorf("stats.links = %d, want 0 (followLinks classifies by resolved type)", links)
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

	for trial := range 20 {
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
// and at least one drained WalkResult carries an Err mentioning "cycle".
// (A tree can legitimately trip cycle detection more than once - e.g. two
// symlinks pointing at each other get caught independently, once from each
// side - so this only asserts presence, not count.)
func wantCycleErr(t *testing.T, results []WalkResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Wait() = %v, want nil (symlink cycle is a per-entry error)", err)
	}
	if countCycleErrs(results) == 0 {
		t.Fatal("got 0 results with a cycle error, want at least 1")
	}
}

func countCycleErrs(results []WalkResult) int {
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
// dirKey already on the current path.
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

	w := NewWalkman(true, 2, nil) // cap depth so c -> d -> c (via file only, no back-link) can't recurse forever regardless
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
	for skipI := range base {
		for skipJ := range base {
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
		if len(r.Entries) != wantRootEntries {
			t.Fatalf("root Ret has %d entries, want %d (total=%d skipped=%d)",
				len(r.Entries), wantRootEntries, total, len(skipNames))
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
	for i := range total {
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
