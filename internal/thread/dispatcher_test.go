// internal/thread/dispatcher_test.go
package thread_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/cli"
	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/thread"
)

// The test binary doubles as the hosted listener ("serve") and as scripted
// agents ("fixture <name>"): each run appends to <name>.count, stores its
// stdin in <name>.last, optionally sleeps for <name>.sleep, and prints
// <name>.reply (default "done by <name>").
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
		case "fixture":
			os.Exit(fixtureAgent(os.Args[2]))
		}
	}
	state, _ := os.MkdirTemp("", "tincan-state-")
	os.Setenv("TINCAN_STATE_DIR", state)
	code := m.Run()
	os.RemoveAll(state)
	os.Exit(code)
}

func fixtureAgent(name string) int {
	dir := os.Getenv("TINCAN_FIXTURE_DIR")
	body, _ := io.ReadAll(os.Stdin)
	f, err := os.OpenFile(filepath.Join(dir, name+".count"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		f.WriteString("x\n")
		f.Close()
	}
	os.WriteFile(filepath.Join(dir, name+".last"), body, 0o600)
	if b, err := os.ReadFile(filepath.Join(dir, name+".sleep")); err == nil {
		d, _ := time.ParseDuration(strings.TrimSpace(string(b)))
		time.Sleep(d)
	}
	if b, err := os.ReadFile(filepath.Join(dir, name+".reply")); err == nil {
		os.Stdout.Write(b)
		return 0
	}
	fmt.Printf("done by %s", name)
	return 0
}

type env struct {
	t       *testing.T
	room    string
	fixture string
	opts    dispatch.Options
}

func newEnv(t *testing.T, names ...string) *env {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts are not supported on Windows")
	}
	fixture := t.TempDir()
	t.Setenv("TINCAN_FIXTURE_DIR", fixture)
	room, _ := filepath.EvalSymlinks(t.TempDir())
	return newEnvIn(t, room, fixture, names...)
}

func newEnvIn(t *testing.T, room, fixture string, names ...string) *env {
	exe, _ := os.Executable()
	presets := map[string]host.Preset{}
	for _, n := range names {
		presets[n] = host.Preset{Exec: []string{exe, "fixture", n}, Stdin: "body", Reply: "stdout", ExecTimeoutSec: 60}
	}
	e := &env{t: t, room: room, fixture: fixture, opts: dispatch.Options{Room: room, Presets: presets}}
	t.Cleanup(func() {
		states, _ := host.ListStates(room)
		for name := range states {
			host.Down(context.Background(), room, name, 5*time.Second)
		}
	})
	return e
}

func (e *env) script(name, file, content string) {
	os.WriteFile(filepath.Join(e.fixture, name+"."+file), []byte(content), 0o600)
}

func (e *env) count(name string) int {
	b, _ := os.ReadFile(filepath.Join(e.fixture, name+".count"))
	return strings.Count(string(b), "x")
}

