// Package spool implements the tincan filesystem spool: atomic sends into
// per-name inbox directories and blocking receives that claim exactly once.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/fsnotify/fsnotify"
)

// Spool is a room's message store rooted at <room>/.tincan.
type Spool struct {
	root string // <room>/.tincan
}

// Open returns the spool for a room directory (typically the repo root).
func Open(room string) (*Spool, error) {
	abs, err := filepath.Abs(room)
	if err != nil {
		return nil, err
	}
	return &Spool{root: filepath.Join(abs, ".tincan")}, nil
}

// InboxDir returns the inbox directory for a participant name.
func (s *Spool) InboxDir(name string) string {
	return filepath.Join(s.root, "inbox", name)
}

func (s *Spool) tmpDir() string     { return filepath.Join(s.root, "tmp") }
func (s *Spool) logDir() string     { return filepath.Join(s.root, "log") }
func (s *Spool) presentDir() string { return filepath.Join(s.root, "present") }

// validName rejects names that could escape the spool root: a name must be a
// single, portable path component (no separators on any OS, no traversal).
func validName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("tincan: invalid name %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("tincan: invalid name %q: path separators not allowed", name)
	}
	return nil
}

// Send delivers e into the To inbox atomically: write to tmp/, then rename.
// Messages queue until a receiver claims them. Fills ID and TS if unset.
func (s *Spool) Send(e *envelope.Envelope) error {
	if err := validName(e.To); err != nil {
		return err
	}
	if e.ID == "" {
		e.ID = envelope.NewID()
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	inbox := s.InboxDir(e.To)
	for _, dir := range []string{inbox, s.tmpDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := envelope.Marshal(e)
	if err != nil {
		return err
	}
	name := envelope.Filename(e)
	tmp := filepath.Join(s.tmpDir(), name)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(inbox, name))
}

// ErrTimeout is returned by Recv when no message arrives within the timeout.
var ErrTimeout = errors.New("tincan: recv timeout")

// Recv blocks until one message is available in name's inbox, claims it,
// removes it from the spool (or moves it to log/ when logConsumed), and
// returns it. Returns ErrTimeout if nothing arrives within timeout. A
// timeout <= 0 means no deadline: Recv blocks until a message is claimable.
func (s *Spool) Recv(name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	s.writePresence(name)
	defer s.removePresence(name)
	inbox := s.InboxDir(name)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	defer watcher.Close()
	// A nil channel blocks forever in a select, so leaving deadlineC nil for
	// timeout <= 0 makes the timeout case below never fire.
	var deadlineC <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadlineC = t.C
	}
	// The watcher is a latency optimization; the claim scan is what is
	// correct. Under concurrent-receiver churn the OS backends surface
	// transient errors (kqueue: ENOENT or EBADF while arming/watching a
	// child that a peer just claimed), so watcher trouble never fails the
	// receive — degrade to a short poll and keep trying to re-arm.
	armed := false
	for {
		if !armed {
			if err := os.MkdirAll(inbox, 0o755); err != nil {
				return nil, err
			}
			armed = watcher.Add(inbox) == nil
		}
		e, ok, err := s.claimOldest(name, logConsumed)
		if err != nil {
			return nil, err
		}
		if ok {
			return e, nil
		}
		if !armed {
			// No watch: poll briefly, bounded by the deadline, then retry
			// arming.
			select {
			case <-time.After(10 * time.Millisecond):
			case <-deadlineC:
				return nil, ErrTimeout
			}
			continue
		}
		select {
		case <-watcher.Events:
			// Inbox changed; rescan.
		case <-watcher.Errors:
			// Transient backend churn; degrade to polling and re-arm.
			armed = false
		case <-deadlineC:
			return nil, ErrTimeout
		}
	}
}

// claimOldest tries to claim the oldest message in name's inbox. The claim is
// an atomic rename into tmp/, so concurrent receivers process each message
// exactly once; losing a rename race just means trying the next file.
func (s *Spool) claimOldest(name string, logConsumed bool) (*envelope.Envelope, bool, error) {
	entries, err := os.ReadDir(s.InboxDir(name))
	if err != nil {
		return nil, false, err
	}
	var files []string
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") {
			files = append(files, ent.Name())
		}
	}
	sort.Strings(files) // filenames sort chronologically
	for _, f := range files {
		if err := os.MkdirAll(s.tmpDir(), 0o755); err != nil {
			return nil, false, err
		}
		claimed := filepath.Join(s.tmpDir(), fmt.Sprintf("claim-%s-%s", envelope.NewID(), f))
		if err := os.Rename(filepath.Join(s.InboxDir(name), f), claimed); err != nil {
			if lostClaimRace(err) {
				continue // another receiver claimed it first
			}
			// Anything else (read-only fs, ...) is a real error; swallowing
			// it would decay into a bogus timeout.
			return nil, false, err
		}
		var data []byte
		err := retryClaimOp(func() error {
			var rerr error
			data, rerr = os.ReadFile(claimed)
			return rerr
		})
		if err != nil {
			if lostClaimRace(err) {
				// Windows: renames are handle-based, so a racing receiver
				// can move the file even after our rename "succeeded". The
				// message is theirs now.
				continue
			}
			return nil, false, err
		}
		e, err := envelope.Unmarshal(data)
		if err != nil {
			// Deliberate quarantine: the claimed file stays in tmp/ (never
			// retried, swept by the Phase-2 gc) so a corrupt message can't
			// cause an infinite reparse loop.
			return nil, false, fmt.Errorf("tincan: bad message %s: %w", f, err)
		}
		// Consuming the claim file decides ownership: whoever removes (or
		// logs) it delivers the message; a loser discards its copy so the
		// message is processed exactly once even where renames race.
		if logConsumed {
			if err := os.MkdirAll(s.logDir(), 0o755); err != nil {
				return nil, false, err
			}
			if err := retryClaimOp(func() error {
				return os.Rename(claimed, filepath.Join(s.logDir(), f))
			}); err != nil {
				if lostClaimRace(err) {
					continue
				}
				return nil, false, err
			}
		} else if err := retryClaimOp(func() error { return os.Remove(claimed) }); err != nil {
			if lostClaimRace(err) {
				continue
			}
			return nil, false, err
		}
		return e, true, nil
	}
	return nil, false, nil
}

