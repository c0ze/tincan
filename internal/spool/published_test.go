package spool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPublishedReportsQueuedInFlightAndConflict(t *testing.T) {
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := msg("orch", "worker", "task", 1)
	if found, err := sp.Published(e); err != nil || found {
		t.Fatalf("before send: %v %v", found, err)
	}
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	if found, err := sp.Published(e); err != nil || !found {
		t.Fatalf("queued: %v %v", found, err)
	}
	other := *e
	other.Body = "different"
	if _, err := sp.Published(&other); !errors.Is(err, ErrEnvelopeConflict) {
		t.Fatalf("conflict: %v", err)
	}
	d, err := sp.ClaimContext(context.Background(), "worker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := sp.Published(e); err != nil || !found {
		t.Fatalf("in flight: %v %v", found, err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	if found, err := sp.Published(e); err != nil || found {
		t.Fatalf("acknowledged: %v %v", found, err)
	}
}
