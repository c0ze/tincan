//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package fsutil

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const openRejectsSymlinks = true

func openFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	for {
		fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
		if err == unix.EINTR {
			continue
		}
		if err == unix.ELOOP {
			return nil, fmt.Errorf("%w: %s", ErrUnsafeFile, path)
		}
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return os.NewFile(uintptr(fd), path), nil
	}
}

// SyncDir makes directory entry changes durable on filesystems that support it.
func SyncDir(path string) error {
	for {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		if err == unix.ELOOP {
			return fmt.Errorf("%w: %s", ErrUnsafeFile, path)
		}
		if err != nil {
			return &os.PathError{Op: "open", Path: path, Err: err}
		}
		f := os.NewFile(uintptr(fd), path)
		err = f.Sync()
		f.Close()
		return err
	}
}

func renameFile(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }
