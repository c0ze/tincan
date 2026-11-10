//go:build !windows

package host

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A host that stops answering (here: stopped outright) still owns its
// listener: its lifetime lock is held as long as the process exists.
func TestOwnedSeesAStoppedHost(t *testing.T) {
	room := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Dir(lifetimePath(room, "agent")), 0o700); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(t.TempDir(), "holder.pid")
	exe, _ := os.Executable()
	holder := exec.Command(exe, "hold", lifetimePath(room, "agent"), pidfile)
	holder.Env = append(os.Environ(), "TINCAN_FAKE_AGENT=1")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		holder.Process.Signal(syscall.SIGCONT)
		holder.Process.Kill()
		holder.Wait()
	})
	readPID(t, pidfile)
	if err := holder.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if _, ok := Existing(context.Background(), room, "agent"); ok {
		t.Fatal("fixture: a stopped holder looks alive to Existing")
	}
	if owned, err := Owned(context.Background(), room, "agent"); err != nil || !owned {
		t.Fatalf("stopped host: owned=%v err=%v", owned, err)
	}
}
