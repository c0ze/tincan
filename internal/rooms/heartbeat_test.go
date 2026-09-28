package rooms

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHeartbeatFreshness(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	now := time.Now()
	if CoordinatorAlive(dir, now) {
		t.Fatal("alive without a heartbeat")
	}
	if err := WriteHeartbeat(dir, Heartbeat{PID: 42, Machine: "macmini", Started: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	if !CoordinatorAlive(dir, now.Add(HeartbeatFresh-time.Second)) {
		t.Fatal("fresh heartbeat not alive")
	}
	if CoordinatorAlive(dir, now.Add(HeartbeatFresh+time.Second)) {
		t.Fatal("stale heartbeat alive")
	}
	if err := RemoveHeartbeat(dir, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(HeartbeatPath(dir)); err != nil {
		t.Fatal("removed another process's heartbeat")
	}
	if err := RemoveHeartbeat(dir, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(HeartbeatPath(dir)); !os.IsNotExist(err) {
		t.Fatal("own heartbeat not removed")
	}
	os.WriteFile(HeartbeatPath(dir), []byte("garbage"), 0o600)
	if CoordinatorAlive(dir, now) {
		t.Fatal("garbage heartbeat counted as alive")
	}
}
func TestHeartbeatCarriesCommitteeSource(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	WriteHeartbeat(dir, Heartbeat{PID: 1, Machine: "cachyos", CommitteesFrom: "macmini", Updated: time.Now()})
	hb, err := ReadHeartbeat(dir)
	if err != nil || hb.CommitteesFrom != "macmini" || hb.Machine != "cachyos" {
		t.Fatalf("%+v %v", hb, err)
	}
}

// A state directory reached through a symlink (macOS /var, or a relocated
// ~/.local) is returned canonically, so fsutil's symlink checks accept it.
func TestStateDirIsCanonical(t *testing.T) {
	real, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	t.Setenv("TINCAN_STATE_DIR", filepath.Join(link, "tincan"))
	if got, want := StateDir(), filepath.Join(real, "tincan"); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}
