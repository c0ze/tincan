//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package spool

import (
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
