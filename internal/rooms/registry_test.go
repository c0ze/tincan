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
