package rooms

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// HeartbeatFresh is how recent web.json must be for MCP and CLI entry points
// to trust that a coordinator (`tincan web`) is running (committees §6.3).
const HeartbeatFresh = 30 * time.Second

// Heartbeat is <state>/web.json, written by `tincan web`.
type Heartbeat struct {
	PID     int       `json:"pid"`
	Machine string    `json:"machine"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
}

func HeartbeatPath(stateDir string) string { return filepath.Join(stateDir, "web.json") }

func WriteHeartbeat(stateDir string, hb Heartbeat) error {
	if err := fsutil.MkdirPrivate(stateDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(hb, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(HeartbeatPath(stateDir), data)
}

func readHeartbeat(stateDir string) (Heartbeat, error) {
	var hb Heartbeat
	data, err := fsutil.ReadFile(HeartbeatPath(stateDir), 4096)
	if err != nil {
		return hb, err
	}
	err = json.Unmarshal(data, &hb)
	return hb, err
}

// RemoveHeartbeat deletes web.json on a clean shutdown, but only if it is
// still this process's (a newer daemon may have replaced it).
func RemoveHeartbeat(stateDir string, pid int) error {
	hb, err := readHeartbeat(stateDir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && hb.PID != pid) {
		return nil
	}
	if err := os.Remove(HeartbeatPath(stateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// CoordinatorAlive reports whether web.json was updated within
// HeartbeatFresh of now. A missing or unreadable file means no coordinator.
func CoordinatorAlive(stateDir string, now time.Time) bool {
	hb, err := readHeartbeat(stateDir)
	return err == nil && !hb.Updated.IsZero() && now.Sub(hb.Updated) < HeartbeatFresh
}
