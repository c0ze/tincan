//go:build !windows

package thread_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/thread"
)

// A busy host that stops answering still owns its listener (host.Owned):
// it is never relaunched, even when its preset has since been removed, its
// request is not finished, and the real result arrives once it answers.
func TestOwnedListenerIsNeverRelaunched(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "gate", "")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	relaunches := countRelaunches(d)
	thread.SetRedrive(d, time.Millisecond, 1)
	if _, err := d.Post(context.Background(), th.ID, "@a hi", ""); err != nil {
		t.Fatal(err)
	}
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) == 1 })
	m := agentMessages(snap, thread.StateRunning)[0]
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if r, _ := request.Get(e.room, m.RequestID); r.Status == "running" {
			break // the host is busy with it, held at the gate
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
	delete(e.opts.Presets, "a") // a relaunch would now fail in preset resolution
	for i := 0; i < 2; i++ {
		if err := d.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if *relaunches != 0 {
		t.Fatalf("relaunched an owned listener %d times", *relaunches)
	}
	if r, err := request.Get(e.room, m.RequestID); err != nil || r.Terminal() {
		t.Fatalf("request of a live host finished: %+v %v", r, err)
	}
	resume()
	os.WriteFile(filepath.Join(e.fixture, "a.open"), nil, 0o600)
	snap = e.settle(d, th, func(s thread.Snapshot) bool { cur, _ := s.Message(m.ID); return !cur.Active() })
	if cur, _ := snap.Message(m.ID); cur.State != thread.StateDone || cur.Text != "done by a" {
		t.Fatalf("turn: %+v", cur)
	}
}
