package spool

import (
	"context"
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
