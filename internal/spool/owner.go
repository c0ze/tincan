package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// Presence tokens and interactive claim records name the process that wrote
// them. A PID alone cannot prove that process still runs: once it exits
// (killed, crashed, rebooted) the OS may give the PID to an unrelated process.
// Records therefore also carry an opaque start identity (processStart) that a
// later process reusing the PID cannot share. Its format is per-OS and must
// stay stable.

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

// ownerDir holds one record per interactive claim of name:
// owners/<name>/<filename> names the process holding inflight/<name>/<filename>/.
// It is kept apart from inflight/ so that older binaries, which know nothing
// of it, never trip over an extra file in a gate or in inflight/<name>/.
// A record exists only while its gate does: it is written after the gate is
// created and removed before the gate is.
func (s *Spool) ownerDir(name string) string { return filepath.Join(s.root, "owners", name) }

// setOwner records this process as the owner of a just-created gate, or for a
// claim that is recovered by its caller (hosted listeners, request polling)
// removes a stale record so it cannot be mistaken for this gate's owner.
// The caller holds the transport lock.
//
// The record is published atomically but not fsynced: it guards against the
// owner process dying while the OS keeps running, and a claim whose record an
// OS crash lost is simply left in flight, as every claim was before records.
// Skipping the syncs keeps an interactive claim as fast as any other.
func (s *Spool) setOwner(name, filename string, owned bool) error {
	if !owned {
		return s.clearOwner(name, filename)
	}
	data, err := json.Marshal(presenceFile{PID: os.Getpid(), Since: time.Now().UTC(), Start: selfStart()})
	if err != nil {
		return err
	}
	// Plain mkdirs for the same reason (MkdirPrivate syncs each new parent);
	// CheckDir then rejects a symlink anywhere on the path.
	dir := s.ownerDir(name)
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	if err := fsutil.CheckDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".owner-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, filename))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// readOwner returns a claim's owner record. found is false only when the
// claim has no record (an unrecorded claim, or one from an older binary). A
// record that exists but cannot be read or parsed is an error: it proves
// nothing about its owner, who may still be running.
func (s *Spool) readOwner(name, filename string) (rec presenceFile, found bool, err error) {
	data, err := fsutil.ReadFile(filepath.Join(s.ownerDir(name), filename), 4096)
	if err != nil {
		if os.IsNotExist(err) {
			return presenceFile{}, false, nil
		}
		return presenceFile{}, true, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return presenceFile{}, true, err
	}
	return rec, true, nil
}

// ownerHolds reports whether a claim must be left to its recorded owner:
// the record names a process that may still run, or exists but cannot be
// read. Only a missing record or a provably exited (or replaced) owner lets
// the caller take the claim over. The caller holds the transport lock.
func (s *Spool) ownerHolds(name, filename string) bool {
	rec, found, err := s.readOwner(name, filename)
	if !found {
		return false
	}
	return err != nil || ownerLiveness(rec.PID, rec.Start, rec.Since) == ownerAlive
}

// clearOwner removes a claim's owner record, if any.
func (s *Spool) clearOwner(name, filename string) error {
	err := os.Remove(filepath.Join(s.ownerDir(name), filename))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// requeueAbandoned returns to the queue every interactive claim of name whose
// recorded owner provably exited before Ack or Nack, and drops records whose
// claim is already gone. The caller holds the transport lock, so no Ack, Nack
// or claim of the same gate can run meanwhile. Claims of live owners and
// claims without a record (hosted listeners, request polling, older
// binaries) are never touched: their owners recover them.
func (s *Spool) requeueAbandoned(name string) error {
	dir := s.ownerDir(name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if err := fsutil.CheckDir(dir); err != nil {
		return err
	}
	for _, ent := range entries {
		filename := ent.Name()
		if ent.IsDir() || strings.HasPrefix(filename, ".") || !strings.HasSuffix(filename, ".json") {
			continue // WriteFileAtomic temporaries and anything unexpected
		}
		// An unreadable or malformed record is left alone with its claim.
		rec, found, err := s.readOwner(name, filename)
		if !found || err != nil || ownerLiveness(rec.PID, rec.Start, rec.Since) != ownerDead {
			continue
		}
		d, err := s.readDelivery(name, filename)
		switch {
		case err == nil:
			if err := d.nackLocked(); err != nil {
				return err
			}
		case isMalformed(err):
			if err := s.discardGate(name, filename, err); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}
