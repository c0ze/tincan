package request

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/filelock"
	"github.com/c0ze/tincan/internal/spool"
)

func pollFixture(t *testing.T, room string) (Record, *spool.Spool) {
	t.Helper()
	r, err := SubmitInteractive(context.Background(), room, "interactive", "test", "task", "poll-job")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the interactive receiver accepting the original request.
	if _, err := sp.Recv(r.Agent, time.Second, false); err != nil {
		t.Fatal(err)
	}
	return r, sp
}

func sendPollReply(t *testing.T, sp *spool.Spool, r Record, from, body string) {
	t.Helper()
	if err := sp.Send(&envelope.Envelope{From: from, To: r.Envelope.ReplyTo, Body: body}); err != nil {
		t.Fatal(err)
	}
}

func assertNoReplyClaims(t *testing.T, sp *spool.Spool, channel string) {
	t.Helper()
	claims, err := sp.ListInFlight(channel)
	if err != nil || len(claims) != 0 {
		t.Fatalf("reply claims=%v, %v", claims, err)
	}
}

func TestPollRecoversReplyAfterCollectorProcessExit(t *testing.T) {
	if room := os.Getenv("TINCAN_TEST_POLL_ROOM"); room != "" {
		r, err := Get(room, "poll-job")
		if err != nil {
			os.Exit(2)
		}
		owner, err := filelock.Acquire(context.Background(), pollLockPath(room, r.Envelope.ReplyTo))
		if err != nil {
			os.Exit(3)
		}
		defer owner.Close()
		sp, err := spool.Open(room)
		if err != nil {
			os.Exit(4)
		}
		d, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second)
		if err != nil {
			os.Exit(5)
		}
		if os.Getenv("TINCAN_TEST_POLL_STAGE") == "finished" {
			if _, err := Finish(room, r.Envelope, d.Envelope.Body); err != nil {
				os.Exit(6)
			}
		}
		os.Exit(0) // OS releases ownership; the durable reply remains unacknowledged.
	}
	for _, stage := range []string{"claimed", "finished"} {
		t.Run(stage, func(t *testing.T) {
			room := t.TempDir()
			r, sp := pollFixture(t, room)
			sendPollReply(t, sp, r, r.Agent, "durable interactive answer")
			cmd := exec.Command(os.Args[0], "-test.run=^TestPollRecoversReplyAfterCollectorProcessExit$")
			cmd.Env = append(os.Environ(), "TINCAN_TEST_POLL_ROOM="+room, "TINCAN_TEST_POLL_STAGE="+stage)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("collector subprocess: %v: %s", err, out)
			}
			finished, err := Poll(context.Background(), room, r.ID)
			if err != nil || finished.Status != "completed" || finished.Result != "durable interactive answer" {
				t.Fatalf("recovered result=%+v, %v", finished, err)
			}
			assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
			if _, err := sp.Recv(r.Agent, 20*time.Millisecond, false); !errors.Is(err, spool.ErrTimeout) {
				t.Fatalf("recovery resubmitted original work: %v", err)
			}
		})
	}
}

