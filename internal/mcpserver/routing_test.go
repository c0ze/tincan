package mcpserver_test

import (
	"context"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/host"
	"github.com/c0ze/tincan/internal/request"
	"github.com/c0ze/tincan/internal/spool"
)

func TestBusyInteractiveRouteAcrossMCPConnections(t *testing.T) {
	room := t.TempDir()
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type received struct {
		d   *spool.Delivery
		err error
	}
	got := make(chan received, 1)
	go func() {
		d, err := sp.ClaimContext(ctx, "fixture", 0)
		got <- received{d, err}
	}()
	for {
		if _, present, err := sp.Present("fixture"); err != nil {
			t.Fatal(err)
		} else if present {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("interactive receiver did not park")
		case <-time.After(10 * time.Millisecond):
		}
	}
	firstConnection := connect(t, room)
	call(t, firstConnection, "tincan_send", map[string]any{"agent": "fixture", "body": "first", "request_id": "interactive-first"})
	first := <-got
	if first.err != nil {
		t.Fatal(first.err)
	}
	if err := first.d.Ack(false); err != nil {
		t.Fatal(err)
	}
	if _, present, err := sp.Present("fixture"); err != nil || present {
		t.Fatalf("receiver should be busy without presence: %v, %v", present, err)
	}
	firstConnection.Close()
	secondConnection := connect(t, room)
	call(t, secondConnection, "tincan_send", map[string]any{"agent": "fixture", "body": "second", "request_id": "interactive-second"})
	second, err := request.Get(room, "interactive-second")
	if err != nil || !second.Interactive || second.Envelope.ReplyTo == "" {
		t.Fatalf("second task changed to hosted routing: %+v, %v", second, err)
	}
	if _, exists, err := host.ReadState(room, "fixture"); err != nil || exists {
		t.Fatalf("busy interactive listener was auto-hosted: %v, %v", exists, err)
	}
	// A status request must collect an ordinary interactive reply too.
	if err := sp.Send(&envelope.Envelope{From: "fixture", To: first.d.Envelope.ReplyTo, Body: "first answer"}); err != nil {
		t.Fatal(err)
	}
	status := call(t, secondConnection, "tincan_status", map[string]any{"request_id": "interactive-first"})
	view, ok := status["request"].(map[string]any)
	if !ok || view["status"] != "completed" || view["result"] != "first answer" {
		t.Fatalf("status failed to collect interactive result: %+v", status)
	}
	message, err := sp.Recv("fixture", time.Second, false)
	if err != nil || message.ID != "interactive-second" {
		t.Fatalf("second task not retained in interactive queue: %+v, %v", message, err)
	}
}
