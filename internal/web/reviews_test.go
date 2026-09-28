package web

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func TestReviewAcrossTwoMachines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts")
	}
	hub, _, _, member := peerPair(t) // hub = cachyos (requester), member = macmini
	bin := buildTincan(t)
	script := filepath.Join(t.TempDir(), "reviewer")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"REVIEWED on $(basename $(dirname $PWD))\"\n"), 0o755)
	presets := map[string]host.Preset{"reviewer": {Exec: []string{script, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}}
	for _, s := range []*Server{hub, member} {
		state, _ := filepath.EvalSymlinks(t.TempDir())
		s.cfg.StateDir = state
		s.cfg.Dispatch.Presets = presets
		s.cfg.Dispatch.Executable = bin
		s.initJobs()
	}
	t.Setenv("TINCAN_STATE_DIR", hub.cfg.StateDir)
	committee.NewStore(hub.cfg.StateDir).Put(context.Background(), committee.Committee{Name: "pair", Members: []string{"reviewer@cachyos", "reviewer@macmini"}})
	room, _ := filepath.EvalSymlinks(t.TempDir())
	hub.cfg.Registry = rooms.Open(filepath.Join(hub.cfg.StateDir, "rooms.json"))
	from := ""
	in, _, err := review.Publish(context.Background(), review.PublishRequest{StateDir: hub.cfg.StateDir, Room: room, Committee: "pair",
		Question: "ok?", Origin: "web", Machine: "cachyos", CommitteesFrom: &from, InCoordinator: true, Registry: hub.cfg.Registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range []*Server{hub, member} {
			s.jobs.Wait()
			jobs, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "reviews", "ws"))
			for _, j := range jobs {
				host.Down(context.Background(), filepath.Join(s.cfg.StateDir, "reviews", "ws", j.Name()), "reviewer", 5*time.Second)
			}
		}
	})
	deadline := time.Now().Add(40 * time.Second)
	var st review.State
	for time.Now().Before(deadline) {
		hub.coord.Reconcile(context.Background(), room)
		st, _ = review.ReadState(room, in.ReviewID)
		if st.Status == "closed" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if st.Status != "closed" {
		t.Fatalf("review did not close: %+v", st)
	}
	b, _, _ := review.ReadBundle(room, in.ReviewID)
	if strings.Count(b, "REVIEWED") != 2 {
		t.Fatalf("bundle:\n%s", b)
	}
}

func TestReviewsNote(t *testing.T) {
	s, rid := apiServer(t)
	r, _, _ := s.cfg.Registry.Get(rid)
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	list, _ := s.cfg.Registry.List()
	s.scan(list)
	drain(ch)
	os.MkdirAll(filepath.Join(review.Root(r.Path), "rv-0000000000000000"), 0o700)
	os.WriteFile(filepath.Join(review.Root(r.Path), "rv-0000000000000000", "review.json"), []byte(`{}`), 0o600)
	s.scan(list)
	for {
		select {
		case n := <-ch:
			if n.Kind == "reviews" && n.Room == rid {
				return
			}
		default:
			t.Fatal("no reviews note")
		}
	}
}
