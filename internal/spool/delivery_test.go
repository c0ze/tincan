package spool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
)

func TestClaimSurvivesProcessExitAndRecoveryDoesNotRequeue(t *testing.T) {
	if room := os.Getenv("TINCAN_TEST_CLAIM_ROOM"); room != "" {
		sp, err := Open(room)
		if err != nil {
			os.Exit(2)
		}
		if _, err := sp.ClaimContext(context.Background(), "worker", time.Second); err != nil {
			os.Exit(3)
		}
		os.Exit(0) // simulate abrupt process loss, without Ack or cleanup
	}
	room := t.TempDir()
	sp, _ := Open(room)
	e := msg("orch", "worker", "preserved request", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestClaimSurvivesProcessExitAndRecoveryDoesNotRequeue$")
	cmd.Env = append(os.Environ(), "TINCAN_TEST_CLAIM_ROOM="+room)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claim subprocess: %v: %s", err, out)
	}
	reopened, _ := Open(room)
	claims, err := reopened.RecoverInFlight("worker")
	if err != nil || len(claims) != 1 {
		t.Fatalf("recovered claims=%v, err=%v", claims, err)
	}
	if claims[0].Envelope.ID != e.ID || claims[0].Envelope.Body != e.Body {
		t.Fatalf("wrong recovered request: %+v", claims[0].Envelope)
	}
	if _, err := reopened.ClaimContext(context.Background(), "worker", 20*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("recovery requeued work: %v", err)
	}
	if err := claims[0].Ack(false); err != nil {
		t.Fatal(err)
	}
	claims, err = reopened.RecoverInFlight("worker")
	if err != nil || len(claims) != 0 {
		t.Fatalf("ack left recoverable claims=%v, err=%v", claims, err)
	}
}

