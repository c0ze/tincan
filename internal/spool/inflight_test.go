package spool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
)

// An interactive receive killed between claim and Ack (SIGKILL, a failed
// Nack) must not strand its message: the next receive of that name requeues
// it, as PROTOCOL promises that a receive which re-arms loses nothing.
func TestAbandonedInteractiveClaimIsRequeued(t *testing.T) {
	if room := os.Getenv("TINCAN_TEST_INTERACTIVE_ROOM"); room != "" {
		sp, err := Open(room)
		if err != nil {
			os.Exit(2)
		}
		if _, err := sp.ClaimInteractive(context.Background(), "worker", time.Second); err != nil {
			os.Exit(3)
		}
		os.Exit(0) // abrupt loss after the claim, without Ack, Nack or cleanup
	}
	room := t.TempDir()
	sp, _ := Open(room)
	e := msg("orch", "worker", "do not lose me", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbandonedInteractiveClaimIsRequeued$")
	cmd.Env = append(os.Environ(), "TINCAN_TEST_INTERACTIVE_ROOM="+room)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claim subprocess: %v: %s", err, out)
	}
	if p, _, _ := sp.Present("worker"); p.Queued != 0 || p.InFlight != 1 {
		t.Fatalf("presence = %+v, want the abandoned claim counted in flight", p)
	}
	got, err := sp.Recv("worker", time.Second, false)
	if err != nil || got.ID != e.ID {
		t.Fatalf("abandoned claim not requeued: %+v, %v", got, err)
	}
	if entries, _ := os.ReadDir(sp.ownerDir("worker")); len(entries) != 0 {
		t.Fatalf("owner records left: %v", entries)
	}
	if p, _, _ := sp.Present("worker"); p.Queued != 0 || p.InFlight != 0 {
		t.Fatalf("presence after delivery = %+v", p)
	}
}

// A live owner's claim is never requeued, and Ack and Nack clear the record.
func TestLiveInteractiveClaimIsNotRequeued(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("orch", "worker", "mine", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimInteractive(context.Background(), "worker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(sp.ownerDir("worker")); len(entries) != 1 {
		t.Fatalf("owner records = %v, want one", entries)
	}
	if _, err := sp.ClaimInteractive(context.Background(), "worker", 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("live claim requeued: %v", err)
	}
	if err := d.Nack(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(sp.ownerDir("worker")); len(entries) != 0 {
		t.Fatalf("Nack left owner records: %v", entries)
	}
	d, err = sp.ClaimInteractive(context.Background(), "worker", time.Second)
	if err != nil || d.Envelope.ID != e.ID {
		t.Fatalf("Nack did not requeue: %v", err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(sp.ownerDir("worker")); len(entries) != 0 {
		t.Fatalf("Ack left owner records: %v", entries)
	}
}

// A record naming a process that no longer holds its PID (here: this PID with
// another start identity) is as dead as an exited PID. Hosted and request
// claims carry no record and are never requeued by a receive.
func TestOnlyRecordedClaimsOfDeadOwnersAreRequeued(t *testing.T) {
	requireStartIdentity(t)
	sp, _ := Open(t.TempDir())
	interactive := msg("orch", "worker", "interactive", 1)
	hosted := msg("orch", "worker", "hosted", 2)
	for _, e := range []*envelope.Envelope{interactive, hosted} {
		if err := sp.Send(e); err != nil {
			t.Fatal(err)
		}
	}
	first, err := sp.ClaimInteractive(context.Background(), "worker", time.Second)
	if err != nil || first.Envelope.ID != interactive.ID {
		t.Fatalf("claim: %v", err)
	}
	if _, err := sp.ClaimContext(context.Background(), "worker", time.Second); err != nil {
		t.Fatal(err)
	}
	// Rewrite the record as if a previous process with this PID had claimed.
	rec := filepath.Join(sp.ownerDir("worker"), envelope.Filename(interactive))
	if err := os.WriteFile(rec, []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"since":"2026-01-01T00:00:00Z","start":"an-earlier-process"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := sp.Recv("worker", time.Second, false)
	if err != nil || got.ID != interactive.ID {
		t.Fatalf("got %+v, %v; want the dead owner's claim requeued", got, err)
	}
	if _, err := sp.Recv("worker", 50*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("hosted claim requeued: %v", err)
	}
	claims, err := sp.ListInFlight("worker")
	if err != nil || len(claims) != 1 || claims[0].Envelope.ID != hosted.ID {
		t.Fatalf("in flight = %v, %v; want only the hosted claim", claims, err)
	}
}
