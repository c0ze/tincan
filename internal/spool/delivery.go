package spool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// Delivery owns a durable envelope claim. Ack commits successful delivery;
// Nack explicitly requeues it. Keeping the Delivery unacknowledged preserves
// evidence across process crashes. This is not an exactly-once side-effect
// guarantee: a crash after an external effect can leave its outcome uncertain.
type Delivery struct {
	Envelope *envelope.Envelope
	spool    *Spool
	name     string
	filename string
	dir      string
	mu       sync.Mutex
	finished bool
}

func (s *Spool) inFlightDir(name string) string { return filepath.Join(s.root, "inflight", name) }
func (d *Delivery) path() string                { return filepath.Join(d.dir, "message.json") }

// Ack removes the in-flight envelope after successful delivery, optionally
// keeping a private copy in log/. A failed Ack leaves the claim available for
// inspection unless the filesystem operation already committed before failure.
func (d *Delivery) Ack(logConsumed bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return nil
	}
	lock, err := d.spool.lockTransport(context.Background())
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := fsutil.CheckDir(d.dir); err != nil {
		return err
	}
	if err := d.spool.quarantineQueuedDuplicate(d.name, d.filename); err != nil {
		return err
	}
	if logConsumed {
		if err := fsutil.MkdirPrivate(d.spool.logDir()); err != nil {
			return err
		}
		logPath := filepath.Join(d.spool.logDir(), envelope.NewID()+"-"+d.filename)
		if err := retryClaimOp(func() error { return os.Rename(d.path(), logPath) }); err != nil {
			return err
		}
		d.finished = true
		stampMoved(logPath)
		if err := fsutil.SyncDir(d.spool.logDir()); err != nil {
			return err
		}
	} else {
		if err := retryClaimOp(func() error { return os.Remove(d.path()) }); err != nil {
			return err
		}
		d.finished = true
	}
	if err := d.spool.clearOwner(d.name, d.filename); err != nil {
		return err
	}
	if err := fsutil.SyncDir(d.dir); err != nil {
		return err
	}
	if err := os.Remove(d.dir); err != nil {
		return err
	}
	return fsutil.SyncDir(d.spool.inFlightDir(d.name))
}

// Nack explicitly requeues an envelope. Callers must use it only when retry is
// safe; interrupted host commands should instead report an uncertain outcome
// and Ack. Recovery never calls Nack automatically; only an interactive
// claim whose owner died is requeued for it (ClaimInteractive).
func (d *Delivery) Nack() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return nil
	}
	lock, err := d.spool.lockTransport(context.Background())
	if err != nil {
		return err
	}
	defer lock.Close()
	return d.nackLocked()
}

// nackLocked is Nack for a caller that holds the transport lock.
func (d *Delivery) nackLocked() error {
	if err := fsutil.CheckDir(d.dir); err != nil {
		return err
	}
	inbox := d.spool.InboxDir(d.name)
	if err := fsutil.MkdirPrivate(inbox); err != nil {
		return err
	}
	dest := filepath.Join(inbox, d.filename)
	// Publish without replacement. The gate prevents receivers from taking
	// this link until the claim is removed. Recovery quarantines any queued
	// duplicate left by a crash in this interval, preserving uncertain work.
	if err := os.Link(d.path(), dest); err != nil {
		// A retry after a partial Nack may already have published this inode.
		claimInfo, claimErr := os.Lstat(d.path())
		queuedInfo, queuedErr := os.Lstat(dest)
		if claimErr != nil || queuedErr != nil || !claimInfo.Mode().IsRegular() ||
			!queuedInfo.Mode().IsRegular() || !os.SameFile(claimInfo, queuedInfo) {
			return err
		}
	}
	if err := fsutil.SyncDir(inbox); err != nil {
		return err
	}
	if err := retryClaimOp(func() error { return os.Remove(d.path()) }); err != nil {
		return err
	}
	d.finished = true
	if err := d.spool.clearOwner(d.name, d.filename); err != nil {
		return err
	}
	if err := fsutil.SyncDir(d.dir); err != nil {
		return err
	}
	if err := os.Remove(d.dir); err != nil {
		return err
	}
	return fsutil.SyncDir(d.spool.inFlightDir(d.name))
}

// claimOldest uses an exclusive mkdir gate before moving a queued file. The
// stable gate remains present throughout delivery, preventing the handle-based
// rename race on Windows from creating multiple owners of one queued file.
func (s *Spool) claimOldest(ctx context.Context, name string, owned bool) (*Delivery, bool, error) {
	lock, err := s.lockTransport(ctx)
	if err != nil {
		return nil, false, err
	}
	defer lock.Close()
	return s.claimOldestLocked(ctx, name, owned)
}

