// internal/web/events_test.go
package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/thread"
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
