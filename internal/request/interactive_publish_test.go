package request

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/spool"
)

func TestInteractiveRetryDoesNotRepublishConsumedRequestBeforeReply(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	r, err := SubmitInteractive(ctx, room, "interactive", "test", "one side effect", "interactive-once")
	if err != nil || !r.InteractivePublished {
		t.Fatalf("publication=%+v, %v", r, err)
	}
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := sp.Recv(r.Agent, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy /listen removed the envelope, but has not replied or changed
	// the queued journal status. Retrying that status must not dispatch again.
	for i := 0; i < 4; i++ {
		retry, err := SubmitInteractive(ctx, room, r.Agent, "test", "one side effect", r.ID)
		if err != nil || retry.Status != "queued" || !retry.InteractivePublished {
			t.Fatalf("retry=%+v, %v", retry, err)
		}
	}
	if _, err := sp.Recv(r.Agent, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
		t.Fatalf("interactive work was republished: %v", err)
	}
	if err := sp.Send(&envelope.Envelope{From: r.Agent, To: accepted.ReplyTo, Body: "eventual result"}); err != nil {
		t.Fatal(err)
	}
	finished, err := Wait(ctx, room, r.ID, time.Second)
	if err != nil || finished.Status != "completed" || finished.Result != "eventual result" {
		t.Fatalf("later reply=%+v, %v", finished, err)
	}
}

func TestInteractivePublicationRecoveryAcrossProcessExit(t *testing.T) {
	if room := os.Getenv("TINCAN_TEST_PUBLICATION_ROOM"); room != "" {
		owner, err := lock(context.Background(), room, "publication-crash")
		if err != nil {
			os.Exit(2)
		}
		defer owner.Close()
		e := &envelope.Envelope{ID: "publication-crash", From: "test", To: "interactive", ReplyTo: "r-publication-crash", CorrID: "r-publication-crash", TS: time.Now().UTC(), Body: "one side effect"}
		r := newRecord(e)
		r.Interactive = true
		if err := save(room, r); err != nil {
			os.Exit(3)
		}
		stage := os.Getenv("TINCAN_TEST_PUBLICATION_STAGE")
		if stage != "intent" {
			sp, err := spool.Open(room)
			if err != nil {
				os.Exit(4)
			}
			if err := sp.Send(e); err != nil {
				os.Exit(5)
			}
			if stage == "consumed" || stage == "replied" {
				if _, err := sp.Recv(e.To, time.Second, false); err != nil {
					os.Exit(6)
				}
				if stage == "replied" {
					if err := sp.Send(&envelope.Envelope{From: e.To, To: e.ReplyTo, Body: "reply proves the result"}); err != nil {
						os.Exit(8)
					}
				}
			} else if stage == "claimed" {
				if _, err := sp.ClaimContext(context.Background(), e.To, time.Second); err != nil {
					os.Exit(7)
				}
			}
		}
		os.Exit(0) // lose the process before saving publication confirmation
	}
	for _, stage := range []string{"intent", "queued", "claimed", "consumed"} {
		t.Run(stage, func(t *testing.T) {
			room := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestInteractivePublicationRecoveryAcrossProcessExit$")
			cmd.Env = append(os.Environ(), "TINCAN_TEST_PUBLICATION_ROOM="+room, "TINCAN_TEST_PUBLICATION_STAGE="+stage)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("dispatch subprocess: %v: %s", err, out)
			}
			r, err := SubmitInteractive(context.Background(), room, "interactive", "test", "one side effect", "publication-crash")
			if err != nil {
				t.Fatal(err)
			}
			sp, err := spool.Open(room)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "queued" || stage == "claimed" {
				if !r.InteractivePublished || r.Status != "queued" {
					t.Fatalf("existing publication not confirmed: %+v", r)
				}
			} else if r.Status != "interrupted" || !strings.Contains(r.Result, "not resent") || r.InteractivePublished {
				t.Fatalf("ambiguous publication not reported explicitly: %+v", r)
			}
			if stage == "queued" {
				if _, err := sp.Recv(r.Agent, time.Second, false); err != nil {
					t.Fatalf("original queue lost: %v", err)
				}
			}
			if _, err := SubmitInteractive(context.Background(), room, "interactive", "test", "one side effect", r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := sp.Recv(r.Agent, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
				t.Fatalf("crash recovery republished work: %v", err)
			}
		})
	}
}

func TestNativeSubmitStillRepairsInterruptedEnqueue(t *testing.T) {
	room := t.TempDir()
	e := &envelope.Envelope{ID: "native-enqueue", From: "test", To: "worker", TS: time.Now().UTC(), Body: "native task"}
	if err := save(room, newRecord(e)); err != nil {
		t.Fatal(err)
	}
	r, err := Submit(context.Background(), room, "worker", "test", "native task", e.ID)
	if err != nil || r.Interactive || r.Status != "queued" {
		t.Fatalf("native repair=%+v, %v", r, err)
	}
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := sp.Recv("worker", time.Second, false)
	if err != nil || accepted.ID != e.ID {
		t.Fatalf("native enqueue was not repaired: %+v, %v", accepted, err)
	}
}

func TestWaitReconcilesPublicationCrashWithoutResubmission(t *testing.T) {
	for _, stage := range []string{"intent", "replied"} {
		t.Run(stage, func(t *testing.T) {
			room := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestInteractivePublicationRecoveryAcrossProcessExit$")
			cmd.Env = append(os.Environ(), "TINCAN_TEST_PUBLICATION_ROOM="+room, "TINCAN_TEST_PUBLICATION_STAGE="+stage)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("dispatch subprocess: %v: %s", err, out)
			}
			result, err := Wait(context.Background(), room, "publication-crash", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "intent" {
				if result.Status != "interrupted" || !strings.Contains(result.Result, "not resent") {
					t.Fatalf("abandoned intent stayed pending: %+v", result)
				}
			} else if result.Status != "completed" || result.Result != "reply proves the result" {
				t.Fatalf("uncertainty overrode an existing reply: %+v", result)
			}
			sp, err := spool.Open(room)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sp.Recv("interactive", 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
				t.Fatalf("Wait republished interactive work: %v", err)
			}
		})
	}
}

func TestWaitLeavesLivePublicationIntentPending(t *testing.T) {
	room := t.TempDir()
	e := &envelope.Envelope{ID: "live-publication", From: "test", To: "interactive", ReplyTo: "r-live-publication", TS: time.Now().UTC(), Body: "one task"}
	owner, err := lock(context.Background(), room, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	r := newRecord(e)
	r.Interactive = true
	if err := save(room, r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	pending, err := Wait(ctx, room, e.ID, 10*time.Millisecond)
	if err != nil || pending.Status != "queued" {
		t.Fatalf("live publisher was interrupted or blocked Wait: %+v, %v", pending, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	finished, err := Wait(context.Background(), room, e.ID, time.Second)
	if err != nil || finished.Status != "interrupted" {
		t.Fatalf("abandoned publication not resolved: %+v, %v", finished, err)
	}
}
