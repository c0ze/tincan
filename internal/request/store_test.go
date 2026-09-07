package request

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"

	"github.com/c0ze/tincan/internal/fsutil"
	"github.com/c0ze/tincan/internal/spool"
)

func TestConcurrentIdempotentSubmissionAndTerminalReplay(t *testing.T) {
	room := t.TempDir()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Submit(context.Background(), room, "worker", "test", "one task", "same-id"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	sp, _ := spool.Open(room)
	d, err := sp.ClaimContext(context.Background(), "worker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(room, d.Envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := Finish(room, d.Envelope, "done"); err != nil {
		t.Fatal(err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	r, err := Submit(context.Background(), room, "worker", "test", "one task", "same-id")
	if err != nil || !r.Terminal() || r.Result != "done" {
		t.Fatalf("replay: %+v %v", r, err)
	}
	if _, err := sp.ClaimContext(context.Background(), "worker", 20*time.Millisecond); !errors.Is(err, spool.ErrTimeout) {
		t.Fatalf("duplicate queued: %v", err)
	}
	if _, err := Submit(context.Background(), room, "worker", "test", "different", "same-id"); err == nil {
		t.Fatal("accepted id collision")
	}
}

func TestCancellationAndTimeoutDoNotResubmit(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	r, err := Submit(ctx, room, "worker", "test", "work", "job")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := Wait(ctx, room, r.ID, 10*time.Millisecond)
	if err != nil || pending.Status != "queued" {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	canceled, err := Cancel(ctx, room, r.ID)
	if err != nil || canceled.Status != "canceled" {
		t.Fatalf("cancel: %+v %v", canceled, err)
	}
	started, err := Start(room, r.Envelope)
	if err != nil || !started.Terminal() {
		t.Fatalf("canceled work started: %+v %v", started, err)
	}
	finished, err := Finish(room, r.Envelope, "late answer")
	if err != nil || finished.Status != "canceled" || finished.Result == "late answer" {
		t.Fatalf("cancellation overwritten: %+v %v", finished, err)
	}
	sp, _ := spool.Open(room)
	p, _, err := sp.Present("worker")
	if err != nil || p.Queued != 1 {
		t.Fatalf("timeout duplicated/lost queued work: %+v %v", p, err)
	}
}

func TestProgressCursorAndBound(t *testing.T) {
	room := t.TempDir()
	for range 200 {
		if err := AppendProgress(room, "job", "stdout", []byte(strings.Repeat("a", 32*1024))); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(progressPath(room, "job"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > MaxProgressBytes {
		t.Fatalf("unbounded progress %d", info.Size())
	}
	events, cursor, err := Progress(room, "job", 0, 1)
	if err != nil || len(events) != 1 || cursor == 0 {
		t.Fatalf("progress: %d %d %v", len(events), cursor, err)
	}
	_, next, err := Progress(room, "job", cursor, 1)
	if err != nil || next <= cursor {
		t.Fatalf("cursor did not advance: %d %d %v", cursor, next, err)
	}
}

func TestGCRetainsQueuedAndInflightTerminalRecords(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	sp, _ := spool.Open(room)
	for _, id := range []string{"queued", "inflight", "done"} {
		r, err := Submit(ctx, room, "worker-"+id, "test", "work", id)
		if err != nil {
			t.Fatal(err)
		}
		var d *spool.Delivery
		if id != "queued" {
			d, err = sp.ClaimContext(ctx, r.Agent, time.Second)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err = Finish(room, r.Envelope, "completed"); err != nil {
			t.Fatal(err)
		}
		if id == "done" {
			if err = d.Ack(false); err != nil {
				t.Fatal(err)
			}
		}
		r, err = Get(room, id)
		if err != nil {
			t.Fatal(err)
		}
		r.Updated = time.Now().Add(-3 * time.Hour)
		data, _ := json.Marshal(r)
		if err = fsutil.WriteFileAtomic(recordPath(room, id), data); err != nil {
			t.Fatal(err)
		}
	}
	count, err := GC(ctx, room, time.Now().Add(-time.Hour))
	if err != nil || count != 1 {
		t.Fatalf("gc: %d %v", count, err)
	}
	for _, id := range []string{"queued", "inflight"} {
		if _, err := Get(room, id); err != nil {
			t.Fatalf("lost %s: %v", id, err)
		}
	}
	if _, err := Get(room, "done"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("done not collected: %v", err)
	}
}

func TestEncodedBoundsRemainReadable(t *testing.T) {
	room := t.TempDir()
	r, err := Submit(context.Background(), room, "worker", "test", strings.Repeat("\x00", MaxBodyBytes), ".legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Start(room, r.Envelope); err != nil {
		t.Fatal(err)
	}
	if _, err = Finish(room, r.Envelope, strings.Repeat("\x00", MaxResultBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err = Get(room, r.ID); err != nil {
		t.Fatalf("accepted record became unreadable: %v", err)
	}
	if err = AppendProgress(room, r.ID, "stdout", []byte(strings.Repeat("\x00", 32*1024))); err != nil {
		t.Fatal(err)
	}
	events, cursor, err := Progress(room, r.ID, 0, 100)
	if err != nil || cursor == 0 || len(events) == 0 {
		t.Fatalf("escaped event stuck: %d %d %v", len(events), cursor, err)
	}
}

func TestInteractiveReplyBridgeAndCancellationBoundary(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	r, err := SubmitInteractive(ctx, room, "interactive", "test", "task", "interactive-job")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Cancel(ctx, room, r.ID); err == nil {
		t.Fatal("claimed unsupported interactive cancellation")
	}
	sp, _ := spool.Open(room)
	e, err := sp.Recv("interactive", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = sp.Send(&envelope.Envelope{From: "interactive", To: e.ReplyTo, Body: "interactive result"}); err != nil {
		t.Fatal(err)
	}
	finished, err := Wait(ctx, room, r.ID, time.Second)
	if err != nil || finished.Status != "completed" || finished.Result != "interactive result" {
		t.Fatalf("bridge: %+v %v", finished, err)
	}
}
