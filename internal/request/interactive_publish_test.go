package request

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
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
	saved := newRecord(e)
	saved.Publication = publicationPending // left by a Send that reported a failure
	if err := save(room, saved); err != nil {
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

func TestNativeRetryDoesNotRepublishConsumedRequest(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	r, err := Submit(ctx, room, "worker", "test", "one side effect", "native-once")
	if err != nil || r.Publication != publicationAttempted {
		t.Fatalf("publication=%+v, %v", r, err)
	}
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	// An interactive `tincan recv --as worker` consumes the envelope without
	// touching the journal, so the record stays queued.
	if _, err := sp.Recv(r.Agent, time.Second, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		retry, err := Submit(ctx, room, r.Agent, "test", "one side effect", r.ID)
		if err != nil || retry.Status != "interrupted" || !strings.Contains(retry.Result, "not resent") {
			t.Fatalf("retry %d=%+v, %v", i, retry, err)
		}
	}
	if _, err := sp.Recv(r.Agent, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
		t.Fatalf("native work was republished: %v", err)
	}
}

// The record must forbid a resend before the envelope can reach the spool:
// a crash after Send (before any later save) must not leave a retry free to
// publish again once a recv has consumed the first copy.
func TestNativeRecordForbidsResendBeforeEnqueue(t *testing.T) {
	room, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	transport, err := filelock.Acquire(ctx, filepath.Join(room, ".tincan", "transport.lock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Submit(ctx, room, "worker", "test", "task", "ordered")
		done <- err
	}()
	// Send now waits for the transport lock; the record is already final.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := Get(room, "ordered")
		if err == nil && r.Publication == publicationAttempted {
			break
		}
		if time.Now().After(deadline) {
			transport.Close()
			<-done
			t.Fatalf("record still allows a resend while Send runs: %+v, %v", r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	transport.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A Send that reports an error before publishing leaves the request for the
// documented same-ID repair.
func TestNativeSendErrorKeepsSameIDRepair(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	tmp := filepath.Join(room, ".tincan", "tmp")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, nil, 0o600); err != nil { // Send cannot stage its write
		t.Fatal(err)
	}
	r, err := Submit(ctx, room, "worker", "test", "task", "send-error")
	if err == nil || !strings.Contains(err.Error(), "retry the same request_id") {
		t.Fatalf("send error: %+v %v", r, err)
	}
	if saved, err := Get(room, r.ID); err != nil || saved.Publication != publicationPending {
		t.Fatalf("saved %+v %v", saved, err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if r, err = Submit(ctx, room, "worker", "test", "task", r.ID); err != nil || r.Status != "queued" {
		t.Fatalf("repair %+v %v", r, err)
	}
	sp, _ := spool.Open(room)
	if _, err := sp.Recv("worker", time.Second, false); err != nil {
		t.Fatalf("not repaired: %v", err)
	}
}

// Idle reconciliation of an unconfirmed interactive publication must not wait
// for a busy transport: Wait keeps its own deadline and the record stays queued.
func TestWaitDoesNotBlockOnBusyTransport(t *testing.T) {
	room, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := SubmitInteractive(ctx, room, "interactive", "test", "task", "busy-transport")
	if err != nil {
		t.Fatal(err)
	}
	r.InteractivePublished = false
	if err := save(room, r); err != nil {
		t.Fatal(err)
	}
	transport, err := filelock.Acquire(ctx, filepath.Join(room, ".tincan", "transport.lock"))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		r   Record
		err error
	}
	done := make(chan result, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		got, err := Wait(waitCtx, room, r.ID, 10*time.Millisecond)
		done <- result{got, err}
	}()
	select {
	case got := <-done:
		transport.Close()
		if got.err != nil || got.r.Status != "queued" || got.r.InteractivePublished {
			t.Fatalf("wait during busy transport: %+v, %v", got.r, got.err)
		}
	case <-time.After(5 * time.Second):
		transport.Close()
		<-done
		t.Fatal("Wait blocked on the transport lock past its deadline")
	}
	// Once the transport is free, the next poll confirms the publication.
	got, err := Wait(ctx, room, r.ID, 10*time.Millisecond)
	if err != nil || got.Status != "queued" || !got.InteractivePublished {
		t.Fatalf("after transport release: %+v, %v", got, err)
	}
}

// A recv whose output fails Nacks its claim back to the inbox under the
// transport lock, briefly holding the envelope in neither location. A retry
// inspecting publication then must wait for the lock, not report it absent.
func TestRetryInspectsPublicationUnderTransportLock(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "interactive"}[interactive], func(t *testing.T) {
			room, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			submit := Submit
			if interactive {
				submit = SubmitInteractive
			}
			r, err := submit(ctx, room, "worker", "test", "task", "nack-race")
			if err != nil {
				t.Fatal(err)
			}
			if interactive {
				r.InteractivePublished = false // retries must inspect the spool
				if err := save(room, r); err != nil {
					t.Fatal(err)
				}
			}
			sp, err := spool.Open(room)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sp.ClaimContext(ctx, r.Agent, time.Second); err != nil {
				t.Fatal(err)
			}
			transport, err := filelock.Acquire(ctx, filepath.Join(room, ".tincan", "transport.lock"))
			if err != nil {
				t.Fatal(err)
			}
			filename := envelope.Filename(r.Envelope)
			claim := filepath.Join(room, ".tincan", "inflight", r.Agent, filename)
			limbo := filepath.Join(t.TempDir(), "message.json")
			if err := os.Rename(filepath.Join(claim, "message.json"), limbo); err != nil {
				t.Fatal(err)
			}
			type result struct {
				r   Record
				err error
			}
			done := make(chan result, 1)
			go func() {
				retry, err := submit(ctx, room, r.Agent, "test", "task", r.ID)
				done <- result{retry, err}
			}()
			time.Sleep(200 * time.Millisecond) // an unlocked scan sees neither location here
			if err := os.Rename(limbo, filepath.Join(room, ".tincan", "inbox", r.Agent, filename)); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(claim); err != nil {
				t.Fatal(err)
			}
			transport.Close()
			got := <-done
			if got.err != nil || got.r.Status != "queued" {
				t.Fatalf("requeued request misreported: %+v, %v", got.r, got.err)
			}
			if _, err := sp.Recv(r.Agent, time.Second, false); err != nil {
				t.Fatalf("requeued envelope lost: %v", err)
			}
			if _, err := sp.Recv(r.Agent, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
				t.Fatalf("request was resent: %v", err)
			}
		})
	}
}

func TestNativeRetryOfLegacyRecord(t *testing.T) {
	for _, queued := range []bool{true, false} {
		t.Run(map[bool]string{true: "queued", false: "consumed"}[queued], func(t *testing.T) {
			room := t.TempDir()
			ctx := context.Background()
			// A record written before publication confirmation existed.
			e := &envelope.Envelope{ID: "legacy-native", From: "test", To: "worker", TS: time.Now().UTC(), Body: "native task"}
			if err := save(room, newRecord(e)); err != nil {
				t.Fatal(err)
			}
			sp, err := spool.Open(room)
			if err != nil {
				t.Fatal(err)
			}
			if err := sp.Send(e); err != nil {
				t.Fatal(err)
			}
			if !queued {
				if _, err := sp.Recv(e.To, time.Second, false); err != nil {
					t.Fatal(err)
				}
			}
			r, err := Submit(ctx, room, "worker", "test", "native task", e.ID)
			if err != nil {
				t.Fatal(err)
			}
			if queued {
				if r.Status != "queued" {
					t.Fatalf("queued legacy request=%+v", r)
				}
				if _, err := sp.Recv(e.To, time.Second, false); err != nil {
					t.Fatalf("original queue lost: %v", err)
				}
			} else if r.Status != "interrupted" || !strings.Contains(r.Result, "not resent") {
				t.Fatalf("consumed legacy request=%+v", r)
			}
			if _, err := sp.Recv(e.To, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
				t.Fatalf("legacy work was republished: %v", err)
			}
		})
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
