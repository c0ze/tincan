package request

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/filelock"
	"github.com/c0ze/tincan/internal/spool"
)

// Reproduce the durable state after publication confirmation was lost while
// the interactive receiver had already consumed the original envelope.
func lostPublicationFixture(t *testing.T) (string, Record, *spool.Spool) {
	t.Helper()
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	r.InteractivePublished = false
	if err := save(room, r); err != nil {
		t.Fatal(err)
	}
	return room, r, sp
}

func retryLostPublication(t *testing.T, room string, r Record) Record {
	t.Helper()
	retry, err := SubmitInteractive(context.Background(), room, r.Agent, r.Envelope.From, r.Envelope.Body, r.ID)
	if err != nil || retry.Status != "interrupted" || !retry.InteractivePublicationUncertain || !retry.Terminal() {
		t.Fatalf("publication uncertainty=%+v, %v", retry, err)
	}
	return retry
}

func assertResolvedInteractiveReply(t *testing.T, r Record, body string) {
	t.Helper()
	if r.Status != "completed" || r.Result != body || !r.InteractivePublished || r.InteractivePublicationUncertain {
		t.Fatalf("real reply did not resolve publication uncertainty: %+v", r)
	}
}

func TestRetryBeforeWaitPreservesAlreadyQueuedRealReply(t *testing.T) {
	room, r, sp := lostPublicationFixture(t)
	sendPollReply(t, sp, r, r.Agent, "real queued answer")
	retryLostPublication(t, room, r)
	finished, err := Wait(context.Background(), room, r.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedInteractiveReply(t, finished, "real queued answer")
	assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
	retry, err := SubmitInteractive(context.Background(), room, r.Agent, r.Envelope.From, r.Envelope.Body, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedInteractiveReply(t, retry, "real queued answer")
	if _, err := sp.Recv(r.Agent, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
		t.Fatalf("reply resolution resent original work: %v", err)
	}
}

func TestRetryWhileCollectorHoldsClaimDoesNotOverrideItsRealReply(t *testing.T) {
	room, r, sp := lostPublicationFixture(t)
	sendPollReply(t, sp, r, r.Agent, "owned collector answer")
	owner, err := filelock.Acquire(context.Background(), pollLockPath(room, r.Envelope.ReplyTo))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	d, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Submit takes the journal lock independently while the live collector
	// owns a reply it has not yet committed through Finish.
	retryLostPublication(t, room, r)
	pending, err := Wait(context.Background(), room, r.ID, 10*time.Millisecond)
	if err != nil || !pending.InteractivePublicationUncertain {
		t.Fatalf("bounded concurrent wait=%+v, %v", pending, err)
	}
	finished, err := finishInteractiveReply(room, r.Envelope, d.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedInteractiveReply(t, finished, "owned collector answer")
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := Poll(context.Background(), room, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedInteractiveReply(t, current, "owned collector answer")
	assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
	if _, err := sp.Recv(r.Agent, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
		t.Fatalf("concurrent retry resent original work: %v", err)
	}
}

func TestOnlyValidatedReplyCanResolvePublicationUncertainty(t *testing.T) {
	room, r, _ := lostPublicationFixture(t)
	uncertain := retryLostPublication(t, room, r)
	ordinary, err := Finish(room, r.Envelope, "unverified replacement")
	if err != nil || ordinary.Result != uncertain.Result || !ordinary.InteractivePublicationUncertain {
		t.Fatalf("ordinary Finish replaced uncertainty: %+v, %v", ordinary, err)
	}
	started, err := Start(room, r.Envelope)
	if err != nil || started.Status != "interrupted" || !started.Terminal() {
		t.Fatalf("uncertain work could execute again: %+v, %v", started, err)
	}
	for _, mismatch := range []string{"agent", "channel"} {
		reply := &envelope.Envelope{ID: envelope.NewID(), From: r.Agent, To: r.Envelope.ReplyTo, TS: time.Now().UTC(), Body: "untrusted answer"}
		if mismatch == "agent" {
			reply.From = "someone-else"
		} else {
			reply.To = "different-channel"
		}
		if _, err := finishInteractiveReply(room, r.Envelope, reply); err == nil {
			t.Fatalf("accepted mismatched reply %s", mismatch)
		}
	}
	current, err := Get(room, r.ID)
	if err != nil || current.Result != uncertain.Result || !current.InteractivePublicationUncertain {
		t.Fatalf("invalid reply changed uncertainty: %+v, %v", current, err)
	}
}

func TestInteractiveExecutionTerminalResultsRemainImmutable(t *testing.T) {
	for _, body := range []string{"first completed result", "ERROR execution failed", "ERROR interrupted: execution stopped", "ERROR canceled: execution canceled"} {
		t.Run(body, func(t *testing.T) {
			room := t.TempDir()
			r, sp := pollFixture(t, room)
			first, err := Finish(room, r.Envelope, body)
			if err != nil || !first.Terminal() || first.InteractivePublicationUncertain {
				t.Fatalf("original result=%+v, %v", first, err)
			}
			sendPollReply(t, sp, r, r.Agent, "late replacement")
			if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
				t.Fatal(err)
			}
			current, err := Poll(context.Background(), room, r.ID)
			if err != nil || current.Status != first.Status || current.Result != body || current.InteractivePublicationUncertain {
				t.Fatalf("terminal execution result replaced=%+v, %v", current, err)
			}
			assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
		})
	}
}

func TestReplyCollectionDoesNotBlockWaitOnLiveJournalWriter(t *testing.T) {
	for _, state := range []string{"queued", "claimed"} {
		t.Run(state, func(t *testing.T) {
			room := t.TempDir()
			r, sp := pollFixture(t, room)
			sendPollReply(t, sp, r, r.Agent, "answer after writer releases")
			d, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if state == "queued" {
				if err := d.Nack(); err != nil {
					t.Fatal(err)
				}
			}
			owner, err := lock(context.Background(), room, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			type result struct {
				record Record
				err    error
			}
			done := make(chan result, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			go func() { record, err := Wait(ctx, room, r.ID, 10*time.Millisecond); done <- result{record, err} }()
			select {
			case got := <-done:
				if got.err != nil || got.record.Status != "queued" {
					t.Fatalf("bounded wait=%+v, %v", got.record, got.err)
				}
			case <-time.After(150 * time.Millisecond):
				owner.Close()
				<-done
				t.Fatal("reply completion blocked Wait on the journal writer")
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			finished, err := Wait(context.Background(), room, r.ID, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			assertResolvedInteractiveReply(t, finished, "answer after writer releases")
			assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
		})
	}
}