// retryClaimOp runs op, retrying briefly while it fails with a Windows
// sharing/lock violation — the signature of a racing receiver's in-flight
// handle-based rename. The race resolves in microseconds: either the file
// moves away (op then fails NotExist → lost race) or the handle is released
// (op succeeds). On POSIX this never retries.
func retryClaimOp(op func() error) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = op(); !isTransientShare(err) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// lostClaimRace reports whether err is the signature of losing a claim race
// to a concurrent receiver rather than a real filesystem failure. On POSIX a
// lost race is ENOENT. On Windows, renames are handle-based, so racing
// receivers can also produce sharing/permission errors mid-claim; those count
// as race losses there (a genuine ACL problem on Windows then shows up as a
// timeout rather than an error — the price of exactly-once on that platform).
func lostClaimRace(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission)
}

// RemoveInbox deletes a participant's inbox directory. Used to clean up
// ephemeral r-<id> reply channels after a successful ask.
func (s *Spool) RemoveInbox(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.InboxDir(name))
}

// Presence describes one name's listener state, as reported by `status`/`ping`.
type Presence struct {
	Name   string    `json:"name"`
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
	Queued int       `json:"queued"`
	Alive  bool      `json:"alive"`
}

// presenceFile is the on-disk JSON shape of <root>/present/<name>.
type presenceFile struct {
	PID   int       `json:"pid"`
	Since time.Time `json:"since"`
}

// writePresence records that a Recv for name is parked right now: it writes
// <root>/present/<name> atomically (tmp/ then rename), mirroring Send. This
// is a heartbeat, not part of message delivery, so any failure here is
// swallowed — it must never change Recv's success/err/timeout result.
func (s *Spool) writePresence(name string) {
	for _, dir := range []string{s.presentDir(), s.tmpDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	data, err := json.Marshal(presenceFile{PID: os.Getpid(), Since: time.Now().UTC()})
	if err != nil {
		return
	}
	tmp := filepath.Join(s.tmpDir(), "present-"+envelope.NewID())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(s.presentDir(), name)); err != nil {
		os.Remove(tmp) // best-effort: don't leak the tmp file on a failed rename
	}
}

// removePresence clears the presence file written by writePresence. Called
// via defer on every Recv exit path (message, timeout, error); best-effort,
// like writePresence.
func (s *Spool) removePresence(name string) {
	os.Remove(filepath.Join(s.presentDir(), name))
}

// ListPresence returns Presence for the union of names that have an inbox
// directory and/or a live present/<name> file, sorted by name.
func (s *Spool) ListPresence() ([]Presence, error) {
	names := map[string]bool{}
	inboxRoot := filepath.Join(s.root, "inbox")
	inboxEntries, err := os.ReadDir(inboxRoot)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, ent := range inboxEntries {
		if ent.IsDir() {
			names[ent.Name()] = true
		}
	}
	presentEntries, err := os.ReadDir(s.presentDir())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, ent := range presentEntries {
		if !ent.IsDir() {
			names[ent.Name()] = true
		}
	}
	list := make([]Presence, 0, len(names))
	for name := range names {
		p, err := s.presenceFor(name)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// Present returns the presence for one name; the second return is false if
// no live presence file exists for it (the name may still have a queue).
func (s *Spool) Present(name string) (Presence, bool, error) {
	if err := validName(name); err != nil {
		return Presence{}, false, err
	}
	p, err := s.presenceFor(name)
	if err != nil {
		return Presence{}, false, err
	}
	return p, p.Alive, nil
}

// presenceFor builds the Presence for one name: queued count from its inbox,
// plus pid/since/alive parsed from its present/<name> file, if any.
func (s *Spool) presenceFor(name string) (Presence, error) {
	p := Presence{Name: name}
	entries, err := os.ReadDir(s.InboxDir(name))
	if err != nil && !os.IsNotExist(err) {
		return Presence{}, err
	}
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") {
			p.Queued++
		}
	}
	data, err := os.ReadFile(filepath.Join(s.presentDir(), name))
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return Presence{}, err
	}
	var pf presenceFile
	if err := json.Unmarshal(data, &pf); err != nil {
		// A malformed presence file (e.g. torn write) is reported as absent
		// rather than a request-wide error: it self-heals on the next Recv.
		return p, nil
	}
	p.PID = pf.PID
	p.Since = pf.Since
	p.Alive = processAlive(pf.PID)
	return p, nil
}