func TestClaimNackExplicitlyRequeues(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "retryable", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimContext(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Recv("b", 20*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("unacknowledged claim reappeared: %v", err)
	}
	if err := d.Nack(); err != nil {
		t.Fatal(err)
	}
	got, err := sp.Recv("b", time.Second, false)
	if err != nil || got.ID != e.ID {
		t.Fatalf("explicit retry=%+v, err=%v", got, err)
	}
	if err := d.Nack(); err != nil {
		t.Fatalf("repeat Nack: %v", err)
	}
}

func TestRecoveryDoesNotReplayInterruptedNack(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "uncertain", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimContext(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce process loss after Nack's no-overwrite publication but
	// before removal of the durable claim.
	if err := os.Link(d.path(), filepath.Join(sp.InboxDir("b"), envelope.Filename(e))); err != nil {
		t.Fatal(err)
	}
	claims, err := sp.RecoverInFlight("b")
	if err != nil || len(claims) != 1 {
		t.Fatalf("recovery=%v, %v", claims, err)
	}
	if err := claims[0].Ack(false); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Recv("b", 20*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("replayed interrupted Nack: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(sp.root, "quarantine"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("duplicate evidence=%v, %v", entries, err)
	}
}

func TestAckFailureRetainsClaimForRetry(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "important", 1)); err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimContext(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp.logDir(), []byte("blocking path"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Ack(true); err == nil {
		t.Fatal("Ack unexpectedly succeeded")
	}
	claims, err := sp.ListInFlight("b")
	if err != nil || len(claims) != 1 {
		t.Fatalf("failed Ack lost the request: %v, %v", claims, err)
	}
	if err := os.Remove(sp.logDir()); err != nil {
		t.Fatal(err)
	}
	if err := d.Ack(true); err != nil {
		t.Fatal(err)
	}
	if err := d.Ack(true); err != nil {
		t.Fatalf("repeat Ack: %v", err)
	}
	logs, _ := os.ReadDir(sp.logDir())
	if len(logs) != 1 {
		t.Fatalf("want one archived request, got %v", logs)
	}
}

func TestRecoverClearsEmptyGateWithoutDiscardingQueue(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "not started", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(sp.inFlightDir("b"), envelope.Filename(e))
	if err := os.MkdirAll(gate, 0o700); err != nil {
		t.Fatal(err)
	}
	claims, err := sp.RecoverInFlight("b")
	if err != nil || len(claims) != 0 {
		t.Fatalf("empty gate recovery: %v, %v", claims, err)
	}
	got, err := sp.Recv("b", time.Second, false)
	if err != nil || got.ID != e.ID {
		t.Fatalf("queued work lost: %+v, %v", got, err)
	}
}

func TestCanceledClaimDoesNotTakeQueuedWork(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "still queued", 1)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sp.ClaimContext(ctx, "b", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := sp.Recv("b", time.Second, false); err != nil {
		t.Fatalf("canceled claim removed work: %v", err)
	}
}

func TestConcurrentClaimsKeepOneOwnerWhileUnacknowledged(t *testing.T) {
	sp, _ := Open(t.TempDir())
	const count = 12
	for i := 0; i < count; i++ {
		if err := sp.Send(msg("a", "b", "work", int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	claimed := make(chan *Delivery, count*2)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				d, err := sp.ClaimContext(context.Background(), "b", 50*time.Millisecond)
				if errors.Is(err, ErrTimeout) {
					return
				}
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				claimed <- d
			}
		}()
	}
	wg.Wait()
	close(claimed)
	seen := map[string]bool{}
	for d := range claimed {
		if seen[d.Envelope.ID] {
			t.Fatalf("duplicate claim for %s", d.Envelope.ID)
		}
		seen[d.Envelope.ID] = true
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != count {
		t.Fatalf("claimed %d of %d", len(seen), count)
	}
}

func TestSendRejectsUnsafeEnvelopeAndOversize(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(nil); err == nil {
		t.Fatal("nil envelope accepted")
	}
	for _, field := range []string{"ID", "From", "To", "ReplyTo", "CorrID"} {
		t.Run(field, func(t *testing.T) {
			e := msg("a", "b", "x", 1)
			switch field {
			case "ID":
				e.ID = "../escape"
			case "From":
				e.From = "a/b"
			case "To":
				e.To = "NUL"
			case "ReplyTo":
				e.ReplyTo = "../escape"
			case "CorrID":
				e.CorrID = "bad:stream"
			}
			if err := sp.Send(e); err == nil {
				t.Fatal("unsafe envelope accepted")
			}
		})
	}
	e := msg("a", "b", strings.Repeat("x", int(MaxMessageBytes)), 1)
	if err := sp.Send(e); err == nil {
		t.Fatal("oversized envelope accepted")
	}
}

func TestSendDuplicateDoesNotReplaceQueuedMessage(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "original", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	e.Body = "replacement"
	if err := sp.Send(e); !errors.Is(err, ErrEnvelopeConflict) {
		t.Fatalf("conflicting envelope error = %v", err)
	}
	got, err := sp.Recv("b", time.Second, false)
	if err != nil || got.Body != "original" {
		t.Fatalf("original changed: %+v, %v", got, err)
	}
}

func TestSendIdenticalRetriesReuseQueuedAndInFlightEnvelope(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "stable request", 1)
	for i := 0; i < 8; i++ {
		if err := sp.Send(e); err != nil {
			t.Fatalf("queued retry %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(sp.InboxDir("b"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("queue=%v, %v", entries, err)
	}
	d, err := sp.ClaimContext(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := sp.Send(e); err != nil {
			t.Fatalf("in-flight retry %d: %v", i, err)
		}
	}
	conflict := *e
	conflict.Body = "different request"
	if err := sp.Send(&conflict); !errors.Is(err, ErrEnvelopeConflict) {
		t.Fatalf("in-flight conflict error=%v", err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Recv("b", 20*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("retry duplicated work: %v", err)
	}
}

func TestSendRetriesWhileConcurrentReceiverClaims(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "stable", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	claimed := make(chan *Delivery, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		d, err := sp.ClaimContext(context.Background(), "b", 2*time.Second)
		if err != nil {
			t.Errorf("Claim: %v", err)
			return
		}
		claimed <- d // retain the claim through every concurrent retry
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := sp.Send(e); err != nil {
				t.Errorf("Send retry: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	select {
	case d := <-claimed:
		if err := d.Ack(false); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("request never claimed")
	}
	if _, err := sp.Recv("b", 20*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("concurrent retry duplicated work: %v", err)
	}
}

func TestSendRetryAfterTerminalReplyPublishedBeforeRequestAck(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	e := msg("orch", "worker", "original request", 1)
	e.ReplyTo = "r-stable"
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.ClaimContext(context.Background(), "worker", time.Second); err != nil {
		t.Fatal(err)
	}
	reply := &envelope.Envelope{ID: "stable-reply-id", From: "worker", To: e.ReplyTo, CorrID: e.ReplyTo, TS: e.TS, Body: "persisted terminal result"}
	if err := sp.Send(reply); err != nil {
		t.Fatal(err)
	}
	// Reopen after losing the host before it acknowledged the original claim.
	restarted, _ := Open(room)
	claims, err := restarted.RecoverInFlight("worker")
	if err != nil || len(claims) != 1 {
		t.Fatalf("recovery=%v, %v", claims, err)
	}
	if err := restarted.Send(reply); err != nil {
		t.Fatalf("terminal reply retry failed: %v", err)
	}
	if err := claims[0].Ack(false); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Recv(e.ReplyTo, time.Second, false)
	if err != nil || got.Body != reply.Body {
		t.Fatalf("reply=%+v, %v", got, err)
	}
	if _, err := restarted.Recv(e.ReplyTo, 20*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("duplicate terminal reply: %v", err)
	}
}

func TestSendRetryAcceptsEquivalentTimestampAndJSONFormatting(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "stable", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	same := *e
	same.TS = same.TS.In(time.FixedZone("offset", 3600))
	data, err := envelope.Marshal(&same)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp.InboxDir(e.To), envelope.Filename(e)), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sp.Send(e); err != nil {
		t.Fatalf("semantically identical retry rejected: %v", err)
	}
}

func TestPrivateSpoolDefaults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "secret", 1)); err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimContext(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Ack(true); err != nil {
		t.Fatal(err)
	}
	sp.writePresence("b", "private-token")
	if err := filepath.WalkDir(sp.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", path, info.Mode().Perm(), want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMutationRestrictsLegacySpoolRootWithoutChangingRoom(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	room := t.TempDir()
	if err := os.Chmod(room, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(room, ".tincan")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	sp, err := Open(room)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(root)
	if info.Mode().Perm() != 0o755 {
		t.Fatal("read-only Open changed root permissions")
	}
	if err := sp.Send(msg("a", "b", "private", 1)); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(root)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("spool root mode=%o", info.Mode().Perm())
	}
	info, _ = os.Stat(room)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("room mode changed to %o", info.Mode().Perm())
	}
}

func TestGCOnlyRemovesOldTerminalArtifacts(t *testing.T) {
	sp, _ := Open(t.TempDir())
	for i := 0; i < 3; i++ {
		if err := sp.Send(msg("a", "b", "work", int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sp.Recv("b", time.Second, true); err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimContext(context.Background(), "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sp.root, "quarantine", "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour)
	old := cutoff.Add(-time.Hour)
	if err := filepath.WalkDir(sp.root, func(path string, ent os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(sp.logDir(), "fresh.json")
	if err := os.WriteFile(fresh, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, err := sp.GC(cutoff)
	if err != nil || stats.Logs != 1 || stats.Quarantined != 1 {
		t.Fatalf("GC=%+v, err=%v", stats, err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh log removed: %v", err)
	}
	if _, err := os.Stat(d.path()); err != nil {
		t.Fatalf("active claim removed: %v", err)
	}
	if _, err := sp.Recv("b", time.Second, false); err != nil {
		t.Fatalf("queued work removed: %v", err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
}
