package reviewjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

// writeFileExclusive creates root/rel as a new 0600 regular file. Every
// directory on the way is created here or must be a real directory (never a
// symlink), and the file itself must not exist (committees §6.6).
func writeFileExclusive(root, rel string, data []byte) error {
	parts := strings.Split(rel, "/")
	dir := root
	for _, c := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, c)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is not a plain directory", dir)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, parts[len(parts)-1]), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// materializePacket builds a packet-only workspace and returns its note.
func materializePacket(ws string, p *packet.Packet, reason string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", invalid(err.Error())
	}
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return "", err
	}
	manifest, _ := json.MarshalIndent(p.Manifest, "", "  ")
	files := map[string][]byte{"packet/manifest.json": manifest, "packet/question.md": []byte(p.Question)}
	if len(p.Patch) > 0 {
		files["packet/diff.patch"] = p.Patch
	}
	for path, b := range p.Files {
		files["packet/files/"+path] = b
	}
	for rel, b := range files {
		if err := packet.ValidPath(rel); err != nil {
			return "", invalid(err.Error())
		}
		if err := writeFileExclusive(ws, rel, b); err != nil {
			return "", err
		}
	}
	note := "Your working directory holds a packet describing the change; this is not a checkout"
	if reason != "" {
		note += " (" + reason + ")"
	}
	note += ". Read packet/manifest.json (changed paths, omissions), packet/diff.patch (the change, when present) and packet/files/ (post-change contents of changed text files)."
	if !p.Manifest.Complete {
		note += " The packet is partial: the full patch was too large."
	}
	if len(note) > 4096 {
		note = note[:4096]
	}
	return note, nil
}

// hermetic runs git for the workspace without the user's system or global
// configuration, so their hooks, attribute files and filters (git-lfs
// smudge) never run and nothing is fetched (committees §6.6).
func hermetic(dir string) packet.Git {
	return packet.Git{Dir: dir, Env: []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_LFS_SKIP_SMUDGE=1"}}
}

// candidates returns the repository roots of registered rooms whose origin
// has repoID, deduplicated and ordered newest first (committees §6.6).
func candidates(ctx context.Context, reg *rooms.Registry, repoID string) []string {
	if repoID == "" || reg == nil {
		return nil
	}
	list, err := reg.List()
	if err != nil {
		return nil
	}
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].LastUsed.Equal(list[j].LastUsed) {
			return list[i].LastUsed.After(list[j].LastUsed)
		}
		return list[i].Path < list[j].Path
	})
	seen := map[string]bool{}
	var out []string
	for _, r := range list {
		if r.Missing {
			continue
		}
		root, err := packet.Git{Dir: r.Path}.Out(ctx, "rev-parse", "--show-toplevel")
		if err != nil || seen[root] {
			continue
		}
		seen[root] = true
		if packet.RepoIDOf(ctx, root) == repoID {
			out = append(out, root)
		}
	}
	return out
}

func partialClone(ctx context.Context, g packet.Git) bool {
	if v, _ := g.Out(ctx, "config", "--get", "extensions.partialClone"); v != "" {
		return true
	}
	out, _ := g.Out(ctx, "config", "--get-regexp", `^remote\..*\.promisor$`)
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == "true" {
			return true
		}
	}
	return false
}

// materializeCheckout builds a checkout workspace: a dissociated clone of the
// first qualifying candidate at the base, with the patch applied to the index
// and verified against included_tree. Any failure removes ws and returns the
// reason for falling back to packet-only mode.
func materializeCheckout(ctx context.Context, ws string, p *packet.Packet, reg *rooms.Registry) (string, string, error) {
	m := p.Manifest
	switch {
	case !m.Complete:
		return "", "the packet is partial (patch too large)", nil
	case m.BaseKind == "none":
		return "", "the review has no code scope", nil
	case m.RepoID == "":
		return "", "the requester's repository has no origin remote", nil
	}
	if err := p.Validate(); err != nil {
		return "", "", invalid(err.Error())
	}
	var chosen string
	for _, root := range candidates(ctx, reg, m.RepoID) {
		g := packet.Git{Dir: root}
		if partialClone(ctx, g) {
			continue
		}
		if m.BaseKind == "commit" {
			if _, err := g.Run(ctx, nil, "cat-file", "-e", m.BaseCommit+"^{commit}"); err != nil {
				continue
			}
		}
		chosen = root
		break
	}
	if chosen == "" {
		return "", "no local clone of " + m.RepoID + " has the base commit", nil
	}
	fail := func(step string, err error) (string, string, error) {
		os.RemoveAll(ws)
		return "", fmt.Sprintf("checkout failed at %s: %v", step, err), nil
	}
	parent := filepath.Dir(ws)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", "", err
	}
	if _, err := hermetic(parent).Run(ctx, nil, "clone", "-q", "--no-checkout", "--shared", chosen, ws); err != nil {
		return fail("clone", err)
	}
	g := hermetic(ws)
	steps := [][]string{{"repack", "-a", "-d", "-q"}, {"remote", "remove", "origin"}}
	for _, s := range steps {
		if _, err := g.Run(ctx, nil, s...); err != nil {
			return fail(s[0], err)
		}
	}
	if err := os.Remove(filepath.Join(ws, ".git", "objects", "info", "alternates")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("alternates", err)
	}
	if m.BaseKind == "commit" {
		if _, err := g.Run(ctx, nil, "-c", "core.symlinks=false", "checkout", "-q", "--detach", m.BaseCommit); err != nil {
			return fail("checkout", err)
		}
	} else {
		if _, err := g.Run(ctx, nil, "symbolic-ref", "HEAD", "refs/heads/tincan-review"); err != nil {
			return fail("empty base", err)
		}
		if _, err := g.Run(ctx, nil, "read-tree", "--empty"); err != nil {
			return fail("empty base", err)
		}
	}
	if len(p.Patch) > 0 {
		allowed := map[string]bool{}
		for _, c := range m.Changes {
			if c.Included {
				allowed[c.Path] = true
				if c.From != "" {
					allowed[c.From] = true
				}
			}
		}
		patchFile := filepath.Join(parent, filepath.Base(ws)+".patch")
		if err := os.WriteFile(patchFile, p.Patch, 0o600); err != nil {
			return fail("patch", err)
		}
		defer os.Remove(patchFile)
		numstat, err := g.Run(ctx, nil, "apply", "--numstat", "-z", patchFile)
		if err != nil {
			return fail("patch check", err)
		}
		for _, rec := range strings.Split(string(numstat), "\x00") {
			if f := strings.SplitN(rec, "\t", 3); len(f) == 3 && f[2] != "" {
				if err := packet.ValidPath(f[2]); err != nil || !allowed[f[2]] {
					return fail("patch check", fmt.Errorf("patch touches %q, which the manifest does not include", f[2]))
				}
			}
		}
		if _, err := g.Run(ctx, nil, "-c", "core.symlinks=false", "apply", "--index", "--binary", patchFile); err != nil {
			return fail("apply", err)
		}
	}
	tree, err := g.Out(ctx, "write-tree")
	if err != nil {
		return fail("verify", err)
	}
	if tree != m.IncludedTree {
		return fail("verify", fmt.Errorf("tree %s does not match the packet's %s", tree, m.IncludedTree))
	}
	base := m.BaseCommit
	if len(base) > 12 {
		base = base[:12]
	}
	if base == "" {
		base = "an empty base"
	}
	note := fmt.Sprintf("Your working directory is a private, disposable checkout of %s at %s with the change under review staged: `git diff --cached` shows it. Do not modify it.", m.RepoID, base)
	return note, "", nil
}
