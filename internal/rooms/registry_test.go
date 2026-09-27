package rooms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempRegistry(t *testing.T) *Registry {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), "state", "rooms.json"))
}

func mkroom(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{t.TempDir()}, parts...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTouchIsIdempotentAndStableID(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "proj")
	for i := 0; i < 2; i++ {
		if err := r.Touch(room); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Path != room || list[0].ID != ID(room) || list[0].Name != "proj" {
		t.Fatalf("list = %+v", list)
	}
	if len(ID(room)) != 12 {
		t.Fatalf("ID length %d", len(ID(room)))
	}
}

func TestListDisambiguatesDuplicateBaseNames(t *testing.T) {
	r := tempRegistry(t)
	a := mkroom(t, "one", "app")
	b := mkroom(t, "two", "app")
	r.Touch(a)
	r.Touch(b)
	list, _ := r.List()
	names := []string{list[0].Name, list[1].Name}
	if names[0] == names[1] || !strings.HasSuffix(names[0], "/app") || !strings.HasSuffix(names[1], "/app") {
		t.Fatalf("names not disambiguated: %q", names)
	}
}

func TestAddRejectsHomeRootAndFiles(t *testing.T) {
	r := tempRegistry(t)
	home := mkroom(t, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, err := r.Add(home); err == nil {
		t.Fatal("home accepted")
	}
	if _, err := r.Add(string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root accepted")
	}
	file := filepath.Join(mkroom(t, "x"), "f")
	os.WriteFile(file, nil, 0o600)
	if _, err := r.Add(file); err == nil {
		t.Fatal("file accepted")
	}
}

func TestSetHiddenAndGet(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "proj")
	got, err := r.Add(room)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetHidden(got.ID, true); err != nil {
		t.Fatal(err)
	}
	g, ok, err := r.Get(got.ID)
	if err != nil || !ok || !g.Hidden {
		t.Fatalf("Get = %+v %v %v", g, ok, err)
	}
	if err := r.SetHidden("000000000000", true); err == nil {
		t.Fatal("unknown id accepted")
	}
}

func TestImportFindsTincanDirs(t *testing.T) {
	r := tempRegistry(t)
	root := mkroom(t, "projects")
	for _, p := range []string{"a/.tincan", "games/b/.tincan", "deep/x/y/z/.tincan", "node_modules/c/.tincan"} {
		os.MkdirAll(filepath.Join(root, p), 0o700)
	}
	n, err := r.Import([]string{root}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("imported %d, want 2 (a, games/b)", n)
	}
}

func TestRegistryHandlesSpacesAndUnicode(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "Application Support", "scratch-ü 1")
	if err := r.Touch(room); err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.Get(ID(room))
	if err != nil || !ok || got.Path != room || got.Name != "scratch-ü 1" {
		t.Fatalf("Get = %+v %v %v", got, ok, err)
	}
}

func TestListMarksMissingRooms(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "gone")
	r.Touch(room)
	os.RemoveAll(room)
	list, _ := r.List()
	if len(list) != 1 || !list[0].Missing {
		t.Fatalf("list = %+v", list)
	}
}

func TestTouchIgnoresReviewWorkspaces(t *testing.T) {
	state := mkroom(t, "state")
	t.Setenv("TINCAN_STATE_DIR", state)
	ws := filepath.Join(state, "reviews", "workspaces", "rv-1-0")
	os.MkdirAll(ws, 0o700)
	r := tempRegistry(t)
	if err := r.Touch(ws); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.List(); len(list) != 0 {
		t.Fatalf("review workspace registered: %+v", list)
	}
	if _, err := r.Add(ws); err == nil {
		t.Fatal("Add accepted a review workspace")
	}
	os.MkdirAll(filepath.Join(state, "reviews", "workspaces", "rv-2-0", ".tincan"), 0o700)
	if n, _ := r.Import([]string{state}, 5); n != 0 {
		t.Fatalf("Import registered %d review workspaces", n)
	}
}

func TestEmptyPathRegistryIsNoop(t *testing.T) {
	r := Open("")
	if err := r.Touch(t.TempDir()); err != nil {
		t.Fatalf("Touch without state dir: %v", err)
	}
}

// TestExcludedResolvesStateDirThroughSymlinkedAncestor is a focused,
// whitebox reproduction of the excluded() bug: StateDir() can return a
// non-canonical path (an ancestor is a symlink) that does not exist yet, and
// naively calling Canonical on it then fails and leaves it raw, so comparing
// it against the already-canonical paths Touch/Add/Import produce silently
// fails to exclude a review workspace that really is nested under it. This
// is verified by toggling the fix locally and observing excluded flip from
// false to true for the same inputs; it is included here as the regression
// check since Touch/Add/Import cannot exercise the divergence directly (see
// TestTouchIgnoresReviewWorkspaceUnderSymlinkedStateDir below).
func TestExcludedResolvesStateDirThroughSymlinkedAncestor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Non-canonical and not yet existing: neither "link/state" nor
	// "real/state" has been created.
	t.Setenv("TINCAN_STATE_DIR", filepath.Join(link, "state"))

	// The canonical form a review workspace under that (not yet created)
	// state dir would have.
	ws := filepath.Join(real, "state", "reviews", "workspaces", "rv-1-0")
	if !excluded(ws) {
		t.Fatalf("excluded(%q) = false, want true", ws)
	}
}

// TestTouchIgnoresReviewWorkspaceUnderSymlinkedStateDir is the integration
// version of the scenario above, going through the public Touch API with a
// review workspace that physically exists under a non-canonical
// TINCAN_STATE_DIR reached through a symlinked ancestor.
func TestTouchIgnoresReviewWorkspaceUnderSymlinkedStateDir(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("TINCAN_STATE_DIR", filepath.Join(link, "state")) // non-canonical

	ws := filepath.Join(real, "state", "reviews", "workspaces", "rv-1-0")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}

	r := tempRegistry(t)
	if err := r.Touch(ws); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.List(); len(list) != 0 {
		t.Fatalf("review workspace registered under symlinked state dir: %+v", list)
	}
}

// TestSetHiddenIsNoopWhenRegistryDisabled guards Open("")'s documented
// contract ("every method is then a no-op") for SetHidden specifically:
// before the fix it called update() unconditionally, which acquired a file
// lock at "<cwd>/.lock" and left that file behind.
func TestSetHiddenIsNoopWhenRegistryDisabled(t *testing.T) {
	t.Chdir(t.TempDir())
	r := Open("")
	if err := r.SetHidden("000000000000", true); err != nil {
		t.Fatalf("SetHidden on disabled registry: %v", err)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("SetHidden left files in the working directory: %+v", entries)
	}
}
