// Package packet captures a change in a room as a size-limited packet for
// committee reviewers (committees spec §6.5), using git plumbing only: a
// private index, tree-to-tree diffs and blobs. It never touches the real
// index or working tree and never contacts a remote.
package packet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Git runs git commands in Dir with a hermetic, non-interactive environment.
type Git struct {
	Dir string
	Env []string
}

func (g Git) command(ctx context.Context, stdin []byte, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, g.Env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd
}

func (g Git) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := g.command(ctx, stdin, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// errEnough stops stream early without being an error to its caller.
var errEnough = errors.New("enough output")

// stream runs git with its stdout handed to read, for output too large to
// buffer. When read returns an error (errEnough: it has what it needs), git
// is killed and reaped and the rest of its output is never produced.
func (g Git) stream(ctx context.Context, stdin []byte, read func(io.Reader) error, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := g.command(ctx, stdin, args)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rerr := read(stdout)
	if rerr != nil {
		cancel()
	} else {
		io.Copy(io.Discard, stdout) // git cannot exit while blocked on a full pipe
	}
	werr := cmd.Wait()
	switch {
	case errors.Is(rerr, errEnough):
		return nil
	case rerr != nil:
		return rerr
	case werr != nil:
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), werr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runLimited is Run for output that may be huge: it keeps at most limit
// bytes, and over reports (with no output) that git had more.
func (g Git) runLimited(ctx context.Context, stdin []byte, limit int, args ...string) (out []byte, over bool, err error) {
	err = g.stream(ctx, stdin, func(r io.Reader) error {
		var b bytes.Buffer // grows by doubling, unlike io.ReadAll
		if _, err := b.ReadFrom(io.LimitReader(r, int64(limit)+1)); err != nil {
			return err
		}
		if b.Len() > limit {
			over = true
			return errEnough
		}
		out = b.Bytes()
		return nil
	}, args...)
	if err != nil || over {
		return nil, over, err
	}
	return out, false, nil
}

func (g Git) Out(ctx context.Context, args ...string) (string, error) {
	out, err := g.Run(ctx, nil, args...)
	return strings.TrimSpace(string(out)), err
}
