// Package spool implements the tincan filesystem spool: atomic sends into
// per-name inbox directories and durable claims acknowledged after delivery.
package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/fsutil"
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
	// Resolve the user-selected room once; symlinks inside its private spool
	// are still rejected. This also handles /var -> /private/var on macOS.
	if canonical, err := filepath.EvalSymlinks(abs); err == nil {
		abs = canonical
	} else if !os.IsNotExist(err) {
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

// Mutating operations close access through a legacy public spool root without
// changing permissions on the caller's room or any of its ancestors.
func (s *Spool) ensurePrivateRoot() error {
	if err := fsutil.MkdirPrivate(s.root); err != nil {
		return err
	}
	return os.Chmod(s.root, 0o700)
}

// validName rejects names that could escape the spool root: a name must be a
// single, portable path component (no separators on any OS, no traversal).
func validName(name string) error { return envelope.ValidComponent(name) }

// ValidName reports whether name is a legal participant name (a single path
// component, no traversal). Exported for the hosted-listener commands, which
// build <room>/.tincan/hosts/<name>.* paths from the name.
func ValidName(name string) error { return validName(name) }

// ProcessAlive reports whether pid names a live process, using the same
// per-OS check status/ping apply to presence tokens. Exported so hosted-listener state
// files get the identical answer.
func ProcessAlive(pid int) bool { return processAlive(pid) }

// MaxMessageBytes bounds both sends and reads, including JSON metadata.
const MaxMessageBytes int64 = 8 << 20

// Send delivers e into the To inbox atomically: sync tmp/, publish a hard link,
// then remove the temporary link. An identical queued/in-flight retry succeeds;
// conflicting content at the same filename is rejected without replacement.
// Messages queue until a receiver claims them. Fills ID and TS if unset.
func (s *Spool) Send(e *envelope.Envelope) error {
	if e == nil {
		return fmt.Errorf("tincan: nil envelope")
	}
	if e.ID == "" {
		e.ID = envelope.NewID()
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	data, err := envelope.Marshal(e)
	if err != nil {
		return err
	}
	if int64(len(data)) > MaxMessageBytes {
		return fsutil.ErrTooLarge
	}
	if err := s.ensurePrivateRoot(); err != nil {
		return err
	}
	lock, err := s.lockTransport(context.Background())
	if err != nil {
		return err
	}
	defer lock.Close()
	inbox := s.InboxDir(e.To)
	for _, dir := range []string{inbox, s.tmpDir()} {
		if err := fsutil.MkdirPrivate(dir); err != nil {
			return err
		}
	}
	if published, err := s.published(e); err != nil || published {
		return err
	}
	f, err := os.CreateTemp(s.tmpDir(), "send-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Link publishes the complete file atomically without overwriting an
	// existing envelope with the same timestamp/ID. Both paths are on the
	// same filesystem; deleting the temporary link leaves the inbox intact.
	if err := os.Link(tmp, filepath.Join(inbox, envelope.Filename(e))); err != nil {
		// A non-cooperating older sender may have published without taking
		// the transport lock. Compare any surviving envelope before failing.
		if os.IsExist(err) {
			if published, checkErr := s.published(e); checkErr != nil || published {
				return checkErr
			}
		}
		return err
	}
	return fsutil.SyncDir(inbox)
}

// ErrTimeout is returned by Recv when no message arrives within the timeout.
var ErrTimeout = errors.New("tincan: recv timeout")

// Recv blocks until one message is available in name's inbox, claims it,
// removes it from the spool (or moves it to log/ when logConsumed), and
// returns it. Returns ErrTimeout if nothing arrives within timeout. A
// timeout <= 0 means no deadline: Recv blocks until a message is claimable.
func (s *Spool) Recv(name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error) {
	return s.RecvContext(context.Background(), name, timeout, logConsumed)
}

// RecvContext is Recv with cancellation: it returns ctx.Err() as soon as ctx
// is done while parked, clearing its presence token on the way out like any
// other exit path. Hosted listeners use it to unpark on SIGTERM/SIGINT
// without leaving a stale presence file behind.
func (s *Spool) RecvContext(ctx context.Context, name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error) {
	delivery, err := s.ClaimContext(ctx, name, timeout)
	if err != nil {
		return nil, err
	}
	if err := delivery.Ack(logConsumed); err != nil {
		return nil, err
	}
	return delivery.Envelope, nil
}

// ClaimContext waits for a queued envelope and moves it to durable in-flight
// storage. The caller must Ack only after delivery succeeds, or explicitly Nack
// to retry. Cancellation never deletes an already claimed envelope.
func (s *Spool) ClaimContext(ctx context.Context, name string, timeout time.Duration) (*Delivery, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.ensurePrivateRoot(); err != nil {
		return nil, err
	}
	inbox := s.InboxDir(name)
	if err := fsutil.MkdirPrivate(inbox); err != nil {
		return nil, err
	}
	token := envelope.NewID()
	s.writePresence(name, token)
	defer s.removePresence(name, token)
	watcher, watchErr := fsnotify.NewWatcher()
	var events <-chan fsnotify.Event
	var watchErrors <-chan error
	if watchErr == nil {
		defer watcher.Close()
		events, watchErrors = watcher.Events, watcher.Errors
	}
	var deadlineC <-chan time.Time
	claimCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		claimCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadlineC = t.C
	}
	// Poll even when the watch succeeds: a dropped/coalesced event or closed
	// watcher must not leave an existing queue parked forever.
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	armed := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-deadlineC:
			return nil, ErrTimeout
		default:
		}
		if !armed && watcher != nil {
			armed = watcher.Add(inbox) == nil
		}
		delivery, ok, err := s.claimOldest(claimCtx, name)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil, ErrTimeout
			}
			return nil, err
		}
		if ok {
			return delivery, nil
		}
		select {
		case _, ok := <-events:
			if !ok {
				events = nil
				armed = false
			}
		case _, ok := <-watchErrors:
			armed = false
			if !ok {
				watchErrors = nil
			}
		case <-poll.C:
		case <-deadlineC:
			return nil, ErrTimeout
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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

// lostClaimRace only treats a missing source as a lost race. Permission and
// sharing failures must surface rather than turning into a misleading timeout.
func lostClaimRace(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// RemoveInbox deletes a participant's inbox directory, and also its
// present/<name>/ dir (which removePresence deliberately leaves behind — see
// removePresence). Used to clean up ephemeral r-<id> reply channels after a
// successful ask, so those per-request presence dirs don't accumulate
// forever. This cannot race a Recv: an r-<id> channel has exactly one
// receiver (the ask caller's Recv), which has already returned — and thus
// already run its own removePresence — before ask calls RemoveInbox.
func (s *Spool) RemoveInbox(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := fsutil.CheckDir(s.root); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lock, err := s.lockTransport(context.Background())
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := fsutil.CheckDir(filepath.Dir(s.InboxDir(name))); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := fsutil.CheckDir(s.presentDir()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(s.InboxDir(name)); err != nil {
		return err
	}
	return os.RemoveAll(s.presentNameDir(name))
}

// Presence describes one name's listener state, as reported by `status`/`ping`.
type Presence struct {
	Name   string    `json:"name"`
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
	Queued int       `json:"queued"`
	Alive  bool      `json:"alive"`
}

// presenceFile is the on-disk JSON shape of one token file under
// <root>/present/<name>/.
type presenceFile struct {
	PID   int       `json:"pid"`
	Since time.Time `json:"since"`
}

// presentNameDir returns <root>/present/<name>, the directory holding one
// token file per Recv currently parked as name.
func (s *Spool) presentNameDir(name string) string {
	return filepath.Join(s.presentDir(), name)
}

// writePresence records that a Recv for name is parked right now: it writes
// <root>/present/<name>/<token> atomically (tmp/ then rename), mirroring
// Send. token is unique per Recv call (see Recv), so concurrent receivers
// parked as the same name each get their own file and never collide. This is
// a heartbeat, not part of message delivery, so any failure here is
// swallowed — it must never change Recv's success/err/timeout result.
func (s *Spool) writePresence(name, token string) {
	dir := s.presentNameDir(name)
	for _, d := range []string{dir, s.tmpDir()} {
		if err := fsutil.MkdirPrivate(d); err != nil {
			return
		}
	}
	data, err := json.Marshal(presenceFile{PID: os.Getpid(), Since: time.Now().UTC()})
	if err != nil {
		return
	}
	tmp := filepath.Join(s.tmpDir(), "present-"+envelope.NewID())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	// Wrap the tmp→final rename in retryClaimOp so a transient Windows sharing
	// violation (a racing token write/remove for the same name briefly holds a
	// handle on the final path) is retried rather than silently dropping the
	// token — which would leak the tmp file and make this presence go missing.
	// On POSIX retryClaimOp never retries, so this is a plain rename there.
	if err := retryClaimOp(func() error {
		return os.Rename(tmp, filepath.Join(dir, token))
	}); err != nil {
		os.Remove(tmp) // best-effort: don't leak the tmp file on a failed rename
	}
}

// removePresence clears only the token file this Recv wrote via
// writePresence — never a sibling receiver's, and never the present/<name>/
// directory itself. Called via defer on every Recv exit path (message,
// timeout, error); best-effort, like writePresence.
//
// Deliberately does NOT remove the now-possibly-empty present/<name>/ dir.
// An earlier version did (best-effort, swallowing "not empty"/any other
// error), which raced a concurrent receiver's startup: that receiver could
// have MkdirAll'd this same dir in writePresence but not yet renamed its
// token in, so this call's rmdir could remove the directory out from under
// it, ENOENT-ing its rename and losing its presence entirely — a listener
// that would then show as wrongly absent in status/ping. Removing only the
// token, never the directory, makes that race impossible by construction:
// there is no code path left that deletes present/<name>/ while a Recv may
// be parked under it. The resulting empty dirs are harmless — presenceFor
// already treats an empty present/<name>/ dir as absent, the same way it
// treats a missing one — and mirror how inbox/<name>/ dirs already persist
// (both bounded by the set of participant names; see RemoveInbox for the
// one place present/<name>/ IS cleaned up, for ephemeral r-<id> channels
// where no such race can occur).
func (s *Spool) removePresence(name, token string) {
	// Wrap the remove in retryClaimOp for the same reason as writePresence's
	// rename: under concurrent same-name churn on Windows a peer's in-flight
	// handle can briefly lock this token file (ERROR_SHARING_VIOLATION), and
	// leaving a stale token behind would keep the name wrongly reported as
	// parked. Still best-effort — after retries are exhausted we give up
	// silently, and on POSIX retryClaimOp never retries.
	retryClaimOp(func() error { return os.Remove(filepath.Join(s.presentNameDir(name), token)) })
}

// ListPresence returns Presence for the union of names that have an inbox
// directory and/or a present/<name>/ dir (regardless of whether any token in
// it is currently live), sorted by name.
func (s *Spool) ListPresence() ([]Presence, error) {
	names := map[string]bool{}
	inboxRoot := filepath.Join(s.root, "inbox")
	if err := fsutil.CheckDir(inboxRoot); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := fsutil.CheckDir(s.presentDir()); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	inboxEntries, err := os.ReadDir(inboxRoot)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, ent := range inboxEntries {
		if ent.IsDir() && validName(ent.Name()) == nil {
			names[ent.Name()] = true
		}
	}
	presentEntries, err := os.ReadDir(s.presentDir())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, ent := range presentEntries {
		if ent.IsDir() && validName(ent.Name()) == nil {
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
// no live token file exists for it (the name may still have a queue).
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
// plus an aggregate view of its present/<name>/ token files. A name is
// present iff at least one token's stored pid is alive (processAlive); the
// representative PID/Since come from the live token with the most-recent
// Since. A missing present/<name>/ dir means absent, not an error. Individual
// token files that are missing (removed mid-scan by a racing Recv exit),
// corrupt, or store a dead pid are skipped rather than failing the whole
// listing — best-effort, same as writePresence/removePresence.
func (s *Spool) presenceFor(name string) (Presence, error) {
	p := Presence{Name: name}
	for _, dir := range []string{s.InboxDir(name), s.presentNameDir(name)} {
		if err := fsutil.CheckDir(dir); err != nil && !os.IsNotExist(err) {
			return Presence{}, err
		}
	}
	entries, err := os.ReadDir(s.InboxDir(name))
	if err != nil && !os.IsNotExist(err) {
		return Presence{}, err
	}
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") {
			p.Queued++
		}
	}
	tokenEntries, err := os.ReadDir(s.presentNameDir(name))
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return Presence{}, err
	}
	var best *presenceFile
	for _, ent := range tokenEntries {
		if ent.IsDir() {
			continue
		}
		data, err := fsutil.ReadFile(filepath.Join(s.presentNameDir(name), ent.Name()), 4096)
		if err != nil {
			// Removed mid-scan by a racing Recv exit, or unreadable; skip
			// rather than fail the whole listing.
			continue
		}
		var pf presenceFile
		if err := json.Unmarshal(data, &pf); err != nil {
			// A malformed token file (e.g. torn write) is skipped, same as a
			// missing one: it self-heals on the next Recv.
			continue
		}
		if !processAlive(pf.PID) {
			continue
		}
		if best == nil || pf.Since.After(best.Since) {
			pfCopy := pf
			best = &pfCopy
		}
	}
	if best == nil {
		return p, nil
	}
	p.PID = best.PID
	p.Since = best.Since
	p.Alive = true
	return p, nil
}
