//go:build !windows

package host

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/spool"
)

// A descendant that left the group and kept stdout open must not turn a
// successful run into an exec error once the output wait delay expires.
func TestRunEscapedDescendantHoldingStdoutKeepsSuccess(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	pidfile := filepath.Join(t.TempDir(), "escaped.pid")
	killEscaped(t, pidfile)
	spec := RunSpec{Argv: fakeExec("escape", pidfile), Dir: t.TempDir()}
	res := Run(context.Background(), spec)
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("result: %+v", res)
	}
	if got := ReplyBody(spec, res); got != "ANSWER\n" {
		t.Fatalf("ReplyBody = %q", got)
	}
}

// Stopping a run (cancel, timeout and shutdown share this path) asks the agent
// to stop before killing its group, so it can clean up descendants that left
// the group.
func TestRunStopLetsAgentCleanUpEscapedDescendants(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	pidfile := filepath.Join(t.TempDir(), "escaped.pid")
	killEscaped(t, pidfile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { readPID(t, pidfile); cancel() }()
	start := time.Now()
	res := Run(ctx, RunSpec{Argv: fakeExec("trap", pidfile), Dir: t.TempDir()})
	if !res.Killed {
		t.Fatalf("result: %+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("stop took %s", time.Since(start))
	}
	pid := readPID(t, pidfile)
	if !waitFor(func() bool { return !spool.ProcessAlive(pid) }, 3*time.Second) {
		t.Fatalf("escaped descendant %d outlived the stopped agent", pid)
	}
}