// TryClaim checks the queue once without parking a receiver or waiting for
// transport ownership. A false result means the queue is empty or busy. This
// avoids using a tiny timeout that can expire during filesystem setup before
// an already queued message is examined. The caller owns successful deliveries
// and must Ack or explicitly Nack them, as with ClaimContext.
func (s *Spool) TryClaim(ctx context.Context, name string) (*Delivery, bool, error) {
	if err := validName(name); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := s.ensurePrivateRoot(); err != nil {
		return nil, false, err
	}
	if err := fsutil.MkdirPrivate(s.InboxDir(name)); err != nil {
		return nil, false, err
	}
	if err := s.checkCaseAlias(name); err != nil {
		return nil, false, err
	}
	lock, err := filelock.Try(filepath.Join(s.root, "transport.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer lock.Close()
	return s.claimOldestLocked(ctx, name, false)
}

// The caller must hold transport ownership throughout the queue transition.
// owned records this process as the claim's owner (ClaimInteractive).
func (s *Spool) claimOldestLocked(ctx context.Context, name string, owned bool) (*Delivery, bool, error) {
	inbox := s.InboxDir(name)
	// Recreate an inbox removed while this receiver waited (gc of an old
	// reply channel by an older tincan, manual cleanup); symlinks still fail.
	if err := fsutil.MkdirPrivate(inbox); err != nil {
		return nil, false, err
	}
	if err := s.requeueAbandoned(name); err != nil {
		return nil, false, err
	}
	entries, err := os.ReadDir(inbox) // sorted by filename, hence timestamp
	if err != nil {
		return nil, false, err
	}
	flight := s.inFlightDir(name)
	if err := fsutil.MkdirPrivate(flight); err != nil {
		return nil, false, err
	}
	for _, ent := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		dir := filepath.Join(flight, ent.Name())
		err := os.Mkdir(dir, 0o700)
		if os.IsExist(err) {
			// Under the transport lock no transition is mid-flight, so an
			// empty gate is crash debris (after the mkdir but before the
			// message moved in, or in Nack after the message went back to
			// the inbox), as published() also assumes. Clear it and claim.
			cleared, clearErr := s.clearEmptyGate(name, ent.Name())
			if clearErr != nil {
				return nil, false, clearErr
			}
			if !cleared {
				continue // a real claim, or unexpected contents for recovery
			}
			err = os.Mkdir(dir, 0o700)
		}
		if err != nil {
			return nil, false, err
		}
		// Record the owner while the gate is still empty: a crash before the
		// rename leaves an empty gate whose record lets a later claim clear it.
		if err := s.setOwner(name, ent.Name(), owned); err != nil {
			os.Remove(dir)
			return nil, false, err
		}
		claimed := filepath.Join(dir, "message.json")
		if err := retryClaimOp(func() error { return os.Rename(filepath.Join(inbox, ent.Name()), claimed) }); err != nil {
			s.clearOwner(name, ent.Name())
			os.Remove(dir)
			if lostClaimRace(err) {
				continue
			}
			return nil, false, err
		}
		// Persist the destination before its removal from the queue. No command
		// may execute until these directory changes have been synced.
		for _, path := range []string{dir, flight, inbox} {
			if err := fsutil.SyncDir(path); err != nil {
				return nil, false, err
			}
		}
		delivery, err := s.readDelivery(name, ent.Name())
		if err != nil {
			if !isMalformed(err) {
				// A transient read failure keeps the claim for recovery.
				return nil, false, err
			}
			if err := s.quarantine(dir); err != nil {
				return nil, false, err
			}
			if err := s.clearOwner(name, ent.Name()); err != nil {
				return nil, false, err
			}
			continue // malformed/unsafe messages never stop a healthy listener
		}
		return delivery, true, nil
	}
	return nil, false, nil
}

// malformedError marks claimed input that can never become a valid delivery:
// a missing, unsafe or oversized file, invalid JSON or envelope, or a routing
// mismatch. Only these are quarantined; other read errors (EMFILE, EIO,
// EACCES, ...) say nothing about the message and leave the claim in place.
type malformedError struct{ err error }

func (e malformedError) Error() string { return e.err.Error() }
func (e malformedError) Unwrap() error { return e.err }

func isMalformed(err error) bool {
	var m malformedError
	return errors.As(err, &m)
}

func (s *Spool) readDelivery(name, filename string) (*Delivery, error) {
	dir := filepath.Join(s.inFlightDir(name), filename)
	data, err := fsutil.ReadFile(filepath.Join(dir, "message.json"), MaxMessageBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, fsutil.ErrUnsafeFile) || errors.Is(err, fsutil.ErrTooLarge) {
			return nil, malformedError{err}
		}
		return nil, err
	}
	e, err := envelope.Unmarshal(data)
	if err != nil {
		return nil, malformedError{err}
	}
	// A spelling that differs only by case and names this same directory on a
	// case-folding filesystem (published by an older binary) is this inbox.
	sameInbox := e.To == name || (strings.EqualFold(e.To, name) && s.sameInbox(e.To, name))
	if !sameInbox || envelope.Filename(e) != filename {
		return nil, malformedError{fmt.Errorf("tincan: envelope routing or filename mismatch")}
	}
	return &Delivery{Envelope: e, spool: s, name: name, filename: filename, dir: dir}, nil
}

