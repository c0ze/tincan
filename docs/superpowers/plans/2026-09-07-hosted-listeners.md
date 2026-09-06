# tincan Hosted Listeners (`up` / `serve` / `down` / `presets`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an orchestrator bring a named headless agent up in a room with one command (`tincan up codex --room R`), see whether it is parked or busy (`status`), and take it down (`down`) — no human terminal per agent.

**Architecture:** A new package `internal/host` holds presets + user config, exec-template rendering, the per-message runner (process-group kill, timeout, error→reply contract), the state file (`<room>/.tincan/hosts/<name>.json`) and the `Serve` loop (park with `spool.RecvContext`, run agent, `spool.Send` the reply to `reply_to`). `internal/cli` gains `cmdUp` (re-exec self as `serve … --daemon` detached via `Setsid`), `cmdServe`, `cmdDown`, `cmdPresets`, and `status` learns MODE/busy. Spool changes are minimal and additive: `RecvContext` (so SIGTERM unparks cleanly), exported `ProcessAlive` and `ValidName`.

**Tech Stack:** Go (go.mod `go 1.23` floor, toolchain 1.26.4), stdlib `flag`/`os/exec`/`encoding/json` + existing fsnotify. No new dependencies.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-09-07-hosted-listeners-design.md` (sections referenced as §N).
- Go commands: `export PATH=/Users/arda/.local/share/mise/installs/go/1.26.4/bin:$PATH` first; never bump the `go 1.23` directive; `go vet ./...` and `go test -race ./...` must stay green.
- Exit-code contract: 0 ok, 1 error, 2 usage, 3 timeout. Stdlib `flag` subcommands, `fs.SetOutput(stderr)`, errors printed as `tincan <cmd>: …`.
- Tests: table-driven where natural; no shell scripts (CI matrix is Linux/macOS/Windows) — fake agents are the test binary re-exec'd via `TestMain`.
- The message body is never passed through a shell: it is one argv element (`{body}`) or stdin.
- Windows: process-group kill via `taskkill /T /F`; detach (`up`) fails loudly with a clear message (§10, owner-accepted fallback).
- Commit after every task: subject imperative, body says why, trailer `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Never push.

---

### Task 1: spool — `RecvContext`, `ProcessAlive`, `ValidName`

**Files:**
- Modify: `internal/spool/spool.go` (Recv → RecvContext; two exported helpers)
- Test: `internal/spool/spool_test.go` (append)

**Interfaces:**
- Produces: `func (s *Spool) RecvContext(ctx context.Context, name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error)` — returns `ctx.Err()` when cancelled while parked; `func ProcessAlive(pid int) bool`; `func ValidName(name string) error`.

- [ ] **Step 1: Write the failing tests** (append to `internal/spool/spool_test.go`; add `"context"` to its imports)

