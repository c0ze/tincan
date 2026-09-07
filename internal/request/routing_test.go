package request

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/fsutil"
	"github.com/c0ze/tincan/internal/spool"
)

func TestInteractiveRouteSurvivesBusyReceiverAndCompletionOrder(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	first, err := SubmitInteractive(ctx, room, "listener", "test", "first", "first")
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(room)
	if _, err := sp.Recv("listener", time.Second, false); err != nil {
		t.Fatal(err)
	}
	if _, present, err := sp.Present("listener"); err != nil || present {
		t.Fatalf("test listener should now be busy and absent: %v, %v", present, err)
	}
	if pending, err := InteractivePending(ctx, room, "listener"); err != nil || !pending {
		t.Fatalf("lost busy interactive route: %v, %v", pending, err)
	}
	second, err := SubmitInteractive(ctx, room, "listener", "test", "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	// Completing one request must not clear another outstanding request's route.
	if _, err := Finish(room, second.Envelope, "second result"); err != nil {
		t.Fatal(err)
	}
	if pending, err := InteractivePending(ctx, room, "listener"); err != nil || !pending {
		t.Fatalf("completion cleared an older outstanding route: %v, %v", pending, err)
	}
	if _, err := Finish(room, first.Envelope, "first result"); err != nil {
		t.Fatal(err)
	}
	if pending, err := InteractivePending(ctx, room, "listener"); err != nil || pending {
		t.Fatalf("completed route stayed pending: %v, %v", pending, err)
	}
}

func TestRouteScanBoundsMetadataAndIsolatesNames(t *testing.T) {
	room := t.TempDir()
	ctx := context.Background()
	r, err := SubmitInteractive(ctx, room, "one", "test", strings.Repeat("x", MaxBodyBytes), "large")
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := InteractivePending(ctx, room, "one"); err != nil || !pending {
		t.Fatalf("large prompt broke metadata scan: %v, %v", pending, err)
	}
	if pending, err := InteractivePending(ctx, room, "two"); err != nil || pending {
		t.Fatalf("route crossed listener name: %v, %v", pending, err)
	}
	if _, err := Finish(room, r.Envelope, strings.Repeat("x", MaxResultBytes)); err != nil {
		t.Fatal(err)
	}
	if pending, err := InteractivePending(ctx, room, "one"); err != nil || pending {
		t.Fatalf("large completed result broke metadata scan: %v, %v", pending, err)
	}
	if err := fsutil.WriteFileAtomic(recordPath(room, "corrupt"), []byte(`{"id":"corrupt","agent":"one","status":"unknown"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := InteractivePending(ctx, room, "one"); err == nil {
		t.Fatal("corrupt routing metadata authorized fallback")
	}
}

func TestRouteLockSerializesConnectionsAndScanHonorsCancellation(t *testing.T) {
	room := t.TempDir()
	first, err := LockRoute(context.Background(), room, "listener")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := LockRoute(ctx, room, "listener"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent route decision was not serialized: %v", err)
	}
	if _, err := InteractivePending(ctx, room, "listener"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("route scan ignored canceled context: %v", err)
	}
	other, err := LockRoute(context.Background(), room, "other")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if _, err := os.Stat(filepath.Join(Dir(room), "routes", "listener.lock")); err != nil {
		t.Fatal(err)
	}
}
