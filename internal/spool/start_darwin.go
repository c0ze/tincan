package spool

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// processStart returns an identity for the process currently holding pid
// that a later process reusing the pid cannot share (its kernel start time),
// plus that start time. ok is false when the process cannot be inspected.
func processStart(pid int) (id string, at time.Time, ok bool) {
	if pid <= 0 {
		return "", time.Time{}, false
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", time.Time{}, false
	}
	tv := k.Proc.P_starttime
	if tv.Sec == 0 && tv.Usec == 0 {
		return "", time.Time{}, false
	}
	return fmt.Sprintf("darwin:%d.%06d", tv.Sec, tv.Usec), time.Unix(int64(tv.Sec), int64(tv.Usec)*1000), true
}
