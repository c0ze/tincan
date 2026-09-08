package spool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTryClaimCollectsQueuedMessageWithoutParking(t *testing.T) {
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d, ok, err := sp.TryClaim(context.Background(), "worker"); err != nil || ok || d != nil {
		t.Fatalf("empty queue: %v, %v, %v", d, ok, err)
	}
	e := msg("orch", "worker", "already queued", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	d, ok, err := sp.TryClaim(context.Background(), "worker")
	if err != nil || !ok || d == nil {
		t.Fatalf("queued message: %v, %v, %v", d, ok, err)
	}
	if d.Envelope.ID != e.ID || d.Envelope.Body != e.Body {
		t.Fatalf("wrong delivery: %+v", d.Envelope)
	}
	if _, present, err := sp.Present("worker"); err != nil || present {
		t.Fatalf("TryClaim parked a receiver: %v, %v", present, err)
	}
	if next, ok, err := sp.TryClaim(context.Background(), "worker"); err != nil || ok || next != nil {
		t.Fatalf("claimed work was duplicated: %v, %v, %v", next, ok, err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
}

func TestTryClaimDoesNotWaitOnBusyTransport(t *testing.T) {
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Send(msg("orch", "worker", "owned", 1)); err != nil {
		t.Fatal(err)
	}
	owner, err := sp.lockTransport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, ok, err := sp.TryClaim(ctx, "worker")
	if err != nil || ok || d != nil {
		t.Fatalf("busy transport: %v, %v, %v", d, ok, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	d, ok, err = sp.TryClaim(context.Background(), "worker")
	if err != nil || !ok || d == nil {
		t.Fatalf("work lost after contention: %v, %v, %v", d, ok, err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, _, err := sp.TryClaim(ctx, "worker"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call: %v", err)
	}
}
