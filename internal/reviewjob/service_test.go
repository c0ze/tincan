package reviewjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func sha(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func testService(t *testing.T, mode string) *Service {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts are not supported on Windows")
	}
	if tincanBin == "" {
		t.Skip("could not build tincan")
	}
	state := stateDir(t)
	t.Setenv("TINCAN_STATE_DIR", state)
	exe, _ := os.Executable()
	svc := &Service{StateDir: state, Registry: rooms.Open(filepath.Join(state, "rooms.json")), Executable: tincanBin,
		Presets: func() (map[string]host.Preset, error) {
			return map[string]host.Preset{"reviewer": {Exec: []string{exe, "fake-review", mode, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}}, nil
		}}
	// Stop every job's listener before the temporary state is removed.
	t.Cleanup(func() {
		svc.Wait()
		jobs, _ := svc.store().List()
		for _, j := range jobs {
			if j.Workspace != "" {
				host.Down(context.Background(), j.Workspace, j.Preset, 5*time.Second)
			}
		}
	})
	return svc
}

func jobRequest(id, prompt string, exp time.Time) CreateRequest {
	return CreateRequest{JobID: id, ReviewID: "rv-0000000000000000", Requester: "cachyos", Preset: "reviewer", ExpiresAt: exp,
		Packet: &packet.Packet{Question: "q", Manifest: packet.Manifest{Scope: "none", BaseKind: "none", Changes: []packet.Change{}}},
		Prompt: prompt, PromptSHA: sha(prompt)}
}

func waitTerminal(t *testing.T, s *Service, id string) Job {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		j, err := s.Status(id)
		if err == nil && j.Terminal() {
			return j
		}
		time.Sleep(100 * time.Millisecond)
	}
	j, _ := s.Status(id)
	t.Fatalf("job %s not terminal: %+v", id, j)
	return j
}

