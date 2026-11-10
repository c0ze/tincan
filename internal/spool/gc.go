package spool

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// GCStats counts terminal artifacts removed by GC.
type GCStats struct {
	Logs        int
	Quarantined int
	Presence    int // tokens of receivers that provably exited without cleanup
	Channels    int // abandoned, empty reply channels (r-<hex>)
	Temp        int // temporary files left by interrupted writes
}

// GC removes log and quarantine artifacts last modified before the cutoff.
// Moving an entry into log/ or quarantine/ stamps its mtime, so retention
// counts from when it was logged or quarantined. It also removes presence
// tokens of receivers that provably exited without cleaning up, regardless
// of age; reply channels that are empty, unowned and unused since the cutoff;
// and temporary files of writes interrupted before the cutoff. It never
// removes queued messages, in-flight claims or host state, regardless of age.
func (s *Spool) GC(before time.Time) (GCStats, error) {
	var stats GCStats
	if err := s.gcPresence(&stats); err != nil {
		return stats, err
	}
	if err := s.gcChannels(before, &stats); err != nil {
		return stats, err
	}
	if err := s.gcTemp(before, &stats); err != nil {
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

// gcChannels removes reply channels nobody will use again: a timed-out ask
// or a request collected through MCP leaves an empty r-<hex> channel behind.
// A channel goes only when its inbox and in-flight directories are empty and
// unchanged since the cutoff, and it has no owner record and no presence
// token left after dead ones are reaped. The transport lock keeps sends and
// claims out meanwhile; a later send or receive simply recreates it.
func (s *Spool) gcChannels(before time.Time, stats *GCStats) error {
	names := map[string]bool{}
	for _, root := range []string{filepath.Join(s.root, "inbox"), filepath.Join(s.root, "inflight"), s.presentDir(), filepath.Join(s.root, "owners")} {
		if err := fsutil.CheckDir(root); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if ent.IsDir() && replyChannel(ent.Name()) {
				names[ent.Name()] = true
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	lock, err := s.lockTransport(context.Background())
	if err != nil {
		return err
	}
	defer lock.Close()
	for name := range names {
		swept, err := s.sweepChannel(name, before)
		if err != nil {
			return err
		}
		if swept {
			stats.Channels++
		}
	}
	return nil
}

// sweepChannel removes one abandoned reply channel; the caller holds the
// transport lock. Presence and owner directories count by emptiness only:
// reaping a dead token itself touches the presence directory.
func (s *Spool) sweepChannel(name string, before time.Time) (bool, error) {
	if _, _, err := s.scanPresence(name); err != nil {
		return false, err
	}
	dirs := []string{s.presentNameDir(name), s.ownerDir(name), s.inFlightDir(name), s.InboxDir(name)}
	for i, dir := range dirs {
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() || (i >= 2 && !info.ModTime().Before(before)) {
			return false, nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false, err
		}
		if len(entries) > 0 {
			return false, nil
		}
	}
	for _, dir := range dirs {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	return true, nil
}

// replyChannel reports the reply channel names tincan creates itself: "r-"
// and 32 (ask) or 64 (MCP requests) lowercase hex digits.
func replyChannel(name string) bool {
	digits, ok := strings.CutPrefix(name, "r-")
	if !ok || (len(digits) != 32 && len(digits) != 64) {
		return false
	}
	for _, c := range digits {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// gcTemp removes temporary files that an interrupted write left behind:
// tmp/send-* and tmp/present-*, and the .write-* files of atomic state writes
// and .owner-* files of claim records anywhere in the spool. Every writer
// removes its own temporary within moments, so one older than the cutoff
// belongs to a process that died mid-write. log/ and quarantine/ hold
// evidence and are not searched.
func (s *Spool) gcTemp(before time.Time, stats *GCStats) error {
	if err := fsutil.CheckDir(s.root); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(s.root, func(path string, ent fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // removed while walking
			}
			return err
		}
		if ent.IsDir() {
			if path == s.logDir() || path == filepath.Join(s.root, "quarantine") {
				return filepath.SkipDir
			}
			return nil
		}
		name := ent.Name()
		temp := strings.HasPrefix(name, ".write-") || strings.HasPrefix(name, ".owner-") ||
			(filepath.Dir(path) == s.tmpDir() && (strings.HasPrefix(name, "send-") || strings.HasPrefix(name, "present-")))
		if !temp || !ent.Type().IsRegular() {
			return nil
		}
		info, err := ent.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.ModTime().Before(before) {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		stats.Temp++
		return nil
	})
}
