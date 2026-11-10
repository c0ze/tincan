//go:build !windows

package thread_test

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/thread"
)

// A busy host whose control endpoint stops answering still owns its
// listener: relaunching fails on its lifetime lock, and that must never be
// taken as "no host" — the request is not finished as interrupted, and the
// real result arrives once the host answers again.
func TestUnresponsiveHostIsNotDeclaredGone(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "sleep", "2s")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	thread.SetRedrive(d, time.Millisecond, 1)
	if _, err := d.Post(context.Background(), th.ID, "@a hi", ""); err != nil {
		t.Fatal(err)
	}
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) == 1 })
	m := agentMessages(snap, thread.StateRunning)[0]
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if r, _ := request.Get(e.room, m.RequestID); r.Status == "running" {
			break // the host is busy with it
		}
		if time.Now().After(deadline) {
			t.Fatal("the host never started the request")
		}
	}
	st, ok, _ := host.ReadState(e.room, m.Listener)
	if !ok || st.PID == 0 {
		t.Fatalf("host state: %+v %v", st, ok)
	}
	if err := syscall.Kill(st.PID, syscall.SIGSTOP); err != nil { // keeps its locks, stops answering
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			syscall.Kill(st.PID, syscall.SIGCONT)
			resumed = true
		}
	}
	t.Cleanup(resume)
	// With a limit of one, the second pass would give up if the first
	// relaunch (refused by the lock) had counted.
	for i := 0; i < 2; i++ {
		// A short pass deadline bounds each relaunch's wait on the lock.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		d.Reconcile(ctx)
		cancel()
	}
	if r, err := request.Get(e.room, m.RequestID); err != nil || r.Terminal() {
		t.Fatalf("request of a live host finished: %+v %v", r, err)
	}
	resume()
	snap = e.settle(d, th, func(s thread.Snapshot) bool { cur, _ := s.Message(m.ID); return !cur.Active() })
	if cur, _ := snap.Message(m.ID); cur.State != thread.StateDone || cur.Text != "done by a" {
		t.Fatalf("turn: %+v", cur)
	}
}
