//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package fsutil

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadPrefixRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadPrefix(path, 16, 1024)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnsafeFile) {
			t.Fatalf("FIFO snapshot accepted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prefix read blocked opening a FIFO")
	}
}
