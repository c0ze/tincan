package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/spool"
)

func TestRunKillsDescendantsAfterDirectChildExits(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	for _, mode := range []string{"fork", "fork-pipes"} {
		t.Run(mode, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "child.pid")
			start := time.Now()
			res := Run(context.Background(), RunSpec{Argv: fakeExec(mode, pidfile), Dir: t.TempDir()})
			if res.Err != nil || res.ExitCode != 0 {
				t.Fatalf("result: %+v", res)
			}
			pid := readPID(t, pidfile)
			if !waitFor(func() bool { return !spool.ProcessAlive(pid) }, 3*time.Second) {
				t.Fatalf("descendant %d survived its parent's exit", pid)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("descendant delayed completion: %s", time.Since(start))
			}
		})
	}
}

func TestRunStreamsAllOutputAndBoundsCapture(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	counts := map[string]int{}
	res := Run(context.Background(), RunSpec{Argv: fakeExec("volume"), Dir: t.TempDir(), Output: func(stream string, p []byte) error { counts[stream] += len(p); return nil }})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("result: %+v", res)
	}
	if len(res.Stdout) != MaxCapturedOutput || len(res.Stderr) != MaxCapturedOutput {
		t.Fatalf("capture sizes: %d %d", len(res.Stdout), len(res.Stderr))
	}
	if !res.StdoutTruncated || !res.StderrTruncated || !strings.HasPrefix(ReplyBody(RunSpec{}, res), "[output truncated;") {
		t.Fatal("output truncation was not reported")
	}
	if counts["stdout"] != 2*MaxCapturedOutput || counts["stderr"] != 2*MaxCapturedOutput {
		t.Fatalf("incomplete streamed output: %v", counts)
	}
}

func TestRunOutputSinkFailureCancelsJob(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	sinkErr := errors.New("durable log unavailable")
	start := time.Now()
	res := Run(context.Background(), RunSpec{Argv: fakeExec("progress"), Dir: t.TempDir(), Output: func(string, []byte) error { return sinkErr }})
	if !errors.Is(res.Err, sinkErr) {
		t.Fatalf("result: %+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("sink failure did not cancel the child")
	}
}

func TestRunCapturesStdoutAndExitZero(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	res := Run(context.Background(), RunSpec{Argv: fakeExec("echo", "hi there"), Dir: t.TempDir()})
	if res.Err != nil || res.ExitCode != 0 || res.TimedOut || res.Killed {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "echo: hi there" {
		t.Fatalf("stdout = %q", got)
	}
	if got := ReplyBody(RunSpec{}, res); got != "echo: hi there\n" {
		t.Fatalf("ReplyBody = %q", got)
	}
}

func TestRunPipesStdinBody(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	body := "line1\nline2"
	res := Run(context.Background(), RunSpec{Argv: fakeExec("stdin"), Dir: t.TempDir(), Stdin: &body})
	if res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "stdin: line1\nline2" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunNonZeroExitBecomesErrorReply(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	spec := RunSpec{Argv: fakeExec("fail", "bad input"), Dir: t.TempDir()}
	res := Run(context.Background(), spec)
	if res.ExitCode != 3 || res.Err != nil {
		t.Fatalf("want exit 3, got %+v", res)
	}
	body := ReplyBody(spec, res)
	if !strings.HasPrefix(body, "ERROR exit=3\nboom: bad input") {
		t.Fatalf("ReplyBody = %q", body)
	}
	if !strings.Contains(body, "partial") {
		t.Fatalf("ReplyBody dropped stdout: %q", body)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	pidfile := filepath.Join(t.TempDir(), "pid")
	spec := RunSpec{Argv: fakeExec("sleep", "30", pidfile), Dir: t.TempDir(), Timeout: time.Second}
	start := time.Now()
	res := Run(context.Background(), spec)
	if !res.TimedOut || res.Killed {
		t.Fatalf("want TimedOut, got %+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	if got := ReplyBody(spec, res); got != "ERROR timeout after 1s" {
		t.Fatalf("ReplyBody = %q", got)
	}
	pid := readPID(t, pidfile)
	if runtime.GOOS != "windows" && !waitFor(func() bool { return !spool.ProcessAlive(pid) }, 3*time.Second) {
		t.Fatalf("agent pid %d still alive after timeout kill", pid)
	}
}

func TestRunContextCancelKillsAndReportsInterrupted(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	pidfile := filepath.Join(t.TempDir(), "pid")
	spec := RunSpec{Argv: fakeExec("sleep", "30", pidfile), Dir: t.TempDir(), Timeout: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		readPID(t, pidfile)
		cancel()
	}()
	res := Run(ctx, spec)
	if !res.Killed || res.TimedOut {
		t.Fatalf("want Killed, got %+v", res)
	}
	if got := ReplyBody(spec, res); !strings.HasPrefix(got, "ERROR interrupted") {
		t.Fatalf("ReplyBody = %q", got)
	}
	pid := readPID(t, pidfile)
	if runtime.GOOS != "windows" && !waitFor(func() bool { return !spool.ProcessAlive(pid) }, 3*time.Second) {
		t.Fatalf("agent pid %d still alive after cancel", pid)
	}
}

func TestRunMissingBinaryIsExecError(t *testing.T) {
	spec := RunSpec{Argv: []string{"tincan-definitely-missing-binary-xyz", "{body}"}, Dir: t.TempDir()}
	res := Run(context.Background(), spec)
	if res.Err == nil || res.ExitCode != -1 {
		t.Fatalf("want start error, got %+v", res)
	}
	body := ReplyBody(spec, res)
	if !strings.HasPrefix(body, "ERROR exec: ") || !strings.Contains(body, "tincan-definitely-missing-binary-xyz") {
		t.Fatalf("ReplyBody = %q", body)
	}
	if strings.HasPrefix(body, "ERROR exec: exec:") {
		t.Fatalf("ReplyBody doubles the exec prefix: %q", body)
	}
}

func TestReplyBodyReadsOutFile(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	out := filepath.Join(canonicalTempDir(t), "reply.md")
	spec := RunSpec{Argv: fakeExec("outfile", out, "the answer"), Dir: t.TempDir(), OutFile: out}
	res := Run(context.Background(), spec)
	if res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	if got := ReplyBody(spec, res); got != "file: the answer" {
		t.Fatalf("ReplyBody = %q (stdout was %q)", got, res.Stdout)
	}
}

func TestReplyBodyMissingOutFileIsAnError(t *testing.T) {
	spec := RunSpec{OutFile: filepath.Join(t.TempDir(), "never-written.md")}
	got := ReplyBody(spec, Result{ExitCode: 0, Stdout: []byte("ignored")})
	if !strings.HasPrefix(got, "ERROR reply file: ") {
		t.Fatalf("ReplyBody = %q", got)
	}
}

func TestRunUsesDirAsWorkingDirectory(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	dir := t.TempDir()
	res := Run(context.Background(), RunSpec{Argv: fakeExec("cwd"), Dir: dir})
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(res.Stdout)))
	if got != want {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
}

func TestTailKeepsLastBytes(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abc", 3, "abc"},
		{"abcdef", 2, "ef"},
		{"", 4, ""},
	}
	for _, c := range cases {
		if got := string(tail([]byte(c.in), c.n)); got != c.want {
			t.Errorf("tail(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	_ = os.Getpid
}
