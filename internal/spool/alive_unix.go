//go:build !windows

package spool

import "syscall"

// processAlive reports whether pid names a live process. Signal 0 performs no
// actual signal delivery, only existence/permission checks: a nil error means
// the process exists and is ours to signal; EPERM means it exists but belongs
// to another user (still alive, just not ours).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
