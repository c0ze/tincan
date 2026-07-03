//go:build !windows

package spool

import (
	"os"
	"testing"
)

func TestProcessAliveTrueForOwnPID(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(os.Getpid()) = false, want true")
	}
}

func TestProcessAliveFalseForDefinitelyDeadPID(t *testing.T) {
	// A PID far above any realistic live range; on unix pids are bounded
	// well under this on every mainstream kernel.
	const deadPID = 1 << 30
	if processAlive(deadPID) {
		t.Fatal("processAlive(huge unused pid) = true, want false")
	}
}

func TestProcessAliveFalseForZeroPID(t *testing.T) {
	// kill(0, 0) checks the caller's own process group, not a specific
	// process, so it returns nil (success) unconditionally: a {"pid":0}
	// heartbeat must not be reported alive on that basis.
	if processAlive(0) {
		t.Fatal("processAlive(0) = true, want false")
	}
}

func TestProcessAliveFalseForNegativePID(t *testing.T) {
	// kill(-1, 0) broadcasts to every process the caller may signal, so it
	// also returns nil (success) unconditionally rather than identifying one
	// process.
	if processAlive(-1) {
		t.Fatal("processAlive(-1) = true, want false")
	}
}
