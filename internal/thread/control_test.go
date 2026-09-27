// internal/thread/control_test.go
package thread_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
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

// Fix round 1, issue 1 (Critical): a Retry of a turn cancelled by Stop must
// not resubmit into the old, now-permanently-stopped chain (submit() would
// refuse it forever); it must run in a fresh chain, as Post does.
func TestRetryAfterStopUsesFreshChain(t *testing.T) {
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
	stopped := agentMessages(snap, "")
	if len(stopped) != 1 || (stopped[0].State != thread.StateCancelled && stopped[0].State != thread.StateError) {
		t.Fatalf("messages %+v", snap.Messages)
	}
	if err := os.Remove(filepath.Join(e.fixture, "a.sleep")); err != nil {
		t.Fatal(err)
	}
	if err := d.Retry(context.Background(), th.ID, stopped[0].ID); err != nil {
		t.Fatal(err)
	}
	snap = e.settle(d, th, quiescent)
	if done := agentMessages(snap, thread.StateDone); len(done) != 1 {
		t.Fatalf("messages %+v", snap.Messages)
	}
	if e.count("a") > 2 {
		t.Fatalf("a ran %d times; want at most 2 (1 stopped + 1 retried)", e.count("a"))
	}
}

// Fix round 1, issue 2 (Important): if the hosted listener died while a turn
// was running, request.Cancel alone can never make its record terminal (no
// process is left to acknowledge host.Cancel), which would strand the
// thread in "stopping" forever. finishStop must settle it directly once the
// listener is confirmed not alive.
func TestStopSettlesTurnWhenListenerHasDied(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "sleep", "30s")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) == 1 })
	requestID := agentMessages(snap, thread.StateRunning)[0].RequestID

	// The journal's Running state is set optimistically by submit() as soon
	// as the request is enqueued; wait for the durable request record itself
	// to reach "running" (the host has actually claimed and started the
	// exec) before killing the process, so the listener is genuinely
	// executing and not merely queued.
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := request.Get(e.room, requestID)
		if err == nil && r.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("request never reached running: %+v, err=%v", r, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	listener := "a." + th.ID
	st, ok, err := host.ReadState(e.room, listener)
	if err != nil || !ok || st.PID == 0 {
		t.Fatalf("no live listener state: ok=%v err=%v pid=%d", ok, err, st.PID)
	}
	proc, err := os.FindProcess(st.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}

	if err := d.RequestStop(context.Background(), th.ID); err != nil {
		t.Fatal(err)
	}
	snap = e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusOpen })
	got := agentMessages(snap, "")
	if len(got) != 1 || (got[0].State != thread.StateCancelled && got[0].State != thread.StateError) {
		t.Fatalf("messages %+v", snap.Messages)
	}
	if !strings.Contains(got[0].Text, "interrupted") {
		t.Fatalf("expected an interrupted result, got %q", got[0].Text)
	}
}

// Fix round 1, issue 3 (Important): a thread's listener may be busy with a
// turn from a different thread (addressed there by its full name), so
// Janitor must compute business across every thread in the room before
// deciding to stop any listener, not just within the owning thread.
func TestJanitorSkipsListenerBusyInAnotherThread(t *testing.T) {
	e := newEnv(t, "a")
	thA, _ := thread.Create(e.room, "tA", "a", "", 6)
	thB, _ := thread.Create(e.room, "tB", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), thA.ID, "@a go", "")
	e.settle(d, thA, quiescent)
	listener := "a." + thA.ID

	e.script("a", "sleep", "5s")
	if _, err := d.Post(context.Background(), thB.ID, "@"+listener+" go", ""); err != nil {
		t.Fatal(err)
	}
	e.settle(d, thB, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) == 1 })

	if err := d.Janitor(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, alive := host.Existing(context.Background(), e.room, listener); !alive {
		t.Fatal("listener stopped despite being busy in another thread")
	}
}

// Fix round 1, issue 4 (Important): Retry must be idempotent — a second call
// on the same original turn must error, not launch a second extra turn.
func TestRetryTwiceErrorsOnSecondCall(t *testing.T) {
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
	e.settle(d, th, quiescent)

	if err := d.Retry(context.Background(), th.ID, failed[0].ID); err == nil {
		t.Fatal("expected an error retrying an already-retried turn")
	}
	if e.count("a") != 2 {
		t.Fatalf("a ran %d times; want exactly 2 (1 failure + 1 retry)", e.count("a"))
	}
}
