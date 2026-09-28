package packet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lsTree(t *testing.T, git func(...string) string, tree string) string {
	return git("ls-tree", "-r", "--name-only", tree)
}

func TestResolveUncommittedSnapshot(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "keep.txt", "a\n")
	write(t, dir, "tracked.log", "t\n")
	write(t, dir, ".gitignore", "*.log\n")
	git("add", "-f", ".")
	git("commit", "-qm", "base")
	head := git("rev-parse", "HEAD")
	write(t, dir, "keep.txt", "a\nb\n")
	write(t, dir, "tracked.log", "t\nmore\n") // tracked but ignored: still captured
	write(t, dir, "new.txt", "n\n")           // untracked: captured
	write(t, dir, "junk.log", "j\n")          // untracked and ignored: not captured
	before := git("status", "--porcelain")
	r, err := Resolve(ctx, filepath.Join(dir), "uncommitted")
	if err != nil {
		t.Fatal(err)
	}
	if r.BaseKind != "commit" || r.BaseCommit != head || r.Root != dir {
		t.Fatalf("resolved: %+v", r)
	}
	files := lsTree(t, git, r.TargetTree)
	if !strings.Contains(files, "new.txt") || strings.Contains(files, "junk.log") || !strings.Contains(files, "tracked.log") {
		t.Fatalf("snapshot files:\n%s", files)
	}
	if got := git("show", r.TargetTree+":tracked.log"); got != "t\nmore" {
		t.Fatalf("tracked ignored file content: %q", got)
	}
	if after := git("status", "--porcelain"); after != before {
		t.Fatalf("snapshot touched the real index:\n%s\nvs\n%s", before, after)
	}
}

func TestResolveUnbornHeadAndSubdirectory(t *testing.T) {
	dir, _ := testRepo(t)
	write(t, dir, "sub/a.txt", "a\n")
	r, err := Resolve(context.Background(), filepath.Join(dir, "sub"), "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Scope != "uncommitted" || r.BaseKind != "empty" || r.Root != dir {
		t.Fatalf("unborn: %+v", r)
	}
}

func TestResolveCommitRangeBranchAndNone(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "a.txt", "1\n")
	git("add", ".")
	git("commit", "-qm", "root")
	root := git("rev-parse", "HEAD")
	write(t, dir, "a.txt", "2\n")
	git("commit", "-qam", "two")
	two := git("rev-parse", "HEAD")
	git("checkout", "-qb", "feature")
	write(t, dir, "b.txt", "b\n")
	git("add", ".")
	git("commit", "-qm", "feature")

	if r, err := Resolve(ctx, dir, "commit:"+root); err != nil || r.BaseKind != "empty" {
		t.Fatalf("root commit: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, dir, "commit:"+two); err != nil || r.BaseCommit != root || r.TargetTree != git("rev-parse", two+"^{tree}") {
		t.Fatalf("commit: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, dir, "range:"+root+".."+two); err != nil || r.BaseCommit != root {
		t.Fatalf("range: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, dir, "branch"); err != nil || r.BaseCommit != two {
		t.Fatalf("branch (merge-base with main): %+v %v", r, err)
	}
	git("checkout", "-q", "main")
	git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	if r, err := Resolve(ctx, dir, "commit:HEAD"); err != nil || r.BaseCommit != two || len(r.Notes) == 0 {
		t.Fatalf("merge commit: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, t.TempDir(), ""); err != nil || r.Scope != "none" || r.BaseKind != "none" {
		t.Fatalf("non-git room: %+v %v", r, err)
	}
	for _, bad := range []string{"commit:nope", "range:a", "sideways", "branch:x"} {
		if _, err := Resolve(ctx, dir, bad); err == nil {
			t.Errorf("scope %q accepted", bad)
		}
	}
}

func TestResolveRefusesUnmergedAndSparse(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "a.txt", "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "other")
	write(t, dir, "a.txt", "other\n")
	git("commit", "-qam", "other")
	git("checkout", "-q", "main")
	write(t, dir, "a.txt", "main\n")
	git("commit", "-qam", "main")
	runQuiet(dir, "merge", "other") // conflicts: exits non-zero
	if _, err := Resolve(ctx, dir, "uncommitted"); err == nil || !strings.Contains(err.Error(), "unmerged") {
		t.Fatalf("unmerged index: %v", err)
	}
	runQuiet(dir, "merge", "--abort")
	git("config", "core.sparseCheckout", "true")
	if _, err := Resolve(ctx, dir, "uncommitted"); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("sparse checkout: %v", err)
	}
}
