// internal/thread/control_test.go
package thread_test

import (
	"context"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/thread"
)

func TestStopCancelsRunningTurn(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "sleep", "30s")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) == 1 })
	if err := d.RequestStop(context.Background(), th.ID); err != nil {
		t.Fatal(err)
	}
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusOpen })
	if got := agentMessages(snap, ""); len(got) != 1 || (got[0].State != thread.StateCancelled && got[0].State != thread.StateError) {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestStopRacingCompletionPreventsHandoff(t *testing.T) {
	e := newEnv(t, "a", "b")
	e.script("a", "reply", "@b next")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := thread.New(e.opts)
	if err := d.Acquire(); err != nil {
		t.Fatal(err)
	}
	stopHere := true
	d.Hook = func(p string) error {
		if p == "after-terminal" && stopHere {
			stopHere = false
			return context.Canceled // leave a done message with unprocessed handoffs
		}
		return nil
	}
	d.Post(context.Background(), th.ID, "@a go", "")
	deadline := time.Now().Add(30 * time.Second)
	for stopHere && time.Now().Before(deadline) {
		d.Reconcile(context.Background())
		time.Sleep(100 * time.Millisecond)
	}
	if err := d.RequestStop(context.Background(), th.ID); err != nil {
		t.Fatal(err)
	}
	e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusOpen && quiescent(s) })
	time.Sleep(time.Second)
	if e.count("b") != 0 {
		t.Fatalf("b ran %d times after Stop", e.count("b"))
	}
}

func TestArchiveCancelsQueuedWorkAndStopsListeners(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "sleep", "3s")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a one", "")
	d.Post(context.Background(), th.ID, "@a two", "")
	e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) >= 1 })
	if err := d.SetArchived(context.Background(), th.ID, true); err != nil {
		t.Fatal(err)
	}
	e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusArchived })
	if _, alive := host.Existing(context.Background(), e.room, "a."+th.ID); alive {
		t.Fatal("thread listener still running after archive")
	}
	if err := d.SetArchived(context.Background(), th.ID, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	if e.count("a") > 1 {
		t.Fatalf("a ran %d times; queued work survived archive", e.count("a"))
	}
}

func TestRetryResubmitsFailedTurn(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	e.script("a", "reply", "ERROR simulated failure")
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, quiescent)
	failed := agentMessages(snap, thread.StateError)
	if len(failed) != 1 {
		t.Fatalf("messages %+v", snap.Messages)
	}
	e.script("a", "reply", "fine now")
	if err := d.Retry(context.Background(), th.ID, failed[0].ID); err != nil {
		t.Fatal(err)
	}
	snap = e.settle(d, th, quiescent)
	if done := agentMessages(snap, thread.StateDone); len(done) != 1 || done[0].Text != "fine now" {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestJanitorStopsIdleThreadListeners(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	e.settle(d, th, quiescent)
	if err := d.Janitor(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, alive := host.Existing(context.Background(), e.room, "a."+th.ID); alive {
		t.Fatal("idle listener not stopped")
	}
}
