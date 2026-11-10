package request

import (
	"context"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/spool"
)

func TestFinishClassifiesOnlyHostedErrorPrefixes(t *testing.T) {
	room := t.TempDir()
	cases := []struct{ body, status string }{
		{"ERRORS found: none", "completed"},
		{"ERROR handling looks fine", "completed"},
		{"ERROR canceled orders are retried", "completed"},
		{"ERROR interruptedly", "completed"},
		{"ERROR exit=1\nboom", "failed"},
		{"ERROR timeout after 30s", "failed"},
		{"ERROR exec: not found", "failed"},
		{"ERROR session: provider failed", "failed"},
		{"ERROR reply file: missing", "failed"},
		{"ERROR interrupted: hosted listener was stopped while running", "interrupted"},
		{"ERROR canceled: execution was canceled before starting", "canceled"},
	}
	for i, c := range cases {
		e := &envelope.Envelope{ID: "outcome-" + string(rune('a'+i)), From: "test", To: "worker", TS: time.Now().UTC(), Body: "task"}
		r, err := Finish(room, e, c.body)
		if err != nil || r.Status != c.status || r.Result != c.body {
			t.Errorf("%q: %s %q %v, want %s", c.body, r.Status, r.Result, err, c.status)
		}
	}
}

func TestInteractiveReplyIsNotClassifiedByPrefix(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	r, err := SubmitInteractive(ctx, room, "interactive", "test", "task", "typed-reply")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(room)
	e, err := sp.Recv("interactive", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	body := "ERROR exit=1 is what the old script printed; fixed now"
	if err = sp.Send(&envelope.Envelope{From: "interactive", To: e.ReplyTo, Body: body}); err != nil {
		t.Fatal(err)
	}
	finished, err := Wait(ctx, room, r.ID, time.Second)
	if err != nil || finished.Status != "completed" || finished.Result != body {
		t.Fatalf("typed reply: %+v %v", finished, err)
	}
}
