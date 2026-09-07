package fsutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReadPrefix reads at most prefixBytes from one regular-file snapshot while
// enforcing a separate total file-size limit. Atomic replacement may select
// either immutable version; it must not invalidate the descriptor already read.
// Symlink/FIFO protections are the same as ReadFile, including no-follow opens
// on supported systems and conservative identity checks on other platforms.
func ReadPrefix(path string, prefixBytes, maxFileBytes int64) ([]byte, error) {
	if prefixBytes < 0 || maxFileBytes < 0 || prefixBytes > maxFileBytes || maxFileBytes == 1<<63-1 {
		return nil, fmt.Errorf("tincan: invalid size limit")
	}
	if err := CheckDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeFile, path)
	}
	if before.Size() > maxFileBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	f, err := openFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || (!openRejectsSymlinks && !os.SameFile(before, opened)) {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeFile, path)
	}
	if opened.Size() > maxFileBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, prefixBytes))
	if err != nil {
		return nil, err
	}
	// The snapshot is normally immutable, but also enforce the total cap if a
	// caller writes in place or appends while the bounded prefix is being read.
	finished, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if finished.Size() > maxFileBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	return data, nil
}
