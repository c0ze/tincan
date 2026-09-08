//go:build windows

package spool

import (
	"errors"

	"golang.org/x/sys/windows"
)

// A signaled process handle identifies an exited process even while another
// handle keeps its kernel object alive. Access failures remain conservative,
// as on Unix: an inaccessible process must not be treated as a dead listener.
func processAlive(pid int) bool {
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	return err != nil || state != windows.WAIT_OBJECT_0
}
