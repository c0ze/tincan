package spool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
)

func TestPublishedReportsQueuedInFlightAndConflict(t *testing.T) {
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := msg("orch", "worker", "task", 1)
	if found, err := sp.Published(context.Background(), e); err != nil || found {
		t.Fatalf("before send: %v %v", found, err)
	}
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	if found, err := sp.Published(context.Background(), e); err != nil || !found {
		t.Fatalf("queued: %v %v", found, err)
	}
	other := *e
	other.Body = "different"
	if _, err := sp.Published(context.Background(), &other); !errors.Is(err, ErrEnvelopeConflict) {
		t.Fatalf("conflict: %v", err)
	}
	d, err := sp.ClaimContext(context.Background(), "worker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := sp.Published(context.Background(), e); err != nil || !found {
		t.Fatalf("in flight: %v %v", found, err)
	}
	if found, err := sp.TryPublished(e); err != nil || !found {
		t.Fatalf("try in flight: %v %v", found, err)
	}
	lock, err := sp.lockTransport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.TryPublished(e); !errors.Is(err, filelock.ErrLocked) {
		t.Fatalf("try while busy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err = sp.Published(ctx, e)
	cancel()
	lock.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait while busy: %v", err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	if found, err := sp.Published(context.Background(), e); err != nil || found {
		t.Fatalf("acknowledged: %v %v", found, err)
	}
}
