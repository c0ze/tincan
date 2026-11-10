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

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/fsutil"
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
	if err := os.Chmod(s.root, 0o700); err != nil {
		return err
	}
	return s.ensureGitignore()
}

// ensureGitignore keeps the room's runtime state out of version control even
// when the project has no .tincan/ ignore rule: prompts, replies and logs may
// contain credentials. A non-empty file or any non-regular entry is left alone;
// a missing or empty one (an interrupted earlier write) is written atomically.
func (s *Spool) ensureGitignore() error {
	path := filepath.Join(s.root, ".gitignore")
	info, err := os.Lstat(path)
	if err == nil && (!info.Mode().IsRegular() || info.Size() > 0) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte("# Created by tincan: runtime state, never commit.\n*\n"))
}

// validName rejects names that could escape the spool root: a name must be a
// single, portable path component (no separators on any OS, no traversal).
func validName(name string) error { return envelope.ValidComponent(name) }

// ValidName reports whether name is a legal participant name (a single path
// component, no traversal). Exported for the hosted-listener commands, which
// build <room>/.tincan/hosts/<name>.* paths from the name.
func ValidName(name string) error { return validName(name) }

// ProcessAlive reports whether pid names a live process, using the same
// per-OS check status/ping apply to presence tokens. Hosted-listener state
// files use the identical check.
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
	if err := s.checkCaseAlias(e.To); err != nil {
		return err
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
// other exit path. Hosted listeners use ClaimContext directly so they can
// defer acknowledgement until the result has been delivered.
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
// to retry. Cancellation never deletes an already claimed envelope. A claim
// left by a process that died stays in flight for the caller's own recovery
// (RecoverInFlight); see ClaimInteractive for claims that should be requeued.
func (s *Spool) ClaimContext(ctx context.Context, name string, timeout time.Duration) (*Delivery, error) {
	return s.claim(ctx, name, timeout, false)
}

// ClaimInteractive is ClaimContext for a receiver that hands the message on
// itself (tincan recv and ask). It records this process as the claim's owner
// under owners/<name>/, so if the process exits before Ack or Nack (killed,
// crashed, a failed Nack), the next claim of the same name requeues the
// message instead of leaving it stranded in flight. Hosted listeners and
// request polling recover their own claims and must not have them delivered
// again, so they use ClaimContext or TryClaim, which record no owner.
func (s *Spool) ClaimInteractive(ctx context.Context, name string, timeout time.Duration) (*Delivery, error) {
	return s.claim(ctx, name, timeout, true)
}

func (s *Spool) claim(ctx context.Context, name string, timeout time.Duration, owned bool) (*Delivery, error) {
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
	token := envelope.NewID()
	if err := s.register(ctx, name, token); err != nil {
		return nil, err
	}
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
		delivery, ok, err := s.claimOldest(claimCtx, name, owned)
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

// checkCaseAlias refuses name when a case-folding filesystem (the macOS and
// Windows defaults) maps it onto an inbox created under another spelling:
// both names would share one directory, and messages addressed to one would
// be claimed by the other. The error names the spelling that owns the inbox.
// Directories that merely differ by case on a case-sensitive filesystem are
// separate inboxes, so the name grammar stays the same everywhere.
func (s *Spool) checkCaseAlias(name string) error {
	root := filepath.Join(s.root, "inbox")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, ent := range entries {
		if ent.Name() == name {
			return nil // the inbox is stored under exactly this spelling
		}
	}
	for _, ent := range entries {
		if strings.EqualFold(ent.Name(), name) && s.sameInbox(ent.Name(), name) {
			return fmt.Errorf("tincan: name %q shares the existing inbox %q on this case-insensitive filesystem; use %q", name, ent.Name(), ent.Name())
		}
	}
	return nil
}

// sameInbox reports whether two spellings resolve to one inbox directory.
func (s *Spool) sameInbox(a, b string) bool {
	ai, err := os.Lstat(s.InboxDir(a))
	if err != nil {
		return false
	}
	bi, err := os.Lstat(s.InboxDir(b))
	return err == nil && os.SameFile(ai, bi)
}

// register creates name's inbox and publishes this receive's presence token
// while holding the transport lock. gc sweeps abandoned reply channels under
// the same lock, so it never removes a channel between a receiver creating
// (or finding) its inbox and announcing itself in it.
func (s *Spool) register(ctx context.Context, name, token string) error {
	lock, err := s.lockTransport(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := fsutil.MkdirPrivate(s.InboxDir(name)); err != nil {
		return err
	}
	if err := s.checkCaseAlias(name); err != nil {
		return err
	}
	s.writePresence(name, token)
	return nil
}

// retryClaimOp runs op, retrying briefly while it fails with a Windows
// sharing/lock violation from a concurrent file operation. A missing source
// ends the retry so the caller can recognize a lost claim race. Persistent
// failures are returned after the bounded retry; POSIX operations never retry.
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

// RemoveInbox deletes a participant's inbox and presence directories. It is
// used to clean up ephemeral reply channels after collection. The caller must
// ensure the channel has no active receiver or pending delivery; this method
// does not cancel a parked receive or an in-flight claim.
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
	if err := os.RemoveAll(s.presentNameDir(name)); err != nil {
		return err
	}
	// The in-flight and owner directories go only when empty: a claim that
	// is still unacknowledged must never be discarded with its channel.
	for _, dir := range []string{s.ownerDir(name), s.inFlightDir(name)} {
		if err := removeEmptyDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// removeEmptyDir removes dir if it exists and is empty.
func removeEmptyDir(dir string) error {
	if err := fsutil.CheckDir(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return err
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Presence describes one name's listener state, as reported by `status`/`ping`.
type Presence struct {
	Name   string    `json:"name"`
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
	Queued int       `json:"queued"`
	Alive  bool      `json:"alive"`
	// InFlight counts claimed messages not yet acknowledged.
	InFlight int `json:"in_flight"`
}

// presenceFile is the on-disk JSON shape of one token file under
// <root>/present/<name>/. Start is the writer's process start identity
// (processStart); tokens written before it existed omit it.
type presenceFile struct {
	PID   int       `json:"pid"`
	Since time.Time `json:"since"`
	Start string    `json:"start,omitempty"`
}

// presentNameDir returns <root>/present/<name>, the directory holding one
// token file per ClaimContext currently parked as name.
func (s *Spool) presentNameDir(name string) string {
	return filepath.Join(s.presentDir(), name)
}

// writePresence records a parked ClaimContext by publishing a temporary token
// with an atomic rename. Each receive gets a unique token so concurrent
// receivers with the same name do not replace one another. Presence is
// best-effort metadata; failure must not change the delivery result.
func (s *Spool) writePresence(name, token string) {
	dir := s.presentNameDir(name)
	for _, d := range []string{dir, s.tmpDir()} {
		if err := fsutil.MkdirPrivate(d); err != nil {
			return
		}
	}
	data, err := json.Marshal(presenceFile{PID: os.Getpid(), Since: time.Now().UTC(), Start: selfStart()})
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

// removePresence clears only the token owned by this ClaimContext, including
// on cancellation or timeout. It leaves the parent directory intact because
// another receiver may be between creating that directory and publishing its
// own token. Empty presence directories count as absent. RemoveInbox performs
// explicit cleanup only when the caller has finished using a reply channel.
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
// it is currently live), sorted by name. Reply channels (r-<hex>) with nothing
// queued, nothing in flight and no live receiver are left out.
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
		if replyChannel(name) && p.Queued == 0 && p.InFlight == 0 && !p.Alive {
			continue // an exhausted or abandoned reply channel; gc removes it
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
// present iff at least one token's writer is still running (ownerLiveness);
// the representative PID/Since come from the live token with the most-recent
// Since. A missing present/<name>/ dir means absent, not an error. Individual
// token files that are missing (removed mid-scan by a racing Recv exit),
// corrupt, or whose writer has exited are skipped rather than failing the
// whole listing — best-effort, same as writePresence/removePresence.
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
	if p.InFlight, err = s.countInFlight(name); err != nil {
		return Presence{}, err
	}
	best, _, err := s.scanPresence(name)
	if err != nil {
		return Presence{}, err
	}
	if best == nil {
		return p, nil
	}
	p.PID = best.PID
	p.Since = best.Since
	p.Alive = true
	return p, nil
}

// countInFlight counts name's claims that hold a message (not yet Ack'd).
func (s *Spool) countInFlight(name string) (int, error) {
	dir := s.inFlightDir(name)
	if err := fsutil.CheckDir(dir); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, ent.Name(), "message.json")); err == nil {
			n++
		}
	}
	return n, nil
}

// scanPresence returns the live token with the most recent Since for name, and
// removes tokens whose writer provably exited without its deferred cleanup
// (killed, crashed, rebooted). Token names are unique per receive, so no live
// receiver can own a removed path; removal is best-effort. A missing
// present/<name>/ dir yields no token and no error.
func (s *Spool) scanPresence(name string) (best *presenceFile, reaped int, err error) {
	dir := s.presentNameDir(name)
	if err := fsutil.CheckDir(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	tokenEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	for _, ent := range tokenEntries {
		if ent.IsDir() {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		data, err := fsutil.ReadFile(path, 4096)
		if err != nil {
			// Removed mid-scan by a racing Recv exit, or unreadable; skip
			// rather than fail the whole listing.
			continue
		}
		var pf presenceFile
		if err := json.Unmarshal(data, &pf); err != nil {
			// A malformed token file is skipped, same as a missing one.
			continue
		}
		switch ownerLiveness(pf.PID, pf.Start, pf.Since) {
		case ownerDead:
			if os.Remove(path) == nil {
				reaped++
			}
			continue
		case ownerReplaced:
			continue
		}
		if best == nil || pf.Since.After(best.Since) {
			pfCopy := pf
			best = &pfCopy
		}
	}
	return best, reaped, nil
}
