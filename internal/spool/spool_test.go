package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
)

// msg builds a test envelope with a deterministic timestamp for ordering.
func msg(from, to, body string, nano int64) *envelope.Envelope {
	return &envelope.Envelope{
		ID:   envelope.NewID(),
		From: from,
		To:   to,
		TS:   time.Unix(0, nano).UTC(),
		Body: body,
	}
}

func TestSendDeliversParseableFileToInbox(t *testing.T) {
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := msg("orch", "codex", "hello", 1)
	if err := sp.Send(e); err != nil {
		t.Fatalf("Send: %v", err)
	}
	entries, err := os.ReadDir(sp.InboxDir("codex"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 file in inbox, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(sp.InboxDir("codex"), entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := envelope.Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Body != "hello" || got.From != "orch" || got.To != "codex" {
		t.Fatalf("wrong envelope: %+v", got)
	}
}

func TestSendLeavesNoTempResidue(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	if err := sp.Send(msg("a", "b", "x", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(room, ".tincan", "tmp"))
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp dir not empty after send: %v", entries)
	}
}

func TestSendQueuesMultipleInChronologicalFilenameOrder(t *testing.T) {
	sp, _ := Open(t.TempDir())
	for i, body := range []string{"m0", "m1", "m2"} {
		if err := sp.Send(msg("a", "b", body, int64(i+1))); err != nil {
			t.Fatalf("Send %s: %v", body, err)
		}
	}
	entries, _ := os.ReadDir(sp.InboxDir("b"))
	if len(entries) != 3 {
		t.Fatalf("want 3 queued files, got %d", len(entries))
	}
	var names []string
	for _, ent := range entries {
		names = append(names, ent.Name())
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("inbox filenames not sorted: %v", names)
	}
	// Oldest file must contain m0.
	data, _ := os.ReadFile(filepath.Join(sp.InboxDir("b"), names[0]))
	got, _ := envelope.Unmarshal(data)
	if got.Body != "m0" {
		t.Fatalf("oldest file body = %q, want m0", got.Body)
	}
}

func TestSendRejectsEmptyTo(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(&envelope.Envelope{From: "a", Body: "x"}); err == nil {
		t.Fatal("expected error for empty To")
	}
}

func TestSendFillsIDAndTS(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := &envelope.Envelope{From: "a", To: "b", Body: "x"}
	if err := sp.Send(e); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if e.ID == "" || e.TS.IsZero() {
		t.Fatalf("Send must fill ID and TS, got %+v", e)
	}
}

func TestRecvDrainsQueuedOldestFirst(t *testing.T) {
	sp, _ := Open(t.TempDir())
	for i, body := range []string{"m0", "m1", "m2"} {
		if err := sp.Send(msg("a", "b", body, int64(i+1))); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	for _, want := range []string{"m0", "m1", "m2"} {
		e, err := sp.Recv("b", 2*time.Second, false)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if e.Body != want {
			t.Fatalf("Recv order: got %q, want %q", e.Body, want)
		}
	}
	// Inbox must now be empty.
	entries, _ := os.ReadDir(sp.InboxDir("b"))
	if len(entries) != 0 {
		t.Fatalf("inbox not drained: %v", entries)
	}
}

func TestRecvBlocksUntilSendArrives(t *testing.T) {
	sp, _ := Open(t.TempDir())
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := sp.Send(msg("a", "b", "wake", time.Now().UnixNano())); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()
	start := time.Now()
	e, err := sp.Recv("b", 10*time.Second, false)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if e.Body != "wake" {
		t.Fatalf("got body %q", e.Body)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("Recv returned after %v; want ~200ms (blocked until send)", elapsed)
	}
}

func TestRecvTimesOutWithErrTimeout(t *testing.T) {
	sp, _ := Open(t.TempDir())
	start := time.Now()
	_, err := sp.Recv("b", 300*time.Millisecond, false)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("returned before the timeout elapsed")
	}
}

func TestRecvZeroTimeoutReturnsAlreadyQueuedMessageImmediately(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "already-here", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	type result struct {
		e   *envelope.Envelope
		err error
	}
	done := make(chan result, 1)
	go func() {
		e, err := sp.Recv("b", 0, false)
		done <- result{e, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Recv: %v", r.err)
		}
		if r.e.Body != "already-here" {
			t.Fatalf("got body %q, want %q", r.e.Body, "already-here")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv with timeout=0 hung on an already-queued message")
	}
}

func TestRecvZeroTimeoutBlocksForeverUntilSendArrives(t *testing.T) {
	sp, _ := Open(t.TempDir())
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := sp.Send(msg("a", "b", "delayed", time.Now().UnixNano())); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()
	type result struct {
		e   *envelope.Envelope
		err error
	}
	done := make(chan result, 1)
	go func() {
		e, err := sp.Recv("b", 0, false)
		done <- result{e, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Recv: %v", r.err)
		}
		if r.e.Body != "delayed" {
			t.Fatalf("got body %q, want %q", r.e.Body, "delayed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv with timeout=0 hung instead of receiving the delayed send")
	}
}

func TestReArmAfterTimeoutLosesNothing(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if _, err := sp.Recv("b", 100*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	// Message lands during the "gap" between recv calls.
	if err := sp.Send(msg("a", "b", "queued-in-gap", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	e, err := sp.Recv("b", 2*time.Second, false)
	if err != nil {
		t.Fatalf("re-armed Recv: %v", err)
	}
	if e.Body != "queued-in-gap" {
		t.Fatalf("got %q", e.Body)
	}
}

func TestConcurrentReceiversClaimExactlyOnce(t *testing.T) {
	sp, _ := Open(t.TempDir())
	const n = 10
	for i := 0; i < n; i++ {
		if err := sp.Send(msg("a", "b", fmt.Sprintf("m%02d", i), int64(i+1))); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	got := make(chan string, n)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				e, err := sp.Recv("b", 500*time.Millisecond, false)
				if errors.Is(err, ErrTimeout) {
					return // drained
				}
				if err != nil {
					t.Errorf("Recv: %v", err)
					return
				}
				got <- e.Body
			}
		}()
	}
	wg.Wait()
	close(got)
	seen := map[string]bool{}
	for body := range got {
		if seen[body] {
			t.Fatalf("message %q delivered twice", body)
		}
		seen[body] = true
	}
	if len(seen) != n {
		t.Fatalf("delivered %d of %d messages", len(seen), n)
	}
}

func TestLogConsumedKeepsCopyInLog(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	if err := sp.Send(msg("a", "b", "keep-me", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := sp.Recv("b", 2*time.Second, true); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	logs, err := os.ReadDir(filepath.Join(room, ".tincan", "log"))
	if err != nil {
		t.Fatalf("ReadDir log: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 logged message, got %d", len(logs))
	}
	inbox, _ := os.ReadDir(sp.InboxDir("b"))
	if len(inbox) != 0 {
		t.Fatalf("inbox should be empty, got %v", inbox)
	}
}

func TestRemoveInbox(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "r-chan1", "reply", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := sp.RemoveInbox("r-chan1"); err != nil {
		t.Fatalf("RemoveInbox: %v", err)
	}
	if _, err := os.Stat(sp.InboxDir("r-chan1")); !os.IsNotExist(err) {
		t.Fatalf("inbox dir still exists (err=%v)", err)
	}
}

func TestRejectsPathTraversalNames(t *testing.T) {
	sp, _ := Open(t.TempDir())
	bad := []string{"../escape", "a/b", `a\b`, ".", "..", ""}
	for _, name := range bad {
		if err := sp.Send(&envelope.Envelope{From: "a", To: name, Body: "x"}); err == nil {
			t.Errorf("Send to %q: want validation error", name)
		}
		if _, err := sp.Recv(name, time.Millisecond, false); err == nil || errors.Is(err, ErrTimeout) {
			t.Errorf("Recv as %q: want validation error, got %v", name, err)
		}
		if err := sp.RemoveInbox(name); err == nil {
			t.Errorf("RemoveInbox %q: want validation error", name)
		}
	}
}

func TestQuarantinesCorruptMessage(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	inbox := sp.InboxDir("b")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "00000000000000000001-bad.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := sp.Recv("b", time.Second, false)
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("want parse error, got %v", err)
	}
	// The corrupt file is quarantined out of the inbox into tmp/.
	entries, _ := os.ReadDir(inbox)
	if len(entries) != 0 {
		t.Fatalf("corrupt file still in inbox: %v", entries)
	}
	tmpEntries, _ := os.ReadDir(filepath.Join(room, ".tincan", "tmp"))
	if len(tmpEntries) != 1 {
		t.Fatalf("want 1 quarantined file in tmp, got %v", tmpEntries)
	}
}

func TestRenameFailureSurfacesAsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}
	room := t.TempDir()
	sp, _ := Open(room)
	if err := sp.Send(msg("a", "b", "x", 1)); err != nil {
		t.Fatal(err)
	}
	inbox := sp.InboxDir("b")
	// Read-only inbox dir: claiming (rename out) fails with EACCES, which must
	// surface as an error, not decay into a bogus timeout.
	if err := os.Chmod(inbox, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(inbox, 0o755)
	_, err := sp.Recv("b", time.Second, false)
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("want permission error, got %v", err)
	}
}
