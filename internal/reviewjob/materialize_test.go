package reviewjob

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/packet"
)

func TestMaterializePacketWritesOnlyInside(t *testing.T) {
	ws := filepath.Join(stateDir(t), "ws")
	p := &packet.Packet{Question: "why?", Patch: []byte("diff --git a/a b/a\n"),
		Manifest: packet.Manifest{Scope: "uncommitted", BaseKind: "commit", Complete: true, Changes: []packet.Change{{Status: "M", Path: "src/a.go", Included: true}}},
		Files:    map[string][]byte{"src/a.go": []byte("package a\n")}}
	note, err := materializePacket(ws, p, "no local clone of github.com/x/y")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"packet/manifest.json", "packet/diff.patch", "packet/question.md", "packet/files/src/a.go"} {
		info, err := os.Lstat(filepath.Join(ws, f))
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s: %v", f, err)
		} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", f, info.Mode().Perm())
		}
	}
	if !strings.Contains(note, "not a checkout") || !strings.Contains(note, "no local clone") || len(note) > 4096 {
		t.Fatalf("note: %q", note)
	}
}

func TestMaterializePacketRejectsHostilePaths(t *testing.T) {
	base := stateDir(t)
	for name, files := range map[string]map[string][]byte{
		"traversal": {"../../etc/x": []byte("x")},
		"git":       {".git/hooks/post-checkout": []byte("x")},
		"collision": {"A.txt": []byte("1"), "a.txt": []byte("2")},
	} {
		ws := filepath.Join(base, name)
		_, err := materializePacket(ws, &packet.Packet{Manifest: packet.Manifest{Scope: "none"}, Files: files}, "")
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(base)); len(entries) > 0 {
		for _, e := range entries {
			if e.Name() == "etc" {
				t.Fatal("wrote outside the workspace")
			}
		}
	}
}

func TestWriteFileExclusiveRefusesSymlinkedDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := stateDir(t)
	outside := stateDir(t)
	os.Symlink(outside, filepath.Join(root, "evil"))
	if err := writeFileExclusive(root, "evil/x.txt", []byte("x")); err == nil {
		t.Fatal("followed a symlinked directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("file written through the symlink")
	}
	os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old"), 0o600)
	if err := writeFileExclusive(root, "exists.txt", []byte("new")); err == nil {
		t.Fatal("overwrote an existing file")
	}
}
