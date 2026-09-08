package spool

import (
	"context"
	"encoding/json"
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

// TestRemoveInboxAlsoRemovesPresentDir asserts RemoveInbox cleans up both
// inbox/<name>/ and present/<name>/. Since removePresence (Task 7) no longer
// rmdirs present/<name>/ on its own, ephemeral r-<id> reply channels (every
// ask's reply Recv writes a presence token under present/r-<id>/ while
// parked) would otherwise accumulate empty dirs forever. RemoveInbox is safe
// to also clean present/<name>/ here because an r-<id> channel has exactly
// one receiver — the ask caller's Recv — which has already returned (and its
// own removePresence already run) before ask calls RemoveInbox.
func TestRemoveInboxAlsoRemovesPresentDir(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	if err := sp.Send(msg("a", "r-chan1", "reply", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Simulate the leftover empty present/<name>/ dir that a completed Recv
	// on this channel leaves behind post-Task-7 (removePresence no longer
	// rmdirs it).
	presentDir := filepath.Join(room, ".tincan", "present", "r-chan1")
	if err := os.MkdirAll(presentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sp.RemoveInbox("r-chan1"); err != nil {
		t.Fatalf("RemoveInbox: %v", err)
	}
	if _, err := os.Stat(sp.InboxDir("r-chan1")); !os.IsNotExist(err) {
		t.Fatalf("inbox dir still exists (err=%v)", err)
	}
	if _, err := os.Stat(presentDir); !os.IsNotExist(err) {
		t.Fatalf("present/r-chan1 dir still exists after RemoveInbox (err=%v)", err)
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
	if err := sp.Send(msg("a", "b", "healthy", 2)); err != nil {
		t.Fatal(err)
	}
	e, err := sp.Recv("b", time.Second, false)
	if err != nil || e.Body != "healthy" {
		t.Fatalf("corrupt file prevented delivery of healthy sibling: %+v, %v", e, err)
	}
	// The corrupt file is quarantined out of the inbox and receive continues.
	entries, _ := os.ReadDir(inbox)
	if len(entries) != 0 {
		t.Fatalf("corrupt file still in inbox: %v", entries)
	}
	tmpEntries, _ := os.ReadDir(filepath.Join(room, ".tincan", "quarantine"))
	if len(tmpEntries) != 1 {
		t.Fatalf("want 1 quarantined file, got %v", tmpEntries)
	}
}

// waitForPresenceFile polls until <root>/present/<name>/ contains at least
// one token file or the deadline elapses, so tests don't race the goroutine
// that starts Recv.
func waitForPresenceFile(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, ".tincan", "present", name)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("presence token under %s never appeared", dir)
}

func TestListPresenceReflectsParkedRecvWithLivePID(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Blocks until we send below; timeout is generous so the assertions
		// below (which run while Recv is still parked) have time to happen.
		if _, err := sp.Recv("b", 10*time.Second, false); err != nil {
			t.Errorf("Recv: %v", err)
		}
	}()
	waitForPresenceFile(t, room, "b")

	list, err := sp.ListPresence()
	if err != nil {
		t.Fatalf("ListPresence: %v", err)
	}
	var got *Presence
	for i := range list {
		if list[i].Name == "b" {
			got = &list[i]
		}
	}
	if got == nil {
		t.Fatalf("ListPresence did not include parked name %q: %+v", "b", list)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("PID = %d, want %d", got.PID, os.Getpid())
	}
	if !got.Alive {
		t.Fatal("Alive = false, want true for os.Getpid()")
	}
	if got.Since.IsZero() {
		t.Fatal("Since is zero")
	}

	// Unblock the parked Recv so the goroutine and test can finish cleanly.
	if err := sp.Send(msg("a", "b", "wake", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not return after Send")
	}
}

func TestPresenceFileRemovedAfterRecvReturnsMessage(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := sp.Recv("b", 10*time.Second, false); err != nil {
			t.Errorf("Recv: %v", err)
		}
	}()
	waitForPresenceFile(t, room, "b")

	if err := sp.Send(msg("a", "b", "wake", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not return after Send")
	}

	// Its own token file is gone, so no live tokens remain and the name is
	// absent. The now-empty present/<name>/ dir is deliberately LEFT in
	// place: removePresence removes only its own token file, never the
	// directory (see Task 7 — removing the dir here is what let a
	// concurrent receiver's in-flight rename lose the startup/exit race).
	// presenceFor already treats an empty dir as absent, so this is
	// harmless and mirrors how inbox/<name>/ dirs already persist.
	if _, ok, err := sp.Present("b"); err != nil {
		t.Fatalf("Present: %v", err)
	} else if ok {
		t.Fatal("Present(\"b\") ok = true after Recv returned, want false (no live tokens)")
	}
	if _, err := os.Stat(filepath.Join(room, ".tincan", "present", "b")); err != nil {
		t.Fatalf("present/b dir should still exist (removePresence must not rmdir it), stat err=%v", err)
	}
}

func TestListPresenceMarksDeadPIDNotAlive(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	presentDir := filepath.Join(room, ".tincan", "present", "ghost")
	if err := os.MkdirAll(presentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":%d,"since":%q}`, 1<<30, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(presentDir, "tok1"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := sp.ListPresence()
	if err != nil {
		t.Fatalf("ListPresence: %v", err)
	}
	var got *Presence
	for i := range list {
		if list[i].Name == "ghost" {
			got = &list[i]
		}
	}
	if got == nil {
		t.Fatalf("ListPresence did not include %q: %+v", "ghost", list)
	}
	if got.Alive {
		t.Fatal("Alive = true for a definitely-dead pid, want false")
	}
}

func TestListPresenceIncludesQueuedNameWithoutPresence(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "queued", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	list, err := sp.ListPresence()
	if err != nil {
		t.Fatalf("ListPresence: %v", err)
	}
	var got *Presence
	for i := range list {
		if list[i].Name == "b" {
			got = &list[i]
		}
	}
	if got == nil {
		t.Fatalf("ListPresence did not include %q: %+v", "b", list)
	}
	if got.Queued != 1 {
		t.Fatalf("Queued = %d, want 1", got.Queued)
	}
	if got.Alive {
		t.Fatal("Alive = true for a name with no presence file")
	}
}

func TestListPresenceSortedByName(t *testing.T) {
	sp, _ := Open(t.TempDir())
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if err := sp.Send(msg("a", name, "x", 1)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	list, err := sp.ListPresence()
	if err != nil {
		t.Fatalf("ListPresence: %v", err)
	}
	var names []string
	for _, p := range list {
		names = append(names, p.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("ListPresence not sorted: %v", names)
	}
}

func TestPresentReturnsFalseForUnknownName(t *testing.T) {
	sp, _ := Open(t.TempDir())
	_, ok, err := sp.Present("nobody")
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if ok {
		t.Fatal("Present(\"nobody\") ok = true, want false")
	}
}

func TestPresentReturnsFalseForDeadPIDPresenceFile(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	presentDir := filepath.Join(room, ".tincan", "present", "ghost")
	if err := os.MkdirAll(presentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":%d,"since":%q}`, 1<<30, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(presentDir, "tok1"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := sp.Present("ghost")
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if ok {
		t.Fatal("Present(\"ghost\") ok = true for a definitely-dead pid, want false")
	}
}

func TestPresentReturnsTrueForLivePresenceFile(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := sp.Recv("b", 10*time.Second, false); err != nil {
			t.Errorf("Recv: %v", err)
		}
	}()
	waitForPresenceFile(t, room, "b")

	p, ok, err := sp.Present("b")
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !ok {
		t.Fatal("Present(\"b\") ok = false, want true")
	}
	if p.PID != os.Getpid() || !p.Alive {
		t.Fatalf("Present(\"b\") = %+v, want live os.Getpid()", p)
	}

	if err := sp.Send(msg("a", "b", "wake", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not return after Send")
	}
}

// TestListPresenceSkipsCorruptTokenFileWithoutErroring asserts that a
// malformed token file (e.g. a torn write) doesn't fail the whole listing:
// it's skipped like a dead-pid token would be, and a sibling live token in
// the same present/<name>/ dir still reports the name present.
func TestListPresenceSkipsCorruptTokenFileWithoutErroring(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	presentDir := filepath.Join(room, ".tincan", "present", "b")
	if err := os.MkdirAll(presentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presentDir, "corrupt"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := fmt.Sprintf(`{"pid":%d,"since":%q}`, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(presentDir, "tok-live"), []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := sp.ListPresence()
	if err != nil {
		t.Fatalf("ListPresence: %v", err)
	}
	var got *Presence
	for i := range list {
		if list[i].Name == "b" {
			got = &list[i]
		}
	}
	if got == nil || !got.Alive {
		t.Fatalf("ListPresence did not show \"b\" as alive despite a live sibling token: %+v", list)
	}
}

// TestPresenceForPicksMostRecentSinceAmongLiveTokens asserts the
// representative pid/since (used by status/ping) come from the most-recent
// since among live tokens when several receivers are parked as the same
// name concurrently.
func TestPresenceForPicksMostRecentSinceAmongLiveTokens(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	presentDir := filepath.Join(room, ".tincan", "present", "b")
	if err := os.MkdirAll(presentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-1 * time.Hour).UTC()
	newer := time.Now().UTC()
	// Encode with json.Marshal (RFC3339Nano, sub-second precision) rather
	// than hand-formatted RFC3339 (second precision), matching what
	// writePresence actually produces: otherwise the two "since" values can
	// truncate to the same second and this test stops distinguishing them.
	oldData, err := json.Marshal(presenceFile{PID: os.Getpid(), Since: older})
	if err != nil {
		t.Fatal(err)
	}
	newData, err := json.Marshal(presenceFile{PID: os.Getpid(), Since: newer})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presentDir, "tok-old"), oldData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presentDir, "tok-new"), newData, 0o644); err != nil {
		t.Fatal(err)
	}

	p, ok, err := sp.Present("b")
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !ok {
		t.Fatal("Present(\"b\") ok = false, want true")
	}
	if !p.Since.Equal(newer) {
		t.Fatalf("Since = %v, want most-recent %v", p.Since, newer)
	}
}

// TestConcurrentSameNamePresenceSurvivesOneReceiverReturning reproduces the
// codex-review bug: tincan supports concurrent receivers parked as the same
// name (see TestConcurrentReceiversClaimExactlyOnce). With a single shared
// present/<name> file, whichever receiver returns first deletes it out from
// under the other still-parked receiver, so status/ping wrongly report the
// name absent. Per-receiver token files fix this: two live tokens under
// present/<name>/, removing one (simulating one of two receivers returning)
// must leave the name present because the other token is still live.
func TestConcurrentSameNamePresenceSurvivesOneReceiverReturning(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	presentDir := filepath.Join(room, ".tincan", "present", "b")
	if err := os.MkdirAll(presentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":%d,"since":%q}`, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	tok1 := filepath.Join(presentDir, "tok1")
	tok2 := filepath.Join(presentDir, "tok2")
	if err := os.WriteFile(tok1, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok2, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	// One of the two "receivers" returns and removes only its own token.
	if err := os.Remove(tok1); err != nil {
		t.Fatal(err)
	}

	p, ok, err := sp.Present("b")
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !ok {
		t.Fatal("Present(\"b\") ok = false after one of two receivers returned, want true (other receiver still parked)")
	}
	if p.PID != os.Getpid() || !p.Alive {
		t.Fatalf("Present(\"b\") = %+v, want live os.Getpid()", p)
	}

	list, err := sp.ListPresence()
	if err != nil {
		t.Fatalf("ListPresence: %v", err)
	}
	var got *Presence
	for i := range list {
		if list[i].Name == "b" {
			got = &list[i]
		}
	}
	if got == nil || !got.Alive {
		t.Fatalf("ListPresence did not show \"b\" as alive after one of two receivers returned: %+v", list)
	}
}

// TestPresenceSurvivesConcurrentChurnOfOtherTokens is the Task 7 regression
// test for the startup/exit race codex's re-review found: removePresence
// used to best-effort rmdir present/<name>/ after removing its own token,
// which could remove the directory out from under a concurrent writePresence
// that had MkdirAll'd it but not yet renamed its token in.
//
// Task 7 made that race impossible *by construction*: removePresence no
// longer ever removes present/<name>/, only its own token file (see
// removePresence's doc comment). So this no longer needs to be a heavy
// probabilistic stress test hunting a narrow timing window across many
// trials — a single round of modest concurrent churn against a handful of
// held tokens is enough to exercise writePresence/removePresence racing on
// the same name and catch a reintroduction of the bug. Kept deliberately
// small (a few churner goroutines, a few iterations each) so it stays fast
// on every platform, including a Windows CI runner where writePresence and
// removePresence retry through transient sharing violations (retryClaimOp,
// 10ms sleeps) — heavy load here previously accumulated enough of those
// retry-sleeps under contention to blow well past a 30s deadline there.
//
// This asserts the brief's core invariant: a token written once and held
// (never removed) must be reported present throughout concurrent churn of
// *other* tokens for the same name — never lost to a sibling's cleanup.
//
// All churner goroutines are stoppable via a done channel and tracked with
// a WaitGroup that is waited on before the test returns on *every* path,
// including the safety-deadline path below: if the deadline ever fires, stop
// is closed and wg.Wait() completes before t.Fatal, so no goroutine can
// outlive the test and keep writing into .tincan after t.TempDir() starts
// tearing it down (which is what left `.tincan` non-empty and made TempDir
// cleanup itself fail under the old, much heavier version of this test).
func TestPresenceSurvivesConcurrentChurnOfOtherTokens(t *testing.T) {
	const heldTokens = 3
	const churners = 6
	const itersPerChurner = 20

	room := t.TempDir()
	sp, err := Open(room)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const name = "b"

	done := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	fail := func(msg string) {
		mu.Lock()
		failures = append(failures, msg)
		mu.Unlock()
	}

	// Held tokens: writePresence starts in the same wave as the churners
	// below, then each is verified present and never removed for the rest
	// of the test.
	heldPaths := make([]string, heldTokens)
	for h := 0; h < heldTokens; h++ {
		tok := envelope.NewID()
		heldPaths[h] = filepath.Join(sp.presentNameDir(name), tok)
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			sp.writePresence(name, tok)
		}(tok)
	}

	// Churners: write+remove a fresh token per iteration, verifying their
	// own write landed before removing it. Each checks done between
	// iterations so a signaled stop cuts the loop short immediately instead
	// of grinding through its remaining iterations.
	for c := 0; c < churners; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < itersPerChurner; i++ {
				select {
				case <-done:
					return
				default:
				}
				tok := envelope.NewID()
				sp.writePresence(name, tok)
				tokPath := filepath.Join(sp.presentNameDir(name), tok)
				if _, err := os.Stat(tokPath); err != nil {
					fail(fmt.Sprintf("churn token missing right after writePresence: %v", err))
					continue // don't try to remove a token that was never there
				}
				sp.removePresence(name, tok)
			}
		}()
	}

	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	// Safety deadline: the small counts above should finish in a few
	// seconds even under -race on a slow filesystem; this stops a
	// reintroduced hang/deadlock from hanging the suite. On the deadline
	// path, stop the churners and join them BEFORE failing the test, so no
	// goroutine outlives this function and keeps writing into .tincan while
	// t.TempDir() tears it down.
	select {
	case <-waitCh:
	case <-time.After(10 * time.Second):
		close(done)
		<-waitCh // wait for every goroutine to actually return
		t.Fatal("safety deadline hit; possible hang/deadlock regression")
	}

	for _, p := range heldPaths {
		if _, err := os.Stat(p); err != nil {
			fail(fmt.Sprintf("held token lost to concurrent churn: %v", err))
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d failure(s), first: %s", len(failures), failures[0])
	}
}

// TestTwoRealRecvSameNamePresenceSurvivesOneReturning is the end-to-end
// counterpart to TestConcurrentSameNamePresenceSurvivesOneReceiverReturning:
// it drives the bug fix through the real Recv code path (two genuinely
// parked goroutines sharing a name, each with its own token from Recv's
// internal envelope.NewID() call) instead of hand-writing token files.
// Guarded by a hard test-level timeout so a regression that reintroduces
// cross-receiver deletion (and thus a stuck second Recv) fails loudly
// instead of hanging the suite.
func TestTwoRealRecvSameNamePresenceSurvivesOneReturning(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)

	type result struct {
		e   *envelope.Envelope
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			e, err := sp.Recv("b", 10*time.Second, false)
			results <- result{e, err}
		}()
	}

	// Wait for both receivers to have written their presence token before
	// asserting anything, so this doesn't race Recv's startup.
	presentDir := filepath.Join(room, ".tincan", "present", "b")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(presentDir)
		if err == nil && len(entries) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("both receivers' tokens never appeared under %s (last read: %v, err=%v)", presentDir, entries, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Unblock exactly one of the two parked receivers.
	if err := sp.Send(msg("a", "b", "wake-one", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case r := <-results:
		if r.err != nil {
			t.Fatalf("first Recv returned error: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no Recv returned after Send; safety timeout hit")
	}

	// The bug: the returning Recv's deferred cleanup must remove only its
	// own token, so the other, still-parked, receiver must still show live.
	p, ok, err := sp.Present("b")
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !ok {
		t.Fatal("Present(\"b\") ok = false after one of two real Recv calls returned, want true (other Recv still parked)")
	}
	if !p.Alive {
		t.Fatalf("Present(\"b\") = %+v, want Alive = true", p)
	}

	// Unblock the second receiver so the goroutine and test finish cleanly.
	if err := sp.Send(msg("a", "b", "wake-two", 2)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case r := <-results:
		if r.err != nil {
			t.Fatalf("second Recv returned error: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Recv did not return after Send; safety timeout hit")
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

func TestRecvContextCancelUnparksAndClearsPresence(t *testing.T) {
	sp, _ := Open(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.RecvContext(ctx, "codex", 0, false)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok, _ := sp.Present("codex"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RecvContext never parked (no presence)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecvContext did not return after cancel")
	}
	if _, ok, _ := sp.Present("codex"); ok {
		t.Fatal("presence still reported after a cancelled RecvContext")
	}
}

func TestRecvContextDeliversLikeRecv(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "hi", 1)); err != nil {
		t.Fatal(err)
	}
	e, err := sp.RecvContext(context.Background(), "b", 5*time.Second, false)
	if err != nil || e.Body != "hi" {
		t.Fatalf("got %+v, %v", e, err)
	}
}

func TestProcessAliveExported(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("ProcessAlive(own pid) = false")
	}
	if ProcessAlive(0) {
		t.Fatal("ProcessAlive(0) = true")
	}
}

func TestValidNameExported(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`} {
		if ValidName(bad) == nil {
			t.Fatalf("ValidName(%q) = nil, want error", bad)
		}
	}
	if err := ValidName("codex"); err != nil {
		t.Fatalf("ValidName(codex) = %v", err)
	}
}
