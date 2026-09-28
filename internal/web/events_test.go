// internal/web/events_test.go
package web

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
	"github.com/fsnotify/fsnotify"
)

func TestSSEStreamsNotes(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	req.Header.Set("Tailscale-User-Login", owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.hub.publish(note{Kind: "messages", Room: "r1", Thread: "t1", Seq: 3})
	}()
	sc := bufio.NewScanner(resp.Body)
	deadline := time.After(5 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, `"thread":"t1"`) {
				return
			}
		case <-deadline:
			t.Fatal("no note received")
		}
	}
}

func TestTickPublishesThreadChanges(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	th, _ := thread.Create(room.Path, "t", "claude", "", 6)
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	s.tick(context.Background()) // baseline
	drain(ch)
	th.Update(context.Background(), func(tx *thread.Tx) error {
		tx.Append(thread.Event{Kind: thread.KindMessage, Author: "you", Role: thread.RoleUser, Text: "x"})
		return nil
	})
	s.tick(context.Background())
	select {
	case n := <-ch:
		if n.Kind != "messages" || n.Room != rid || n.Thread != th.ID {
			t.Fatalf("note %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no note after journal change")
	}
}

func TestLoopSkipsMissingRoom(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	os.RemoveAll(room.Path)
	s.tick(context.Background()) // must not panic or block
	if rec := do(t, s.Handler(), "GET", "/api/rooms", "", ownerHdr()); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"missing":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func drain(ch chan note) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// Fix round 1, finding 1: fsnotify's Events channel is unbuffered on
// darwin/linux, so every event used to trigger a full tick, and continuous
// ".log" writes from a running host would otherwise storm the loop.

func TestShouldTriggerFiltersLogWrites(t *testing.T) {
	if shouldTrigger(fsnotify.Event{Name: "/room/.tincan/hosts/claude.log", Op: fsnotify.Write}) {
		t.Fatal("a Write on a .log file should not trigger an early tick")
	}
	if !shouldTrigger(fsnotify.Event{Name: "/room/.tincan/threads/abcd1234/thread.json", Op: fsnotify.Create}) {
		t.Fatal("a Create on a .json file should trigger an early tick")
	}
	if !shouldTrigger(fsnotify.Event{Name: "/room/.tincan/hosts/claude.log", Op: fsnotify.Remove}) {
		t.Fatal("a non-Write op on a .log file should still trigger")
	}
}

func TestDebouncerCoalescesRapidTriggers(t *testing.T) {
	d := newDebouncer(30 * time.Millisecond)
	defer d.stop()
	for i := 0; i < 20; i++ {
		d.arm()
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case <-d.C:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("expected exactly one delivery once the burst went quiet")
	}
	select {
	case <-d.C:
		t.Fatal("expected only one delivery for the whole burst")
	case <-time.After(80 * time.Millisecond):
	}
}

// Fix round 1, finding 2: tick used to reconcile every room serially on the
// loop goroutine, so a slow room (host.Up/host.Down waits) delayed
// reconcile and SSE notes for every other room.

func TestTickRunsRoomPassesConcurrentlyAndSkipsInFlight(t *testing.T) {
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	dirA, _ := filepath.EvalSymlinks(t.TempDir())
	dirB, _ := filepath.EvalSymlinks(t.TempDir())
	qdir := t.TempDir()
	s, err := New(Config{Owner: owner, AllowedHosts: testHosts, Machine: "box", Registry: reg, ChainBudget: 6, QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json")})
	if err != nil {
		t.Fatal(err)
	}
	ra, err := reg.Add(dirA)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := reg.Add(dirB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := thread.Create(ra.Path, "a", "claude", "", 6); err != nil {
		t.Fatal(err)
	}
	thB, err := thread.Create(rb.Path, "b", "claude", "", 6)
	if err != nil {
		t.Fatal(err)
	}

	block := make(chan struct{})
	var callsA, callsB int32
	s.reconcileRoom = func(ctx context.Context, room rooms.Room, janitor bool) {
		if room.ID == ra.ID {
			atomic.AddInt32(&callsA, 1)
			<-block
			return
		}
		atomic.AddInt32(&callsB, 1)
	}

	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	s.tick(context.Background()) // baseline scan; also starts room A's blocking pass
	drain(ch)
	// Room B's first pass runs in its own goroutine; wait until it has fully
	// finished (in-flight flag cleared) so the next tick is not a legitimate skip.
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return atomic.LoadInt32(&callsB) == 1 && !s.inFlight[rb.ID]
	})

	// Room B's thread changes; scan must notice it on the next tick
	// regardless of room A's pass still being in flight.
	if err := thB.Update(context.Background(), func(tx *thread.Tx) error {
		tx.Append(thread.Event{Kind: thread.KindMessage, Author: "you", Role: thread.RoleUser, Text: "x"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		s.tick(context.Background()) // must return promptly, not wait on room A
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not return promptly while a room's pass was in flight")
	}

	select {
	case n := <-ch:
		if n.Room != rb.ID {
			t.Fatalf("expected a note for room B, got %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no note published for room B's thread change")
	}

	if got := atomic.LoadInt32(&callsA); got != 1 {
		t.Fatalf("room A's in-flight pass was re-entered: calls=%d", got)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&callsB) >= 2 })
	if got := atomic.LoadInt32(&callsB); got != 2 {
		t.Fatalf("room B's pass should have run on both ticks: calls=%d", got)
	}

	close(block)
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out)
}

// A room whose Reconcile and Janitor fail the same way every tick logs each
// failure once, not once per tick.
func TestRoomPassErrorsAreLoggedOnce(t *testing.T) {
	s := testServer(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	room, err := s.cfg.Registry.Add(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A complete but corrupt journal line makes Snapshot fail the same way on
	// every platform, so both Reconcile and Janitor report an error.
	th, err := thread.Create(dir, "broken", "claude", "", 6)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(thread.Root(dir), th.ID, "events.jsonl")
	if err := os.WriteFile(journal, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.dropDispatcher(room.ID) })
	out := captureStderr(t, func() {
		for i := 0; i < 3; i++ {
			s.runRoomPass(context.Background(), room, true)
		}
	})
	if n := strings.Count(out, "reconcile:"); n != 1 {
		t.Errorf("reconcile error logged %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "janitor:"); n != 1 {
		t.Errorf("janitor error logged %d times, want 1:\n%s", n, out)
	}
}

// waitFor polls cond until it holds or 5 s pass.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestTickWritesHeartbeat(t *testing.T) {
	s := testServer(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	s.cfg.StateDir = state
	s.tick(context.Background())
	hb, err := os.ReadFile(rooms.HeartbeatPath(state))
	if err != nil || !strings.Contains(string(hb), `"machine": "testbox"`) {
		t.Fatalf("heartbeat: %s %v", hb, err)
	}
	if !rooms.CoordinatorAlive(state, time.Now()) {
		t.Fatal("tick did not make the coordinator alive")
	}
}
