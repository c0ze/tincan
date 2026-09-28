package packet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Resolved struct {
	Root       string
	Scope      string
	BaseKind   string // commit | empty | none
	BaseCommit string
	BaseTree   string
	TargetTree string
	Notes      []string
}

// EmptyTree returns the empty tree's ID in the repository's object format.
func EmptyTree(ctx context.Context, g Git) (string, error) {
	out, err := g.Run(ctx, []byte{}, "mktree")
	return strings.TrimSpace(string(out)), err
}

// Resolve maps a scope to its base and target trees (committees §6.5). ""
// means "uncommitted" in a Git room and "none" elsewhere.
func Resolve(ctx context.Context, dir, scope string) (Resolved, error) {
	root, err := Git{Dir: dir}.Out(ctx, "rev-parse", "--show-toplevel")
	inGit := err == nil
	if scope == "" {
		scope = "none"
		if inGit {
			scope = "uncommitted"
		}
	}
	if scope == "none" {
		return Resolved{Scope: "none", BaseKind: "none"}, nil
	}
	if !inGit {
		return Resolved{}, fmt.Errorf("scope %s needs a Git repository", scope)
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	g := Git{Dir: root}
	r := Resolved{Root: root, Scope: scope}
	empty, err := EmptyTree(ctx, g)
	if err != nil {
		return r, err
	}
	commit := func(rev string) (string, error) {
		return g.Out(ctx, "rev-parse", "--verify", "-q", rev+"^{commit}")
	}
	setBase := func(c string) error {
		if c == "" {
			r.BaseKind, r.BaseTree = "empty", empty
			return nil
		}
		r.BaseKind, r.BaseCommit = "commit", c
		t, err := g.Out(ctx, "rev-parse", c+"^{tree}")
		r.BaseTree = t
		return err
	}
	switch {
	case scope == "uncommitted":
		head, _ := commit("HEAD")
		if err := setBase(head); err != nil {
			return r, err
		}
		r.TargetTree, err = snapshot(ctx, g, r.BaseTree)
		return r, err
	case scope == "branch":
		head, err := commit("HEAD")
		if err != nil {
			return r, errors.New("scope branch needs a commit on HEAD")
		}
		def := ""
		if ref, err := g.Out(ctx, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
			def = ref
		} else if _, err := commit("refs/heads/main"); err == nil {
			def = "refs/heads/main"
		} else if _, err := commit("refs/heads/master"); err == nil {
			def = "refs/heads/master"
		} else {
			return r, errors.New("scope branch: no origin/HEAD, main or master to compare with")
		}
		mb, err := g.Out(ctx, "merge-base", head, def)
		if err != nil {
			return r, fmt.Errorf("scope branch: no merge-base with %s", def)
		}
		if err := setBase(mb); err != nil {
			return r, err
		}
		r.TargetTree, err = snapshot(ctx, g, r.BaseTree)
		return r, err
	case strings.HasPrefix(scope, "commit:"):
		c, err := commit(strings.TrimPrefix(scope, "commit:"))
		if err != nil || c == "" {
			return r, fmt.Errorf("scope %s: no such commit", scope)
		}
		parents, err := g.Out(ctx, "rev-list", "--parents", "-n", "1", c)
		if err != nil {
			return r, err
		}
		fields := strings.Fields(parents)[1:]
		first := ""
		if len(fields) > 0 {
			first = fields[0]
		}
		if len(fields) > 1 {
			r.Notes = append(r.Notes, "merge commit: diffed against its first parent")
		}
		if err := setBase(first); err != nil {
			return r, err
		}
		r.TargetTree, err = g.Out(ctx, "rev-parse", c+"^{tree}")
		return r, err
	case strings.HasPrefix(scope, "range:"):
		a, b, ok := strings.Cut(strings.TrimPrefix(scope, "range:"), "..")
		if !ok || a == "" || b == "" || strings.HasPrefix(b, ".") {
			return r, fmt.Errorf("scope %s: want range:<a>..<b>", scope)
		}
		ca, err := commit(a)
		if err != nil || ca == "" {
			return r, fmt.Errorf("scope %s: no such commit %s", scope, a)
		}
		cb, err := commit(b)
		if err != nil || cb == "" {
			return r, fmt.Errorf("scope %s: no such commit %s", scope, b)
		}
		if err := setBase(ca); err != nil {
			return r, err
		}
		r.TargetTree, err = g.Out(ctx, "rev-parse", cb+"^{tree}")
		return r, err
	}
	return r, fmt.Errorf("unknown scope %q (none, uncommitted, branch, commit:<rev>, range:<a>..<b>)", scope)
}

// snapshot captures the working tree as a tree object through a private
// index seeded from base, so tracked files keep their tracked status even
// when they match ignore rules. It never touches the real index.
func snapshot(ctx context.Context, g Git, baseTree string) (string, error) {
	if u, err := g.Out(ctx, "ls-files", "-u"); err != nil || u != "" {
		return "", errors.New("the index has unmerged entries; resolve the merge first")
	}
	if sparse, _ := g.Out(ctx, "config", "--bool", "core.sparseCheckout"); sparse == "true" {
		return "", errors.New("sparse checkouts are not supported for working-tree scopes")
	}
	tmp, err := os.MkdirTemp("", "tincan-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ig := Git{Dir: g.Dir, Env: append(append([]string(nil), g.Env...), "GIT_INDEX_FILE="+filepath.Join(tmp, "index"))}
	if _, err := ig.Run(ctx, nil, "read-tree", baseTree); err != nil {
		return "", err
	}
	if _, err := ig.Run(ctx, nil, "add", "-A"); err != nil {
		return "", err
	}
	return ig.Out(ctx, "write-tree")
}
