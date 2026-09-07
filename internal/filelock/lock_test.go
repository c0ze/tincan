package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusiveAndCancelable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	first, err := Try(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Try(path); !errors.Is(err, ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second owner: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	first.Close()
	last, err := Try(path)
	if err != nil {
		t.Fatal(err)
	}
	last.Close()
}
