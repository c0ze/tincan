package packet

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var secretPatterns = []string{".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ed25519*", ".npmrc", ".netrc", ".pypirc", "credentials*", "*secret*"}

func secretPath(p string) bool {
	base := strings.ToLower(path.Base(p))
	for _, pat := range secretPatterns {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// judgedChange is a manifest entry with the raw record it came from.
type judgedChange struct {
	Change
	raw rawChange
}

// rawChange is one diff-tree -z record.
type rawChange struct {
	oldMode, newMode, oldSHA, newSHA, status, src, dst string
}

func parseRaw(out []byte) ([]rawChange, error) {
	fields := bytes.Split(out, []byte{0})
	var list []rawChange
	for i := 0; i+1 < len(fields); {
		meta := strings.Fields(strings.TrimPrefix(string(fields[i]), ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected diff-tree record %q", fields[i])
		}
		c := rawChange{oldMode: meta[0], newMode: meta[1], oldSHA: meta[2], newSHA: meta[3], status: meta[4][:1]}
		if c.status == "R" || c.status == "C" {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("truncated rename record")
			}
			c.src, c.dst = string(fields[i+1]), string(fields[i+2])
			i += 3
		} else {
			c.src, c.dst = string(fields[i+1]), string(fields[i+1])
			i += 2
		}
		list = append(list, c)
	}
	return list, nil
}

// blobSizes returns the sizes of the given blobs via one cat-file --batch-check.
func blobSizes(ctx context.Context, g Git, shas []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(shas) == 0 {
		return out, nil
	}
	res, err := g.Run(ctx, []byte(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objectsize)")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(res)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			out[f[0]] = n
		}
	}
	return out, nil
}

// Build captures scope's change in dir as a packet (committees §6.5).
func Build(ctx context.Context, dir, scope, question string) (*Packet, error) {
	if len(question) > MaxQuestion {
		return nil, fmt.Errorf("question exceeds %d bytes", MaxQuestion)
	}
	r, err := Resolve(ctx, dir, scope)
	if err != nil {
		return nil, err
	}
	p := &Packet{Question: question, Manifest: Manifest{Scope: r.Scope, BaseKind: r.BaseKind, BaseCommit: r.BaseCommit,
		TargetTree: r.TargetTree, Captured: time.Now().UTC(), Complete: true, Notes: r.Notes, Changes: []Change{}}}
	if r.BaseKind == "none" {
		return p, nil
	}
	g := Git{Dir: r.Root}
	p.Manifest.RepoID = RepoIDOf(ctx, r.Root)
	out, err := g.Run(ctx, nil, "diff-tree", "-r", "-z", "--find-renames", r.BaseTree, r.TargetTree)
	if err != nil {
		return nil, err
	}
	raws, err := parseRaw(out)
	if err != nil {
		return nil, err
	}
	if len(raws) > MaxPaths {
		return nil, fmt.Errorf("the change touches %d paths (limit %d); narrow the scope", len(raws), MaxPaths)
	}
	var shas []string
	for _, c := range raws {
		if c.status != "D" {
			shas = append(shas, c.newSHA)
		}
	}
	sizes, err := blobSizes(ctx, g, shas)
	if err != nil {
		return nil, err
	}
	// Judge each change; a rename with a bad endpoint becomes D + A.
	var judged []judgedChange
	judge := func(c Change, raw rawChange) {
		reason := ""
		for _, pth := range []string{c.From, c.Path} {
			if pth == "" {
				continue
			}
			if secretPath(pth) {
				reason = "secret-looking path"
			} else if err := ValidPath(pth); err != nil && reason == "" {
				reason = "path policy: " + err.Error()
			}
		}
		switch {
		case reason != "":
		case raw.newMode == "120000" || raw.oldMode == "120000":
			reason = "symlink (listed, not included)"
		case raw.newMode == "160000" || raw.oldMode == "160000":
			reason = "submodule (listed, not included)"
		case c.Status != "D" && sizes[raw.newSHA] > MaxFileContent:
			reason = fmt.Sprintf("target content larger than %d bytes", MaxFileContent)
		}
		c.Included, c.Reason = reason == "", reason
		judged = append(judged, judgedChange{c, raw})
	}
	for _, raw := range raws {
		c := Change{Status: raw.status, Path: raw.dst, OldMode: raw.oldMode, NewMode: raw.newMode}
		if raw.status == "C" {
			c.Status = "A" // a copy is an addition of the destination
		}
		if raw.status == "R" {
			c.From = raw.src
			if ValidPath(raw.src) != nil || ValidPath(raw.dst) != nil || secretPath(raw.src) != secretPath(raw.dst) {
				del := raw
				del.status, del.newMode, del.newSHA, del.dst = "D", "000000", strings.Repeat("0", len(raw.oldSHA)), raw.src
				judge(Change{Status: "D", Path: raw.src, OldMode: raw.oldMode}, del)
				add := raw
				add.status, add.oldMode, add.src = "A", "000000", raw.dst
				judge(Change{Status: "A", Path: raw.dst, NewMode: raw.newMode}, add)
				continue
			}
		}
		judge(c, raw)
	}
	var includedPaths []string
	for _, j := range judged {
		if j.Included {
			includedPaths = append(includedPaths, j.Path)
		}
	}
	collide := FoldCollisions(includedPaths)
	var info bytes.Buffer
	zero := strings.Repeat("0", len(r.BaseTree))
	for i := range judged {
		j := &judged[i]
		if j.Included {
			if first, ok := collide[j.Path]; ok {
				j.Included, j.Reason = false, "path collides with "+first+" when case is ignored"
			}
		}
		if j.Included {
			if j.Status == "D" || j.Status == "R" {
				src := j.Path
				if j.Status == "R" {
					src = j.From
				}
				fmt.Fprintf(&info, "0 %s\t%s\x00", zero, src)
			}
			if j.Status != "D" {
				fmt.Fprintf(&info, "%s %s\t%s\x00", j.raw.newMode, j.raw.newSHA, j.Path)
			}
		}
		p.Manifest.Changes = append(p.Manifest.Changes, j.Change)
	}
	tmp, err := os.MkdirTemp("", "tincan-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	ig := Git{Dir: r.Root, Env: []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}}
	if _, err := ig.Run(ctx, nil, "read-tree", r.BaseTree); err != nil {
		return nil, err
	}
	if info.Len() > 0 {
		if _, err := ig.Run(ctx, info.Bytes(), "update-index", "-z", "--index-info"); err != nil {
			return nil, err
		}
	}
	if p.Manifest.IncludedTree, err = ig.Out(ctx, "write-tree"); err != nil {
		return nil, err
	}
	// Plumbing with every presentation knob pinned, so user configuration
	// (noprefix, colour, external diff, textconv) cannot reshape the patch.
	patch, err := g.Run(ctx, nil, "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false", "diff-tree", "-p", "-r",
		"--binary", "--full-index", "--find-renames", "--no-color", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/", r.BaseTree, p.Manifest.IncludedTree)
	if err != nil {
		return nil, err
	}
	if len(patch) > MaxPatch {
		p.Manifest.Complete = false
		p.Manifest.Notes = append(p.Manifest.Notes, fmt.Sprintf("patch of %d bytes exceeds %d; post-change file contents only", len(patch), MaxPatch))
	} else if len(patch) > 0 {
		p.Patch = patch
	}
	if err := collectFiles(ctx, g, p, judged); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("packet: %w", err)
	}
	return p, nil
}

// collectFiles adds post-change contents of included text files, in manifest
// order, until the MaxFilesTotal budget is spent.
func collectFiles(ctx context.Context, g Git, p *Packet, judged []judgedChange) error {
	var want []struct{ path, sha string }
	for _, j := range judged {
		if j.Included && j.Status != "D" {
			want = append(want, struct{ path, sha string }{j.Path, j.raw.newSHA})
		}
	}
	if len(want) == 0 {
		return nil
	}
	var in bytes.Buffer
	for _, w := range want {
		in.WriteString(w.sha + "\n")
	}
	out, err := g.Run(ctx, in.Bytes(), "cat-file", "--batch")
	if err != nil {
		return err
	}
	br := bufio.NewReader(bytes.NewReader(out))
	total := 0
	p.Files = map[string][]byte{}
	for _, w := range want {
		header, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return fmt.Errorf("cat-file: unexpected header %q", header)
		}
		n, _ := strconv.Atoi(f[2])
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		br.ReadByte() // trailing newline
		if bytes.IndexByte(body, 0) >= 0 {
			continue // binary: the patch carries it
		}
		if total+n > MaxFilesTotal {
			p.Manifest.FilesOmitted = append(p.Manifest.FilesOmitted, w.path)
			continue
		}
		total += n
		p.Files[w.path] = body
	}
	if len(p.Files) == 0 {
		p.Files = nil
	}
	return nil
}
