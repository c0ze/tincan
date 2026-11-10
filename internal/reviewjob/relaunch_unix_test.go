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

// stoppedOwner creates job id with the gated reviewer, waits until its host
// is busy with the request, and stops the host: it keeps owning the
// listener but answers nothing. resume continues it; the gate is opened at
// cleanup so an orphaned reviewer exits.
func stoppedOwner(t *testing.T, s *Service, id string) (j Job, pid int, resume func()) {
	t.Helper()
	req := jobRequest(id, "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(context.Background(), req)
	s.Wait()
	j, _ = s.Status(id)
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
	resume = func() {
		if !resumed {
			syscall.Kill(hs.PID, syscall.SIGCONT)
			resumed = true
		}
	}
	ws := j.Workspace
	t.Cleanup(func() {
		resume()
		os.WriteFile(filepath.Join(ws, "gate.open"), nil, 0o600)
	})
	return j, hs.PID, resume
}

// boundedCancel cancels as a requester does (the web transport allows 10 s):
// a stopped host may hold the request journal it was writing to.
func boundedCancel(s *Service, id string) (Job, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.Cancel(ctx, id, time.Time{})
}

// A job with cancel intent whose request is running under a host that owns
// its listener but does not answer is not terminal, and its cancel is not
// acknowledged, however Existing sees it; once the host answers it observes
// the cancel and the job ends cancelled.
func TestCancelWaitsForAnUnresponsiveOwner(t *testing.T) {
	s := testService(t, "gate")
	s.cancelWait = time.Second
	ctx := context.Background()
	const id = "rv-0000000000000000-r"
	_, _, resume := stoppedOwner(t, s, id)
	st := s.store()
	j, _, _ := st.Get(id)
	j.State = "creating" // the creator died before recording "running"
	st.Save(j)
	st.requestCancel(id) // and a cancel arrived meanwhile
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ = st.Get(id); j.Terminal() {
		t.Fatalf("job recorded terminal while its host may still run it: %+v", j)
	}
	if j, acked, _ := boundedCancel(s, id); acked || j.Terminal() {
		t.Fatalf("cancel acknowledged while its host may still run it: %+v", j)
	}
	resume()
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.Janitor(ctx)
		s.Wait()
		j, acked, err := boundedCancel(s, id)
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

// When the owner that kept a settling job running dies without finishing
// the request, the next janitor pass settles it: the request is finished
// and the job records its cancellation or pending failure, with no launch.
func TestSettlementSurvivesItsOwnerDying(t *testing.T) {
	for name, tc := range map[string]struct {
		id     string
		intent func(st Store, j *Job)
		state  string
		result string
	}{
		"cancel intent": {"rv-0000000000000000-u", func(st Store, j *Job) { st.requestCancel(j.ID) }, "cancelled", "ERROR canceled"},
		"pending error": {"rv-0000000000000000-v", func(st Store, j *Job) { j.PendingError = "launch: boom" }, "error", "ERROR launch: boom"},
	} {
		t.Run(name, func(t *testing.T) {
			s := testService(t, "gate")
			s.cancelWait = time.Second
			ctx := context.Background()
			j, pid, _ := stoppedOwner(t, s, tc.id)
			launches := countLaunches(s)
			st := s.store()
			// As settlement leaves a job whose listener a host owns: running,
			// with its cancel intent or pending failure.
			j, _, _ = st.Get(tc.id)
			tc.intent(st, &j)
			st.Save(j)
			s.Janitor(ctx)
			s.Wait()
			if j, _, _ = st.Get(tc.id); j.State != "running" {
				t.Fatalf("settled while owned: %+v", j)
			}
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil { // the owner dies, request unfinished
				t.Fatal(err)
			}
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if owned, err := host.Owned(ctx, j.Workspace, j.Preset); err == nil && !owned {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the killed host still owns its listener")
				}
			}
			s.Janitor(ctx)
			s.Wait()
			j, _, _ = st.Get(tc.id)
			if j.State != tc.state || !strings.HasPrefix(j.Result, tc.result) || *launches != 0 {
				t.Fatalf("job: %+v (launches %d)", j, *launches)
			}
			// The same pass may clean the workspace up; either way the request
			// can no longer run.
			if r, err := request.Get(j.Workspace, tc.id); err == nil && r.Status != "canceled" {
				t.Fatalf("request: %+v", r)
			}
		})
	}
}
