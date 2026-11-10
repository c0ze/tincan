package spool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
)

func channelDirs(sp *Spool, name string) []string {
	return []string{sp.InboxDir(name), sp.inFlightDir(name), sp.presentNameDir(name), sp.ownerDir(name)}
}

// A completed ask (claim its reply, Ack, RemoveInbox) leaves no directory of
// its reply channel behind, in-flight and owner directories included.
func TestCompletedAskLeavesNoChannelDirectories(t *testing.T) {
	sp, _ := Open(t.TempDir())
	channel := "r-" + envelope.NewID()
	if err := sp.Send(msg("worker", channel, "answer", 1)); err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimInteractive(context.Background(), channel, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Ack(false); err != nil {
		t.Fatal(err)
	}
	if err := sp.RemoveInbox(channel); err != nil {
		t.Fatal(err)
	}
	for _, dir := range channelDirs(sp, channel) {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("%s left behind (%v)", dir, err)
		}
	}
}

// A reply channel nobody will use again (a timed-out ask, an MCP request) is
// hidden from status while empty and swept by gc once old. Channels holding a
// reply, a claim or a live receiver, fresh channels and ordinary names stay.
func TestGCSweepsAbandonedReplyChannelsOnly(t *testing.T) {
	sp, _ := Open(t.TempDir())
	cutoff := time.Now().Add(-time.Hour)
	old := cutoff.Add(-time.Hour)
	mk := func(name string) {
		for _, dir := range []string{sp.InboxDir(name), sp.inFlightDir(name), sp.presentNameDir(name)} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	age := func(name string) {
		for _, dir := range channelDirs(sp, name) {
			if _, err := os.Lstat(dir); err == nil {
				if err := os.Chtimes(dir, old, old); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	abandoned := "r-" + envelope.NewID()
	mk(abandoned)
	writeToken(t, sp, abandoned, "dead", presenceFile{PID: 1 << 30, Since: old})
	mcp := "r-" + envelope.NewID() + envelope.NewID() // request channels are 64 hex digits
	mk(mcp)
	answered := "r-" + envelope.NewID()
	if err := sp.Send(msg("worker", answered, "late reply", 1)); err != nil {
		t.Fatal(err)
	}
	parked := "r-" + envelope.NewID()
	mk(parked)
	writeToken(t, sp, parked, "live", presenceFile{PID: os.Getpid(), Since: old, Start: selfStart()})
	fresh := "r-" + envelope.NewID()
	mk(fresh)
	ordinary := "worker"
	mk(ordinary)
	for _, name := range []string{abandoned, mcp, answered, parked, ordinary} {
		age(name)
	}

	list, err := sp.ListPresence()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, p := range list {
		listed[p.Name] = true
	}
	for name, want := range map[string]bool{abandoned: false, mcp: false, fresh: false, answered: true, parked: true, ordinary: true} {
		if listed[name] != want {
			t.Errorf("ListPresence lists %s = %v, want %v", name, listed[name], want)
		}
	}

	stats, err := sp.GC(cutoff)
	if err != nil || stats.Channels != 2 {
		t.Fatalf("GC = %+v, %v; want two channels swept", stats, err)
	}
	for _, name := range []string{abandoned, mcp} {
		for _, dir := range channelDirs(sp, name) {
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Errorf("%s kept (%v)", dir, err)
			}
		}
	}
	for _, name := range []string{answered, parked, fresh, ordinary} {
		if _, err := os.Lstat(sp.InboxDir(name)); err != nil {
			t.Errorf("%s removed: %v", name, err)
		}
	}
	if got, err := sp.Recv(answered, time.Second, false); err != nil || got.Body != "late reply" {
		t.Fatalf("late reply lost: %+v, %v", got, err)
	}
}

// A receiver creates its inbox and publishes its presence token under the
// transport lock, so gc (which sweeps under the same lock) never sees an old
// empty reply channel half-registered and removes it underneath the receiver.
func TestReceiverRegistersUnderTransportLock(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	channel := "r-" + envelope.NewID()
	lock, err := sp.lockTransport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sp.ClaimContext(ctx, channel, 10*time.Second)
	}()
	time.Sleep(200 * time.Millisecond)
	if entries, _ := os.ReadDir(sp.presentNameDir(channel)); len(entries) != 0 {
		t.Fatalf("token published without the transport lock: %v", entries)
	}
	lock.Close()
	waitForPresenceFile(t, room, channel)
	cancel()
	<-done
}

// Registration waits for the transport lock within the receive's own
// timeout: a sender or gc holding the lock past it yields ErrTimeout on time,
// with no presence token left behind, rather than a receive that hangs.
func TestReceiveTimesOutWhileTransportLockIsHeld(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	lock, err := sp.lockTransport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release := time.AfterFunc(3*time.Second, func() { lock.Close() })
	defer release.Stop()
	defer lock.Close()
	for _, claim := range []func() error{
		func() error {
			_, err := sp.ClaimContext(context.Background(), "worker", 200*time.Millisecond)
			return err
		},
		func() error {
			_, err := sp.ClaimInteractive(context.Background(), "worker", 200*time.Millisecond)
			return err
		},
		func() error { _, err := sp.Recv("worker", 200*time.Millisecond, false); return err },
	} {
		start := time.Now()
		if err := claim(); !errors.Is(err, ErrTimeout) || time.Since(start) > 1500*time.Millisecond {
			t.Fatalf("claim behind a held lock: %v after %v, want ErrTimeout by its deadline", err, time.Since(start))
		}
	}
	if entries, _ := os.ReadDir(sp.presentNameDir("worker")); len(entries) != 0 {
		t.Fatalf("timed-out receive left presence: %v", entries)
	}
	if _, ok, err := sp.TryClaim(context.Background(), "worker"); ok || err != nil {
		t.Fatalf("TryClaim behind a held lock: %v, %v; want an immediate empty result", ok, err)
	}
}

// If a parked receiver's inbox disappears between polls anyway, the receiver
// recreates it under the lock and keeps waiting.
func TestParkedReceiverSurvivesItsInboxBeingRemoved(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	channel := "r-" + envelope.NewID()
	got := make(chan error, 1)
	go func() {
		_, err := sp.Recv(channel, 10*time.Second, false)
		got <- err
	}()
	waitForPresenceFile(t, room, channel)
	// Remove it the way a sweep would: between polls, under the lock.
	lock, err := sp.lockTransport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = os.Remove(sp.InboxDir(channel))
	lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // several polls without an inbox
	if err := sp.Send(msg("worker", channel, "reply", 1)); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err != nil {
		t.Fatalf("receiver failed after its inbox was removed: %v", err)
	}
}

// A channel directory that gained an entry after gc checked it stops the
// sweep of that channel instead of failing gc.
func TestChannelRemovalStopsAtNonEmptyDirectory(t *testing.T) {
	sp, _ := Open(t.TempDir())
	channel := "r-" + envelope.NewID()
	writeToken(t, sp, channel, "late", presenceFile{PID: os.Getpid(), Since: time.Now().UTC(), Start: selfStart()})
	if err := os.MkdirAll(sp.InboxDir(channel), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := removeChannelDirs([]string{sp.presentNameDir(channel), sp.InboxDir(channel)})
	if err != nil || removed {
		t.Fatalf("removeChannelDirs = %v, %v; want false, nil", removed, err)
	}
	if _, err := os.Lstat(sp.InboxDir(channel)); err != nil {
		t.Fatalf("inbox removed although the channel is in use: %v", err)
	}
}

// Temporary files of a crashed send, presence write, claim record or
// .gitignore write are collected once older than the retention; fresh ones
// may still be in use. Only the exact names the spool itself generates, in
// the directories it generates them in, are candidates: other packages'
// state (a request whose ID is ".write-job", its lock, host files) is never
// touched however it is named.
func TestGCSweepsOldTemporaryFiles(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "x", 1)); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour)
	old := time.Now().Add(-48 * time.Hour)
	deadToken := `{"pid":1073741824,"since":"2026-01-01T00:00:00Z","start":"x"}`
	write := func(path, content string, stale bool) string {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if stale {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	requests := filepath.Join(sp.root, "requests")
	stale := []string{
		write(filepath.Join(sp.tmpDir(), "send-123"), "partial", true),
		write(filepath.Join(sp.tmpDir(), "present-"+envelope.NewID()), deadToken, true),
		write(filepath.Join(sp.root, ".write-789"), "partial", true),
		write(filepath.Join(sp.ownerDir("b"), ".owner-456"), "partial", true),
	}
	kept := []string{
		write(filepath.Join(sp.tmpDir(), "send-999"), "in use", false),
		write(filepath.Join(sp.tmpDir(), "send-x1"), "not generated", true),
		write(filepath.Join(sp.ownerDir("b"), ".owner-job"), "not generated", true),
		write(filepath.Join(sp.root, "hosts", ".write-123"), "host state", true),
		write(filepath.Join(sp.root, "hosts", "codex.json"), "host state", true),
		write(filepath.Join(requests, ".write-job.json"), `{"status":"queued"}`, true),
		write(filepath.Join(requests, ".write-job.jsonl"), "progress", true),
		write(filepath.Join(requests, ".write-123"), "request write", true),
		write(filepath.Join(requests, "locks", ".write-job.lock"), "", true),
	}
	stats, err := sp.GC(cutoff)
	if err != nil || stats.Temp != len(stale) {
		t.Fatalf("GC = %+v, %v; want %d temporary files", stats, err, len(stale))
	}
	for _, path := range stale {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s kept (%v)", path, err)
		}
	}
	for _, path := range kept {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s removed: %v", path, err)
		}
	}
	if _, err := sp.Recv("b", time.Second, false); err != nil {
		t.Fatalf("queued message lost: %v", err)
	}
}

