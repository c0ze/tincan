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
}

// GC removes log and quarantine artifacts last modified before the cutoff.
// It never visits inboxes, in-flight claims, temporary writes, presence tokens,
// or host state: those may contain queued/active work, regardless of age.
func (s *Spool) GC(before time.Time) (GCStats, error) {
	var stats GCStats
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
