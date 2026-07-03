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
