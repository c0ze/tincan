package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/c0ze/tincan/internal/fsutil"
)

// RunSpec is one rendered agent invocation.
type RunSpec struct {
	Argv    []string      // rendered command line; Argv[0] is looked up on PATH
	Dir     string        // working directory (the room)
	Stdin   *string       // body to pipe to stdin, or nil for no stdin at all
	OutFile string        // the {out} path when the preset replies via file, else ""
	Timeout time.Duration // 0 = unbounded
	// Output receives stdout/stderr chunks as they arrive. Calls are serialized.
	Output         func(stream string, data []byte) error
	MaxOutputBytes int // per stream; zero uses MaxCapturedOutput
}

// MaxCapturedOutput bounds each captured stream and file reply in memory.
const MaxCapturedOutput = 1 << 20

type capture struct {
	data      []byte
	limit     int
	stream    string
	emit      func(string, []byte) error
	mu        *sync.Mutex
	truncated bool
}

func (w *capture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.emit != nil {
		if err := w.emit(w.stream, p); err != nil {
			return 0, err
		}
	}
	if len(w.data)+len(p) > w.limit {
		w.truncated = true
	}
	if len(p) >= w.limit {
		w.data = append(w.data[:0], p[len(p)-w.limit:]...)
	} else {
		if extra := len(w.data) + len(p) - w.limit; extra > 0 {
			copy(w.data, w.data[extra:])
			w.data = w.data[:len(w.data)-extra]
		}
		w.data = append(w.data, p...)
	}
	return len(p), nil
}

// Result is what one run produced.
type Result struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int   // -1 when the process never started or was killed
	Err             error // start failure, or the ctx/timeout error
	TimedOut        bool  // Timeout elapsed; the process group was killed
	Killed          bool  // the parent ctx was cancelled (serve shutting down)
	Duration        time.Duration
	StdoutTruncated bool
	StderrTruncated bool
}

// Run streams stdout/stderr to Output while retaining bounded tails in memory.
// The agent runs in an owned process group/job, which is cleaned up on every
// exit path, including normal direct-child exit with descendants still alive.
func Run(ctx context.Context, spec RunSpec) Result {
	start := time.Now()
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return Result{ExitCode: -1, Err: errors.New("empty agent command")}
	}
	runCtx, stopOutput := context.WithCancel(ctx)
	defer stopOutput()
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, spec.Timeout)
		defer cancel()
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.SysProcAttr = childAttr()
	if spec.Stdin != nil {
		cmd.Stdin = strings.NewReader(*spec.Stdin)
	}
	limit := spec.MaxOutputBytes
	if limit <= 0 || limit > MaxCapturedOutput {
		limit = MaxCapturedOutput
	}
	var outputMu sync.Mutex
	var outputErr error
	emit := func(stream string, data []byte) error {
		if outputErr != nil {
			return outputErr
		}
		if spec.Output != nil {
			outputErr = spec.Output(stream, data)
			if outputErr != nil {
				stopOutput()
			}
		}
		return outputErr
	}
	stdout := capture{limit: limit, stream: "stdout", emit: emit, mu: &outputMu}
	stderr := capture{limit: limit, stream: "stderr", emit: emit, mu: &outputMu}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second

	res := Result{ExitCode: -1}
	err := runProcess(runCtx, cmd)
	res.Stdout, res.Stderr = stdout.data, stderr.data
	res.StdoutTruncated, res.StderrTruncated = stdout.truncated, stderr.truncated
	res.Duration = time.Since(start)
	switch {
	case outputErr != nil:
		res.Err = fmt.Errorf("output sink: %w", outputErr)
	case ctx.Err() != nil:
		res.Killed = true
		res.Err = ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		res.Err = runCtx.Err()
	case err == nil:
		res.ExitCode = 0
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
		data, err := fsutil.ReadFile(spec.OutFile, MaxCapturedOutput)
		if err != nil {
			return "ERROR reply file: " + err.Error()
		}
		return string(data)
	default:
		if res.StdoutTruncated {
			return "[output truncated; recent output is in the host log]\n" + string(res.Stdout)
		}
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