func TestCreateRunsOnceAndReplaysIdentically(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	req := jobRequest("rv-0000000000000000-0", "review this", exp)
	j, err := s.Create(ctx, req)
	if err != nil || j.State != "creating" {
		t.Fatalf("create: %+v %v", j, err)
	}
	s.Wait()
	done := waitTerminal(t, s, req.JobID)
	if done.State != "done" || !strings.Contains(done.Result, "REVIEW:") || !strings.Contains(done.Result, "review this") || done.Mode != "packet" {
		t.Fatalf("result: %+v", done)
	}
	again, err := s.Create(ctx, req)
	if err != nil || again.State != "done" {
		t.Fatalf("replay: %+v %v", again, err)
	}
	changed := req
	changed.Prompt, changed.PromptSHA = "other", sha("other")
	var je *Error
	if _, err := s.Create(ctx, changed); !errors.As(err, &je) || je.Status != 409 {
		t.Fatalf("different identity: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	now := time.Now().UTC()
	for name, c := range map[string]struct {
		req    CreateRequest
		status int
	}{
		"expired": {jobRequest("rv-0000000000000000-1", "p", now.Add(-time.Minute)), 410},
		"too far": {jobRequest("rv-0000000000000000-2", "p", now.Add(MaxLifetime+time.Hour)), 422},
		"bad hash": {func() CreateRequest {
			r := jobRequest("rv-0000000000000000-3", "p", now.Add(time.Hour))
			r.PromptSHA = "x"
			return r
		}(), 422},
		"unknown": {func() CreateRequest {
			r := jobRequest("rv-0000000000000000-4", "p", now.Add(time.Hour))
			r.Preset = "nope"
			return r
		}(), 404},
		"bad id": {jobRequest("../x", "p", now.Add(time.Hour)), 422},
		"hostile file": {func() CreateRequest {
			r := jobRequest("rv-0000000000000000-5", "p", now.Add(time.Hour))
			r.Packet.Files = map[string][]byte{"../x": nil}
			return r
		}(), 422},
	} {
		var je *Error
		if _, err := s.Create(ctx, c.req); !errors.As(err, &je) || je.Status != c.status {
			t.Errorf("%s: %v (want %d)", name, err, c.status)
		}
	}
}

func TestPinnedExecutableIgnoresWorkspaceDecoys(t *testing.T) {
	s := testService(t, "pwd")
	ctx := context.Background()
	req := jobRequest("rv-0000000000000000-6", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	exe, _ := os.Executable()
	req.Packet.Files = map[string][]byte{filepath.Base(exe): []byte("#!/bin/sh\necho decoy\n")}
	if _, err := s.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	j := waitTerminal(t, s, req.JobID)
	if j.State != "done" || strings.Contains(j.Result, "decoy") || !strings.HasPrefix(j.Result, j.Workspace) {
		t.Fatalf("pinned run: %+v", j)
	}
}

func TestCancelKeepsAFinishedResult(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	req := jobRequest("rv-0000000000000000-7", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	waitTerminal(t, s, req.JobID)
	j, acked, err := s.Cancel(ctx, req.JobID, time.Time{})
	if err != nil || !acked || j.State != "done" || j.Result == "" {
		t.Fatalf("cancel after finish: %+v %v %v", j, acked, err)
	}
}

func TestCancelRunningAndBeforeCreate(t *testing.T) {
	s := testService(t, "sleep")
	ctx := context.Background()
	req := jobRequest("rv-0000000000000000-8", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	deadline := time.Now().Add(20 * time.Second)
	acked := false
	var j Job
	for !acked && time.Now().Before(deadline) {
		var err error
		j, acked, err = s.Cancel(ctx, req.JobID, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !acked || j.State != "cancelled" {
		t.Fatalf("cancel running: %+v %v", j, acked)
	}
	// Cancel before create leaves a tombstone that refuses the create.
	early := jobRequest("rv-0000000000000000-9", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	if _, acked, err := s.Cancel(ctx, early.JobID, early.ExpiresAt); err != nil || !acked {
		t.Fatalf("cancel unknown: %v %v", acked, err)
	}
	var je *Error
	if _, err := s.Create(ctx, early); !errors.As(err, &je) || je.Status != 410 {
		t.Fatalf("create after tombstone: %v", err)
	}
}

func TestAck(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	var je *Error
	if _, err := s.Ack(ctx, "rv-0000000000000000-a"); !errors.As(err, &je) || je.Status != 404 {
		t.Fatalf("ack unknown: %v", err)
	}
	req := jobRequest("rv-0000000000000000-b", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	waitTerminal(t, s, req.JobID)
	if j, err := s.Ack(ctx, req.JobID); err != nil || !j.Acked {
		t.Fatalf("ack: %+v %v", j, err)
	}
}

// Time spent materializing counts: the launch rechecks the time left, so a
// slow checkout can never start a member past its expiry.
func TestLaunchRechecksExpiryAfterMaterializing(t *testing.T) {
	s := testService(t, "echo")
	real := time.Now()
	calls := 0
	s.Now = func() time.Time { // validation sees now; the launch sees 90 s later
		calls++
		if calls == 1 {
			return real
		}
		return real.Add(90 * time.Second)
	}
	req := jobRequest("rv-0000000000000000-d", "p", real.Add(2*time.Minute).UTC().Truncate(time.Second))
	if _, err := s.Create(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	j, _ := s.Status(req.JobID)
	if j.State != "error" || !strings.Contains(j.Result, "expired") {
		t.Fatalf("late launch: %+v", j)
	}
	if _, err := os.Stat(filepath.Join(j.Workspace, ".tincan", "requests")); err == nil {
		t.Fatal("a request was saved for an expired job")
	}
}

// A job's stored input is never mistaken for a job record: it is not
// listed, and a request naming it cannot overwrite the real record.
func TestInputFilesAreNotJobs(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	req := jobRequest("rv-0000000000000000-f", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	done := waitTerminal(t, s, req.JobID)
	jobs, _ := s.store().List()
	if len(jobs) != 1 {
		t.Fatalf("listed %d jobs: %+v", len(jobs), jobs)
	}
	s.Cancel(ctx, req.JobID+".input", time.Time{})
	if j, _ := s.Status(req.JobID); j.State != "done" || j.Result != done.Result {
		t.Fatalf("real record changed: %+v", j)
	}
}

// A committee with the maximum deadline asks for exactly MaxLifetime; a
// member whose clock runs a little behind must still accept it.
func TestCreateToleratesClockSkewAtMaxLifetime(t *testing.T) {
	s := testService(t, "echo")
	req := jobRequest("rv-0000000000000000-g", "p", time.Now().Add(MaxLifetime+2*time.Minute).UTC().Truncate(time.Second))
	if _, err := s.Create(context.Background(), req); err != nil {
		t.Fatalf("max-deadline job refused: %v", err)
	}
	s.Wait()
}
