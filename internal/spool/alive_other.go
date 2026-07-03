//go:build windows

package spool

// processAlive is a conservative fallback on Windows and does NOT perform a
// real liveness check — a documented limitation of this build:
//
//   - pid <= 0 is rejected (returns false): it can never name a real process,
//     so a bogus heartbeat like {"pid":0} does not read as alive.
//   - ANY positive pid is treated as alive (returns true), whether or not a
//     process by that pid actually exists.
//
// A true check would need OpenProcess + GetExitCodeProcess (there is no
// dependency-free stdlib equivalent to POSIX kill(pid, 0)); rather than add a
// dependency or shell out, we accept that a crashed listener's presence row
// lingers as "parked" in `status`/`ping` until something removes its token
// file (its own Recv defer can't, since the process is gone). Because of the
// always-true-for-positive-pid behavior, the unix-only dead-PID tests are
// skipped on Windows (see the runtime.GOOS guards in the spool/cli tests).
func processAlive(pid int) bool {
	return pid > 0
}
