//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package spool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"golang.org/x/sys/unix"
)

func TestUnsafeInboxFilesQuarantinedWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "oversize", "directory", "invalid-routing"} {
		t.Run(kind, func(t *testing.T) {
			sp, _ := Open(t.TempDir())
			if err := sp.Send(msg("a", "b", "healthy", 2)); err != nil {
				t.Fatal(err)
			}
			e := msg("a", "b", "unsafe", 1)
			path := filepath.Join(sp.InboxDir("b"), envelope.Filename(e))
			switch kind {
			case "fifo":
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(target, []byte("outside data"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(MaxMessageBytes + 1); err != nil {
					f.Close()
					t.Fatal(err)
				}
				f.Close()
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "invalid-routing":
				e.To = "elsewhere"
				data, err := envelope.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				got, err := sp.Recv("b", 100*time.Millisecond, false)
				if err == nil && got.Body != "healthy" {
					t.Errorf("delivered unsafe file: %+v", got)
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("unsafe file stopped receive: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("unsafe file blocked receive")
			}
			entries, err := os.ReadDir(filepath.Join(sp.root, "quarantine"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("want one quarantined file, got %v, %v", entries, err)
			}
		})
	}
}

// A read failure that says nothing about the message itself (here EACCES; in
// the field EMFILE or EIO) is returned, and the claim stays in flight instead
// of being quarantined as malformed.
func TestClaimReadErrorKeepsClaimInsteadOfQuarantining(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads mode 000 files")
	}
	sp, _ := Open(t.TempDir())
	e := msg("a", "b", "valid but unreadable for now", 1)
	if err := sp.Send(e); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(sp.InboxDir("b"), envelope.Filename(e)), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.ClaimContext(context.Background(), "b", time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("claim error = %v, want the read error", err)
	}
	claimed := filepath.Join(sp.inFlightDir("b"), envelope.Filename(e), "message.json")
	if _, err := os.Lstat(claimed); err != nil {
		t.Fatalf("claim not kept: %v", err)
	}
	if _, err := sp.RecoverInFlight("b"); err == nil {
		t.Fatal("recovery hid the read error")
	}
	if _, err := os.Lstat(filepath.Join(sp.root, "quarantine")); !os.IsNotExist(err) {
		t.Fatalf("readable-later message was quarantined: %v", err)
	}
	if err := os.Chmod(claimed, 0o600); err != nil {
		t.Fatal(err)
	}
	claims, err := sp.RecoverInFlight("b")
	if err != nil || len(claims) != 1 || claims[0].Envelope.ID != e.ID {
		t.Fatalf("recovered %v, %v", claims, err)
	}
}

// Stamping a quarantined entry's mtime must touch the entry itself: for a
// symlink that is the link, never its target outside the spool.
func TestQuarantinedSymlinkStampsLinkNotTarget(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "x", 1)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sp.InboxDir("b"), "stray-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lutimes(link, []unix.Timeval{unix.NsecToTimeval(old.UnixNano()), unix.NsecToTimeval(old.UnixNano())}); err != nil {
		t.Fatal(err)
	}
	if err := sp.quarantine(link); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(target); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("target outside the spool restamped: %v, %v", info.ModTime(), err)
	}
	entries, _ := os.ReadDir(filepath.Join(sp.root, "quarantine"))
	if len(entries) != 1 {
		t.Fatalf("quarantine = %v", entries)
	}
	info, err := os.Lstat(filepath.Join(sp.root, "quarantine", entries[0].Name()))
	if err != nil || info.Mode()&os.ModeSymlink == 0 || time.Since(info.ModTime()) > time.Hour {
		t.Fatalf("quarantined link not stamped: %v, %v", info, err)
	}
}

func TestPresenceIgnoresFIFOAndSymlinkWithoutBlocking(t *testing.T) {
	sp, _ := Open(t.TempDir())
	sp.writePresence("b", "live")
	if err := unix.Mkfifo(filepath.Join(sp.presentNameDir("b"), "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sp.presentNameDir("b"), "live"), filepath.Join(sp.presentNameDir("b"), "link")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, ok, err := sp.Present("b")
		if err == nil && !ok {
			t.Error("live token ignored")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("presence read blocked on FIFO")
	}
}

func TestSpoolRejectsSymlinkedStorageDirectories(t *testing.T) {
	for _, part := range []string{".tincan", ".tincan/inbox", ".tincan/inbox/b", ".tincan/tmp"} {
		t.Run(part, func(t *testing.T) {
			room := t.TempDir()
			outside := t.TempDir()
			path := filepath.Join(room, part)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			sp, err := Open(room)
			if err != nil {
				t.Fatal(err)
			}
			if err := sp.Send(msg("a", "b", "secret", 1)); err == nil {
				t.Fatal("followed symlinked spool path")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote through symlink: %v, %v", entries, err)
			}
		})
	}
}
