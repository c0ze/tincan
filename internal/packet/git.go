// Package packet captures a change in a room as a size-limited packet for
// committee reviewers (committees spec §6.5), using git plumbing only: a
// private index, tree-to-tree diffs and blobs. It never touches the real
// index or working tree and never contacts a remote.
package packet

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Git runs git commands in Dir with a hermetic, non-interactive environment.
type Git struct {
	Dir string
	Env []string
}

func (g Git) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, g.Env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g Git) Out(ctx context.Context, args ...string) (string, error) {
	out, err := g.Run(ctx, nil, args...)
	return strings.TrimSpace(string(out)), err
}
