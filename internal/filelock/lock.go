// Package filelock provides process-scoped advisory file locks. Lock files are
// deliberately never unlinked: removing a locked inode permits split ownership.
package filelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/internal/fsutil"
)

var ErrLocked = errors.New("tincan: lock is held")

type Lock struct{ file *os.File }

func Try(path string) (*Lock, error) {
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := fsutil.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{file: f}, nil
}

func Acquire(ctx context.Context, path string) (*Lock, error) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := Try(path)
		if !errors.Is(err, ErrLocked) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close() // closing the descriptor releases the OS lock
}