```go
func TestRecvContextCancelUnparksAndClearsPresence(t *testing.T) {
	sp, _ := Open(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sp.RecvContext(ctx, "codex", 0, false)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok, _ := sp.Present("codex"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RecvContext never parked (no presence)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RecvContext did not return after cancel")
	}
	if _, ok, _ := sp.Present("codex"); ok {
		t.Fatal("presence still reported after a cancelled RecvContext")
	}
}

func TestRecvContextDeliversLikeRecv(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "b", "hi", 1)); err != nil {
		t.Fatal(err)
	}
	e, err := sp.RecvContext(context.Background(), "b", 5*time.Second, false)
	if err != nil || e.Body != "hi" {
		t.Fatalf("got %+v, %v", e, err)
	}
}

func TestProcessAliveExported(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("ProcessAlive(own pid) = false")
	}
	if ProcessAlive(0) {
		t.Fatal("ProcessAlive(0) = true")
	}
}

func TestValidNameExported(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`} {
		if ValidName(bad) == nil {
			t.Fatalf("ValidName(%q) = nil, want error", bad)
		}
	}
	if err := ValidName("codex"); err != nil {
		t.Fatalf("ValidName(codex) = %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/spool/ -run 'RecvContext|ProcessAliveExported|ValidNameExported'`
Expected: FAIL to compile (`undefined: RecvContext`, `ProcessAlive`, `ValidName`).

- [ ] **Step 3: Implement** in `internal/spool/spool.go`

Add `"context"` to imports. Replace the `Recv` signature/body header with:

```go
// Recv blocks until one message is available in name's inbox, claims it,
// removes it from the spool (or moves it to log/ when logConsumed), and
// returns it. Returns ErrTimeout if nothing arrives within timeout. A
// timeout <= 0 means no deadline: Recv blocks until a message is claimable.
func (s *Spool) Recv(name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error) {
	return s.RecvContext(context.Background(), name, timeout, logConsumed)
}

// RecvContext is Recv with cancellation: it returns ctx.Err() as soon as ctx
// is done while parked, clearing its presence token on the way out like any
// other exit path. Hosted listeners use it to unpark on SIGTERM/SIGINT
// without leaving a stale presence file behind.
func (s *Spool) RecvContext(ctx context.Context, name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error) {
```
(the body is the old `Recv` body) and add `case <-ctx.Done(): return nil, ctx.Err()` to **both** `select` statements in the loop (the un-armed poll select and the armed watcher select).

Then:

```go
// ValidName reports whether name is a legal participant name (single path
// component, no traversal). Exported for the hosted-listener commands, which
// build <room>/.tincan/hosts/<name>.* paths from the name.
func ValidName(name string) error { return validName(name) }

// ProcessAlive reports whether pid names a live process, using the same
// per-OS check status/ping apply to presence tokens (see alive_unix.go and
// alive_other.go for the Windows caveat). Exported so hosted-listener state
// files get the identical answer.
func ProcessAlive(pid int) bool { return processAlive(pid) }
```

- [ ] **Step 4: Run the whole spool package**

Run: `go test -race ./internal/spool/`
Expected: PASS (all existing tests plus the four new ones).

- [ ] **Step 5: Commit**

`git add internal/spool && git commit` — subject `Add spool.RecvContext and export ProcessAlive/ValidName`; body: hosted listeners need to unpark on SIGTERM without a stale presence token, and need the same liveness/name checks for their state files.

---

### Task 2: host — paths and exec templates

**Files:**
- Create: `internal/host/host.go`, `internal/host/template.go`
- Test: `internal/host/template_test.go`

**Interfaces:**
- Produces: `Dir(room)`, `StatePath(room, name)`, `LogPath(room, name) string`; `type Vars struct{Body, Out, Room, Name, ID string}`; `Render(exec []string, v Vars) []string`; `SplitTemplate(s string) ([]string, error)`.

- [ ] **Step 1: Failing tests** — `internal/host/template_test.go`

```go
package host

import (
	"reflect"
	"testing"
)

func TestSplitTemplate(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "codex exec -o {out} -", want: []string{"codex", "exec", "-o", "{out}", "-"}},
		{in: `grok -p "{body}" --always-approve`, want: []string{"grok", "-p", "{body}", "--always-approve"}},
		{in: `my-llm --system 'be brief'  x`, want: []string{"my-llm", "--system", "be brief", "x"}},
		{in: `a\ b c`, want: []string{"a b", "c"}},
		{in: `x '' y`, want: []string{"x", "", "y"}},
		{in: `a "unterminated`, wantErr: true},
		{in: "   ", wantErr: true},
		{in: `x \`, wantErr: true},
	}
	for _, c := range cases {
		got, err := SplitTemplate(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("SplitTemplate(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitTemplate(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitTemplate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderSubstitutesEveryPlaceholderPerToken(t *testing.T) {
	v := Vars{Body: "review  PR 56\n\"quoted\" 'single' $HOME", Out: "/tmp/o.md", Room: "/r", Name: "codex", ID: "abc"}
	in := []string{"agent", "-p", "{body}", "--out={out}", "{room}/{name}/{id}", "plain"}
	got := Render(in, v)
	want := []string{"agent", "-p", v.Body, "--out=/tmp/o.md", "/r/codex/abc", "plain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Render = %q, want %q", got, want)
	}
	if in[2] != "{body}" {
		t.Fatal("Render mutated its input")
	}
}

func TestRenderDoesNotRescanSubstitutedText(t *testing.T) {
	got := Render([]string{"{body}"}, Vars{Body: "{out}", Out: "X"})
	if got[0] != "{out}" {
		t.Fatalf("body containing a placeholder was re-substituted: %q", got[0])
	}
}

func TestPaths(t *testing.T) {
	if got := StatePath("/r", "codex"); got != "/r/.tincan/hosts/codex.json" {
		t.Fatalf("StatePath = %q", got)
	}
	if got := LogPath("/r", "codex"); got != "/r/.tincan/hosts/codex.log" {
		t.Fatalf("LogPath = %q", got)
	}
}
```
(On Windows `TestPaths` compares with `filepath.Join`-built expectations instead of literals — use `filepath.Join("/r", ".tincan", "hosts", "codex.json")`.)

- [ ] **Step 2: Run** `go test ./internal/host/` — Expected: FAIL (package has no non-test files / undefined symbols).

- [ ] **Step 3: Implement**

`internal/host/host.go`:
```go
// Package host implements hosted listeners: a detached `tincan serve`
// process that parks on an inbox, runs a configured headless agent CLI once
// per message with the body as the prompt, and sends the output back as the
// reply. See PROTOCOL.md "Hosted listeners".
package host

import "path/filepath"

// Dir is <room>/.tincan/hosts, where state files and host logs live.
func Dir(room string) string { return filepath.Join(room, ".tincan", "hosts") }

// StatePath is the state file of the hosted listener name (see State).
func StatePath(room, name string) string { return filepath.Join(Dir(room), name+".json") }

// LogPath is the host log of the hosted listener name.
func LogPath(room, name string) string { return filepath.Join(Dir(room), name+".log") }
```

`internal/host/template.go`: `Vars`, `Render` (uses `strings.NewReplacer` over the five placeholders, applied per token into a fresh slice — a Replacer makes one pass, so substituted text is never rescanned), `SplitTemplate` (rune loop: quote state `'`/`"`, backslash escape outside quotes, whitespace splits; errors: unterminated quote, trailing backslash, empty template). Write the full code as designed; doc comments must state that this is the only shell-like parsing tincan does and that it happens before substitution.

- [ ] **Step 4: Run** `go test -race ./internal/host/` — Expected: PASS.
- [ ] **Step 5: Commit** — `Add internal/host paths and exec template rendering`.

---

### Task 3: host — presets, config, resolve, post-processing

**Files:**
- Create: `internal/host/preset.go`
- Test: `internal/host/preset_test.go`

**Interfaces:**
- Produces: `const DefaultExecTimeoutSec = 3600`; `type Preset struct{Exec []string; Stdin, Reply string; ExecTimeoutSec int}` (json tags `exec`, `stdin`, `reply`, `exec_timeout_sec`); `Builtin() map[string]Preset`; `ConfigPath() string` (empty when home unknown); `LoadConfig(path string) (map[string]Preset, error)` (missing/empty path → empty map; config entries decode `exec_timeout_sec` as `*int`, nil → default); `Effective(path string) (map[string]Preset, error)`; `Names(map[string]Preset) []string`; `type Overrides struct{Exec []string; Stdin, Reply string; ExecTimeoutSec int}` (nil/""/-1 = not given); `Resolve(presets map[string]Preset, name, presetFlag string, ov Overrides) (label string, p Preset, err error)`; `PostProcess(label, body string) string`; `(p *Preset) normalize() error`.

- [ ] **Step 1: Failing tests** — cover: `Builtin` has exactly codex/grok/kimi/agy/gemini/claude with the §5 argv (assert codex and agy exactly, `codex` Stdin "body"/Reply "file", others "none"/"stdout", all timeout 3600); `LoadConfig` on a missing path → empty map, nil error; parse error mentions the path; defaults filled (`{"myllm":{"exec":["my-llm","-"]}}` → Stdin none, Reply stdout, 3600) and explicit `exec_timeout_sec: 1800`/`0` kept; invalid `stdin: "pipe"`, `reply: "file"` without `{out}`, empty exec, negative timeout → errors; `Effective` config entry replaces a built-in wholesale; `Resolve` table: (`codex`, "", none) → label codex; ("x", "kimi", none) → kimi preset; ("x", "nope", none) → error; ("x", "", none) → error; ("x", "", Exec given) → label "custom", timeout 3600; ("codex", "", Exec+Stdin "none"+Reply "stdout"+timeout 5) → overrides applied; ("codex", "", Reply "file" on exec without {out}) → error; `PostProcess` table: trims trailing whitespace; kimi drops the final `To resume this session: kimi --resume x` line and trailing blank lines before it; non-kimi keeps that line; a kimi body that is only the resume line → "".

- [ ] **Step 2: Run** `go test ./internal/host/ -run 'Builtin|LoadConfig|Effective|Resolve|PostProcess'` — Expected: FAIL (undefined).

- [ ] **Step 3: Implement** `internal/host/preset.go` as designed. `ConfigPath` = `filepath.Join(os.UserHomeDir(), ".config", "tincan", "agents.json")` or "" on error. `Resolve` logic: `presetFlag` given → base = presets[presetFlag] (missing and no `ov.Exec` → `unknown preset %q (see tincan presets, or pass --exec)`); else presets[name] if known → label name; else `ov.Exec != nil` → label "custom" with `ExecTimeoutSec: DefaultExecTimeoutSec`; else error `%q is not a known preset; pass --preset <p> or --exec '<template>' (see tincan presets)`. Apply overrides, then `p.normalize()`.

- [ ] **Step 4: Run** `go test -race ./internal/host/` — PASS.
- [ ] **Step 5: Commit** — `Add hosted-listener presets, user config and reply post-processing`.

---

### Task 4: host — state file

**Files:**
- Create: `internal/host/state.go`
- Test: `internal/host/state_test.go`

**Interfaces:**
- Produces: `type State struct{PID int; Preset string; Exec []string; Started time.Time; State string; CurrentID string}` (json: `pid, preset, exec, started, state, current_id omitempty`); `WriteState(room, name string, st State) error` (tmp + rename in `Dir(room)`, tmp named `.<name>.<id>.tmp`); `ReadState(room, name) (State, bool, error)`; `RemoveState(room, name) error` (missing = nil); `(State) Alive() bool` (= `spool.ProcessAlive(PID)`); `ListStates(room) (map[string]State, error)` (skips dot-files, unreadable files; missing dir → empty map).

- [ ] **Step 1: Failing tests**: round trip Write→Read (`ok == true`, fields equal, `Started` compared with `Equal`); Read missing → `ok == false, err == nil`; Write overwrites; hosts dir contains only `<name>.json` after Write (no tmp residue); Remove twice → nil; `Alive()` true for own pid and false for `1<<30` (skip the false half on Windows); `ListStates` returns both names written and ignores a stray `.x.tmp` and a corrupt `bad.json`.

- [ ] **Step 2: Run** — FAIL (undefined). **Step 3: Implement** as designed. **Step 4:** `go test -race ./internal/host/` PASS. **Step 5: Commit** — `Add hosted-listener state file read/write`.

---

### Task 5: host — process control (unix/windows) and the runner

**Files:**
- Create: `internal/host/proc_unix.go` (`//go:build !windows`), `internal/host/proc_windows.go` (`//go:build windows`), `internal/host/runner.go`
- Test: `internal/host/main_test.go` (TestMain fake agent), `internal/host/runner_test.go`

**Interfaces:**
- Produces (proc): `childAttr() *syscall.SysProcAttr` (unix `Setpgid: true`; windows `CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP`); `killGroup(cmd *exec.Cmd) error` (unix `syscall.Kill(-pid, SIGKILL)`, ESRCH ok, else fallback `Process.Kill`; windows `taskkill /T /F /PID`, fallback `Process.Kill`); `Terminate(pid int) error` (unix SIGTERM; windows `os.FindProcess(pid).Kill()`); `Kill(pid int) error` (unix SIGKILL; windows same as Terminate).
- Produces (runner): `type RunSpec struct{Argv []string; Dir string; Stdin *string; OutFile string; Timeout time.Duration}`; `type Result struct{Stdout, Stderr []byte; ExitCode int; Err error; TimedOut, Killed bool; Duration time.Duration}`; `Run(ctx context.Context, spec RunSpec) Result`; `ReplyBody(spec RunSpec, res Result) string`; `tail(b []byte, n int) []byte`.
- Test helper: `fakeExec(mode string, rest ...string) []string` = `[os.Executable(), mode, rest...]`; tests call `t.Setenv("TINCAN_FAKE_AGENT", "1")` so the child takes the fake branch. Fake modes: `echo <text…>` → stdout `echo: <text>\n`; `stdin` → `stdin: <all stdin>\n`; `fail <text…>` → stderr `boom: <text>\n`, stdout `partial`, exit 3; `sleep <sec> [pidfile]` → writes own pid to pidfile then sleeps; `outfile <path> <text…>` → writes `file: <text>` to path, stdout `noise on stdout`; `cwd` → prints `os.Getwd()`.

- [ ] **Step 1: Failing tests** (`runner_test.go`): exit-zero capture (`echo`); stdin body (`stdin`, `Stdin: &body` with a two-line body); non-zero exit (`fail`) → `ExitCode == 3`, stderr has `boom`, `ReplyBody` has prefix `ERROR exit=3\nboom: x` and contains `partial`; timeout (`sleep 30 <pidfile>`, `Timeout: time.Second`) → `TimedOut`, `Duration < 10s`, `ReplyBody == "ERROR timeout after 1s"`, pid from pidfile dead within 3 s (liveness assertion skipped on Windows); context cancel after 200 ms → `Killed`, reply has prefix `ERROR interrupted`, pid dead; missing binary `tincan-definitely-missing-binary-xyz` → `Err != nil`, `ReplyBody` prefix `ERROR exec: ` and contains the name; `outfile {out}` with `OutFile` set → `ReplyBody == "file: <text>"`; `Result{ExitCode: 0}` with nonexistent `OutFile` → prefix `ERROR reply file:`; `cwd` mode with `Dir` → output equals `filepath.EvalSymlinks(dir)`; `tail` table (shorter, equal, longer input).

- [ ] **Step 2: Run** — FAIL. **Step 3: Implement** `main_test.go` (TestMain + `fakeAgent(args []string) int` + `fakeExec`), `proc_unix.go`, `proc_windows.go`, `runner.go` as designed: `exec.CommandContext(runCtx, …)`, `cmd.Dir`, `cmd.SysProcAttr = childAttr()`, `cmd.Stdin = strings.NewReader(*spec.Stdin)` when set (nil otherwise → no stdin inherited), buffers for stdout/stderr, `cmd.Cancel = func() error { return killGroup(cmd) }`, `cmd.WaitDelay = 2 * time.Second`; classify after `Wait`: `err != nil && spec.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil` → TimedOut; `err != nil && ctx.Err() != nil` → Killed; `err == nil` → ExitCode 0; `*exec.ExitError` → its code; else Err. `ReplyBody` order: Killed → `ERROR interrupted: hosted listener was stopped while running`; TimedOut → `ERROR timeout after %ds`; Err → `ERROR exec: ` + err (strip a leading `exec: `); ExitCode≠0 → `ERROR exit=%d\n` + `tail(stderr, 4096)` (right-trimmed) + `\n` + stdout if non-blank; OutFile → file contents or `ERROR reply file: `; else stdout.

- [ ] **Step 4: Run** `go test -race ./internal/host/` PASS; also `GOOS=windows go vet ./internal/host/` compiles. **Step 5: Commit** — `Add hosted-listener runner with process-group kill and reply contract`.

---

### Task 6: host — the `Serve` loop

**Files:**
- Create: `internal/host/serve.go`
- Test: `internal/host/serve_test.go`

**Interfaces:**
- Produces: `type ServeOptions struct{Room, Name, Label string; Preset Preset; Log io.Writer}`; `Serve(ctx context.Context, o ServeOptions) error` (nil on stop/cancel); `handle(ctx, o, e) string`; `logf(w, format, args...)`.
- Consumes: `spool.Open/RecvContext/Send`, `WriteState/RemoveState`, `Render`, `Run`, `ReplyBody`, `PostProcess`.

Loop (§3, §4, §7): write state parked → `RecvContext(ctx, name, 0, false)`; ctx done → log, return nil; `Kind=="stop"` → log, return nil (deferred `RemoveState`); other non-empty kind → log `ignored`, continue; else state busy+CurrentID → `handle` → if `ReplyTo != ""` `Send(&Envelope{CorrID: ReplyTo, From: Name, To: ReplyTo, Body})` (send error logged) else log `has no reply_to; output logged only` → if ctx done return nil → state parked. `handle`: log `=== <id> from=<from> started=<RFC3339>`; `Reply=="file"` → `os.CreateTemp("", "tincan-reply-*.md")`, close, `defer os.Remove`, `vars.Out`; `Argv = Render(Exec, vars)`; `Stdin=="body"` → `spec.Stdin = &e.Body`; `Timeout = ExecTimeoutSec s`; `Run`; append stdout+stderr to log; log `=== exit=<code|timeout|killed|error> duration=%.1fs`; return `PostProcess(label, ReplyBody(spec, res))`.

- [ ] **Step 1: Failing tests** with helpers `startServe(t, preset Preset, label string) (room string, sp *spool.Spool, logbuf *bytes.Buffer, done <-chan error, cancel func())` and `askVia(t, sp, to, body string) *envelope.Envelope` (Send with `ReplyTo: "r-"+NewID()`, then `sp.Recv(channel, 10s, false)`), `stopServe(t, sp, name, done)` (Send `Kind:"stop"`, wait `<-done` == nil within 10 s). Cases: echo reply body == `echo: <two-line body>` and `From == name`, state file shows `parked`+label before the ask and is gone after stop; stdin preset; reply=file preset (`outfile {out} {body}`) → `file: <body>`; `fail` → reply prefix `ERROR exit=3`; `sleep 30 <pidfile>` with `ExecTimeoutSec: 1` → `ERROR timeout after 1s`, pid dead (skip liveness on Windows); busy state visible: `sleep 30` preset, send ask (don't wait), poll `ReadState` until `State=="busy" && CurrentID == sent id`, then `cancel()` → `<-done` nil within 5 s, reply `ERROR interrupted…` collectable, pid dead, state file gone; message without `reply_to` → after stop, log contains `has no reply_to`; `Kind:"weird"` → log contains `ignored` and a following ask still gets a reply.

- [ ] **Step 2: Run** — FAIL. **Step 3: Implement** `serve.go`. **Step 4:** `go test -race ./internal/host/` PASS (whole package, twice with `-count=2`). **Step 5: Commit** — `Add hosted-listener serve loop`.

---

### Task 7: host — detach (`StartDetached`)

**Files:**
- Create: `internal/host/detach_unix.go` (`//go:build !windows`), `internal/host/detach_windows.go`

**Interfaces:**
- Produces: `StartDetached(exe string, args []string, dir, logPath string) (*os.Process, <-chan error, error)` — unix: mkdir log dir, open log `O_CREATE|O_WRONLY|O_APPEND 0644`, `exec.Command(exe, args...)`, `Dir=dir`, `Stdin=nil`, `Stdout=Stderr=log`, `SysProcAttr{Setsid: true}`, `Start`, close our log handle, goroutine `exited <- cmd.Wait()`; windows: returns error `detached hosted listeners are not supported on Windows yet; run `tincan serve <name>` in a terminal instead`.

- [ ] **Step 1:** Unit test in `detach_test.go` (skip on Windows): `StartDetached(fakeExec-exe, ["echo","detached"], dir, logPath)` with `TINCAN_FAKE_AGENT=1` → `<-exited` nil within 5 s, log file contains `echo: detached`. Windows: test that the error message mentions `tincan serve`.
- [ ] **Step 2:** FAIL. **Step 3:** implement both files. **Step 4:** PASS + `GOOS=windows go vet ./...`. **Step 5: Commit** — `Add detached start for hosted listeners (unix; loud failure on Windows)`.

---

### Task 8: cli — `presets`, `serve`, shared host flags

**Files:**
- Create: `internal/cli/host.go`
- Modify: `internal/cli/cli.go` (dispatch + usage text)
- Test: `internal/cli/host_test.go`, `internal/cli/cli_test.go` (add `TestMain`)

**Interfaces:**
- Produces: `positional(args) (string, []string)`; `type hostFlags struct{name, room, preset, execTpl, stdin, reply string; execTimeout int}` with `(h hostFlags) forward() []string`; `parseHostFlags(cmd string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (hostFlags, int)`; `resolveHost(cmd string, h hostFlags, stderr io.Writer) (label string, p host.Preset, code int)`; `cmdPresets`, `cmdServe`.
- `TestMain` in `cli_test.go`: `TINCAN_AS_CLI=1` → `os.Args[1]=="fake-agent"` ? `fakeAgent(os.Args[2:])` (echo only) : `Run(os.Args[1:], os.Stdout, os.Stderr)`.

Usage text gains:
```
  tincan up      <name> [--preset <p>] [--exec '<tpl>'] [--stdin body|none] [--reply stdout|file] [--exec-timeout <sec>] [--wait <sec>] [flags]
  tincan serve   <name> [same flags as up] [--daemon]
  tincan down    <name> [--wait <sec>] [flags]
  tincan presets [--format table|json]
```

- [ ] **Step 1: Failing tests**: usage exit 2 table — `{"serve"}`, `{"serve","x","--stdin","weird"}`, `{"serve","x","--reply","mail"}`, `{"serve","x"}` (unknown preset, no exec), `{"serve","x","--exec","a 'unterminated"}`, `{"serve","x","extra"}`, `{"presets","--format","yaml"}`; `presets` table with a temp HOME config (`setHomeEnv`) adding `fake` and overriding `kimi` exec → output lists `fake`, `codex`, and the overridden kimi argv, exit 0; `presets --format json` parses into `map[string]host.Preset` with 7 entries; corrupt config → exit 1, stderr names the file; `serve` foreground round trip: `t.Setenv("TINCAN_AS_CLI","1")`, config preset `fake` exec `[exe,"fake-agent","echo","{body}"]`, run `Run(["serve","fake","--room",room])` in a goroutine, `ask --to fake … --format body` → `echo: hi`, `stop --to fake --from orch` → serve exit 0, state file gone, `host.LogPath` exists and contains `=== `.

- [ ] **Step 2:** FAIL. **Step 3: Implement** `host.go` (`positional`, `parseHostFlags`, `resolveHost`, `cmdPresets`, `cmdServe` with `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`; log writer: `--daemon` → `stderr` (already the log), else `io.MultiWriter(logfile, stderr)`; foreground banner `serving name=… preset=… room=… (Ctrl-C to stop)` on stdout), wire `"serve"`, `"presets"` in `Run`, update usage. **Step 4:** `go test -race ./internal/cli/` PASS. **Step 5: Commit** — `Add tincan serve and presets commands`.

---

### Task 9: cli — `status` MODE / busy

**Files:**
- Modify: `internal/cli/cli.go` (`cmdStatus`, `printStatusTable`; add `statusRow`, `statusRows`)
- Test: `internal/cli/cli_test.go` (append)

**Interfaces:**
- Produces: `type statusRow struct{spool.Presence; Mode string "json:mode"; Preset string "json:preset,omitempty"; Busy bool "json:busy"; CurrentID string "json:current_id,omitempty"}`; `statusRows(sp *spool.Spool, room string) ([]statusRow, error)`; `printStatusTable(rows []statusRow, w io.Writer)` header `NAME QUEUED MODE STATE PID SINCE`.

Rules: state file with live pid → `Mode="hosted:<preset>"`, `PID=st.PID`, `Busy = st.State=="busy"`, `CurrentID`; STATE column `busy` if Busy, else `parked` if presence alive, else `—`. No live state file: `Mode="agent"` if presence alive, else `""` (rendered `—`). Stale state files (dead pid) are ignored. Names appearing only in `hosts/` are added via `sp.Present(name)`.

- [ ] **Step 1: Failing tests** (use `writePresenceFile` + a `writeStateFile(t, room, name, pid, preset, state, currentID)` helper calling `host.WriteState`): hosted parked → table contains `hosted:codex` and `parked`, JSON `mode=="hosted:codex"`, `busy==false`; hosted busy (no presence) → table `busy`, pid shown, JSON `busy==true`, `current_id=="m1"`; stale (pid `1<<30`, skip Windows) → table has `—` and not `busy`, JSON mode `""`; plain presence → `agent`; existing `TestStatusJSONFormatRoundTrips` still passes.
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS. **Step 5: Commit** — `Show hosted listener mode and busy state in status`.

---

### Task 10: cli — `up` and `down` + integration test

**Files:**
- Modify: `internal/cli/host.go` (add `cmdUp`, `cmdDown`, `hostedAlready`, `waitUntil`), `internal/cli/cli.go` (dispatch)
- Test: `internal/cli/host_test.go`

`cmdUp` (§4): parse (`--wait` default 10) → `ValidName` → `filepath.Abs(room)` + root warning → `hostedAlready` (presence alive, or live state pid) → `already up name=<n> pid=<p>` exit 0 → `resolveHost` → `exec.LookPath(p.Exec[0])` else exit 1 `agent binary %q not found on PATH` → `os.Executable()` → `StartDetached(exe, ["serve", name, "--room", room, "--daemon"] + h.forward(), room, LogPath)` → poll `sp.Present` every 50 ms; `<-exited` first → exit 1 `serve exited before parking (%v); see <log>`; deadline → exit 1 `did not park within %ds (pid %d); see <log>`; success prints `up name=<n> pid=<pid> preset=<label>`.

`cmdDown` (§4): parse (`--wait` default 15) → `ValidName` → `ReadState`; stale (dead pid) → `RemoveState`, pid=0 → if pid==0 and not present → print `down name=<n>`, exit 0 (nothing to stop; do **not** queue a stop) → Send stop (`From:"orch"`, `Kind:"stop"`) → `waitUntil(gone, wait)` where gone = pid>0 ? `!ProcessAlive(pid)` : `!present` → if pid>0 still alive: stderr `did not stop in %ds; terminating`, `host.Terminate(pid)`, `waitUntil(gone, 5s)`, else `host.Kill(pid)` + `waitUntil(gone, 2s)` → if pid>0 && gone: `RemoveState` → best-effort remove our own unconsumed stop file `filepath.Join(sp.InboxDir(name), envelope.Filename(stopEnv))` → print `down name=<n>`; exit 0 if gone else 1 with `still running`.

- [ ] **Step 1: Failing tests**: usage — `{"up"}`, `{"down"}`, `{"up","x"}` (unknown preset) → 2; `up x --exec 'tincan-no-such-binary-xyz {body}'` → 1, stderr contains `not found on PATH`; `up codex` with `writePresenceFile(own pid)` → `already up name=codex pid=<own>` exit 0 (no codex binary needed); `down nobody` on empty room → exit 0, stdout `down name=nobody`, inbox stays empty (no stop queued); integration `TestUpAskStatusDownRoundTrip` (skip on Windows): `TINCAN_AS_CLI=1`, temp HOME config `fake` = `[exe,"fake-agent","echo","{body}"]`, `up fake --room R --wait 10` → 0 and `up name=fake pid=… preset=fake`; `up` again → `already up`; `ask … --body "hello world" --format body` → `echo: hello world`; poll `status --format json` until row `fake` has `mode=="hosted:fake"`, `alive`, `!busy`; `down fake --room R --wait 10` → 0, `down name=fake`; then `ping` → 1 `absent`, pid dead (poll 3 s), state file gone, log file contains `=== `.

- [ ] **Step 2:** FAIL. **Step 3:** implement + wire `"up"`, `"down"`. **Step 4:** `go test -race ./...` PASS, `go vet ./...` clean, `GOOS=windows go vet ./...` clean. **Step 5: Commit** — `Add tincan up and down for hosted listeners`.

---

### Task 11: docs and skill

**Files:**
- Modify: `PROTOCOL.md` (CLI block + new section "Hosted listeners" after "Control messages"), `README.md` (quick start one-liner, `.tincan/` gitignore note, reinstall note), `skills/tell/SKILL.md` (auto-`up` step, `down` at end of session).

- [ ] **Step 1:** PROTOCOL.md: add `up/serve/down/presets` lines to the CLI block; new section covering model (§3), CLI (§4 incl. exit codes and idempotency), presets table + `agents.json` example + placeholders (§5), files (§6), error contract (§7), security caveat (§8), Windows note; mention `status` MODE/STATE columns in the Presence section.
- [ ] **Step 2:** README.md: Quick start gains `tincan up codex --room "$(git rev-parse --show-toplevel)"` then `/tell codex …`, and `tincan down codex --room …`; note to add `.tincan/` to `.gitignore`; Install note: an older installed binary must be reinstalled (`go install ./cmd/tincan` from a clone or `go install github.com/c0ze/tincan/cmd/tincan@main`) to get `status/ping/up/down`.
- [ ] **Step 3:** skills/tell/SKILL.md: new "Ensure the target is up" step before ask: `tincan ping --to <who> --room "$ROOM"`; exit 1 and `<who>` in `tincan presets` (or `--exec` known) → `tincan up <who> --room "$ROOM"`, abort with the stderr if that exits non-zero; Notes: `tincan down <who> --room "$ROOM"` when the session ends; hosted replies starting `ERROR ` are the agent failing, not tincan.
- [ ] **Step 4:** `go test ./...` still green (docs only). **Step 5: Commit** — `Document hosted listeners in PROTOCOL, README and the tell skill`.

---

### Task 12: verification and install

- [ ] **Step 1:** `gofmt -l .` (no output), `go vet ./...`, `GOOS=windows go vet ./...`, `GOOS=linux go vet ./...`, `go test -race -count=1 ./...` — all green.
- [ ] **Step 2:** Demo in a temp room with a fake agent preset in a temp HOME (`up` → `ask` → `status` → `down`) using the installed binary; capture the transcript for the report.
- [ ] **Step 3:** `go install ./cmd/tincan`; `tincan --help` shows `up/serve/down/presets`; `tincan presets` lists the six built-ins.
- [ ] **Step 4:** `git log --oneline main..feat/hosted-listeners`; report.
