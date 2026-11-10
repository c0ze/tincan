//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package spool

import (
	"os"
	"time"
)

// stampMoved sets a just-moved log or quarantine entry's mtime to now (see
// stamp_unix.go). Without a no-follow timestamp call here, a symlink is left
// unstamped rather than following it to a target outside the spool.
func stampMoved(path string) {
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	now := time.Now()
	os.Chtimes(path, now, now)
}
