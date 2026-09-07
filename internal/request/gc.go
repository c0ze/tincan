package request

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GC removes only old terminal request results and progress. Locks remain in
// place so a concurrent sender cannot create a second lock inode for the same ID.
// Queued, running, malformed, and unacknowledged work is never swept here.
func GC(ctx context.Context, room string, before time.Time) (int, error) {
	room = canonicalRoom(room)
	entries, err := os.ReadDir(Dir(room))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
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
			if err != nil || !r.Terminal() || !r.Updated.Before(before) {
				return err
			}
			// A terminal result may still be needed to finish a crashed delivery.
			// Retain it whenever any corresponding inflight claim remains.
			// Scan inbox first: an executing receiver moves only inbox -> inflight,
			// so the subsequent scan cannot miss a claim made between the scans.
			for _, kind := range []string{"inbox", "inflight"} {
				claims, err := os.ReadDir(filepath.Join(room, ".tincan", kind, r.Agent))
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				for _, claim := range claims {
					if strings.HasSuffix(claim.Name(), "-"+id+".json") {
						return nil
					}
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
	return count, nil
}
