package spool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeToken publishes one presence token the way writePresence lays it out.
func writeToken(t *testing.T, sp *Spool, name, token string, pf presenceFile) string {
	t.Helper()
	dir := sp.presentNameDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(pf)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, token)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireStartIdentity(t *testing.T) {
	t.Helper()
	if _, _, ok := processStart(os.Getpid()); !ok {
		t.Skip("no process start identity on this OS")
	}
}

// A token whose recorded process identity differs from the process now
// holding its PID belongs to a dead receiver: the PID was reused. It must not
// read as a parked listener, and it is removed.
func TestPresenceTokenOfReusedPIDIsAbsentAndReaped(t *testing.T) {
	requireStartIdentity(t)
	sp, _ := Open(t.TempDir())
	path := writeToken(t, sp, "b", "reused", presenceFile{PID: os.Getpid(), Since: time.Now().UTC(), Start: "an-earlier-process"})
	if p, ok, err := sp.Present("b"); err != nil || ok {
		t.Fatalf("Present = %+v, %v, %v; want absent", p, ok, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("reused-PID token kept: %v", err)
	}
}

// A legacy token (no start identity) whose PID now belongs to a process that
// started after the token was written is absent. That proof rests on the wall
// clock, so the token is only ignored, not deleted, while the PID lives.
func TestLegacyTokenWithReusedPIDIsAbsentButKept(t *testing.T) {
	requireStartIdentity(t)
	sp, _ := Open(t.TempDir())
	stale := writeToken(t, sp, "b", "legacy-stale", presenceFile{PID: os.Getpid(), Since: time.Now().Add(-24 * time.Hour).UTC()})
	if _, ok, err := sp.Present("b"); err != nil || ok {
		t.Fatalf("legacy token of a reused PID is present (%v, %v)", ok, err)
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Fatalf("legacy token deleted on a clock-based proof: %v", err)
	}
	writeToken(t, sp, "b", "legacy-live", presenceFile{PID: os.Getpid(), Since: time.Now().UTC()})
	if p, ok, err := sp.Present("b"); err != nil || !ok || p.PID != os.Getpid() {
		t.Fatalf("legacy token of a live receiver: %+v, %v, %v", p, ok, err)
	}
}

// Tokens of provably dead receivers are removed when presence is read and
// by GC; a live receiver's token is never touched.
func TestDeadOwnerTokensAreReapedByPresenceAndGC(t *testing.T) {
	sp, _ := Open(t.TempDir())
	const deadPID = 1 << 30
	live := writeToken(t, sp, "b", "live", presenceFile{PID: os.Getpid(), Since: time.Now().UTC(), Start: selfStart()})
	dead := []string{
		writeToken(t, sp, "b", "legacy-dead", presenceFile{PID: deadPID, Since: time.Now().UTC()}),
		writeToken(t, sp, "b", "dead", presenceFile{PID: deadPID, Since: time.Now().UTC(), Start: "x"}),
	}
	if _, err := sp.ListPresence(); err != nil {
		t.Fatal(err)
	}
	for _, path := range dead {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("dead token %s kept by ListPresence: %v", path, err)
		}
	}
	gone := writeToken(t, sp, "ghost", "dead", presenceFile{PID: deadPID, Since: time.Now().UTC()})
	stats, err := sp.GC(time.Now().Add(-time.Hour))
	if err != nil || stats.Presence != 1 {
		t.Fatalf("GC = %+v, %v; want one reaped token", stats, err)
	}
	if _, err := os.Lstat(gone); !os.IsNotExist(err) {
		t.Fatalf("GC kept a dead token: %v", err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Fatalf("live token removed: %v", err)
	}
}

func TestParkedReceiveRecordsStartIdentity(t *testing.T) {
	requireStartIdentity(t)
	room := t.TempDir()
	sp, _ := Open(room)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sp.ClaimContext(ctx, "b", 10*time.Second)
	}()
	waitForPresenceFile(t, room, "b")
	entries, _ := os.ReadDir(sp.presentNameDir("b"))
	data, err := os.ReadFile(filepath.Join(sp.presentNameDir("b"), entries[0].Name()))
	cancel()
	<-done
	var pf presenceFile
	if err != nil || json.Unmarshal(data, &pf) != nil {
		t.Fatalf("token %q: %v", data, err)
	}
	if pf.PID != os.Getpid() || pf.Start == "" || pf.Start != selfStart() {
		t.Fatalf("token = %+v, want pid %d start %q", pf, os.Getpid(), selfStart())
	}
}

// A receiver killed while parked (SIGKILL, reboot) never runs its cleanup.
// Its token must stop counting as a listener and be reaped.
func TestKilledReceiverIsAbsentAndItsTokenReaped(t *testing.T) {
	if room := os.Getenv("TINCAN_TEST_PARK_ROOM"); room != "" {
		sp, err := Open(room)
		if err != nil {
			os.Exit(2)
		}
		sp.ClaimContext(context.Background(), "parked", 0)
		os.Exit(3)
	}
	room := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledReceiverIsAbsentAndItsTokenReaped$")
	cmd.Env = append(os.Environ(), "TINCAN_TEST_PARK_ROOM="+room)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPresenceFile(t, room, "parked")
	sp, _ := Open(room)
	if p, ok, err := sp.Present("parked"); err != nil || !ok || p.PID != cmd.Process.Pid {
		t.Fatalf("parked child not present: %+v, %v, %v", p, ok, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if _, ok, err := sp.Present("parked"); err != nil || ok {
		t.Fatalf("killed receiver still present (%v, %v)", ok, err)
	}
	if entries, _ := os.ReadDir(sp.presentNameDir("parked")); len(entries) != 0 {
		t.Fatalf("killed receiver's token kept: %v", entries)
	}
}
