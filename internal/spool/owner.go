package spool

import (
	"os"
	"sync"
	"time"
)

// A presence token records the process that wrote it. A PID alone cannot
// prove that process still runs: once it exits (killed, crashed, rebooted)
// the OS may give the PID to an unrelated process. Records therefore also
// carry an opaque start identity (processStart) that a later process reusing
// the PID cannot share. Its format is per-OS and must stay stable.

// legacyStartSlack absorbs clock granularity when a record without a start
// identity (written by an older tincan) is compared with the start time of
// the process now holding its PID.
const legacyStartSlack = 5 * time.Second

var selfStart = sync.OnceValue(func() string {
	id, _, _ := processStart(os.Getpid())
	return id
})

type liveness int

const (
	// ownerAlive: the recording process is, or may be, still running.
	ownerAlive liveness = iota
	// ownerReplaced: the PID now belongs to a process that started after a
	// legacy record was written. Proven only by wall-clock comparison, so it
	// counts as absent but its record is not deleted on that basis.
	ownerReplaced
	// ownerDead: provably gone; its record may be removed or recovered.
	ownerDead
)

// ownerLiveness classifies the process that recorded pid and start at since.
// A PID with no live process is dead. A recorded start identity must match
// the PID's current process exactly. A legacy record without one is alive
// unless the PID's current process demonstrably started after the record
// was written. Whenever the OS cannot tell, the answer stays alive.
func ownerLiveness(pid int, start string, since time.Time) liveness {
	if !processAlive(pid) {
		return ownerDead
	}
	current, at, ok := processStart(pid)
	if !ok {
		return ownerAlive
	}
	if start != "" {
		if current != start {
			return ownerDead
		}
		return ownerAlive
	}
	if !at.IsZero() && !since.IsZero() && at.After(since.Add(legacyStartSlack)) {
		return ownerReplaced
	}
	return ownerAlive
}
