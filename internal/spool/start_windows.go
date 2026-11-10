package spool

import (
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

// processStart returns an identity for the process currently holding pid
// that a later process reusing the pid cannot share (its creation time),
// plus that time. ok is false when the process cannot be inspected.
func processStart(pid int) (id string, at time.Time, ok bool) {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return "", time.Time{}, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", time.Time{}, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", time.Time{}, false
	}
	ns := created.Nanoseconds()
	return "windows:" + strconv.FormatInt(ns, 10), time.Unix(0, ns), true
}
