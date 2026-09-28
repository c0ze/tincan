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
	"github.com/c0ze/tincan/v2/internal/thread"
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
		hub.coord.Reconcile(context.Background(), room, nil)
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
func TestReviewAPI(t *testing.T) {
	s, rid := apiServer(t) // machine "box", preset "claude" available
	state, _ := filepath.EvalSymlinks(t.TempDir())
	s.cfg.StateDir = state
	s.initJobs()
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"claude@box"}})
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/reviews", `{"committee":"solo","question":"ok?","scope":"none","request_id":"k"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var sum reviewSummary
	decode(t, rec.Body.String(), &sum)
	if sum.Committee != "solo" || len(sum.Members) != 1 || sum.Status != "running" {
		t.Fatalf("summary: %+v", sum)
	}
	if rec := do(t, h, "POST", "/api/rooms/"+rid+"/reviews", `{"committee":"solo","question":"different","scope":"none","request_id":"k"}`, mut()); rec.Code != 409 {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/rooms/"+rid+"/reviews", `{"committee":"nobody","question":"q"}`, mut()); rec.Code != 404 {
		t.Fatalf("unknown committee: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/reviews", "", ownerHdr())
	var list []reviewSummary
	decode(t, rec.Body.String(), &list)
	if len(list) != 1 || list[0].ReviewID != sum.ReviewID {
		t.Fatalf("list: %s", rec.Body)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/reviews/"+sum.ReviewID, "", ownerHdr())
	var det reviewDetail
	decode(t, rec.Body.String(), &det)
	if det.Question != "ok?" || det.Scope != "none" {
		t.Fatalf("detail: %s", rec.Body)
	}
	rec = do(t, h, "POST", "/api/rooms/"+rid+"/reviews/"+sum.ReviewID+"/cancel", "", mut())
	decode(t, rec.Body.String(), &sum)
	if rec.Code != 200 || sum.Status != "cancelled" {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/rooms/"+rid+"/reviews/rv-../../x", "", ownerHdr()); rec.Code == 200 {
		t.Fatal("traversal accepted")
	}
}
func TestCommitteeMentionEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts")
	}
	s, rid := apiServer(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TINCAN_STATE_DIR", state)
	s.cfg.StateDir = state
	script := filepath.Join(t.TempDir(), "reviewer")
	os.WriteFile(script, []byte("#!/bin/sh\necho LGTM from the committee\n"), 0o755)
	s.cfg.Dispatch.Presets["reviewer"] = host.Preset{Exec: []string{script, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}
	s.cfg.Dispatch.Executable = buildTincan(t)
	s.initJobs()
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"reviewer@box"}})
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"t","primary":"claude"}`, mut())
	var th struct {
		ID string `json:"id"`
	}
	decode(t, rec.Body.String(), &th)
	do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+th.ID+"/messages", `{"text":"@solo please check"}`, mut())
	room, _, _ := s.cfg.Registry.Get(rid)
	t.Cleanup(func() {
		s.jobs.Wait()
		ws, _ := os.ReadDir(filepath.Join(state, "reviews", "ws"))
		for _, j := range ws {
			host.Down(context.Background(), filepath.Join(state, "reviews", "ws", j.Name()), "reviewer", 5*time.Second)
		}
	})
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		s.runRoomPass(context.Background(), room, false)
		tt, _ := thread.Open(room.Path, th.ID)
		snap, _ := tt.Snapshot()
		var cm, rv *thread.Message
		for i := range snap.Messages {
			switch snap.Messages[i].Role {
			case thread.RoleCommittee:
				cm = &snap.Messages[i]
			case thread.RoleReview:
				rv = &snap.Messages[i]
			}
		}
		if cm != nil && cm.State == thread.StateDone && rv != nil {
			if !strings.Contains(rv.Text, "LGTM from the committee") {
				t.Fatalf("review message: %+v", rv)
			}
			st, _ := review.ReadState(room.Path, cm.Review)
			if !st.ThreadDone {
				t.Fatalf("thread_done not set: %+v", st)
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("committee mention did not complete")
}
