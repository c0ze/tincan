//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows)

package fsutil

import "os"

const openRejectsSymlinks = false

func openFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flags, mode)
}

// Directory syncing is not exposed portably on these platforms. Atomic file
// replacement still protects against process crashes; power loss is OS-specific.
func SyncDir(string) error { return nil }

func renameFile(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }
