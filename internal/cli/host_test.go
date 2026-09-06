package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/host"
	"github.com/c0ze/tincan/internal/spool"
)

// TestMain lets this test binary stand in for the tincan binary. `up`
// re-execs os.Executable() as `serve …`; with TINCAN_AS_CLI=1 in the
// environment that re-exec runs the CLI instead of the test suite. The same
// switch turns the binary into a fake agent when its first argument is
// "fake-agent" (see fakeAgent), so hosted-listener tests need no shell or
// external tools and run on the whole CI matrix.
func TestMain(m *testing.M) {
	if os.Getenv("TINCAN_AS_CLI") == "1" {
		if len(os.Args) > 1 && os.Args[1] == "fake-agent" {
			os.Exit(fakeAgent(os.Args[2:]))
		}
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeAgent is the headless CLI stand-in: `echo <text…>` prints
// "echo: <text>"; anything else is a usage error.
func fakeAgent(args []string) int {
	if len(args) > 0 && args[0] == "echo" {
		fmt.Printf("echo: %s\n", strings.Join(args[1:], " "))
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake-agent: unknown mode")
	return 2
}

// fakeAgentConfig points $HOME at a temp dir holding an agents.json that
// defines preset "fake" as this test binary in fake-agent echo mode, and
// sets TINCAN_AS_CLI=1 so re-execs of the binary act as tincan / the agent.
func fakeAgentConfig(t *testing.T) {
	t.Helper()
	t.Setenv("TINCAN_AS_CLI", "1")
	home := t.TempDir()
	setHomeEnv(t, home)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"fake": {"exec": [%q, "fake-agent", "echo", "{body}"]}, "kimi": {"exec": ["kimi", "--print", "{body}"]}}`, exe)
	writeAgentsConfig(t, home, cfg)
}

func writeAgentsConfig(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "tincan")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitUntilTrue polls cond every 20ms for up to d.
func waitUntilTrue(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHostUsageErrorsExitTwo(t *testing.T) {
	setHomeEnv(t, t.TempDir()) // no user presets
	cases := [][]string{
		{"serve"},                               // missing name
		{"serve", "x", "--stdin", "weird"},      // bad enum
		{"serve", "x", "--reply", "mail"},       // bad enum
		{"serve", "x"},                          // unknown preset, no --exec
		{"serve", "x", "--exec", "a 'unterm"},   // bad template
		{"serve", "x", "extra"},                 // stray positional
		{"serve", "codex", "--room"},            // flag without value
		{"serve", "../x", "--exec", "a {body}"}, // invalid name
		{"presets", "--format", "yaml"},
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != ExitUsage {
			t.Errorf("args %v: want exit %d, got %d", args, ExitUsage, code)
		}
	}
}

func TestPresetsTableListsBuiltinsAndConfig(t *testing.T) {
	fakeAgentConfig(t)
	code, stdout, stderr := run("presets")
	if code != ExitOK {
		t.Fatalf("presets exit %d: %s", code, stderr)
	}
	for _, want := range []string{"NAME", "codex", "fake", "kimi --print {body}", "codex exec -s workspace-write"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("presets output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "kimi -p") {
		t.Errorf("config override did not replace built-in kimi:\n%s", stdout)
	}
}

func TestPresetsJSONParses(t *testing.T) {
	fakeAgentConfig(t)
	code, stdout, stderr := run("presets", "--format", "json")
	if code != ExitOK {
		t.Fatalf("presets exit %d: %s", code, stderr)
	}
	var got map[string]host.Preset
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(got) != 7 || got["fake"].Exec[1] != "fake-agent" || got["codex"].Reply != "file" {
		t.Fatalf("unexpected presets: %+v", got)
	}
}

func TestPresetsCorruptConfigExitsOne(t *testing.T) {
	home := t.TempDir()
	setHomeEnv(t, home)
	writeAgentsConfig(t, home, "{not json")
	code, _, stderr := run("presets")
	if code != ExitError || !strings.Contains(stderr, "agents.json") {
		t.Fatalf("want exit 1 naming agents.json, got %d: %q", code, stderr)
	}
}

func TestServeForegroundAnswersAndStopsOnStop(t *testing.T) {
	fakeAgentConfig(t)
	room := t.TempDir()
	type result struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errs := run("serve", "fake", "--room", room)
		done <- result{code, out, errs}
	}()
	if !waitUntilTrue(func() bool { c, _, _ := run("ping", "--room", room, "--to", "fake"); return c == ExitOK }, 10*time.Second) {
		t.Fatal("serve never parked")
	}
	code, reply, stderr := run("ask", "--room", room, "--to", "fake", "--from", "orch", "--body", "hello there", "--timeout", "20", "--format", "body")
	if code != ExitOK || strings.TrimSpace(reply) != "echo: hello there" {
		t.Fatalf("ask: code=%d out=%q err=%q", code, reply, stderr)
	}
	if code, _, stderr := run("stop", "--room", room, "--to", "fake", "--from", "orch"); code != ExitOK {
		t.Fatalf("stop: %d %s", code, stderr)
	}
	select {
	case r := <-done:
		if r.code != ExitOK {
			t.Fatalf("serve exit %d: %s", r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "serving name=fake preset=fake") {
			t.Fatalf("foreground banner missing: %q", r.stdout)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after stop")
	}
	if _, ok, _ := host.ReadState(room, "fake"); ok {
		t.Fatal("state file left behind")
	}
	log, err := os.ReadFile(host.LogPath(room, "fake"))
	if err != nil || !strings.Contains(string(log), "=== ") {
		t.Fatalf("host log missing or empty: %v %q", err, log)
	}
}

func TestUpDownUsageErrorsExitTwo(t *testing.T) {
	setHomeEnv(t, t.TempDir()) // no user presets
	cases := [][]string{
		{"up"},                             // missing name
		{"up", "x"},                        // unknown preset, no --exec
		{"up", "x", "--exec", "a 'unterm"}, // bad template
		{"up", "../x", "--exec", "a"},      // invalid name
		{"down"},                           // missing name
		{"down", "../x"},                   // invalid name
		{"down", "x", "--wait"},            // flag without value
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != ExitUsage {
			t.Errorf("args %v: want exit %d, got %d", args, ExitUsage, code)
		}
	}
}

func TestUpMissingAgentBinaryExitsOne(t *testing.T) {
	setHomeEnv(t, t.TempDir())
	code, _, stderr := run("up", "x", "--room", t.TempDir(), "--exec", "tincan-no-such-binary-xyz {body}")
	if code != ExitError || !strings.Contains(stderr, "not found on PATH") {
		t.Fatalf("want exit 1 naming the missing binary, got %d: %q", code, stderr)
	}
}

func TestUpIsIdempotentWhenAlreadyPresent(t *testing.T) {
	setHomeEnv(t, t.TempDir())
	room := t.TempDir()
	writePresenceFile(t, room, "codex", os.Getpid(), time.Now())
	code, stdout, stderr := run("up", "codex", "--room", room)
	if code != ExitOK {
		t.Fatalf("up exit %d: %s", code, stderr)
	}
	if want := fmt.Sprintf("already up name=codex pid=%d", os.Getpid()); strings.TrimSpace(stdout) != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestUpIsIdempotentWhenHostIsBusy(t *testing.T) {
	setHomeEnv(t, t.TempDir())
	room := t.TempDir()
	writeStateFile(t, room, "codex", os.Getpid(), "codex", "busy", "m1") // live pid, not parked
	code, stdout, _ := run("up", "codex", "--room", room)
	if code != ExitOK || !strings.HasPrefix(stdout, "already up name=codex") {
		t.Fatalf("up on a busy host: code=%d out=%q", code, stdout)
	}
}

func TestDownWithNothingRunningIsANoop(t *testing.T) {
	room := t.TempDir()
	code, stdout, stderr := run("down", "nobody", "--room", room)
	if code != ExitOK || strings.TrimSpace(stdout) != "down name=nobody" {
		t.Fatalf("down: code=%d out=%q err=%q", code, stdout, stderr)
	}
	if entries, _ := os.ReadDir(filepath.Join(room, ".tincan", "inbox", "nobody")); len(entries) != 0 {
		t.Fatalf("down queued a stop for a listener that does not exist: %v", entries)
	}
}

func TestDownRemovesStaleStateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dead-pid detection is unix-only (see spool/alive_other.go)")
	}
	room := t.TempDir()
	writeStateFile(t, room, "ghost", 1<<30, "codex", "busy", "m1")
	if code, _, stderr := run("down", "ghost", "--room", room); code != ExitOK {
		t.Fatalf("down exit %d: %s", code, stderr)
	}
	if _, ok, _ := host.ReadState(room, "ghost"); ok {
		t.Fatal("stale state file not removed")
	}
}

func TestUpAskStatusDownRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached up is unsupported on Windows (see host.StartDetached)")
	}
	fakeAgentConfig(t)
	room := t.TempDir()
	code, stdout, stderr := run("up", "fake", "--room", room, "--wait", "10")
	if code != ExitOK {
		t.Fatalf("up exit %d: out=%q err=%q", code, stdout, stderr)
	}
	t.Cleanup(func() { run("down", "fake", "--room", room, "--wait", "5") })
	var pid int
	if _, err := fmt.Sscanf(stdout, "up name=fake pid=%d preset=fake", &pid); err != nil || pid <= 0 {
		t.Fatalf("up stdout = %q (%v)", stdout, err)
	}
	if code, out, _ := run("up", "fake", "--room", room); code != ExitOK || !strings.HasPrefix(out, "already up name=fake pid=") {
		t.Fatalf("second up: code=%d out=%q", code, out)
	}
	code, reply, stderr := run("ask", "--room", room, "--to", "fake", "--from", "orch", "--body", "hello world", "--timeout", "30", "--format", "body")
	if code != ExitOK || strings.TrimSpace(reply) != "echo: hello world" {
		t.Fatalf("ask: code=%d out=%q err=%q", code, reply, stderr)
	}
	// serve re-parks right after replying; allow a moment for presence.
	if !waitUntilTrue(func() bool {
		r := statusJSON(t, room)["fake"]
		return r.Mode == "hosted:fake" && r.Alive && !r.Busy && r.PID == pid
	}, 10*time.Second) {
		t.Fatalf("status never showed hosted/parked: %+v", statusJSON(t, room)["fake"])
	}
	code, stdout, stderr = run("down", "fake", "--room", room, "--wait", "10")
	if code != ExitOK || strings.TrimSpace(stdout) != "down name=fake" {
		t.Fatalf("down: code=%d out=%q err=%q", code, stdout, stderr)
	}
	if code, out, _ := run("ping", "--room", room, "--to", "fake"); code != ExitError || strings.TrimSpace(out) != "absent" {
		t.Fatalf("ping after down: code=%d out=%q", code, out)
	}
	if !waitUntilTrue(func() bool { return !spool.ProcessAlive(pid) }, 5*time.Second) {
		t.Fatalf("serve pid %d still alive after down", pid)
	}
	if _, ok, _ := host.ReadState(room, "fake"); ok {
		t.Fatal("state file left behind after down")
	}
	log, err := os.ReadFile(host.LogPath(room, "fake"))
	if err != nil || !strings.Contains(string(log), "=== ") || !strings.Contains(string(log), "echo: hello world") {
		t.Fatalf("host log missing the run: %v\n%s", err, log)
	}
}
