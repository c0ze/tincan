package reviewjob

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture: a requester repo with a change, and a member-side clone
// registered as a room.
func checkoutFixture(t *testing.T) (*packet.Packet, *rooms.Registry, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	req := stateDir(t)
	gitIn(t, req, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(req, "a.txt"), []byte("a\n"), 0o644)
	gitIn(t, req, "add", ".")
	gitIn(t, req, "commit", "-qm", "base")
	gitIn(t, req, "remote", "add", "origin", "git@github.com:c0ze/fixture.git")
	member := filepath.Join(stateDir(t), "member")
	gitIn(t, filepath.Dir(member), "clone", "-q", req, member)
	gitIn(t, member, "remote", "set-url", "origin", "https://github.com/c0ze/fixture")
	os.WriteFile(filepath.Join(req, "a.txt"), []byte("a\nb\n"), 0o644)
	os.WriteFile(filepath.Join(req, "new.txt"), []byte("n\n"), 0o644)
	p, err := packet.Build(context.Background(), req, "uncommitted", "q")
	if err != nil {
		t.Fatal(err)
	}
	reg := rooms.Open(filepath.Join(stateDir(t), "rooms.json"))
	if _, err := reg.Add(member); err != nil {
		t.Fatal(err)
	}
	return p, reg, member
}

func TestCheckoutReproducesIncludedTree(t *testing.T) {
	p, reg, member := checkoutFixture(t)
	ws := filepath.Join(stateDir(t), "ws")
	note, reason, err := materializeCheckout(context.Background(), ws, p, reg)
	if err != nil || reason != "" {
		t.Fatalf("checkout: %q %v", reason, err)
	}
	if got := gitIn(t, ws, "write-tree"); got != p.Manifest.IncludedTree {
		t.Fatalf("tree %s != %s", got, p.Manifest.IncludedTree)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("clone still borrows the candidate's objects")
	}
	if remotes := gitIn(t, ws, "remote"); remotes != "" {
		t.Fatalf("remotes left: %q", remotes)
	}
	if !strings.Contains(note, "git diff --cached") {
		t.Fatalf("note: %q", note)
	}
	// The clone survives aggressive maintenance in the candidate.
	gitIn(t, member, "gc", "-q", "--prune=now")
	gitIn(t, ws, "fsck", "--no-progress")
}

func TestCheckoutFallsBack(t *testing.T) {
	p, reg, member := checkoutFixture(t)
	ctx := context.Background()
	cases := map[string]func(){
		"missing base": func() { p.Manifest.BaseCommit = strings.Repeat("1", len(p.Manifest.BaseCommit)) },
		"partial clone": func() {
			gitIn(t, member, "config", "remote.origin.promisor", "true")
		},
		"other repo":    func() { p.Manifest.RepoID = "github.com/else/where" },
		"incomplete":    func() { p.Manifest.Complete = false },
		"tree mismatch": func() { p.Manifest.IncludedTree = strings.Repeat("2", len(p.Manifest.IncludedTree)) },
	}
	for name, mutate := range cases {
		orig := *p
		origBase := p.Manifest
		mutate()
		ws := filepath.Join(stateDir(t), "ws")
		_, reason, err := materializeCheckout(ctx, ws, p, reg)
		if err != nil || reason == "" {
			t.Errorf("%s: reason=%q err=%v", name, reason, err)
		}
		if _, err := os.Stat(ws); err == nil {
			t.Errorf("%s: workspace left behind", name)
		}
		*p = orig
		p.Manifest = origBase
		unset := exec.Command("git", "config", "--unset-all", "remote.origin.promisor")
		unset.Dir = member
		unset.Run() // absent key exits non-zero; best effort
	}
}

// hostilePacket turns the fixture's packet into one a compromised peer could
// send: its manifest lists only the given paths, and its included_tree is
// whatever the patch produces on the base, so the tree check alone cannot
// catch it.
func hostilePacket(t *testing.T, p *packet.Packet, member, patch string, listed ...packet.Change) *packet.Packet {
	t.Helper()
	q := *p
	q.Patch = []byte(patch)
	q.Files = nil
	q.Manifest.Changes = listed
	q.Manifest.IncludedTree = treeAfter(t, member, q.Manifest.BaseCommit, q.Patch)
	return &q
}

// treeAfter is the tree patch yields on base, computed in a scratch index.
func treeAfter(t *testing.T, repo, base string, patch []byte) string {
	t.Helper()
	idx := filepath.Join(stateDir(t), "index")
	pf := filepath.Join(stateDir(t), "p.patch")
	os.WriteFile(pf, patch, 0o600)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(cmd.Environ(), "GIT_INDEX_FILE="+idx)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("read-tree", base)
	run("apply", "--cached", pf)
	return run("write-tree")
}

