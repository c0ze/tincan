package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func publishFixture(t *testing.T) (PublishRequest, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	state := roomDir(t)
	t.Setenv("TINCAN_STATE_DIR", state)
	room := roomDir(t)
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = room
	cmd.Run()
	os.WriteFile(filepath.Join(room, "a.txt"), []byte("a\n"), 0o644)
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "reviewers", Members: []string{"codex@macmini", "grok@cachyos"}, DeadlineMinutes: 20})
	rooms.WriteHeartbeat(state, rooms.Heartbeat{PID: 1, Machine: "macmini", Updated: time.Now()})
	return PublishRequest{StateDir: state, Room: room, Committee: "reviewers", Question: "safe?", Origin: "mcp",
		Registry: rooms.Open(filepath.Join(state, "rooms.json"))}, room
}

func TestPublishCreatesACompleteReview(t *testing.T) {
	req, room := publishFixture(t)
	in, st, err := Publish(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if in.Requester != "macmini" || in.Committee.Version != 1 || in.Deadline.Sub(in.Created) != 20*time.Minute || in.Scope != "uncommitted" {
		t.Fatalf("input: %+v", in)
	}
	if st.Status != "running" || len(st.Members) != 2 || st.Members[1].JobID != in.ReviewID+"-1" || !st.Members[0].ExpiresAt.Equal(in.Deadline.Add(30*time.Minute)) {
		t.Fatalf("state: %+v", st)
	}
	for _, f := range []string{"input.json", "packet.json", "prompts/0.txt", "prompts/1.txt", "review.json"} {
		if _, err := os.Stat(filepath.Join(Dir(room, in.ReviewID), f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if prompt, _ := ReadPrompt(room, in.ReviewID, 1); !strings.Contains(prompt, "grok@cachyos") {
		t.Fatalf("prompt: %q", prompt)
	}
	if list, _ := req.Registry.List(); len(list) != 1 {
		t.Fatalf("room not registered: %+v", list)
	}
}

func TestPublishIsIdempotentPerRequestID(t *testing.T) {
	req, room := publishFixture(t)
	req.RequestID = "k1"
	var wg sync.WaitGroup
	ids := make([]string, 4)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, _, err := Publish(context.Background(), req)
			if err != nil {
				t.Error(err)
			}
			ids[i] = in.ReviewID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent publications diverged: %v", ids)
		}
	}
	if list, _ := List(room); len(list) != 1 {
		t.Fatalf("reviews: %v", list)
	}
	if entries, _ := os.ReadDir(filepath.Join(Root(room), "staging")); len(entries) != 0 {
		t.Fatalf("staging left behind: %d", len(entries))
	}
	req.Question = "different"
	if _, _, err := Publish(context.Background(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("different question: %v", err)
	}
}

func TestPublishRefusals(t *testing.T) {
	req, room := publishFixture(t)
	stale := req
	rooms.WriteHeartbeat(req.StateDir, rooms.Heartbeat{PID: 1, Machine: "macmini", Updated: time.Now().Add(-time.Hour)})
	if _, _, err := Publish(context.Background(), stale); !errors.Is(err, ErrNoCoordinator) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if _, err := os.Stat(Root(room)); err == nil {
		t.Fatal("refused publication wrote reviews")
	}
	rooms.WriteHeartbeat(req.StateDir, rooms.Heartbeat{PID: 1, Machine: "macmini", Updated: time.Now()})
	for name, mutate := range map[string]func(*PublishRequest){
		"unknown committee": func(r *PublishRequest) { r.Committee = "nobody" },
		"empty question":    func(r *PublishRequest) { r.Question = "  " },
		"bad scope":         func(r *PublishRequest) { r.Scope = "sideways" },
		"excluded room": func(r *PublishRequest) {
			r.Room = filepath.Join(r.StateDir, "reviews", "ws", "x")
			os.MkdirAll(r.Room, 0o700)
		},
	} {
		r := req
		mutate(&r)
		if _, _, err := Publish(context.Background(), r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRequestCancel(t *testing.T) {
	req, room := publishFixture(t)
	in, _, _ := Publish(context.Background(), req)
	st, err := RequestCancel(context.Background(), room, in.ReviewID)
	if err != nil || !st.CancelRequested || st.Status != "cancelled" {
		t.Fatalf("%+v %v", st, err)
	}
}
