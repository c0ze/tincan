package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func evalDir(t *testing.T, dir string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInferRoomClimbsToGitWorkTree(t *testing.T) {
	repo := evalDir(t, t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "cmd", "tool")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	got, err := inferRoom()
	if err != nil {
		t.Fatal(err)
	}
	if evalDir(t, got) != repo {
		t.Fatalf("inferRoom = %q, want %q", got, repo)
	}
}

func TestInferRoomUsesDirectoryOutsideRepo(t *testing.T) {
	dir := evalDir(t, t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(dir)
	got, err := inferRoom()
	if err != nil {
		t.Fatal(err)
	}
	if evalDir(t, got) != dir {
		t.Fatalf("inferRoom = %q, want %q", got, dir)
	}
}

func TestInferRoomRefusesHomeAndAncestors(t *testing.T) {
	root := evalDir(t, t.TempDir())
	home := filepath.Join(root, "home", "user")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, dir := range []string{home, filepath.Dir(home)} {
		t.Chdir(dir)
		if got, err := inferRoom(); err == nil || !strings.Contains(err.Error(), "--room") {
			t.Fatalf("inferRoom in %s = %q, %v; want an error naming --room", dir, got, err)
		}
	}
}

func TestInferRoomDoesNotClimbIntoHomeDotfilesRepo(t *testing.T) {
	home := evalDir(t, t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.Mkdir(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, "scratch")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	got, err := inferRoom()
	if err != nil {
		t.Fatal(err)
	}
	if evalDir(t, got) != project {
		t.Fatalf("inferRoom = %q, want %q (not the home dotfiles repo)", got, project)
	}
}

func TestInferRoomComparesCanonicalPaths(t *testing.T) {
	real := evalDir(t, t.TempDir())
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Home spelled through the alias, cwd through the real path.
	t.Setenv("HOME", alias)
	t.Setenv("USERPROFILE", alias)
	t.Chdir(real)
	if got, err := inferRoom(); err == nil {
		t.Fatalf("inferRoom accepted the home directory via its alias: %q", got)
	}
}

func TestInferRoomRefusesFilesystemRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	root, err := filepath.Abs(string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if got, err := inferRoom(); err == nil {
		t.Fatalf("inferRoom accepted filesystem root: %q", got)
	}
}
