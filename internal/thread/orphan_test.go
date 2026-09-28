// internal/thread/orphan_test.go
package thread_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/thread"
)

// failingLaunch returns a dispatcher whose detached hosts exit immediately,
// so dispatch.Send saves and enqueues the request but Launch fails.
func (e *env) failingLaunch() *thread.Dispatcher {
	e.t.Helper()
	exe := filepath.Join(e.t.TempDir(), "no-serve")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	o := e.opts
	o.Executable = exe
	d := thread.New(o)
	if err := d.Acquire(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(d.Close)
	return d
}

// launchAndWait starts listener with the working test binary and gives it
// time to drain whatever is left in its inbox.
func (e *env) launchAndWait(listener, preset string) {
	e.t.Helper()
	if _, err := dispatch.Launch(context.Background(), e.opts, listener, preset, ""); err != nil {
		e.t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
}

func wantTerminal(t *testing.T, room, rid string) request.Record {
	t.Helper()
	r, err := request.Get(room, rid)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Terminal() {
		t.Fatalf("request %s left runnable: %+v", rid, r)
	}
	return r
}

// A launch failure after the request was saved must not leave a queued
// record behind: the next launch of that listener would run the stale prompt
// uncollected, and Retry would then run the work a second time.
func TestLaunchFailureCancelsSavedRequest(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.failingLaunch()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateError)) == 1 })
	m := agentMessages(snap, thread.StateError)[0]
	if !strings.HasPrefix(m.Text, "ERROR submit: ") {
		t.Fatalf("error text %q", m.Text)
	}
	if r := wantTerminal(t, e.room, m.RequestID); r.Status != "canceled" {
		t.Fatalf("record %+v, want canceled", r)
	}
	e.launchAndWait(m.Listener, m.Preset)
	if n := e.count("a"); n != 0 {
		t.Fatalf("stale prompt ran %d times after a later launch", n)
	}
	// Retry now runs the turn exactly once, as a fresh turn.
	d.Close()
	good := e.dispatcher()
	if err := good.Retry(context.Background(), th.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	snap = e.settle(good, th, quiescent)
	if done := agentMessages(snap, thread.StateDone); len(done) != 1 || e.count("a") != 1 {
		t.Fatalf("after retry: a ran %d times; messages %+v", e.count("a"), snap.Messages)
	}
}

func TestArchiveAfterLaunchFailureLeavesNoRunnableRecord(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.failingLaunch()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateError)) == 1 })
	m := agentMessages(snap, thread.StateError)[0]
	if err := d.SetArchived(context.Background(), th.ID, true); err != nil {
		t.Fatal(err)
	}
	e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusArchived })
	wantTerminal(t, e.room, m.RequestID)
	e.launchAndWait(m.Listener, m.Preset)
	if n := e.count("a"); n != 0 {
		t.Fatalf("stale prompt ran %d times after archive", n)
	}
}

// orphanErrorTurn fabricates the state an older build (or a crash between
// the journal write and the cancel) leaves behind: an agent message in
// error whose saved request is still queued in the listener's inbox.
func orphanErrorTurn(t *testing.T, e *env, th *thread.Thread, d *thread.Dispatcher) thread.Message {
	t.Helper()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap, _ := th.Snapshot()
	m := agentMessages(snap, thread.StatePending)[0]
	prompt, err := th.ReadPrompt(m.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := request.Submit(context.Background(), e.room, m.Listener, "web", prompt, m.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := th.Update(context.Background(), func(tx *thread.Tx) error {
		tx.Append(thread.Event{Kind: thread.KindState, Message: m.ID, State: thread.StateError, Text: "ERROR submit: launch failed"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStopAndArchiveCancelOrphanedErrorRecords(t *testing.T) {
	for _, archive := range []bool{false, true} {
		name := "stop"
		if archive {
			name = "archive"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "a")
			th, _ := thread.Create(e.room, "t", "a", "", 6)
			d := e.dispatcher()
			m := orphanErrorTurn(t, e, th, d)
			want := thread.StatusOpen
			if archive {
				want = thread.StatusArchived
				if err := d.SetArchived(context.Background(), th.ID, true); err != nil {
					t.Fatal(err)
				}
			} else if err := d.RequestStop(context.Background(), th.ID); err != nil {
				t.Fatal(err)
			}
			e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == want })
			wantTerminal(t, e.room, m.RequestID)
			e.launchAndWait(m.Listener, m.Preset)
			if n := e.count("a"); n != 0 {
				t.Fatalf("orphaned prompt ran %d times", n)
			}
		})
	}
}

// A same-ID retry (the request was never saved) must not use up the turn's
// only retry: if it fails again, it can be retried again.
func TestSameIDRetryCanBeRetriedAgain(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.From = "bad/name" // Send refuses it before saving anything
	d.Post(context.Background(), th.ID, "@a go", "")
	failed := func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateError)) == 1 }
	snap := e.settle(d, th, failed)
	m := agentMessages(snap, thread.StateError)[0]
	for i := 0; i < 2; i++ {
		if err := d.Retry(context.Background(), th.ID, m.ID); err != nil {
			t.Fatalf("retry %d: %v", i+1, err)
		}
		snap = e.settle(d, th, failed)
		if len(agentMessages(snap, "")) != 1 {
			t.Fatalf("same-ID retry planned a new turn: %+v", snap.Messages)
		}
	}
	d.From = "web"
	if err := d.Retry(context.Background(), th.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	snap = e.settle(d, th, quiescent)
	if done := agentMessages(snap, thread.StateDone); len(done) != 1 || done[0].ID != m.ID || e.count("a") != 1 {
		t.Fatalf("a ran %d times; messages %+v", e.count("a"), snap.Messages)
	}
}

// A turn whose running request already has a cancel request pending can
// never finish usefully under its old ID, so Retry plans a fresh turn.
func TestRetryOfCancelRequestedTurnIsFresh(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	m := orphanErrorTurn(t, e, th, d)
	r, err := request.Get(e.room, m.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := request.Start(e.room, r.Envelope); err != nil {
		t.Fatal(err)
	}
	if r, err := request.Cancel(context.Background(), e.room, m.RequestID); err != nil || r.Status != "running" || !r.CancelRequested {
		t.Fatalf("%+v %v", r, err)
	}
	if err := d.Retry(context.Background(), th.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	snap, _ := th.Snapshot()
	agents := agentMessages(snap, "")
	if len(agents) != 2 || agents[0].State != thread.StateError || !agents[0].Retried || agents[1].State != thread.StatePending {
		t.Fatalf("messages %+v", snap.Messages)
	}
}
