package host

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeProgram creates an executable named name in dir and returns its path.
func fakeProgram(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveExecutableFallsBackFromStaleAbsolutePath(t *testing.T) {
	bin := t.TempDir()
	want := fakeProgram(t, bin, "tincan-fake-agent")
	t.Setenv("PATH", bin)
	stale := filepath.Join(t.TempDir(), "removed", "versions", "node", "v22", "bin", "tincan-fake-agent")
	got, err := ResolveExecutable(stale, "")
	if err != nil {
		t.Fatalf("ResolveExecutable(%q): %v", stale, err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

func TestResolveExecutableSearchesUserBinDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	shims := filepath.Join(home, ".local", "share", "mise", "shims")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	want := fakeProgram(t, shims, "tincan-fake-agent")
	got, err := ResolveExecutable("tincan-fake-agent", "")
	if err != nil {
		t.Fatalf("ResolveExecutable: %v", err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

func TestResolveExecutableMissingReportsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	if _, err := ResolveExecutable("tincan-definitely-missing", ""); err == nil {
		t.Fatal("expected an error for a missing program")
	}
}

func TestResolveExecutableKeepsExistingUnusableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute permission bits are POSIX-only")
	}
	bin := t.TempDir()
	fakeProgram(t, bin, "tincan-fake-agent")
	t.Setenv("PATH", bin)
	wrapper := filepath.Join(t.TempDir(), "tincan-fake-agent")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveExecutable(wrapper, ""); err == nil {
		t.Fatalf("non-executable %s was substituted with %s", wrapper, got)
	}
}

func TestResolveExecutableRelativePathUsesRunDirectory(t *testing.T) {
	room := t.TempDir()
	if err := os.Mkdir(filepath.Join(room, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := fakeProgram(t, filepath.Join(room, "bin"), "tincan-fake-agent")
	t.Setenv("PATH", t.TempDir())
	rel := filepath.Join("bin", "tincan-fake-agent")
	got, err := ResolveExecutable(rel, room)
	if err != nil {
		t.Fatalf("ResolveExecutable(%q, room): %v", rel, err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
	if _, err := ResolveExecutable(rel, t.TempDir()); err == nil {
		t.Fatal("relative path resolved outside its run directory")
	}
}

func TestAugmentPATHAppendsMissingUserDirsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	local := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	first := t.TempDir()
	t.Setenv("PATH", first)
	AugmentPATH()
	AugmentPATH()
	parts := filepath.SplitList(os.Getenv("PATH"))
	if parts[0] != first {
		t.Fatalf("PATH lost its original precedence: %q", parts)
	}
	count := 0
	for _, p := range parts {
		if p == local {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%s appears %d times in PATH %q", local, count, strings.Join(parts, ","))
	}
}
