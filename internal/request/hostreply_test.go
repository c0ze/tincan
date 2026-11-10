package request_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
)

// Every failure body the hosted runner produces must still be recorded as a
// failure: request matches the runner's exact prefixes, not a bare "ERROR".
func TestFinishRecognisesHostRunnerFailures(t *testing.T) {
	room := t.TempDir()
	cases := []struct {
		res    host.Result
		status string
	}{
		{host.Result{Killed: true}, "interrupted"},
		{host.Result{TimedOut: true, Stderr: []byte("slow")}, "failed"},
		{host.Result{Err: errors.New("exec: \"x\": not found")}, "failed"},
		{host.Result{ExitCode: 2, Stderr: []byte("boom")}, "failed"},
		{host.Result{Stdout: []byte("fine")}, "completed"},
	}
	for i, c := range cases {
		body := host.ReplyBody(host.RunSpec{Timeout: time.Minute}, c.res)
		e := &envelope.Envelope{ID: fmt.Sprintf("runner-%d", i), From: "test", To: "worker", TS: time.Now().UTC(), Body: "task"}
		r, err := request.Finish(room, e, body)
		if err != nil || r.Status != c.status {
			t.Errorf("%q: %s %v, want %s", body, r.Status, err, c.status)
		}
	}
	// A reply-file read failure has its own prefix.
	body := host.ReplyBody(host.RunSpec{OutFile: room + "/missing"}, host.Result{})
	e := &envelope.Envelope{ID: "runner-file", From: "test", To: "worker", TS: time.Now().UTC(), Body: "task"}
	if r, err := request.Finish(room, e, body); err != nil || r.Status != "failed" {
		t.Errorf("%q: %s %v, want failed", body, r.Status, err)
	}
}
