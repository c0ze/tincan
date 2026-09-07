package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/c0ze/tincan/internal/fsutil"
	"github.com/c0ze/tincan/internal/spool"
)

// State is the on-disk record of one hosted listener,
// <room>/.tincan/hosts/<name>.json. serve writes it on start and around
// every message; status reads it for MODE/busy; down authenticates its control endpoint.
type State struct {
	Owner          string    `json:"owner,omitempty"`
	ControlAddress string    `json:"control_address,omitempty"`
	ControlToken   string    `json:"control_token,omitempty"`
	PID            int       `json:"pid"`
	Preset         string    `json:"preset"`
	Session        string    `json:"session_mode,omitempty"`
	Config         *Preset   `json:"config,omitempty"`
	Exec           []string  `json:"exec"`
	Started        time.Time `json:"started"`
	State          string    `json:"state"`                // "parked" | "busy"
	CurrentID      string    `json:"current_id,omitempty"` // message id while busy
}

// WriteState atomically replaces the state file (tmp file in the same dir,
// then rename), so a concurrent status never sees a torn write.
func WriteState(room, name string, st State) error {
	if err := spool.ValidName(name); err != nil {
		return err
	}
	dir := Dir(room)
	if err := fsutil.MkdirPrivate(dir); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	previous, ok, err := ReadState(room, name)
	if err != nil {
		return err
	}
	if ok && previous.Owner != "" && previous.Owner != st.Owner {
		return errors.New("host state belongs to another lifetime")
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(StatePath(room, name), data)
}

// ReadState loads the state file for name; ok is false when there is none.
// A corrupt file is an error (it is a real state file that cannot be trusted).
func ReadState(room, name string) (st State, ok bool, err error) {
	if err := spool.ValidName(name); err != nil {
		return State{}, false, err
	}
	data, err := fsutil.ReadFile(StatePath(room, name), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false, fmt.Errorf("%s: %w", StatePath(room, name), err)
	}
	return st, true, nil
}

// RemoveState deletes the state file; a missing file is not an error.
func RemoveState(room, name string) error {
	if err := spool.ValidName(name); err != nil {
		return err
	}
	err := os.Remove(StatePath(room, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RemoveOwnedState removes only the state written by this host lifetime.
// Callers must hold the listener's lifetime lock to exclude replacements.
func RemoveOwnedState(room, name, owner string) error {
	st, ok, err := ReadState(room, name)
	if err != nil || !ok {
		return err
	}
	if st.Owner != owner {
		return nil
	}
	return RemoveState(room, name)
}

// Alive authenticates this exact host lifetime. Legacy PID-only records are
// untrusted, because the recorded PID may now belong to another process.
func (st State) Alive() bool { return st.Ready(context.Background()) }

// ListStates returns every readable state file in the room keyed by
// listener name, stale ones included (callers decide via Alive). Temp files
// and unreadable/corrupt files are skipped, best-effort like presence.
func ListStates(room string) (map[string]State, error) {
	entries, err := os.ReadDir(Dir(room))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]State{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]State{}
	for _, ent := range entries {
		n := ent.Name()
		if ent.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".json") {
			continue
		}
		name := strings.TrimSuffix(n, ".json")
		st, ok, err := ReadState(room, name)
		if err != nil || !ok {
			continue
		}
		out[name] = st
	}
	return out, nil
}
