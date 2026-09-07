// Package fsutil provides private filesystem storage and bounded regular-file reads.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var ErrUnsafeFile = errors.New("tincan: expected a regular file without symlinks")
var ErrTooLarge = errors.New("tincan: file exceeds size limit")

// OpenFile opens a regular file after checking its parent and identity. Unix
// opens use O_NOFOLLOW and O_NONBLOCK, including for writable lock/log files,
// so a FIFO cannot hang the process. O_TRUNC is deferred until after validation.
// Callers create the parent with MkdirPrivate and pass 0600 for private files.
func OpenFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	if err := CheckDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil && !(os.IsNotExist(err) && flags&os.O_CREATE != 0) {
		return nil, err
	}
	if before != nil && !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeFile, path)
	}
	f, err := openFile(path, flags&^os.O_TRUNC, mode)
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	current, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !info.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(current, info) || (before != nil && !os.SameFile(before, info)) {
		f.Close()
		if statErr != nil {
			return nil, statErr
		}
		if pathErr != nil {
			return nil, pathErr
		}
		return nil, fmt.Errorf("%w: %s", ErrUnsafeFile, path)
	}
	if flags&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// CheckDir checks every existing path component, rejecting directory symlinks.
// A missing component is returned as os.ErrNotExist. Paths above the private
// store are checked but their permissions are never modified.
func CheckDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(abs)
	if parent != abs {
		if err := CheckDir(parent); err != nil {
			return err
		}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrUnsafeFile, abs)
	}
	return nil
}

// MkdirPrivate creates missing directories with mode 0700 and rejects symlinks.
// Existing permissions are left alone; in particular a caller's room directory
// and its ancestors are never chmodded.
func MkdirPrivate(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(abs)
	if parent != abs {
		if err := MkdirPrivate(parent); err != nil {
			return err
		}
	}
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		if err = os.Mkdir(abs, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := SyncDir(parent); err != nil {
			return err
		}
		info, err = os.Lstat(abs)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrUnsafeFile, abs)
	}
	return nil
}

// ReadFile refuses symlinks, FIFOs, devices and sockets and bounds allocation
// even if the file grows during the read. Unix opens also use O_NONBLOCK and
// O_NOFOLLOW, so replacing the last component with a FIFO cannot block open.
func ReadFile(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 || maxBytes == 1<<63-1 {
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
	if before.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	f, err := openFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// On platforms with no-follow opens, either immutable version of an
	// atomically replaced regular file is a valid snapshot. Requiring the
	// path to still identify the opened inode would reject healthy readers.
	if !after.Mode().IsRegular() || (!openRejectsSymlinks && !os.SameFile(before, after)) {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeFile, path)
	}
	if after.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	return data, nil
}

// WriteFileAtomic writes and syncs a private temporary file before renaming it
// into place. The final file is 0600 even when replacing an older public file.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := MkdirPrivate(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := renameFile(tmp, path); err != nil {
		return err
	}
	return SyncDir(dir)
}
