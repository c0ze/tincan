package spool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/fsutil"
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
		if err := fsutil.SyncDir(d.spool.logDir()); err != nil {
			return err
		}
	} else {
		if err := retryClaimOp(func() error { return os.Remove(d.path()) }); err != nil {
			return err
		}
		d.finished = true
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
// and Ack. Recovery never calls Nack automatically.
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
func (s *Spool) claimOldest(ctx context.Context, name string) (*Delivery, bool, error) {
	lock, err := s.lockTransport(ctx)
	if err != nil {
		return nil, false, err
	}
	defer lock.Close()
	inbox := s.InboxDir(name)
	if err := fsutil.CheckDir(inbox); err != nil {
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
		if err := os.Mkdir(dir, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil, false, err
		}
		claimed := filepath.Join(dir, "message.json")
		if err := retryClaimOp(func() error { return os.Rename(filepath.Join(inbox, ent.Name()), claimed) }); err != nil {
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
			if err := s.quarantine(dir); err != nil {
				return nil, false, err
			}
			continue // malformed/unsafe messages never stop a healthy listener
		}
		return delivery, true, nil
	}
	return nil, false, nil
}

func (s *Spool) readDelivery(name, filename string) (*Delivery, error) {
	dir := filepath.Join(s.inFlightDir(name), filename)
	data, err := fsutil.ReadFile(filepath.Join(dir, "message.json"), MaxMessageBytes)
	if err != nil {
		return nil, err
	}
	e, err := envelope.Unmarshal(data)
	if err != nil {
		return nil, err
	}
	if e.To != name || envelope.Filename(e) != filename {
		return nil, fmt.Errorf("tincan: envelope routing or filename mismatch")
	}
	return &Delivery{Envelope: e, spool: s, name: name, filename: filename, dir: dir}, nil
}

func (s *Spool) quarantine(path string) error {
	dir := filepath.Join(s.root, "quarantine")
	if err := fsutil.MkdirPrivate(dir); err != nil {
		return err
	}
	if err := retryClaimOp(func() error { return os.Rename(path, filepath.Join(dir, envelope.NewID())) }); err != nil {
		return err
	}
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
		d, err := s.readDelivery(name, ent.Name())
		if err != nil {
			if !recoverEmpty {
				continue
			}
			path := filepath.Join(dir, ent.Name())
			if errors.Is(err, os.ErrNotExist) {
				// Remove only an empty gate; unexpected contents are quarantined.
				if err := os.Remove(path); err == nil {
					continue
				}
			}
			if err := s.quarantine(path); err != nil {
				return nil, err
			}
			continue
		}
		if recoverEmpty {
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
