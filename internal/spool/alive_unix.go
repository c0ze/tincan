//go:build !windows

package spool

import "syscall"

// processAlive reports whether pid names a live process. Signal 0 performs no
// actual signal delivery, only existence/permission checks: a nil error means
// the process exists and is ours to signal; EPERM means it exists but belongs
// to another user (still alive, just not ours).
//
// pid <= 0 is rejected before the syscall: kill(0, pid) targets the caller's
// own process group rather than one process, and kill(-1, pid) broadcasts to
// every process the caller may signal, so both return nil (success)
// unconditionally rather than confirming any specific pid is alive. A
// heartbeat with a bogus {"pid":0} would otherwise always read as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
