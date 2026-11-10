//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package spool

import (
	"time"

	"golang.org/x/sys/unix"
)

// stampMoved sets a just-moved log or quarantine entry's mtime to now. Rename
// keeps the original mtime, and GC ages these entries by mtime, so without it
// a message that waited longer than the retention would be collected by the
// very next gc. A quarantined symlink is stamped itself, never its target.
// Best-effort: the move has already committed.
func stampMoved(path string) {
	now := unix.NsecToTimespec(time.Now().UnixNano())
	unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{now, now}, unix.AT_SYMLINK_NOFOLLOW)
}
