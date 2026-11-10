package spool

import (
	"os"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// GCStats counts terminal artifacts removed by GC.
type GCStats struct {
	Logs        int
	Quarantined int
	Presence    int // tokens of receivers that provably exited without cleanup
}

// GC removes log and quarantine artifacts last modified before the cutoff.
// Moving an entry into log/ or quarantine/ stamps its mtime, so retention
// counts from when it was logged or quarantined. It also removes presence
// tokens of receivers that provably exited without cleaning up, regardless
// of age. It never visits inboxes, in-flight claims, temporary writes, or
// host state: those may contain queued/active work, regardless of age.
func (s *Spool) GC(before time.Time) (GCStats, error) {
	var stats GCStats
	if err := s.gcPresence(&stats); err != nil {
		return stats, err
	}
	for _, kind := range []string{"log", "quarantine"} {
		dir := filepath.Join(s.root, kind)
		if err := fsutil.CheckDir(dir); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return stats, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return stats, err
		}
		for _, ent := range entries {
			path := filepath.Join(dir, ent.Name())
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return stats, err
			}
			if !info.ModTime().Before(before) {
				continue
			}
			// Logs are flat files; avoid recursively sweeping an unexpected
			// directory. Quarantine owns complete terminal claim directories.
			if kind == "log" && info.IsDir() {
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				return stats, err
			}
			if kind == "log" {
				stats.Logs++
			} else {
				stats.Quarantined++
			}
		}
		if err := fsutil.SyncDir(dir); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// gcPresence reaps dead receivers' tokens under every present/<name>/.
func (s *Spool) gcPresence(stats *GCStats) error {
	if err := fsutil.CheckDir(s.presentDir()); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(s.presentDir())
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if !ent.IsDir() || validName(ent.Name()) != nil {
			continue
		}
		_, reaped, err := s.scanPresence(ent.Name())
		if err != nil {
			return err
		}
		stats.Presence += reaped
	}
	return nil
}
