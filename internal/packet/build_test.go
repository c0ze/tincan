package packet

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func change(p *Packet, path string) (Change, bool) {
	for _, c := range p.Manifest.Changes {
		if c.Path == path {
			return c, true
		}
	}
	return Change{}, false
}

func TestBuildOmitsAndReproducesIncludedTree(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "keep.txt", "a\n")
	write(t, dir, "old.txt", "rename me\n")
	write(t, dir, "gone.txt", "bye\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("remote", "add", "origin", "https://github.com/c0ze/tincan.git")
	write(t, dir, "keep.txt", "a\nb\n")
	git("mv", "old.txt", "new.txt")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, dir, ".env", "TOKEN=1\n")
	write(t, dir, "certs/server.PEM", "x\n")
	write(t, dir, "big.bin", strings.Repeat("x", MaxFileContent+1))
	write(t, dir, "bin.dat", "a\x00b")
	if runtime.GOOS != "windows" {
		os.Symlink("keep.txt", filepath.Join(dir, "link"))
	}
	p, err := Build(ctx, dir, "uncommitted", "Is this safe?")
	if err != nil {
		t.Fatal(err)
	}
	m := p.Manifest
	if m.RepoID != "github.com/c0ze/tincan" || !m.Complete || m.BaseKind != "commit" || m.IncludedTree == "" {
		t.Fatalf("manifest: %+v", m)
	}
	for path, reason := range map[string]string{".env": "secret", "certs/server.PEM": "secret", "big.bin": "larger than"} {
		if c, ok := change(p, path); !ok || c.Included || !strings.Contains(c.Reason, reason) {
			t.Errorf("%s: %+v", path, c)
		}
	}
	if runtime.GOOS != "windows" {
		if c, _ := change(p, "link"); c.Included || !strings.Contains(c.Reason, "symlink") {
			t.Errorf("symlink: %+v", c)
		}
	}
	if c, _ := change(p, "new.txt"); !c.Included || c.Status != "R" || c.From != "old.txt" {
		t.Errorf("rename: %+v", c)
	}
	if c, _ := change(p, "gone.txt"); !c.Included || c.Status != "D" {
		t.Errorf("deletion: %+v", c)
	}
	if string(p.Files["keep.txt"]) != "a\nb\n" || p.Files["bin.dat"] != nil || p.Files[".env"] != nil {
		t.Errorf("files: %v", keys(p.Files))
	}
	if strings.Contains(string(p.Patch), "TOKEN=1") {
		t.Fatal("secret content in the patch")
	}
	// The patch reproduces included_tree from the base.
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	runIn(t, ws, "clone", "-q", "--no-checkout", dir, "c")
	c := filepath.Join(ws, "c")
	runIn(t, c, "checkout", "-q", "--detach", m.BaseCommit)
	os.WriteFile(filepath.Join(ws, "p.patch"), p.Patch, 0o600)
	runIn(t, c, "-c", "core.symlinks=false", "apply", "--index", "--binary", filepath.Join(ws, "p.patch"))
	if got := runIn(t, c, "write-tree"); got != m.IncludedTree {
		t.Fatalf("applied tree %s != included %s", got, m.IncludedTree)
	}
	if Hash(p) == "" || Hash(p) != Hash(p) {
		t.Fatal("hash")
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("own packet invalid: %v", err)
	}
}

func TestBuildIncompleteWhenPatchTooLarge(t *testing.T) {
	dir, git := testRepo(t)
	write(t, dir, "a.txt", "a\n")
	git("add", ".")
	git("commit", "-qm", "base")
	for i := 0; i < 4; i++ {
		write(t, dir, filepath.Join("gen", string(rune('a'+i))+".txt"), strings.Repeat("line of text\n", 80000)) // ~1 MiB each, 4 MiB patch
	}
	p, err := Build(context.Background(), dir, "uncommitted", "q")
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Complete || p.Patch != nil {
		t.Fatalf("oversized patch kept: complete=%v patch=%d", p.Manifest.Complete, len(p.Patch))
	}
	total := 0
	for _, b := range p.Files {
		total += len(b)
	}
	if total > MaxFilesTotal || len(p.Manifest.FilesOmitted) == 0 {
		t.Fatalf("file budget: total=%d omitted=%v", total, p.Manifest.FilesOmitted)
	}
}

func TestBuildEmptyChangeNoneScopeAndLimits(t *testing.T) {
	dir, git := testRepo(t)
	write(t, dir, "a.txt", "a\n")
	git("add", ".")
	git("commit", "-qm", "base")
	p, err := Build(context.Background(), dir, "uncommitted", "q")
	if err != nil || !p.Manifest.Complete || len(p.Patch) != 0 || len(p.Manifest.Changes) != 0 {
		t.Fatalf("empty change: %+v %v", p, err)
	}
	p, err = Build(context.Background(), t.TempDir(), "", "just a question")
	if err != nil || p.Manifest.Scope != "none" || p.Question != "just a question" {
		t.Fatalf("none scope: %+v %v", p, err)
	}
	if _, err := Build(context.Background(), dir, "", strings.Repeat("q", MaxQuestion+1)); err == nil {
		t.Fatal("oversized question accepted")
	}
}

func TestValidateRejectsHostilePackets(t *testing.T) {
	for name, p := range map[string]Packet{
		"traversal":  {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "A", Path: "../x", Included: true}}}},
		"git dir":    {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "A", Path: ".GIT/config", Included: true}}}},
		"file path":  {Manifest: Manifest{Scope: "none"}, Files: map[string][]byte{"a/../../b": []byte("x")}},
		"collision":  {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "A", Path: "A.txt", Included: true}, {Status: "A", Path: "a.txt", Included: true}}}},
		"rename src": {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "R", Path: "ok.txt", From: ".tincan/x", Included: true}}}},
	} {
		p := p
		if err := p.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func runIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The patch must not depend on the user's diff configuration: noprefix,
// forced colour or an external diff tool would make it unappliable.
func TestBuildPatchIgnoresUserDiffConfig(t *testing.T) {
	dir, git := testRepo(t)
	write(t, dir, "a.txt", "a\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("config", "diff.noprefix", "true")
	git("config", "color.ui", "always")
	git("config", "diff.external", "false")
	git("config", "diff.mnemonicPrefix", "true")
	write(t, dir, "a.txt", "a\nb\n")
	p, err := Build(context.Background(), dir, "uncommitted", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Patch), "\x1b[") || !strings.Contains(string(p.Patch), "diff --git a/a.txt b/a.txt") {
		t.Fatalf("patch shaped by user config:\n%q", p.Patch)
	}
}