func (e *env) dispatcher() *thread.Dispatcher {
	d := thread.New(e.opts)
	if err := d.Acquire(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(d.Close)
	return d
}

// settle reconciles until cond holds for the thread snapshot.
func (e *env) settle(d *thread.Dispatcher, th *thread.Thread, cond func(thread.Snapshot) bool) thread.Snapshot {
	e.t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		if err := d.Reconcile(context.Background()); err != nil {
			e.t.Fatalf("reconcile: %v", err)
		}
		snap, err := th.Snapshot()
		if err != nil {
			e.t.Fatal(err)
		}
		if cond(snap) {
			return snap
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("thread did not settle: %+v", snap.Messages)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func quiescent(s thread.Snapshot) bool {
	for _, m := range s.Messages {
		if m.Active() || (m.Role == thread.RoleAgent && m.State == thread.StateDone && !m.Handoffs) {
			return false
		}
	}
	return len(s.Messages) > 0
}

func agentMessages(s thread.Snapshot, state string) []thread.Message {
	var out []thread.Message
	for _, m := range s.Messages {
		if m.Role == thread.RoleAgent && (state == "" || m.State == state) {
			out = append(out, m)
		}
	}
	return out
}

func TestRoundTripWithHandoff(t *testing.T) {
	e := newEnv(t, "a", "b")
	e.script("a", "reply", "fixed it. @b please review")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	if _, err := d.Post(context.Background(), th.ID, "@a do x", "k1"); err != nil {
		t.Fatal(err)
	}
	snap := e.settle(d, th, quiescent)
	done := agentMessages(snap, thread.StateDone)
	if len(done) != 2 || done[0].Listener != "a."+th.ID || done[1].Listener != "b."+th.ID || done[1].Text != "done by b" {
		t.Fatalf("messages %+v", snap.Messages)
	}
	last, _ := os.ReadFile(filepath.Join(e.fixture, "b.last"))
	if !strings.Contains(string(last), "fixed it. @b please review") {
		t.Fatalf("b did not see a's reply in its prompt:\n%s", last)
	}
	if e.count("a") != 1 || e.count("b") != 1 {
		t.Fatalf("counts a=%d b=%d", e.count("a"), e.count("b"))
	}
}

func TestPostWithoutMentionGoesToPrimaryAndDedupesClientID(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	m1, _ := d.Post(context.Background(), th.ID, "hello", "same")
	m2, _ := d.Post(context.Background(), th.ID, "hello", "same")
	if m1.ID != m2.ID {
		t.Fatal("duplicate client_id created a second message")
	}
	e.settle(d, th, quiescent)
	if e.count("a") != 1 {
		t.Fatalf("a ran %d times", e.count("a"))
	}
}

func TestUnresolvedMentionPostsSystemMessage(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@nobody hi", "")
	snap, _ := th.Snapshot()
	var sys []thread.Message
	for _, m := range snap.Messages {
		if m.Role == thread.RoleSystem {
			sys = append(sys, m)
		}
	}
	if len(sys) != 1 || !strings.Contains(sys[0].Text, "@nobody") || len(agentMessages(snap, "")) != 0 {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestBudgetBoundsBranchingChains(t *testing.T) {
	e := newEnv(t, "a", "b", "c", "d", "e", "f")
	e.script("a", "reply", "@b @c @d")
	for _, n := range []string{"b", "c", "d"} {
		e.script(n, "reply", "@e @f")
	}
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, quiescent)
	total := 0
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		total += e.count(n)
	}
	suggested := 0
	budgetNotices := 0
	for _, m := range snap.Messages {
		if m.State == thread.StateSuggested {
			suggested++
		}
		if m.Role == thread.RoleSystem && strings.Contains(m.Text, "Chain budget of 6 reached") {
			budgetNotices++
		}
	}
	if total != 7 || suggested != 3 {
		t.Fatalf("executions %d (want 1 user + 6 automatic), suggested %d (want 3)", total, suggested)
	}
	if budgetNotices != 1 {
		t.Fatalf("budget notices %d (want exactly 1): %+v", budgetNotices, snap.Messages)
	}
}

func TestSelfMentionIsIgnored(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "reply", "@a again")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	e.settle(d, th, quiescent)
	if e.count("a") != 1 {
		t.Fatalf("a ran %d times", e.count("a"))
	}
}

func TestEmptyReplyCompletes(t *testing.T) {
	// An agent that prints nothing is a silently failing agent (the host
	// layer's own empty-reply guard), so its turn must still reach a
	// TERMINAL state (not stay stuck in pending/running) and the error text
	// must say why.
	e := newEnv(t, "a")
	e.script("a", "reply", "")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, quiescent)
	errs := agentMessages(snap, thread.StateError)
	if len(errs) != 1 || !strings.Contains(errs[0].Text, "empty reply") {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestDispatchInRoomWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts are not supported on Windows")
	}
	fixture := t.TempDir()
	t.Setenv("TINCAN_FIXTURE_DIR", fixture)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	room := filepath.Join(base, "Application Support", "scratch-ü 1")
	os.MkdirAll(room, 0o755)
	e := newEnvIn(t, room, fixture, "a")
	th, _ := thread.Create(room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, quiescent)
	if len(agentMessages(snap, thread.StateDone)) != 1 {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestOnlyLockHolderDispatches(t *testing.T) {
	e := newEnv(t, "a")
	e.dispatcher()
	d2 := thread.New(e.opts)
	if err := d2.Acquire(); !errors.Is(err, thread.ErrNotOwner) {
		t.Fatalf("second Acquire = %v", err)
	}
	if err := d2.Reconcile(context.Background()); !errors.Is(err, thread.ErrNotOwner) {
		t.Fatalf("second Reconcile = %v", err)
	}
}

// writeAgentsConfig writes a ~/.config/tincan/agents.json mapping each name to
// a fixture preset invoking this test binary, exercising the same config path
// Resolver() loads through when Options.Presets is nil (host.Effective).
func writeAgentsConfig(t *testing.T, path, exe string, names ...string) {
	t.Helper()
	type entry struct {
		Exec           []string `json:"exec"`
		Stdin          string   `json:"stdin"`
		Reply          string   `json:"reply"`
		ExecTimeoutSec int      `json:"exec_timeout_sec"`
	}
	m := map[string]entry{}
	for _, n := range names {
		m[n] = entry{Exec: []string{exe, "fixture", n}, Stdin: "body", Reply: "stdout", ExecTimeoutSec: 60}
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPostFailsWithoutAppendingWhenConfigIsMalformed exercises Resolver()'s
// propagated PresetMap error (Options.Presets nil, so it falls through to
// host.Effective(host.ConfigPath())): a malformed user config must fail Post
// before anything is appended to the journal, not silently resolve nothing
// and mark every mention "not dispatched".
func TestPostFailsWithoutAppendingWhenConfigIsMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".config", "tincan")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "agents.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	room, _ := filepath.EvalSymlinks(t.TempDir())
	th, err := thread.Create(room, "t", "a", "", 6)
	if err != nil {
		t.Fatal(err)
	}
	d := thread.New(dispatch.Options{Room: room}) // Presets nil: loads (the malformed) user config
	if err := d.Acquire(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)

	if _, err := d.Post(context.Background(), th.ID, "@a go", ""); err == nil {
		t.Fatal("expected an error from the malformed config")
	}
	snap, err := th.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 0 {
		t.Fatalf("Post appended messages despite a resolver error: %+v", snap.Messages)
	}
}

// TestHandoffMarkerWaitsForConfigErrorToClear covers the other half of the
// same ruling: a done agent message whose handoff processing hits a config
// load error must not have its Handoffs marker committed (which would lose
// the handoff for good); once the config is fixed, the same pass produces it.
func TestHandoffMarkerWaitsForConfigErrorToClear(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts are not supported on Windows")
	}
	fixture := t.TempDir()
	t.Setenv("TINCAN_FIXTURE_DIR", fixture)
	os.WriteFile(filepath.Join(fixture, "a.reply"), []byte("@b check"), 0o600)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(home, ".config", "tincan")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "agents.json")
	exe, _ := os.Executable()
	writeAgentsConfig(t, cfgPath, exe, "a", "b")

	room, _ := filepath.EvalSymlinks(t.TempDir())
	t.Cleanup(func() {
		states, _ := host.ListStates(room)
		for name := range states {
			host.Down(context.Background(), room, name, 5*time.Second)
		}
	})
	th, err := thread.Create(room, "t", "a", "", 6)
	if err != nil {
		t.Fatal(err)
	}
	d := thread.New(dispatch.Options{Room: room}) // Presets nil throughout
	if err := d.Acquire(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)

	if _, err := d.Post(context.Background(), th.ID, "@a go", ""); err != nil {
		t.Fatal(err)
	}

	aDone := func(s thread.Snapshot) bool {
		for _, m := range s.Messages {
			if m.Role == thread.RoleAgent && m.Listener == "a."+th.ID && m.State == thread.StateDone {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		if err := d.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		snap, err := th.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if aDone(snap) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a did not complete: %+v", snap.Messages)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Corrupt the config right as a's turn is about to hand off to b.
	if err := os.WriteFile(cfgPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Reconcile(context.Background()); err == nil {
		t.Fatal("expected Reconcile to surface the config error")
	}
	snap, err := th.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range snap.Messages {
		if m.Role == thread.RoleAgent && m.Listener == "a."+th.ID && m.Handoffs {
			t.Fatalf("handoffs marker committed despite the resolver error: %+v", snap.Messages)
		}
	}

	// Fix the config; the retried pass now runs the handoff to b.
	writeAgentsConfig(t, cfgPath, exe, "a", "b")
	deadline = time.Now().Add(45 * time.Second)
	for {
		if err := d.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		snap, err = th.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if quiescent(snap) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread did not settle: %+v", snap.Messages)
		}
		time.Sleep(100 * time.Millisecond)
	}
	done := agentMessages(snap, thread.StateDone)
	if len(done) != 2 || done[0].Listener != "a."+th.ID || done[1].Listener != "b."+th.ID {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestCrashAtEveryBoundaryRunsEachTurnOnce(t *testing.T) {
	for _, point := range []string{"before-submit", "after-submit", "after-terminal", "after-handoffs"} {
		t.Run(point, func(t *testing.T) {
			e := newEnv(t, "a", "b")
			e.script("a", "reply", "@b check")
			th, _ := thread.Create(e.room, "t", "a", "", 6)
			crashed := false
			d := thread.New(e.opts)
			if err := d.Acquire(); err != nil {
				t.Fatal(err)
			}
			d.Hook = func(p string) error {
				if p == point && !crashed {
					crashed = true
					return errors.New("simulated crash")
				}
				return nil
			}
			d.Post(context.Background(), th.ID, "@a go", "")
			deadline := time.Now().Add(45 * time.Second)
			for !crashed && time.Now().Before(deadline) {
				d.Reconcile(context.Background())
				time.Sleep(100 * time.Millisecond)
			}
			if !crashed {
				t.Fatal("hook never fired")
			}
			d.Close()
			d2 := e.dispatcher()
			e.settle(d2, th, quiescent)
			if e.count("a") != 1 || e.count("b") != 1 {
				t.Fatalf("after crash at %s: a=%d b=%d", point, e.count("a"), e.count("b"))
			}
		})
	}
}
