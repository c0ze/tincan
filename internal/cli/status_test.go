package cli

import (
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/host"
)

// writeStateFile records a hosted listener the way serve does, so status
// tests stay hermetic (no daemon started).
func writeStateFile(t *testing.T, room, name string, pid int, preset, state, currentID string) {
	t.Helper()
	err := host.WriteState(room, name, host.State{
		PID: pid, Preset: preset, Exec: []string{preset, "-p", "{body}"},
		Started: time.Now().UTC(), State: state, CurrentID: currentID,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func statusJSON(t *testing.T, room string) map[string]statusRow {
	t.Helper()
	code, stdout, stderr := run("status", "--room", room, "--format", "json")
	if code != ExitOK {
		t.Fatalf("status exit %d: %s", code, stderr)
	}
	var rows []statusRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("status json: %v\n%s", err, stdout)
	}
	byName := map[string]statusRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	return byName
}

func TestStatusHostedParked(t *testing.T) {
	room := t.TempDir()
	writePresenceFile(t, room, "codex", os.Getpid(), time.Now())
	writeStateFile(t, room, "codex", os.Getpid(), "codex", "parked", "")
	_, table, _ := run("status", "--room", room)
	if !strings.Contains(table, "MODE") || !strings.Contains(table, "hosted:codex") || !strings.Contains(table, "parked") {
		t.Fatalf("table = %q", table)
	}
	r := statusJSON(t, room)["codex"]
	if r.Mode != "hosted:codex" || r.Preset != "codex" || r.Busy || !r.Alive || r.PID != os.Getpid() {
		t.Fatalf("row = %+v", r)
	}
}

func TestStatusHostedBusyWithoutPresence(t *testing.T) {
	room := t.TempDir()
	writeStateFile(t, room, "kimi", os.Getpid(), "kimi", "busy", "m1")
	_, table, _ := run("status", "--room", room)
	if !strings.Contains(table, "hosted:kimi") || !strings.Contains(table, "busy") {
		t.Fatalf("table = %q", table)
	}
	if !strings.Contains(table, " "+strconv.Itoa(os.Getpid())+" ") {
		t.Fatalf("busy row should show the host pid %d: %q", os.Getpid(), table)
	}
	r := statusJSON(t, room)["kimi"]
	if r.Mode != "hosted:kimi" || !r.Busy || r.CurrentID != "m1" || r.PID != os.Getpid() || r.Alive {
		t.Fatalf("row = %+v", r)
	}
}

func TestStatusStaleStateFileIsIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dead-pid detection is unix-only (see spool/alive_other.go)")
	}
	room := t.TempDir()
	writeStateFile(t, room, "ghost", 1<<30, "codex", "busy", "m9")
	_, table, _ := run("status", "--room", room)
	if strings.Contains(table, "busy") || strings.Contains(table, "hosted") || !strings.Contains(table, "ghost") {
		t.Fatalf("stale state must render as absent: %q", table)
	}
	r := statusJSON(t, room)["ghost"]
	if r.Mode != "" || r.Busy || r.Alive {
		t.Fatalf("row = %+v", r)
	}
}

func TestStatusPlainListenerIsAgentMode(t *testing.T) {
	room := t.TempDir()
	writePresenceFile(t, room, "claude", os.Getpid(), time.Now())
	_, table, _ := run("status", "--room", room)
	if !strings.Contains(table, "agent") || !strings.Contains(table, "parked") {
		t.Fatalf("table = %q", table)
	}
	if r := statusJSON(t, room)["claude"]; r.Mode != "agent" || r.Busy {
		t.Fatalf("row = %+v", r)
	}
}

func TestStatusQueuedOnlyHasNoMode(t *testing.T) {
	room := t.TempDir()
	if code, _, stderr := run("send", "--room", room, "--to", "nobody", "--from", "orch", "--body", "hi"); code != 0 {
		t.Fatal(stderr)
	}
	if r := statusJSON(t, room)["nobody"]; r.Mode != "" || r.Queued != 1 {
		t.Fatalf("row = %+v", r)
	}
	_, table, _ := run("status", "--room", room)
	if !strings.Contains(table, "—") {
		t.Fatalf("table should render absent mode/state as —: %q", table)
	}
}
