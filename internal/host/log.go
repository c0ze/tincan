package host

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// OpenLog opens a private regular append-only log, refusing special files.
func OpenLog(path string) (*os.File, error) {
	if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", fsutil.ErrUnsafeFile, path)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := openLog(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fsutil.ErrUnsafeFile
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
