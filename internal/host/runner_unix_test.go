//go:build !windows

package host

import (
	"context"
	"path/filepath"
	"testing"
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
