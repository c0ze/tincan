package spool

import (
	"context"
	"os"
	"path/filepath"
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

// Temporary files of a crashed send, presence write or atomic state write are
// collected once older than the retention; fresh ones may still be in use.
func TestGCSweepsOldTemporaryFiles(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "x", 1)); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-time.Hour)
	old := cutoff.Add(-time.Hour)
	write := func(path string, stale bool) string {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if stale {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	stale := []string{
		write(filepath.Join(sp.tmpDir(), "send-123"), true),
		write(filepath.Join(sp.tmpDir(), "present-456"), true),
		write(filepath.Join(sp.root, ".write-789"), true),
		write(filepath.Join(sp.root, "hosts", ".write-abc"), true),
		write(filepath.Join(sp.ownerDir("b"), ".owner-def"), true),
	}
	kept := []string{
		write(filepath.Join(sp.tmpDir(), "send-fresh"), false),
		write(filepath.Join(sp.root, "hosts", ".write-fresh"), false),
		write(filepath.Join(sp.root, "hosts", "codex.json"), true),
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
