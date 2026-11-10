package reviewjob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/request"
)

func TestJanitorResumesAdoptsAndCleans(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	// A creation interrupted after the request ran: the janitor adopts it.
	req := jobRequest("rv-0000000000000000-c", "p", exp)
	s.Create(ctx, req)
	s.Wait()
	done := waitTerminal(t, s, req.JobID)
	st := s.store()
	j, _, _ := st.Get(req.JobID)
	j.State, j.Result = "creating", ""
	st.Save(j) // as if the process died before recording "running"
	runs := countRuns(t, done.Workspace, req.JobID)
	if err := s.Janitor(ctx); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	j, _ = s.Status(req.JobID)
	if j.State != "done" || countRuns(t, done.Workspace, req.JobID) != runs {
		t.Fatalf("resume did not adopt: %+v", j)
	}

	// Acknowledged jobs are cleaned: listener stopped, workspace removed.
	s.Ack(ctx, req.JobID)
	if err := s.Janitor(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(done.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace kept after ack: %v", err)
	}
	if _, alive := host.Existing(ctx, done.Workspace, "reviewer"); alive {
		t.Fatal("listener still running")
	}

	// Records go 7 days after expiry.
	s.Now = func() time.Time { return exp.Add(TombstoneKeep + time.Hour) }
	s.Janitor(ctx)
	if _, ok, _ := st.Get(req.JobID); ok {
		t.Fatal("old record kept")
	}
}

// countRuns counts the fake reviewer's executions in a workspace.
func countRuns(t *testing.T, ws, _ string) int {
	data, _ := os.ReadFile(filepath.Join(ws, "runs.log"))
	return strings.Count(string(data), "\n")
}

// The creator died after saving the request but before launching its
// listener: resuming the creation launches it instead of adopting a request
// nothing will ever run.
func TestJanitorRelaunchesASavedRequestWithoutHost(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	req := jobRequest("rv-0000000000000000-i", "p", exp)
	st := s.store()
	if err := writeInput(s.packetPath(req.JobID), req); err != nil {
		t.Fatal(err)
	}
	ws := s.workspace(req.JobID)
	os.MkdirAll(ws, 0o700)
	if _, err := request.Submit(ctx, ws, "reviewer", "review", "saved body", req.JobID); err != nil {
		t.Fatal(err)
	}
	st.Save(Job{ID: req.JobID, ReviewID: req.ReviewID, Preset: "reviewer", PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA,
		ExpiresAt: exp, State: "creating", Workspace: ws, Created: time.Now().UTC()})
	if err := s.Janitor(ctx); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	j := waitTerminal(t, s, req.JobID)
	if j.State != "done" || !strings.Contains(j.Result, "saved body") || countRuns(t, ws, req.JobID) != 1 {
		t.Fatalf("job: %+v", j)
	}
}

// A resumed creation whose job has expired, with no host left but a
// lifetime lock file from an earlier one, ends in the expiry error in one
// pass instead of staying creating on every pass.
func TestJanitorSettlesAnExpiredResumedJob(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	req := jobRequest("rv-0000000000000000-l", "p", exp)
	st := s.store()
	if err := writeInput(s.packetPath(req.JobID), req); err != nil {
		t.Fatal(err)
	}
	ws := s.workspace(req.JobID)
	os.MkdirAll(ws, 0o700)
	if _, err := request.Submit(ctx, ws, "reviewer", "review", "saved body", req.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := host.WithoutHost(ctx, ws, "reviewer", func() error { return nil }); err != nil { // leaves the lock file
		t.Fatal(err)
	}
	st.Save(Job{ID: req.JobID, ReviewID: req.ReviewID, Preset: "reviewer", PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA,
		ExpiresAt: exp, State: "creating", Workspace: ws, Created: time.Now().UTC()})
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ := st.Get(req.JobID); j.State != "error" || !strings.Contains(j.Result, "expired") {
		t.Fatalf("job: %+v", j)
	}
	if r, _ := request.Get(ws, req.JobID); !r.Terminal() {
		t.Fatalf("request still runnable: %+v", r)
	}
}

// holdJournal holds a request's journal lock (request's own lock file), as a
// writer stopped while holding it would.
func holdJournal(t *testing.T, ws, id string) *filelock.Lock {
	t.Helper()
	l, err := filelock.Try(filepath.Join(request.Dir(ws), "locks", id+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// An expired job is recorded terminal only once its request can no longer
// run: while the journal is locked the cancel cannot be recorded, so the job
// stays retryable, and the next pass cancels it and records the expiry.
func TestExpiredJobWaitsForTheCancelToBeRecorded(t *testing.T) {
	s := testService(t, "echo")
	s.cancelWait = 200 * time.Millisecond
	ctx := context.Background()
	exp := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	req := jobRequest("rv-0000000000000000-m", "p", exp)
	st := s.store()
	writeInput(s.packetPath(req.JobID), req)
	ws := s.workspace(req.JobID)
	os.MkdirAll(ws, 0o700)
	if _, err := request.Submit(ctx, ws, "reviewer", "review", "saved body", req.JobID); err != nil {
		t.Fatal(err)
	}
	st.Save(Job{ID: req.JobID, ReviewID: req.ReviewID, Preset: "reviewer", PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA,
		ExpiresAt: exp, State: "creating", Workspace: ws, Created: time.Now().UTC()})
	held := holdJournal(t, ws, req.JobID)
	start := time.Now()
	s.Janitor(ctx)
	s.Wait()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("pass took %v", d)
	}
	if j, _, _ := st.Get(req.JobID); j.State != "creating" {
		t.Fatalf("job settled without a recorded cancel: %+v", j)
	}
	if r, _ := request.Get(ws, req.JobID); r.Terminal() || r.CancelRequested {
		t.Fatalf("request: %+v", r)
	}
	held.Close()
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ := st.Get(req.JobID); j.State != "error" || !strings.Contains(j.Result, "expired") {
		t.Fatalf("job: %+v", j)
	}
	if r, _ := request.Get(ws, req.JobID); r.Status != "canceled" {
		t.Fatalf("request: %+v", r)
	}
}

// Cancel acknowledges a job whose host is gone only once the request's
// cancellation is recorded.
func TestCancelWithoutHostWaitsForTheRecordedCancel(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	req := jobRequest("rv-0000000000000000-n", "p", exp)
	st := s.store()
	ws := s.workspace(req.JobID)
	os.MkdirAll(ws, 0o700)
	if _, err := request.Submit(ctx, ws, "reviewer", "review", "saved body", req.JobID); err != nil {
		t.Fatal(err)
	}
	st.Save(Job{ID: req.JobID, ReviewID: req.ReviewID, Preset: "reviewer", PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA,
		ExpiresAt: exp, State: "running", Workspace: ws, Created: time.Now().UTC()})
	held := holdJournal(t, ws, req.JobID)
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	j, acked, _ := s.Cancel(short, req.JobID, exp)
	cancel()
	if acked || j.Terminal() {
		t.Fatalf("acknowledged without a recorded cancel: %+v acked=%v", j, acked)
	}
	held.Close()
	if j, acked, err := s.Cancel(ctx, req.JobID, exp); err != nil || !acked || j.State != "cancelled" {
		t.Fatalf("cancel: %+v acked=%v err=%v", j, acked, err)
	}
	if r, _ := request.Get(ws, req.JobID); r.Status != "canceled" {
		t.Fatalf("request: %+v", r)
	}
}

// savedCreatingJob records job id as creating with a saved, queued request
// in its workspace and no host: a creator that died after saving.
func savedCreatingJob(t *testing.T, s *Service, id string, exp time.Time) (CreateRequest, string) {
	t.Helper()
	req := jobRequest(id, "p", exp)
	if err := writeInput(s.packetPath(id), req); err != nil {
		t.Fatal(err)
	}
	ws := s.workspace(id)
	os.MkdirAll(ws, 0o700)
	if _, err := request.Submit(context.Background(), ws, "reviewer", "review", "saved body", id); err != nil {
		t.Fatal(err)
	}
	s.store().Save(Job{ID: id, ReviewID: req.ReviewID, Preset: "reviewer", PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA,
		ExpiresAt: exp, State: "creating", Workspace: ws, Created: time.Now().UTC()})
	return req, ws
}

// countLaunches counts the service's dispatch.Send calls.
func countLaunches(s *Service) *int {
	n := new(int)
	s.hook = func(point string) {
		if point == "launch" {
			*n++
		}
	}
	return n
}

// A cancel that could not be recorded (busy journal) still leaves cancel
// intent: the resumed creation only retries the cancel, never a launch.
func TestCancelIntentIsNeverLaunched(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, ws := savedCreatingJob(t, s, "rv-0000000000000000-o", exp)
	launches := countLaunches(s)
	held := holdJournal(t, ws, "rv-0000000000000000-o")
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	if _, acked, _ := s.Cancel(short, "rv-0000000000000000-o", exp); acked {
		t.Fatal("acknowledged without a recorded cancel")
	}
	cancel()
	held.Close()
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ := s.store().Get("rv-0000000000000000-o"); j.State != "cancelled" || *launches != 0 {
		t.Fatalf("job: %+v (launches %d)", j, *launches)
	}
	if r, _ := request.Get(ws, "rv-0000000000000000-o"); r.Status != "canceled" {
		t.Fatalf("request: %+v", r)
	}
}

// A relaunch that failed while the journal was busy keeps its failure
// pending: once the cause is gone, the next pass cancels and records that
// error, and never launches again.
func TestPendingRelaunchFailureIsNeverRelaunched(t *testing.T) {
	s := testService(t, "echo")
	s.cancelWait = 200 * time.Millisecond
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, ws := savedCreatingJob(t, s, "rv-0000000000000000-p", exp)
	launches := countLaunches(s)
	good := s.Presets
	s.Presets = func() (map[string]host.Preset, error) {
		return map[string]host.Preset{"reviewer": {Exec: []string{filepath.Join(ws, "not-installed")}, Stdin: "none", Reply: "stdout"}}, nil
	}
	held := holdJournal(t, ws, "rv-0000000000000000-p")
	s.Janitor(ctx)
	s.Wait()
	j, _, _ := s.store().Get("rv-0000000000000000-p")
	if j.State != "creating" || j.PendingError == "" {
		t.Fatalf("job: %+v", j)
	}
	pending := j.PendingError
	s.Presets = good
	held.Close()
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ = s.store().Get("rv-0000000000000000-p"); j.State != "error" || j.Result != "ERROR "+pending || *launches != 0 {
		t.Fatalf("job: %+v (launches %d)", j, *launches)
	}
	if r, _ := request.Get(ws, "rv-0000000000000000-p"); r.Status != "canceled" {
		t.Fatalf("request: %+v", r)
	}
}

// The same for the first launch: Send saved the request and failed, and
// the journal was busy when its cancel was attempted.
func TestPendingSendFailureIsNeverRelaunched(t *testing.T) {
	s := testService(t, "echo")
	s.cancelWait = 200 * time.Millisecond
	ctx := context.Background()
	exe, _ := os.Executable()
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	pinned := filepath.Join(stateDir(t), "reviewer-bin")
	if err := os.WriteFile(pinned, data, 0o755); err != nil {
		t.Fatal(err)
	}
	s.Presets = func() (map[string]host.Preset, error) {
		return map[string]host.Preset{"reviewer": {Exec: []string{pinned, "fake-review", "echo", "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}}, nil
	}
	req := jobRequest("rv-0000000000000000-q", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.materialized = func(context.Context) { os.Remove(pinned) } // the pinned binary vanishes: the launch fails
	launches := 0
	var held *filelock.Lock
	s.hook = func(point string) {
		switch point {
		case "launch":
			launches++
		case "cancel":
			if held == nil {
				held = holdJournal(t, s.workspace(req.JobID), req.JobID)
			}
		}
	}
	if _, err := s.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	j, _, _ := s.store().Get(req.JobID)
	if j.State != "creating" || !strings.HasPrefix(j.PendingError, "launch: ") || launches != 1 {
		t.Fatalf("job: %+v (launches %d)", j, launches)
	}
	pending := j.PendingError
	os.WriteFile(pinned, data, 0o755) // the cause is gone
	held.Close()
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ = s.store().Get(req.JobID); j.State != "error" || j.Result != "ERROR "+pending || launches != 1 {
		t.Fatalf("job: %+v (launches %d)", j, launches)
	}
	if r, _ := request.Get(j.Workspace, req.JobID); r.Status != "canceled" {
		t.Fatalf("request: %+v", r)
	}
}

// A request that already ended keeps its outcome when the job is settled
// for a cancel: its failure is recorded, not "canceled before launch".
func TestSettleKeepsAnEndedRequestsOutcome(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, ws := savedCreatingJob(t, s, "rv-0000000000000000-s", exp)
	r, _ := request.Get(ws, "rv-0000000000000000-s")
	if fin, err := request.Finish(ws, r.Envelope, "ERROR exit=3: reviewer crashed"); err != nil || fin.Status != "failed" {
		t.Fatalf("fixture: %+v %v", fin, err)
	}
	s.store().requestCancel("rv-0000000000000000-s")
	launches := countLaunches(s)
	s.Janitor(ctx)
	s.Wait()
	j, _, _ := s.store().Get("rv-0000000000000000-s")
	if j.State != "error" || j.Result != "ERROR exit=3: reviewer crashed" || *launches != 0 {
		t.Fatalf("job: %+v (launches %d)", j, *launches)
	}
}

// A saved request that cannot be read is not "no request": the workspace is
// kept and nothing is launched over it (a fresh creation would run the
// member a second time).
func TestUnreadableRequestIsNeverRestartedOver(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	_, ws := savedCreatingJob(t, s, "rv-0000000000000000-t", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	if err := os.WriteFile(filepath.Join(request.Dir(ws), "rv-0000000000000000-t.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	launches := countLaunches(s)
	s.Janitor(ctx)
	s.Wait()
	if j, _, _ := s.store().Get("rv-0000000000000000-t"); j.State != "creating" || *launches != 0 {
		t.Fatalf("job: %+v (launches %d)", j, *launches)
	}
	if _, err := os.Stat(filepath.Join(request.Dir(ws), "rv-0000000000000000-t.json")); err != nil {
		t.Fatalf("workspace wiped: %v", err)
	}
}

// A relaunch that times out before launching anything (here on the route
// lock another submitter holds) proves nothing about a host: the job stays
// creating, the request untouched, and the next janitor pass runs it.
func TestJanitorRetriesARelaunchThatTimedOut(t *testing.T) {
	s := testService(t, "echo")
	s.relaunchWait = 200 * time.Millisecond
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	req := jobRequest("rv-0000000000000000-k", "p", exp)
	st := s.store()
	if err := writeInput(s.packetPath(req.JobID), req); err != nil {
		t.Fatal(err)
	}
	ws := s.workspace(req.JobID)
	os.MkdirAll(ws, 0o700)
	if _, err := request.Submit(ctx, ws, "reviewer", "review", "saved body", req.JobID); err != nil {
		t.Fatal(err)
	}
	st.Save(Job{ID: req.JobID, ReviewID: req.ReviewID, Preset: "reviewer", PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA,
		ExpiresAt: exp, State: "creating", Workspace: ws, Created: time.Now().UTC()})
	route, err := request.LockRoute(ctx, ws, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	s.Janitor(ctx)
	s.Wait()
	route.Close()
	if j, _, _ := st.Get(req.JobID); j.State != "creating" {
		t.Fatalf("job settled on a timed-out relaunch: %+v", j)
	}
	if r, _ := request.Get(ws, req.JobID); r.Status != "queued" || r.CancelRequested {
		t.Fatalf("request: %+v", r)
	}
	s.Janitor(ctx)
	s.Wait()
	if j := waitTerminal(t, s, req.JobID); j.State != "done" || countRuns(t, ws, req.JobID) != 1 {
		t.Fatalf("job: %+v", j)
	}
}

// Seven days after expiry a record is forgotten only once its workspace is
// cleaned up, under the job lock, and the lock file stays (lock files are
// never unlinked: that would allow split ownership).
func TestJanitorForgetsOnlyCleanedRecords(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	req := jobRequest("rv-0000000000000000-f", "p", exp)
	s.Create(ctx, req)
	s.Wait()
	done := waitTerminal(t, s, req.JobID)
	st := s.store()
	s.Now = func() time.Time { return exp.Add(TombstoneKeep + time.Hour) }
	s.down = func(context.Context, string, string, time.Duration) error { return errors.New("host busy") }
	s.Janitor(ctx)
	if _, ok, _ := st.Get(req.JobID); !ok {
		t.Fatal("record forgotten although its workspace could not be cleaned")
	}
	if _, err := os.Stat(done.Workspace); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	s.down = nil
	held, err := st.Lock(ctx, req.JobID)
	if err != nil {
		t.Fatal(err)
	}
	s.Janitor(ctx)
	if _, ok, _ := st.Get(req.JobID); !ok {
		t.Fatal("record removed while another operation held the job lock")
	}
	held.Close()
	s.Janitor(ctx)
	if _, ok, _ := st.Get(req.JobID); ok {
		t.Fatal("cleaned record kept")
	}
	if _, err := os.Stat(done.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.jobsDir(), req.JobID+".lock")); err != nil {
		t.Fatalf("lock file unlinked: %v", err)
	}
}

// A cleanup that could not stop the listener is retried on the next pass,
// not recorded as done.
func TestJanitorRetriesFailedCleanup(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	req := jobRequest("rv-0000000000000000-e", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	done := waitTerminal(t, s, req.JobID)
	s.Ack(ctx, req.JobID)
	s.down = func(context.Context, string, string, time.Duration) error { return errors.New("host busy") }
	s.Janitor(ctx)
	if j, _, _ := s.store().Get(req.JobID); j.Cleaned {
		t.Fatal("failed cleanup recorded as done")
	}
	if _, err := os.Stat(done.Workspace); err != nil {
		t.Fatalf("workspace removed although the listener was not stopped: %v", err)
	}
	s.down = nil
	s.Janitor(ctx)
	if j, _, _ := s.store().Get(req.JobID); !j.Cleaned {
		t.Fatal("cleanup not retried")
	}
	if _, err := os.Stat(done.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace kept: %v", err)
	}
}
