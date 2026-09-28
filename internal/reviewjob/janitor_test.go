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
