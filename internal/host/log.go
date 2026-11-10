package host

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// maxLogBytes caps a host log at the start of a listener lifetime: a larger
// log becomes <path>.1, replacing the previous generation.
const maxLogBytes = 16 << 20

// rotateLog renames a regular log over maxLogBytes to <path>.1. Callers hold
// the listener's lifetime lock, so no other listener is writing the log.
// Rename never follows a symlink at either name. A log that cannot be rotated
// (open in another process on Windows, say) keeps growing rather than
// failing the listener.
func rotateLog(path string) {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
}

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