// A compromised peer cannot stage a symlink or touch a path the manifest
// does not list (a rename source), even when included_tree matches
// (committees §6.6: symlink and gitlink modes are never applied; both
// rename endpoints obey the path policy).
func TestCheckoutRejectsUnlistedPathsAndSymlinks(t *testing.T) {
	p, reg, member := checkoutFixture(t)
	symlink := "diff --git a/link b/link\nnew file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+/etc/hosts\n\\ No newline at end of file\n"
	rename := "diff --git a/a.txt b/moved.txt\nsimilarity index 100%\nrename from a.txt\nrename to moved.txt\n"
	add := func(path string) packet.Change { return packet.Change{Status: "A", Path: path, Included: true} }
	cases := map[string]struct {
		patch  string
		listed []packet.Change
	}{
		"symlink":         {symlink, []packet.Change{add("link")}},
		"rename source":   {rename, []packet.Change{add("moved.txt")}},
		"rename and link": {rename + symlink, []packet.Change{add("moved.txt"), add("link")}},
	}
	for name, tc := range cases {
		q := hostilePacket(t, p, member, tc.patch, tc.listed...)
		ws := filepath.Join(stateDir(t), "ws")
		_, reason, err := materializeCheckout(context.Background(), ws, q, reg)
		if err != nil || reason == "" {
			t.Errorf("%s: accepted (reason=%q err=%v)", name, reason, err)
		}
		if _, err := os.Stat(ws); err == nil {
			t.Errorf("%s: workspace left behind", name)
		}
	}
	// A rename whose both endpoints are listed is fine.
	q := hostilePacket(t, p, member, rename, packet.Change{Status: "R", Path: "moved.txt", From: "a.txt", Included: true})
	ws := filepath.Join(stateDir(t), "ws")
	if _, reason, err := materializeCheckout(context.Background(), ws, q, reg); err != nil || reason != "" {
		t.Fatalf("listed rename: %q %v", reason, err)
	}
}

// Tracked symlinks in an honest base are checked out as plain files, and the
// workspace remembers core.symlinks=false, so the reviewer's own git status
// does not report them as type changes (and git restore cannot recreate a
// real link).
func TestCheckoutKeepsSymlinksOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	req := stateDir(t)
	gitIn(t, req, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(req, "a.txt"), []byte("a\n"), 0o644)
	os.Symlink("a.txt", filepath.Join(req, "link"))
	gitIn(t, req, "add", ".")
	gitIn(t, req, "commit", "-qm", "base")
	gitIn(t, req, "remote", "add", "origin", "https://github.com/c0ze/fixture")
	member := filepath.Join(stateDir(t), "member")
	gitIn(t, filepath.Dir(member), "clone", "-q", req, member)
	gitIn(t, member, "remote", "set-url", "origin", "https://github.com/c0ze/fixture")
	os.WriteFile(filepath.Join(req, "a.txt"), []byte("a\nb\n"), 0o644)
	p, err := packet.Build(context.Background(), req, "uncommitted", "q")
	if err != nil {
		t.Fatal(err)
	}
	reg := rooms.Open(filepath.Join(stateDir(t), "rooms.json"))
	reg.Add(member)
	ws := filepath.Join(stateDir(t), "ws")
	if _, reason, err := materializeCheckout(context.Background(), ws, p, reg); err != nil || reason != "" {
		t.Fatalf("checkout: %q %v", reason, err)
	}
	if st := gitIn(t, ws, "status", "--porcelain"); st != "M  a.txt" {
		t.Fatalf("status:\n%s", st)
	}
	gitIn(t, ws, "checkout", "--", "link")
	if info, err := os.Lstat(filepath.Join(ws, "link")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("link: %v %v", info, err)
	}
}

// Workspace git ignores the user's global and system configuration: global
// hooks, attribute files and smudge filters (git-lfs installs one) never run.
func TestCheckoutIgnoresGlobalGitConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hooks")
	}
	p, reg, _ := checkoutFixture(t)
	tmp := stateDir(t)
	marker := filepath.Join(tmp, "ran")
	hooks := filepath.Join(tmp, "hooks")
	os.Mkdir(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch "+marker+".hook\n"), 0o755)
	attrs := filepath.Join(tmp, "attributes")
	os.WriteFile(attrs, []byte("* filter=probe\n"), 0o644)
	smudge := filepath.Join(tmp, "smudge")
	os.WriteFile(smudge, []byte("#!/bin/sh\ntouch "+marker+".filter\ncat\n"), 0o755)
	global := filepath.Join(tmp, "gitconfig")
	os.WriteFile(global, []byte("[core]\n\thooksPath = "+hooks+"\n\tattributesFile = "+attrs+"\n[filter \"probe\"]\n\tsmudge = "+smudge+"\n\trequired = true\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	ws := filepath.Join(stateDir(t), "ws")
	if _, reason, err := materializeCheckout(context.Background(), ws, p, reg); err != nil || reason != "" {
		t.Fatalf("checkout: %q %v", reason, err)
	}
	for _, m := range []string{marker + ".hook", marker + ".filter"} {
		if _, err := os.Stat(m); err == nil {
			t.Errorf("%s: user configuration ran inside the workspace", filepath.Base(m))
		}
	}
}
