package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

func TestReviewJobsOverTheMachineLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts")
	}
	hub, _, _, member := peerPair(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TINCAN_STATE_DIR", state)
	member.cfg.StateDir = state
	bin := filepath.Join(t.TempDir(), "reviewer")
	os.WriteFile(bin, []byte("#!/bin/sh\necho REVIEWED \"$1\"\n"), 0o755)
	member.cfg.Dispatch.Presets = map[string]host.Preset{"reviewer": {Exec: []string{bin, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}}
	member.initJobs() // Server builds its reviewjob.Service from cfg
	member.jobs.Executable = buildTincan(t)
	t.Cleanup(func() { // stop the job's listener before its workspace is removed
		member.jobs.Wait()
		host.Down(context.Background(), filepath.Join(state, "reviews", "ws", "rv-1111111111111111-0"), "reviewer", 5*time.Second)
	})

	prompt := "look at this"
	sum := sha256.Sum256([]byte(prompt))
	req := reviewjob.CreateRequest{JobID: "rv-1111111111111111-0", ReviewID: "rv-1111111111111111", Requester: "cachyos", Preset: "reviewer",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second), Prompt: prompt, PromptSHA: hex.EncodeToString(sum[:]),
		Packet: &packet.Packet{Question: "q", Manifest: packet.Manifest{Scope: "none", BaseKind: "none", Changes: []packet.Change{}}}}
	p := hub.peers["macmini"]
	var created jobView
	if err := p.call(context.Background(), "POST", "review-jobs", req, 1<<20, &created); err != nil || created.State != "creating" {
		t.Fatalf("create: %+v %v", created, err)
	}
	var got jobView
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := p.call(context.Background(), "GET", "review-jobs/"+req.JobID, nil, 2<<20, &got); err == nil && got.State == "done" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.State != "done" || !strings.Contains(got.Result, "REVIEWED") {
		t.Fatalf("status: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), state) {
		t.Fatal("status exposes the workspace path")
	}
	if err := p.call(context.Background(), "POST", "review-jobs/"+req.JobID+"/ack", map[string]string{}, 1<<20, nil); err != nil {
		t.Fatalf("ack: %v", err)
	}
	var pe *PeerError
	changed := req
	changed.Prompt, changed.PromptSHA = "x", "y"
	if err := p.call(context.Background(), "POST", "review-jobs", changed, 1<<20, nil); !asPeerError(err, &pe) || pe.Status != 422 {
		t.Fatalf("bad hash: %v", err)
	}
	var cancel struct {
		Acked bool `json:"acked"`
	}
	if err := p.call(context.Background(), "POST", "review-jobs/rv-2222222222222222-0/cancel", map[string]string{}, 1<<20, &cancel); err != nil || !cancel.Acked {
		t.Fatalf("cancel unknown: %+v %v", cancel, err)
	}
	member.jobs.Wait()
}

func asPeerError(err error, target **PeerError) bool {
	pe, ok := err.(*PeerError)
	if ok {
		*target = pe
	}
	return ok
}

func TestReviewJobsNeedStateDir(t *testing.T) {
	s := testServer(t)
	if rec := do(t, s.Handler(), "GET", "/api/review-jobs/rv-1-0", "", ownerHdr()); rec.Code != 503 {
		t.Fatalf("no state dir: %d", rec.Code)
	}
}

var (
	tincanOnce sync.Once
	tincanPath string
)

// buildTincan builds ./cmd/tincan once per test binary.
func buildTincan(t *testing.T) string {
	t.Helper()
	tincanOnce.Do(func() {
		dir, err := os.MkdirTemp("", "tincan-bin-")
		if err != nil {
			return
		}
		p := filepath.Join(dir, "tincan")
		if exec.Command("go", "build", "-o", p, "../../cmd/tincan").Run() == nil {
			tincanPath = p
		}
	})
	if tincanPath == "" {
		t.Skip("could not build tincan")
	}
	return tincanPath
}
