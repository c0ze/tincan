//go:build !windows

package reviewjob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
)

// Resuming a creation whose listener is busy but not answering adopts the
// request: a host owns it (host.Owned), so there is no relaunch — which here
// would fail, the preset having been removed — and no cancel, and its real
// result arrives once the host answers.
func TestResumeAdoptsARequestOwnedByAnUnresponsiveHost(t *testing.T) {
	s := testService(t, "gate")
	ctx := context.Background()
	req := jobRequest("rv-0000000000000000-j", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	j, _ := s.Status(req.JobID)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if r, _ := request.Get(j.Workspace, j.ID); r.Status == "running" {
			break // the host is busy with it, held at the gate
		}
		if time.Now().After(deadline) {
			t.Fatal("the host never started the request")
		}
	}
	hs, ok, _ := host.ReadState(j.Workspace, j.Preset)
	if !ok || hs.PID == 0 {
		t.Fatalf("host state: %+v %v", hs, ok)
	}
	if err := syscall.Kill(hs.PID, syscall.SIGSTOP); err != nil { // keeps its locks, stops answering
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			syscall.Kill(hs.PID, syscall.SIGCONT)
			resumed = true
		}
	}
	t.Cleanup(resume)
	presets := s.Presets
	s.Presets = func() (map[string]host.Preset, error) { return map[string]host.Preset{}, nil }
	st := s.store()
	j, _, _ = st.Get(req.JobID)
	j.State = "creating" // as if the creator died before recording "running"
	st.Save(j)
	s.Janitor(ctx)
	s.Wait()
	if j, _ = s.Status(req.JobID); j.State != "running" {
		t.Fatalf("job: %+v", j)
	}
	if r, _ := request.Get(j.Workspace, j.ID); r.CancelRequested || r.Terminal() {
		t.Fatalf("request of a live host given up: %+v", r)
	}
	s.Presets = presets
	resume()
	os.WriteFile(filepath.Join(j.Workspace, "gate.open"), nil, 0o600)
	if done := waitTerminal(t, s, req.JobID); done.State != "done" || !strings.Contains(done.Result, "REVIEW:") {
		t.Fatalf("result: %+v", done)
	}
}

// A job with cancel intent whose request is running under a host that owns
// its listener but does not answer is not terminal, and its cancel is not
// acknowledged, however Existing sees it; once the host answers it observes
// the cancel and the job ends cancelled.
func TestCancelWaitsForAnUnresponsiveOwner(t *testing.T) {
	s := testService(t, "gate")
	s.cancelWait = time.Second
	ctx := context.Background()
	// Requesters' cancels are bounded (the web transport allows 10 s): a
	// stopped host may hold the request journal it was writing to.
	cancelJob := func() (Job, bool, error) {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return s.Cancel(cctx, "rv-0000000000000000-r", time.Time{})
	}
	req := jobRequest("rv-0000000000000000-r", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	j, _ := s.Status(req.JobID)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if r, _ := request.Get(j.Workspace, j.ID); r.Status == "running" {
			break // the host is busy with it, held at the gate
		}
		if time.Now().After(deadline) {
			t.Fatal("the host never started the request")
		}
	}
	hs, ok, _ := host.ReadState(j.Workspace, j.Preset)
	if !ok || hs.PID == 0 {
		t.Fatalf("host state: %+v %v", hs, ok)
	}
	if err := syscall.Kill(hs.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			syscall.Kill(hs.PID, syscall.SIGCONT)
			resumed = true
		}
	}
	t.Cleanup(resume)
	st := s.store()
	j, _, _ = st.Get(req.JobID)
	j.State = "creating" // the creator died before recording "running"
	st.Save(j)
	st.requestCancel(req.JobID) // and a cancel arrived meanwhile
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ = st.Get(req.JobID); j.Terminal() {
		t.Fatalf("job recorded terminal while its host may still run it: %+v", j)
	}
	if j, acked, _ := cancelJob(); acked || j.Terminal() {
		t.Fatalf("cancel acknowledged while its host may still run it: %+v", j)
	}
	resume()
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.Janitor(ctx)
		s.Wait()
		j, acked, err := cancelJob()
		if err == nil && acked {
			if j.State != "cancelled" {
				t.Fatalf("job: %+v", j)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never cancelled: %+v", j)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
