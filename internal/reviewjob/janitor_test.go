package reviewjob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