func (s *Spool) quarantine(path string) error {
	dir := filepath.Join(s.root, "quarantine")
	if err := fsutil.MkdirPrivate(dir); err != nil {
		return err
	}
	dest := filepath.Join(dir, envelope.NewID())
	if err := retryClaimOp(func() error { return os.Rename(path, dest) }); err != nil {
		return err
	}
	stampMoved(dest)
	if err := fsutil.SyncDir(dir); err != nil {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
}

func (s *Spool) quarantineQueuedDuplicate(name, filename string) error {
	inbox := s.InboxDir(name)
	if err := fsutil.CheckDir(inbox); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	queued := filepath.Join(inbox, filename)
	if _, err := os.Lstat(queued); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return s.quarantine(queued)
}

// clearEmptyGate removes name's gate for filename and its owner record if the
// gate is empty. The caller holds the transport lock.
func (s *Spool) clearEmptyGate(name, filename string) (bool, error) {
	gate := filepath.Join(s.inFlightDir(name), filename)
	entries, err := os.ReadDir(gate)
	if err != nil || len(entries) > 0 {
		return false, nil
	}
	if err := os.Remove(gate); err != nil {
		return false, nil
	}
	return true, s.clearOwner(name, filename)
}

// discardGate clears a gate that holds no valid claim (readErr is malformed):
// an empty gate, left by a crash before rename or after acknowledgment, is
// removed; unexpected contents are quarantined. Its owner record goes too.
func (s *Spool) discardGate(name, filename string, readErr error) error {
	path := filepath.Join(s.inFlightDir(name), filename)
	removed := false
	if errors.Is(readErr, os.ErrNotExist) {
		err := os.Remove(path)
		removed = err == nil || os.IsNotExist(err)
	}
	if !removed {
		if err := s.quarantine(path); err != nil {
			return err
		}
	}
	return s.clearOwner(name, filename)
}

// ListInFlight inspects durable deliveries for a listener. It never queues or
// executes them. Do not Ack these returned handles concurrently with an active
// owner; hosted listeners must hold their exclusive lifetime lock first.
// Malformed claims are skipped; RecoverInFlight can quarantine them once no
// active owner remains.
func (s *Spool) ListInFlight(name string) ([]*Delivery, error) {
	return s.listInFlight(name, false)
}

// RecoverInFlight is for startup after acquiring the exclusive listener lock.
// It returns interrupted claims for reporting/acknowledgment, and clears empty
// gates left by a crash before rename or after acknowledgment. It never retries
// commands: callers should report their outcome as interrupted/uncertain.
// Interactive claims (ClaimInteractive) whose owner is still running are left
// to that owner; those of a dead owner are taken over and no longer requeued.
func (s *Spool) RecoverInFlight(name string) ([]*Delivery, error) {
	return s.listInFlight(name, true)
}

func (s *Spool) listInFlight(name string, recoverEmpty bool) ([]*Delivery, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	dir := s.inFlightDir(name)
	if err := fsutil.CheckDir(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lock, err := s.lockTransport(context.Background())
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var deliveries []*Delivery
	for _, ent := range entries {
		if !ent.IsDir() {
			if recoverEmpty {
				if err := s.quarantine(filepath.Join(dir, ent.Name())); err != nil {
					return nil, err
				}
			}
			continue
		}
		// An interactive claim belongs to its recorded owner while that
		// process may run (or while its record cannot be read); the owner
		// will Ack or Nack it itself. Recovery leaves such a gate alone.
		if recoverEmpty && s.ownerHolds(name, ent.Name()) {
			continue
		}
		d, err := s.readDelivery(name, ent.Name())
		if err != nil {
			if !recoverEmpty {
				continue
			}
			if !isMalformed(err) {
				return nil, err
			}
			if err := s.discardGate(name, ent.Name(), err); err != nil {
				return nil, err
			}
			continue
		}
		if recoverEmpty {
			// Otherwise recovery takes the claim over, under this same lock,
			// so that requeueAbandoned can no longer deliver it a second time.
			if err := s.clearOwner(name, ent.Name()); err != nil {
				return nil, err
			}
			// An interrupted explicit Nack may have published an inbox link
			// before removing its durable claim. Preserve that duplicate as
			// evidence rather than executing uncertain work after recovery.
			if err := s.quarantineQueuedDuplicate(name, ent.Name()); err != nil {
				return nil, err
			}
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, nil
}
