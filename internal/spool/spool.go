// Package spool implements the tincan filesystem spool: atomic sends into
// per-name inbox directories and blocking receives that claim exactly once.
package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func (s *Spool) tmpDir() string { return filepath.Join(s.root, "tmp") }
func (s *Spool) logDir() string { return filepath.Join(s.root, "log") }

// Send delivers e into the To inbox atomically: write to tmp/, then rename.
// Messages queue until a receiver claims them. Fills ID and TS if unset.
func (s *Spool) Send(e *envelope.Envelope) error {
	if e.To == "" {
		return errors.New("tincan: send: empty To")
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
// returns it. Returns ErrTimeout if nothing arrives within timeout.
func (s *Spool) Recv(name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error) {
	inbox := s.InboxDir(name)
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		return nil, err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	defer watcher.Close()
	// Watch before scanning, so a message landing mid-scan is never missed.
	if err := watcher.Add(inbox); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		e, ok, err := s.claimOldest(name, logConsumed)
		if err != nil {
			return nil, err
		}
		if ok {
			return e, nil
		}
		select {
		case <-watcher.Events:
			// Inbox changed; rescan.
		case werr := <-watcher.Errors:
			return nil, werr
		case <-deadline.C:
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
			continue // another receiver claimed it first
		}
		data, err := os.ReadFile(claimed)
		if err != nil {
			return nil, false, err
		}
		e, err := envelope.Unmarshal(data)
		if err != nil {
			// Deliberate quarantine: the claimed file stays in tmp/ (never
			// retried, swept by the Phase-2 gc) so a corrupt message can't
			// cause an infinite reparse loop.
			return nil, false, fmt.Errorf("tincan: bad message %s: %w", f, err)
		}
		if logConsumed {
			if err := os.MkdirAll(s.logDir(), 0o755); err != nil {
				return nil, false, err
			}
			if err := os.Rename(claimed, filepath.Join(s.logDir(), f)); err != nil {
				return nil, false, err
			}
		} else if err := os.Remove(claimed); err != nil {
			return nil, false, err
		}
		return e, true, nil
	}
	return nil, false, nil
}