// Age alone does not prove a temporary file abandoned: its writer may be
// paused (SIGSTOP, a debugger, a sleeping machine) and --older-than may be
// short. Send and claim-record temporaries exist only while their writer holds
// the transport lock, so gc removes them only while holding it; a presence
// temporary names its writer; a .gitignore temporary, which carries no proof,
// is left for a day whatever the retention.
func TestGCRemovesOnlyProvablyAbandonedTemporaryFiles(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "x", 1)); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Minute)
	write := func(path, content string, age time.Duration) string {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}
	live := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"since":"2026-01-01T00:00:00Z","start":"` + selfStart() + `"}`
	if selfStart() == "" {
		live = `{"pid":` + strconv.Itoa(os.Getpid()) + `,"since":"2099-01-01T00:00:00Z"}`
	}
	kept := []string{
		write(filepath.Join(sp.tmpDir(), "present-"+envelope.NewID()), live, 2*time.Hour),
		write(filepath.Join(sp.tmpDir(), "present-"+envelope.NewID()), `{"pid":`, 2*time.Hour),
		write(filepath.Join(sp.root, ".write-1"), "partial", 2*time.Hour),
	}
	gone := []string{
		write(filepath.Join(sp.tmpDir(), "present-"+envelope.NewID()), `{"pid":`, 48*time.Hour),
		write(filepath.Join(sp.root, ".write-2"), "partial", 48*time.Hour),
		write(filepath.Join(sp.tmpDir(), "send-1"), "partial", 2*time.Hour),
		write(filepath.Join(sp.ownerDir("b"), ".owner-1"), "partial", 2*time.Hour),
	}
	// A writer holding the transport lock keeps its temporaries.
	lock, err := sp.lockTransport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := sp.GC(cutoff)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	for _, path := range gone[2:] {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s removed while its writer may hold the transport lock: %v", path, err)
		}
	}
	lock.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, path := range gone {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s kept (%v)", path, err)
		}
	}
	for _, path := range kept {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s removed: %v", path, err)
		}
	}
}
