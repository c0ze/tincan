//go:build windows

package spool

// processAlive is a conservative fallback on Windows: pid-liveness checks
// require OpenProcess + GetExitCodeProcess (no dependency-free stdlib
// equivalent to POSIX kill(pid, 0)). Rather than add a dependency or shell
// out, treat any existing, parseable presence file as alive — a live-but-
// stale presence file just means a crashed listener's row lingers as
// "parked" in `status`/`ping` until Recv's defer would have removed it (it
// won't have, since the process is gone); accepted limitation on Windows.
//
// pid <= 0 is still rejected: it can never be a real process, regardless of
// the conservative-true fallback above.
func processAlive(pid int) bool {
	return pid > 0
}
