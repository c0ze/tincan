package request

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/spool"
)

// GC removes only old terminal request results and progress. Locks remain in
// place so a concurrent sender cannot create a second lock inode for the same ID.
// Queued, running, malformed, and unacknowledged work is never swept here.
// An unreadable record is skipped without stopping the sweep; the returned
// error then names every skipped record alongside the count removed.
func GC(ctx context.Context, room string, before time.Time) (int, error) {
	room = canonicalRoom(room)
	dir := Dir(room)
	if err := fsutil.CheckDir(dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	sp, err := spool.Open(room)
	if err != nil {
		return 0, err
	}
	count := 0
	var skipped []error
	presentJSON := map[string]bool{}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			presentJSON[strings.TrimSuffix(entry.Name(), ".json")] = true
		}
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		// Sweep orphaned progress files (no matching .json) and aged atomic temps.
		if strings.HasSuffix(name, ".jsonl") {
			id := strings.TrimSuffix(name, ".jsonl")
			if validID(id) == nil && !presentJSON[id] {
				l, err := lock(ctx, room, id)
				if err != nil {
					return count, err
				}
				func() {
					defer l.Close()
					if _, err := os.Lstat(recordPath(room, id)); !errors.Is(err, os.ErrNotExist) {
						return
					}
					p := progressPath(room, id)
					if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() && info.ModTime().Before(before) {
						if err := os.Remove(p); err == nil {
							count++
						}
					}
				}()
			}
			continue
		}
		if strings.HasPrefix(name, ".write-") {
			if info, err := entry.Info(); err == nil && info.ModTime().Before(before) && info.ModTime().Before(time.Now().Add(-24*time.Hour)) {
				_ = os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if validID(id) != nil {
			continue
		}
		l, err := lock(ctx, room, id)
		if err != nil {
			return count, err
		}
		err = func() error {
			defer l.Close()
			r, err := Get(room, id)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				skipped = append(skipped, fmt.Errorf("skipped request %q: %w", id, err))
				return nil
			}
			if !r.Terminal() || !r.Updated.Before(before) {
				return nil
			}
			if r.InteractivePublicationUncertain {
				return nil
			}
			// Check both inbox and inflight atomically under transport.lock so
			// a concurrent Nack/requeue (inflight -> inbox) cannot hide a claim.
			published, err := sp.Published(ctx, r.Envelope)
			if err != nil {
				if errors.Is(err, spool.ErrEnvelopeConflict) {
					return nil
				}
				return err
			}
			if published {
				return nil
			}
			// For interactive requests, retain the terminal record while any
			// reply is still queued or in flight so Poll cannot resurrect it.
			if r.Interactive && r.Envelope.ReplyTo != "" {
				p, _, err := sp.Present(r.Envelope.ReplyTo)
				if err != nil {
					return err
				}
				if p.Queued > 0 || p.InFlight > 0 {
					return nil
				}
			}
			if err := os.Remove(recordPath(room, id)); err != nil {
				return err
			}
			if err := os.Remove(progressPath(room, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			count++
			return nil
		}()
		if err != nil {
			return count, err
		}
	}
	return count, errors.Join(skipped...)
}
