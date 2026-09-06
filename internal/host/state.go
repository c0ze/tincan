package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/spool"
)

// State is the on-disk record of one hosted listener,
// <room>/.tincan/hosts/<name>.json. serve writes it on start and around
// every message; status reads it for MODE/busy; down reads it for the pid.
type State struct {
	PID       int       `json:"pid"`
	Preset    string    `json:"preset"`
	Exec      []string  `json:"exec"`
	Started   time.Time `json:"started"`
	State     string    `json:"state"`                // "parked" | "busy"
	CurrentID string    `json:"current_id,omitempty"` // message id while busy
}

// WriteState atomically replaces the state file (tmp file in the same dir,
// then rename), so a concurrent status never sees a torn write.
func WriteState(room, name string, st State) error {
	dir := Dir(room)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+"."+envelope.NewID()+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, StatePath(room, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ReadState loads the state file for name; ok is false when there is none.
// A corrupt file is an error (it is a real state file that cannot be trusted).
func ReadState(room, name string) (st State, ok bool, err error) {
	data, err := os.ReadFile(StatePath(room, name))
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
	err := os.Remove(StatePath(room, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Alive reports whether the recorded host process is still running. A state
// file whose pid is dead is stale — the serve process was killed without a
// chance to clean up — and callers must not trust its "busy"/"parked".
func (st State) Alive() bool { return spool.ProcessAlive(st.PID) }

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
