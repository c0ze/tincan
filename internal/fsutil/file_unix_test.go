//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenFileRefusesFIFOForReadWriteAndAppend(t *testing.T) {
	path := filepath.Join(canonicalTemp(t), "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, flags := range []int{os.O_RDONLY, os.O_RDWR | os.O_CREATE, os.O_WRONLY | os.O_CREATE | os.O_APPEND} {
		done := make(chan error, 1)
		go func() {
			f, err := OpenFile(path, flags, 0o600)
			if f != nil {
				f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrUnsafeFile) {
				t.Fatalf("flags %d: %v", flags, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("flags %d blocked opening FIFO", flags)
		}
	}
}