func TestConcurrentWaitersRecoverOneReplyWithoutStealingClaims(t *testing.T) {
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	sendPollReply(t, sp, r, r.Agent, "shared result")
	if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := Wait(context.Background(), room, r.ID, time.Second)
			if err != nil || got.Status != "completed" || got.Result != "shared result" {
				t.Errorf("waiter result=%+v, %v", got, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
}

func TestPollWaitsForCollectorOwnershipAndCanCancel(t *testing.T) {
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	sendPollReply(t, sp, r, r.Agent, "still owned")
	owner, err := filelock.Acquire(context.Background(), pollLockPath(room, r.Envelope.ReplyTo))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Poll(ctx, room, r.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("competing collector did not wait: %v", err)
	}
	current, err := Get(room, r.ID)
	if err != nil || current.Terminal() {
		t.Fatalf("competing waiter changed result=%+v, %v", current, err)
	}
	claims, err := sp.ListInFlight(r.Envelope.ReplyTo)
	if err != nil || len(claims) != 1 {
		t.Fatalf("live collector's claim changed=%v, %v", claims, err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	finished, err := Poll(context.Background(), room, r.ID)
	if err != nil || finished.Result != "still owned" {
		t.Fatalf("later recovery=%+v, %v", finished, err)
	}
}

func TestPollPreservesTerminalResultWhileCleaningUnacknowledgedReply(t *testing.T) {
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	if _, err := Finish(room, r.Envelope, "first durable result"); err != nil {
		t.Fatal(err)
	}
	sendPollReply(t, sp, r, r.Agent, "late different answer")
	if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
		t.Fatal(err)
	}
	finished, err := Poll(context.Background(), room, r.ID)
	if err != nil || finished.Result != "first durable result" {
		t.Fatalf("terminal result changed=%+v, %v", finished, err)
	}
	assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
}

func TestPollKeepsReplyWhenSavingResultFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires enforced POSIX directory permissions")
	}
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	sendPollReply(t, sp, r, r.Agent, "must survive failed save")
	if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
		t.Fatal(err)
	}
	owner, err := filelock.Acquire(context.Background(), pollLockPath(room, r.Envelope.ReplyTo))
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(Dir(room), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(Dir(room), 0o700)
	if _, err := Poll(context.Background(), room, r.ID); err == nil {
		t.Fatal("result save unexpectedly succeeded")
	}
	claims, err := sp.ListInFlight(r.Envelope.ReplyTo)
	if err != nil || len(claims) != 1 {
		t.Fatalf("failed save discarded reply=%v, %v", claims, err)
	}
	current, err := Get(room, r.ID)
	if err != nil || current.Terminal() {
		t.Fatalf("failed save changed journal=%+v, %v", current, err)
	}
	if err := os.Chmod(Dir(room), 0o700); err != nil {
		t.Fatal(err)
	}
	finished, err := Poll(context.Background(), room, r.ID)
	if err != nil || finished.Result != "must survive failed save" {
		t.Fatalf("retry=%+v, %v", finished, err)
	}
	assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
}

func TestPollValidatesSenderOnRecoveredReply(t *testing.T) {
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	sendPollReply(t, sp, r, "different-agent", "untrusted answer")
	if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
		t.Fatal(err)
	}
	current, err := Poll(context.Background(), room, r.ID)
	if err == nil || !strings.Contains(err.Error(), "does not match") || current.Terminal() {
		t.Fatalf("mismatched recovered reply=%+v, %v", current, err)
	}
	assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
	sendPollReply(t, sp, r, r.Agent, "valid later answer")
	finished, err := Poll(context.Background(), room, r.ID)
	if err != nil || finished.Result != "valid later answer" {
		t.Fatalf("valid later reply=%+v, %v", finished, err)
	}
}

func TestPollAcceptsLegacyReplyWithoutSender(t *testing.T) {
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	sendPollReply(t, sp, r, "", "legacy anonymous reply")
	if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
		t.Fatal(err)
	}
	finished, err := Poll(context.Background(), room, r.ID)
	if err != nil || finished.Status != "completed" || finished.Result != "legacy anonymous reply" {
		t.Fatalf("legacy reply=%+v, %v", finished, err)
	}
	assertNoReplyClaims(t, sp, r.Envelope.ReplyTo)
}

func TestPollDoesNotCollectReplyAfterHostedHandoff(t *testing.T) {
	room := t.TempDir()
	r, sp := pollFixture(t, room)
	started, err := Start(room, r.Envelope)
	if err != nil || started.Interactive || started.Status != "running" {
		t.Fatalf("handoff=%+v, %v", started, err)
	}
	sendPollReply(t, sp, r, r.Agent, "must not finish native work")
	if _, err := sp.ClaimContext(context.Background(), r.Envelope.ReplyTo, time.Second); err != nil {
		t.Fatal(err)
	}
	current, err := Poll(context.Background(), room, r.ID)
	if err != nil || current.Status != "running" || current.Result != "" {
		t.Fatalf("reply changed native journal=%+v, %v", current, err)
	}
	claims, err := sp.ListInFlight(r.Envelope.ReplyTo)
	if err != nil || len(claims) != 1 {
		t.Fatalf("native handoff reply was claimed/acked=%v, %v", claims, err)
	}
}
