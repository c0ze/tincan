package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RunSpec is one rendered agent invocation.
type RunSpec struct {
	Argv    []string      // rendered command line; Argv[0] is looked up on PATH
	Dir     string        // working directory (the room)
	Stdin   *string       // body to pipe to stdin, or nil for no stdin at all
	OutFile string        // the {out} path when the preset replies via file, else ""
	Timeout time.Duration // 0 = unbounded
}

// Result is what one run produced.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int   // -1 when the process never started or was killed
	Err      error // start failure, or the ctx/timeout error
	TimedOut bool  // Timeout elapsed; the process group was killed
	Killed   bool  // the parent ctx was cancelled (serve shutting down)
	Duration time.Duration
}

// Run executes spec with stdout/stderr captured to memory, cwd = spec.Dir and
// no inherited stdin. The agent runs in its own process group (childAttr); a
// Timeout or a cancelled ctx kills that whole group (killGroup), and WaitDelay
// bounds how long a grandchild holding the pipes can delay the return.
func Run(ctx context.Context, spec RunSpec) Result {
	start := time.Now()
	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(runCtx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.SysProcAttr = childAttr()
	if spec.Stdin != nil {
		cmd.Stdin = strings.NewReader(*spec.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	res := Result{ExitCode: -1}
	if err := cmd.Start(); err != nil {
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}
	err := cmd.Wait()
	res.Stdout, res.Stderr = stdout.Bytes(), stderr.Bytes()
	res.Duration = time.Since(start)
	switch {
	case err == nil:
		res.ExitCode = 0
	case ctx.Err() != nil:
		res.Killed = true
		res.Err = ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.Err = runCtx.Err()
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.Err = err
		}
	}
	return res
}

// ReplyBody turns a run into the text sent back on the reply channel (spec
// §7): the asker always gets a reply, and every failure starts with "ERROR".
func ReplyBody(spec RunSpec, res Result) string {
	switch {
	case res.Killed:
		return "ERROR interrupted: hosted listener was stopped while running"
	case res.TimedOut:
		return fmt.Sprintf("ERROR timeout after %ds", int(spec.Timeout/time.Second))
	case res.Err != nil:
		return "ERROR exec: " + strings.TrimPrefix(res.Err.Error(), "exec: ")
	case res.ExitCode != 0:
		var b strings.Builder
		fmt.Fprintf(&b, "ERROR exit=%d\n", res.ExitCode)
		b.Write(bytes.TrimRight(tail(res.Stderr, 4096), " \t\r\n"))
		if out := bytes.TrimSpace(res.Stdout); len(out) > 0 {
			b.WriteString("\n")
			b.Write(out)
		}
		return b.String()
	case spec.OutFile != "":
		data, err := os.ReadFile(spec.OutFile)
		if err != nil {
			return "ERROR reply file: " + err.Error()
		}
		return string(data)
	default:
		return string(res.Stdout)
	}
}

// tail returns the last n bytes of b.
func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
