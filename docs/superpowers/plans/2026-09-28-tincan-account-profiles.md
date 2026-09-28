# tincan account profiles (phase 2a) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one agent CLI run under several accounts, selected per preset through `env`, `env_unset` and `provider` fields, without leaking env values and without losing persistent sessions for aliased presets.

**Architecture:** `host.Preset` gains three fields, validated in `normalize`. `host.Run` layers them over the scrubbed environment. The session adapter asks the new `SessionProvider` helper which adapter a preset uses instead of trusting its label. The fingerprint covers the account configuration, and phase 1 session records migrate in place. `Up` hands the resolved preset to its daemon through a private file instead of argv. Public views show env keys, never values.

**Tech Stack:** Go 1.25, standard library only; the existing `internal/host`, `internal/cli` and `internal/mcpserver` packages.

**Spec:** `docs/superpowers/specs/2026-09-28-tincan-committees-design.md` §5 (phase 2a), with the round-3 amendments in §5.2 and §5.3.

## Global Constraints

- Env keys match `^[A-Z_][A-Z0-9_]{0,63}$`. `PATH`, the reserved prefix `TINCAN_`, and every key in `parentSessionEnv` (`internal/host/env.go`) are rejected. The same rules apply to `env_unset`.
- `env` values are strings. A leading `~/` expands to the home directory at launch.
- Environment order for a run: `agentEnv()`, minus `env_unset`, plus `env`. Phase 2b's protocol variables come later still; nothing in 2a sets `TINCAN_*` for the agent.
- `provider` ∈ {`claude`, `grok`, `agy`, `kimi`}.
- `env` values never appear in process arguments or public views: `tincan presets` (table and JSON), `tincan_presets`, `status`, and web.
- The listener's 0600 state file (`State.Config`) stays the durable record of the running configuration. `existingResult` keeps comparing against it.
- Codex remains stateless.
- No new dependencies. gofmt clean. `go vet ./...` clean. The Windows CI matrix must keep passing: guard Unix-only assertions with `runtime.GOOS`.
- Never use `git stash`. Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Empty collections.** An `agents.json` entry with `"env": {}` or `"env_unset": []` must relaunch as "already up", not "different configuration". `normalize` makes empty collections nil, so the JSON round trip through the state file compares equal. Pinned in Task 1 and Task 4.
2. **Upgrading with a live Claude conversation.** A phase 1 session record (no `version`) for a native `claude` listener must resume after the upgrade, not demand a reset. Pinned in Task 3.
3. **`~/` without a home directory.** A value starting with `~/` when the home directory is unknown must fail that request with a clear `ERROR exec: env …` reply. It must never run the agent with a literal `~`. Pinned in Task 2.
4. **Secret leakage.** `tincan presets --format json`, the table view, `tincan_presets`, and the serve process's argv must never contain an env value. Pinned in Tasks 1, 3 and 4.
5. **Ad hoc `--exec 'claude …'`** (label `custom`) stays stateless by default, as in phase 1. It must not suddenly produce stream-json. Pinned in Task 3.

---

### Task 1: Preset fields, validation and redacted views

**Files:**
- Modify: `internal/host/preset.go` (Preset, configPreset, LoadConfig, normalize; new `PublicPreset`/`Public`)
- Modify: `internal/cli/host.go:121-162` (`cmdPresets`)
- Modify: `PROTOCOL.md` ("Presets and `~/.config/tincan/agents.json`" section)
- Test: `internal/host/preset_test.go`, `internal/cli/host_test.go`

**Interfaces:**
- Produces: the fields `Preset.Env map[string]string` (`json:"env,omitempty"`), `Preset.EnvUnset []string` (`json:"env_unset,omitempty"`) and `Preset.Provider string` (`json:"provider,omitempty"`).
- Produces: `func (p Preset) Public() PublicPreset`.
- Produces: `type PublicPreset struct { Exec []string; Stdin, Reply string; ExecTimeoutSec int; Session, Provider string; EnvKeys, EnvUnset []string }`, with JSON tags `exec`, `stdin`, `reply`, `exec_timeout_sec`, `session,omitempty`, `provider,omitempty`, `env_keys,omitempty`, `env_unset,omitempty`.
- Produces: `func validEnvKey(k string) error`.

- [ ] **Step 1: Write the failing tests** in `internal/host/preset_test.go` (add `encoding/json`, `fmt`, `reflect` to the imports if missing):

```go
func TestLoadConfigAccountProfileFields(t *testing.T) {
	path := writeConfig(t, `{"claude-personal":{"exec":["claude","-p","{body}"],
		"env":{"CLAUDE_CONFIG_DIR":"~/.claude-personal"},"env_unset":["ANTHROPIC_API_KEY"],"provider":"claude"}}`)
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := got["claude-personal"]
	if p.Env["CLAUDE_CONFIG_DIR"] != "~/.claude-personal" || !reflect.DeepEqual(p.EnvUnset, []string{"ANTHROPIC_API_KEY"}) || p.Provider != "claude" {
		t.Fatalf("profile fields lost: %+v", p)
	}
}

func TestLoadConfigRejectsBadProfiles(t *testing.T) {
	for name, entry := range map[string]string{
		"lowercase key":    `{"exec":["x"],"env":{"home":"y"}}`,
		"leading digit":    `{"exec":["x"],"env":{"1A":"y"}}`,
		"PATH":             `{"exec":["x"],"env":{"PATH":"/bin"}}`,
		"reserved prefix":  `{"exec":["x"],"env":{"TINCAN_ROOM":"/"}}`,
		"scrubbed key":     `{"exec":["x"],"env":{"CLAUDECODE":"1"}}`,
		"too long":         fmt.Sprintf(`{"exec":["x"],"env":{"A%s":"y"}}`, strings.Repeat("B", 64)),
		"NUL value":        `{"exec":["x"],"env":{"A":"b\u0000c"}}`,
		"unset PATH":       `{"exec":["x"],"env_unset":["PATH"]}`,
		"unset reserved":   `{"exec":["x"],"env_unset":["TINCAN_ROOM"]}`,
		"set and unset":    `{"exec":["x"],"env":{"A":"1"},"env_unset":["A"]}`,
		"duplicate unset":  `{"exec":["x"],"env_unset":["A","A"]}`,
		"unknown provider": `{"exec":["x"],"provider":"codex"}`,
	} {
		if _, err := LoadConfig(writeConfig(t, `{"p":`+entry+`}`)); err == nil {
			t.Errorf("%s: accepted %s", name, entry)
		}
	}
}

func TestNormalizeDropsEmptyProfileCollections(t *testing.T) {
	got, err := LoadConfig(writeConfig(t, `{"p":{"exec":["x"],"env":{},"env_unset":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got["p"].Env != nil || got["p"].EnvUnset != nil {
		t.Fatalf("empty collections kept: %#v", got["p"])
	}
	data, _ := json.Marshal(got["p"])
	var back Preset
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back, got["p"]) {
		t.Fatalf("round trip changed preset: %#v vs %#v (%v)", back, got["p"], err)
	}
}

func TestPublicPresetWithholdsEnvValues(t *testing.T) {
	p := Preset{Exec: []string{"codex", "exec"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60,
		Env:      map[string]string{"CODEX_HOME": "~/.codex-gmail", "B_TOKEN": "s3cret"},
		EnvUnset: []string{"OPENAI_API_KEY"}}
	v := p.Public()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret") || strings.Contains(string(data), ".codex-gmail") {
		t.Fatalf("public view leaked a value: %s", data)
	}
	if !reflect.DeepEqual(v.EnvKeys, []string{"B_TOKEN", "CODEX_HOME"}) || !reflect.DeepEqual(v.EnvUnset, []string{"OPENAI_API_KEY"}) {
		t.Fatalf("public view keys: %+v", v)
	}
	if !reflect.DeepEqual(v.Exec, p.Exec) || v.ExecTimeoutSec != 60 {
		t.Fatalf("public view lost fields: %+v", v)
	}
}
```

Also add this to `internal/cli/host_test.go`:

```go
func TestPresetsViewsWithholdEnvValues(t *testing.T) {
	fakeAgentConfig(t)
	writeAgentsConfig(t, os.Getenv("HOME"), `{"acct":{"exec":["codex","exec"],"env":{"CODEX_HOME":"/secret/profile"},"env_unset":["OPENAI_API_KEY"]}}`)
	for _, format := range []string{"json", "table"} {
		code, out, errOut := run("presets", "--format", format)
		if code != ExitOK {
			t.Fatalf("%s: %d %s", format, code, errOut)
		}
		if strings.Contains(out, "/secret/profile") || !strings.Contains(out, "CODEX_HOME") || !strings.Contains(out, "OPENAI_API_KEY") {
			t.Fatalf("%s view:\n%s", format, out)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/host -run 'TestLoadConfigAccountProfileFields|TestLoadConfigRejectsBadProfiles|TestNormalizeDropsEmptyProfileCollections|TestPublicPresetWithholdsEnvValues' && go test ./internal/cli -run TestPresetsViewsWithholdEnvValues`
Expected: compile failure (`p.Env undefined`, `p.Public undefined`).

- [ ] **Step 3: Implement.** In `internal/host/preset.go`, add these fields to `Preset` after `Session`:

```go
	// Env sets variables for the agent process, after EnvUnset (spec §5.1).
	// Values are private: public views show keys only (see Public).
	Env map[string]string `json:"env,omitempty"`
	// EnvUnset removes inherited variables, e.g. ANTHROPIC_API_KEY so a
	// profile's own login is used.
	EnvUnset []string `json:"env_unset,omitempty"`
	// Provider names the persistent-session adapter explicitly, for wrappers
	// and executables whose base name is not the adapter's (spec §5.3).
	Provider string `json:"provider,omitempty"`
```

Add the same three fields, with the same tags, to `configPreset`. In `LoadConfig`, copy them: `p := Preset{…, Session: c.Session, Env: c.Env, EnvUnset: c.EnvUnset, Provider: c.Provider}`.

At the end of `normalize`, before `return nil`, add:

```go
	if len(p.Env) == 0 {
		p.Env = nil
	}
	if len(p.EnvUnset) == 0 {
		p.EnvUnset = nil
	}
	for k, v := range p.Env {
		if err := validEnvKey(k); err != nil {
			return err
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("env %s: value contains a NUL byte", k)
		}
	}
	seen := map[string]bool{}
	for _, k := range p.EnvUnset {
		if err := validEnvKey(k); err != nil {
			return err
		}
		if seen[k] {
			return fmt.Errorf("env_unset lists %s twice", k)
		}
		if _, ok := p.Env[k]; ok {
			return fmt.Errorf("%s is both set in env and removed by env_unset", k)
		}
		seen[k] = true
	}
	if p.Provider != "" && !SupportsSessions(p.Provider) {
		return fmt.Errorf("provider must be claude, grok, agy or kimi, got %q", p.Provider)
	}
```

Add the helper and the public view below `hasPlaceholder` (add `regexp` to the imports):

```go
var envKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// validEnvKey applies the spec §5.1 key rules to env and env_unset names.
func validEnvKey(k string) error {
	switch {
	case !envKeyPattern.MatchString(k):
		return fmt.Errorf("env key %q must match %s", k, envKeyPattern)
	case k == "PATH":
		return errors.New("env cannot change PATH; tincan resolves agent executables itself")
	case strings.HasPrefix(k, "TINCAN_"):
		return fmt.Errorf("env key %s uses the reserved TINCAN_ prefix", k)
	case parentSessionEnv[k]:
		return fmt.Errorf("env key %s is a parent-session variable tincan always removes", k)
	}
	return nil
}

// PublicPreset is a preset as shown by `tincan presets` and other public
// views: env values are withheld, only their keys are listed (spec §5.2).
type PublicPreset struct {
	Exec           []string `json:"exec"`
	Stdin          string   `json:"stdin"`
	Reply          string   `json:"reply"`
	ExecTimeoutSec int      `json:"exec_timeout_sec"`
	Session        string   `json:"session,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	EnvKeys        []string `json:"env_keys,omitempty"`
	EnvUnset       []string `json:"env_unset,omitempty"`
}

// Public returns the redacted view of p.
func (p Preset) Public() PublicPreset {
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		keys = nil
	}
	return PublicPreset{Exec: p.Exec, Stdin: p.Stdin, Reply: p.Reply, ExecTimeoutSec: p.ExecTimeoutSec,
		Session: p.Session, Provider: p.Provider, EnvKeys: keys, EnvUnset: p.EnvUnset}
}
```

In `internal/cli/host.go` `cmdPresets`, marshal the public views:

```go
	if *format == "json" {
		views := make(map[string]host.PublicPreset, len(presets))
		for name, p := range presets {
			views[name] = p.Public()
		}
		data, err := json.MarshalIndent(views, "", "  ")
```

Add an `ENV` column to the table, before `EXEC`:

```go
	fmt.Fprintln(tw, "NAME\tSTDIN\tREPLY\tTIMEOUT\tBINARY\tENV\tEXEC")
	…
		env := envSummary(p.Public())
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n, p.Stdin, p.Reply, timeout, binary, env, strings.Join(p.Exec, " "))
```

and add:

```go
// envSummary lists a preset's env keys and, prefixed with "-", its removed
// variables; values are never shown.
func envSummary(v host.PublicPreset) string {
	parts := append([]string(nil), v.EnvKeys...)
	for _, k := range v.EnvUnset {
		parts = append(parts, "-"+k)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}
```

In `PROTOCOL.md`, after the `agents.json` example block in "Presets and `~/.config/tincan/agents.json`", add:

````markdown
**Account profiles.** One CLI can serve several accounts: `env` sets
variables for the agent process, `env_unset` removes inherited ones, and
`provider` names the session adapter for wrappers. Keys match
`^[A-Z_][A-Z0-9_]{0,63}$`; `PATH`, the `TINCAN_` prefix and the parent-session
variables tincan always scrubs are rejected. A value starting with `~/` expands
to the home directory at launch.

```json
{
  "codex-gmail":     { "exec": ["codex", "exec", "-s", "read-only", "--skip-git-repo-check", "-o", "{out}", "-"],
                       "stdin": "body", "reply": "file", "env": {"CODEX_HOME": "~/.codex-gmail"} },
  "claude-personal": { "exec": ["claude", "-p", "{body}", "--permission-mode", "plan"],
                       "env": {"CLAUDE_CONFIG_DIR": "~/.claude-personal"}, "env_unset": ["ANTHROPIC_API_KEY"] }
}
```

Env values stay private: `tincan presets`, MCP `tincan_presets` and `status`
show keys only, and a hosted listener receives its resolved preset through a
0600 file rather than its command line.
````

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/host ./internal/cli`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/host/preset.go internal/host/preset_test.go internal/cli/host.go internal/cli/host_test.go PROTOCOL.md
git commit -m "Add env, env_unset and provider to presets with redacted views"
```

---

### Task 2: Apply the account environment to agent runs

**Files:**
- Modify: `internal/host/env.go` (new `composeEnv`, `envName`)
- Modify: `internal/host/runner.go` (the `RunSpec` fields; `Run` uses `composeEnv`)
- Modify: `internal/host/serve.go` (`handle` passes the preset's env)
- Test: `internal/host/runner_test.go`, `internal/host/serve_test.go`

**Interfaces:**
- Consumes: `Preset.Env` and `Preset.EnvUnset` (Task 1).
- Produces: the fields `RunSpec.Env map[string]string` and `RunSpec.EnvUnset []string`.
- Produces: `func composeEnv(base, unset []string, set map[string]string) ([]string, error)`.

- [ ] **Step 1: Write the failing tests.** Add to `internal/host/runner_test.go` (imports `path/filepath`, `runtime`, `strings` if missing):

```go
func TestRunAppliesAccountProfileEnv(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	t.Setenv("ANTHROPIC_API_KEY", "inherited")
	t.Setenv("PROFILE_DIR", "inherited")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	res := Run(context.Background(), RunSpec{
		Argv: fakeExec("env", "ANTHROPIC_API_KEY", "PROFILE_DIR", "PROFILE_MODE"), Dir: t.TempDir(),
		Env:      map[string]string{"PROFILE_DIR": "~/.claude-personal", "PROFILE_MODE": "work"},
		EnvUnset: []string{"ANTHROPIC_API_KEY"},
	})
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	want := "PROFILE_DIR=" + filepath.Join(home, ".claude-personal") + "\nPROFILE_MODE=work\n"
	if string(res.Stdout) != want {
		t.Fatalf("agent environment:\n%s\nwant:\n%s", res.Stdout, want)
	}
}

func TestRunProfileTildeNeedsHomeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("home directory comes from USERPROFILE and the profile API")
	}
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	t.Setenv("HOME", "")
	spec := RunSpec{Argv: fakeExec("env", "PROFILE_DIR"), Dir: t.TempDir(), Env: map[string]string{"PROFILE_DIR": "~/.p"}}
	res := Run(context.Background(), spec)
	if res.Err == nil || res.ExitCode != -1 {
		t.Fatalf("ran without a home directory: %+v", res)
	}
	if body := ReplyBody(spec, res); !strings.HasPrefix(body, "ERROR exec: env PROFILE_DIR") {
		t.Fatalf("reply = %q", body)
	}
}

func TestComposeEnvWithoutProfileKeepsBase(t *testing.T) {
	base := []string{"A=1", "B=2"}
	got, err := composeEnv(base, nil, nil)
	if err != nil || !reflect.DeepEqual(got, base) {
		t.Fatalf("composeEnv changed base: %v %v", got, err)
	}
}
```

Add to `internal/host/serve_test.go`:

```go
func TestServeRunsAgentWithPresetEnv(t *testing.T) {
	p := Preset{Exec: fakeExec("env", "PROFILE_MODE"), Env: map[string]string{"PROFILE_MODE": "work"}}
	_, sp, _, done, _ := startServe(t, p, "fake")
	if reply := askVia(t, sp, "hi"); reply.Body != "PROFILE_MODE=work" {
		t.Fatalf("reply body = %q", reply.Body)
	}
	stopServe(t, sp, done)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/host -run 'TestRunAppliesAccountProfileEnv|TestRunProfileTildeNeedsHomeDirectory|TestComposeEnvWithoutProfileKeepsBase|TestServeRunsAgentWithPresetEnv'`
Expected: compile failure (`unknown field Env in struct literal of type RunSpec`).

- [ ] **Step 3: Implement.** Append to `internal/host/env.go` (imports `fmt`, `os`, `path/filepath`, `runtime`, `sort`, `strings`):

```go
// composeEnv applies a preset's account profile on top of the scrubbed
// environment (spec §5.1): names in unset are removed, then set adds or
// replaces variables in key order. A value starting with "~/" expands against
// the home directory; an unknown home directory is an error rather than a
// literal "~". Names compare case-insensitively on Windows.
func composeEnv(base, unset []string, set map[string]string) ([]string, error) {
	if len(unset) == 0 && len(set) == 0 {
		return base, nil
	}
	drop := make(map[string]bool, len(unset)+len(set))
	for _, k := range unset {
		drop[envName(k)] = true
	}
	for k := range set {
		drop[envName(k)] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[envName(name)] {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := set[k]
		if strings.HasPrefix(v, "~/") {
			home, err := os.UserHomeDir()
			if err != nil || home == "" {
				return nil, fmt.Errorf("env %s: cannot expand ~/ because the home directory is unknown", k)
			}
			v = filepath.Join(home, v[2:])
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

func envName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
```

In `internal/host/runner.go`, add to `RunSpec` after `Stdin`:

```go
	Env      map[string]string // preset env, applied after EnvUnset (spec §5.1)
	EnvUnset []string          // inherited variables to remove
```

and in `Run`, replace `cmd.Env = agentEnv()` with:

```go
	env, err := composeEnv(agentEnv(), spec.EnvUnset, spec.Env)
	if err != nil {
		return Result{ExitCode: -1, Err: err, Duration: time.Since(start)}
	}
```

and assign `cmd.Env = env` after `cmd.Dir = spec.Dir`. The resolve step above it also declares `err`, so reuse it with `=` where the compiler requires it. `ReplyBody` already renders `res.Err` as `ERROR exec: <msg>`.

In `internal/host/serve.go` `handle`, extend the `RunSpec` literal:

```go
	spec := RunSpec{Dir: o.Room, Timeout: time.Duration(p.ExecTimeoutSec) * time.Second, Env: p.Env, EnvUnset: p.EnvUnset,
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/host`
Expected: PASS, including the existing `TestRunDropsParentAgentSessionEnv`.

- [ ] **Step 5: Commit**

```bash
git add internal/host/env.go internal/host/runner.go internal/host/serve.go internal/host/runner_test.go internal/host/serve_test.go
git commit -m "Run agents with their preset's account environment"
```

---

### Task 3: Session provider identity, fingerprint v2 and legacy migration

**Files:**
- Modify: `internal/host/session.go` (`SessionProvider`, `WithSession`, `PrepareSession`, `sessionRecord`, the fingerprint helpers)
- Modify: `internal/host/serve.go:243` (`PostProcess` by provider)
- Modify: `internal/host/preset.go` (`PostProcess` parameter renamed to `provider`; doc comment)
- Modify: `internal/mcpserver/server.go:97` (`session_supported` from the provider)
- Modify: `PROTOCOL.md` ("Conversations and reset" section)
- Test: `internal/host/session_test.go`, `internal/host/serve_test.go`, `internal/mcpserver/server_test.go`

**Interfaces:**
- Consumes: `Preset.Provider`, `Preset.Env`, `Preset.EnvUnset` (Task 1).
- Produces: `func SessionProvider(p Preset, label string) (provider string, implicit bool)`.
- Produces: `func sessionFingerprint(p Preset, provider string) string` and `func legacySessionFingerprint(p Preset) string`.
- Produces: `const sessionRecordVersion = 2`.
- Unchanged signatures: `WithSession(p Preset, label, mode string) (Preset, error)`, `PrepareSession(room, name, label string, p Preset) (*SessionRun, error)`, `PostProcess(provider, body string) string`.

- [ ] **Step 1: Write the failing tests.** Add to `internal/host/session_test.go`:

```go
func TestSessionProviderFromExecutableNotLabel(t *testing.T) {
	for _, c := range []struct {
		name, label string
		p           Preset
		provider    string
		implicit    bool
	}{
		{"native", "claude", Preset{Exec: []string{"claude", "-p", "{body}"}}, "claude", true},
		{"alias", "claude-personal", Preset{Exec: []string{"claude", "-p", "{body}"}}, "claude", true},
		{"windows exe", "kimi-work", Preset{Exec: []string{"kimi.exe", "-p", "{body}"}}, "kimi", true},
		{"absolute path", "grok-2", Preset{Exec: []string{"/opt/bin/grok", "-p", "{body}"}}, "grok", true},
		{"explicit wrapper", "mine", Preset{Exec: []string{"my-wrapper"}, Provider: "agy"}, "agy", true},
		{"wrapper under supported label", "claude", Preset{Exec: []string{"custom-wrapper"}}, "claude", false},
		{"ad hoc exec", "custom", Preset{Exec: []string{"claude", "-p", "{body}"}}, "claude", false},
		{"unsupported", "codex-gmail", Preset{Exec: []string{"codex", "exec"}}, "", false},
	} {
		provider, implicit := SessionProvider(c.p, c.label)
		if provider != c.provider || implicit != c.implicit {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, provider, implicit, c.provider, c.implicit)
		}
	}
}

func TestWithSessionDefaultsFollowProvider(t *testing.T) {
	claude := Preset{Exec: []string{"claude", "-p", "{body}"}}
	if p, err := WithSession(claude, "claude-personal", ""); err != nil || p.Session != "persistent" {
		t.Fatalf("alias not persistent: %+v %v", p, err)
	}
	if p, err := WithSession(claude, "custom", ""); err != nil || p.Session != "stateless" {
		t.Fatalf("ad hoc exec became persistent: %+v %v", p, err)
	}
	if p, err := WithSession(claude, "custom", "persistent"); err != nil || p.Session != "persistent" {
		t.Fatalf("ad hoc exec cannot opt in: %+v %v", p, err)
	}
	codex := Preset{Exec: []string{"codex", "exec"}}
	if _, err := WithSession(codex, "codex-gmail", "persistent"); err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("unsupported persistent accepted or unhelpful: %v", err)
	}
}

func TestAliasPresetKeepsPersistentSession(t *testing.T) {
	room := t.TempDir()
	p := Builtin()["claude"]
	first, err := PrepareSession(room, "one", "claude-personal", p)
	if err != nil {
		t.Fatal(err)
	}
	if first.Preset.Session != "persistent" || first.record.Provider != "claude" || !hasExactArg(first.Preset.Exec, "--session-id") {
		t.Fatalf("alias did not use the claude adapter: %+v %q", first.record, first.Preset.Exec)
	}
	id := first.record.ID
	feedSession(t, first, sessionFixture("claude", id))
	if _, err := first.Reply(RunSpec{}, Result{ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	second, err := PrepareSession(room, "one", "claude-personal", p)
	if err != nil || !hasExactArg(second.Preset.Exec, "--resume") || !hasExactArg(second.Preset.Exec, id) {
		t.Fatalf("alias did not resume: %q %v", second.Preset.Exec, err)
	}
}

func TestSessionFingerprintCoversAccountProfile(t *testing.T) {
	room := t.TempDir()
	p := Builtin()["claude"]
	p.Env = map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-a"}
	first, err := PrepareSession(room, "one", "claude", p)
	if err != nil {
		t.Fatal(err)
	}
	feedSession(t, first, sessionFixture("claude", first.record.ID))
	if _, err := first.Reply(RunSpec{}, Result{ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	if again, err := PrepareSession(room, "one", "claude", p); err != nil || !hasExactArg(again.Preset.Exec, first.record.ID) {
		t.Fatalf("same profile did not resume: %v", err)
	}
	otherAccount := p
	otherAccount.Env = map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-b"}
	unset := p
	unset.EnvUnset = []string{"ANTHROPIC_API_KEY"}
	for name, changed := range map[string]Preset{"env value": otherAccount, "env_unset": unset} {
		if _, err := PrepareSession(room, "one", "claude", changed); err == nil || !strings.Contains(err.Error(), "preset changed") {
			t.Errorf("%s change resumed another account's session: %v", name, err)
		}
	}
	data, err := os.ReadFile(first.path)
	if err != nil || strings.Contains(string(data), ".claude-a") {
		t.Fatalf("session record stores an env value: %s", data)
	}
}

func TestLegacySessionRecordMigrates(t *testing.T) {
	const id = "0b8a3f7e-8f5c-4c1e-9a55-3f0e6d2c1b4a"
	write := func(t *testing.T, room string, rec sessionRecord) string {
		t.Helper()
		canonical, err := canonicalRoom(room)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(rec)
		path := sessionPath(canonical, "one")
		if err := fsutil.WriteFileAtomic(path, data); err != nil {
			t.Fatal(err)
		}
		return path
	}
	p := Builtin()["claude"]
	wp, err := WithSession(p, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	room := t.TempDir()
	path := write(t, room, sessionRecord{Provider: "claude", Preset: legacySessionFingerprint(wp), ID: id, Ready: true})
	s, err := PrepareSession(room, "one", "claude", p)
	if err != nil || !hasExactArg(s.Preset.Exec, id) {
		t.Fatalf("legacy record not resumed: %q %v", s.Preset.Exec, err)
	}
	data, _ := os.ReadFile(path)
	var rec sessionRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Version != sessionRecordVersion || rec.Preset != sessionFingerprint(wp, "claude") || rec.ID != id {
		t.Fatalf("legacy record not upgraded: %s %v", data, err)
	}

	stale := t.TempDir()
	write(t, stale, sessionRecord{Provider: "claude", Preset: "not-the-fingerprint", ID: id, Ready: true})
	if _, err := PrepareSession(stale, "one", "claude", p); err == nil || !strings.Contains(err.Error(), "preset changed") {
		t.Fatalf("mismatched legacy record accepted: %v", err)
	}

	profiled := t.TempDir()
	write(t, profiled, sessionRecord{Provider: "claude", Preset: legacySessionFingerprint(wp), ID: id, Ready: true})
	withEnv := p
	withEnv.Env = map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-b"}
	if _, err := PrepareSession(profiled, "one", "claude", withEnv); err == nil {
		t.Fatal("legacy record migrated onto an account profile")
	}
}
```

Add to `internal/host/serve_test.go`:

```go
func TestServeKimiCleanupFollowsProviderNotLabel(t *testing.T) {
	p := Preset{Exec: fakeExec("stdout", "answer\nTo resume this session: kimi -r x"), Provider: "kimi", Session: "stateless"}
	_, sp, _, done, _ := startServe(t, p, "kimi-work")
	if reply := askVia(t, sp, "hi"); reply.Body != "answer" {
		t.Fatalf("reply body = %q", reply.Body)
	}
	stopServe(t, sp, done)
}
```

In `internal/mcpserver/server_test.go`, split `connect` into `connect(t, room)`, which calls `connectPresets(t, room, map[string]host.Preset{"fixture": fixture(t)})`, and `connectPresets`, which holds the current body with the map passed through. Then add:

```go
func TestPresetsReportSessionSupportFromProvider(t *testing.T) {
	cs := connectPresets(t, t.TempDir(), map[string]host.Preset{
		"claude-personal": {Exec: []string{"claude", "-p", "{body}"}, Stdin: "none", Reply: "stdout", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/secret"}},
		"codex-gmail":     {Exec: []string{"codex", "exec"}, Stdin: "none", Reply: "stdout"},
	})
	out := call(t, cs, "tincan_presets", map[string]any{})
	data, _ := json.Marshal(out)
	if strings.Contains(string(data), "/secret") {
		t.Fatalf("presets tool leaked an env value: %s", data)
	}
	got := map[string]bool{}
	for _, v := range out["presets"].([]any) {
		m := v.(map[string]any)
		got[m["name"].(string)] = m["session_supported"] == true
	}
	if !got["claude-personal"] || got["codex-gmail"] {
		t.Fatalf("session_supported = %v", got)
	}
}
```

Check the JSON names on `PresetView` in `internal/mcpserver/server.go:75` and use those exact names (`name` and `session_supported` are expected).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/host -run 'TestSessionProvider|TestWithSessionDefaults|TestAliasPreset|TestSessionFingerprint|TestLegacySession|TestServeKimiCleanup' ; go test ./internal/mcpserver -run TestPresetsReportSessionSupportFromProvider`
Expected: compile failure (`undefined: SessionProvider`).

- [ ] **Step 3: Implement.** In `internal/host/session.go`, add below `SupportsSessions`:

```go
// SessionProvider names the persistent-session adapter for a preset (spec
// §5.3): the explicit provider, else the executable's base name when it is a
// supported adapter, else a supported label. implicit reports whether the
// configuration itself identifies the adapter, which makes persistent the
// default. A label alone (a wrapper under a provider's name) and an ad hoc
// --exec (label "custom") only permit an explicit opt-in, as in phase 1.
func SessionProvider(p Preset, label string) (provider string, implicit bool) {
	if p.Provider != "" {
		return p.Provider, true
	}
	if len(p.Exec) > 0 {
		if base := strings.TrimSuffix(filepath.Base(p.Exec[0]), ".exe"); SupportsSessions(base) {
			return base, label != "custom"
		}
	}
	if SupportsSessions(label) {
		return label, false
	}
	return "", false
}
```

Replace the body of `WithSession`:

```go
func WithSession(p Preset, label, mode string) (Preset, error) {
	provider, implicit := SessionProvider(p, label)
	if mode == "" {
		mode = p.Session
	}
	if mode == "" {
		mode = "stateless"
		if implicit {
			mode = "persistent"
		}
	}
	if mode != "persistent" && mode != "stateless" {
		return Preset{}, fmt.Errorf("session must be persistent or stateless, got %q", mode)
	}
	if mode == "persistent" && provider == "" {
		return Preset{}, fmt.Errorf("preset %q has no supported persistent session adapter; set \"provider\" to claude, grok, agy or kimi", label)
	}
	p.Session = mode
	p.Exec = append([]string(nil), p.Exec...)
	return p, nil
}
```

Update its doc comment: "Custom commands remain stateless unless their configuration names a provider or the caller opts in explicitly."

Change `sessionRecord` and add the fingerprints (import `encoding/hex` is not needed; keep `fmt.Sprintf("%x", …)`; add `sort`):

```go
// sessionRecordVersion 2 fingerprints provider and account profile; records
// without a version are phase 1's (see PrepareSession's migration).
const sessionRecordVersion = 2

type sessionRecord struct {
	Version  int       `json:"version,omitempty"`
	Provider string    `json:"provider"`
	Preset   string    `json:"preset"`
	ID       string    `json:"id,omitempty"`
	Ready    bool      `json:"ready"`
	Updated  time.Time `json:"updated"`
}

// sessionFingerprint identifies the conversation's owner: provider, command
// and account profile. Env values are hashed, never stored.
func sessionFingerprint(p Preset, provider string) string {
	unset := append([]string(nil), p.EnvUnset...)
	sort.Strings(unset)
	identity, _ := json.Marshal(struct {
		Version      int
		Provider     string
		Exec         []string
		Stdin, Reply string
		Env          map[string]string
		EnvUnset     []string
	}{sessionRecordVersion, provider, p.Exec, p.Stdin, p.Reply, p.Env, unset})
	return fmt.Sprintf("%x", sha256.Sum256(identity))
}

// legacySessionFingerprint is phase 1's fingerprint, kept to migrate records.
func legacySessionFingerprint(p Preset) string {
	identity, _ := json.Marshal(struct {
		Exec         []string
		Stdin, Reply string
	}{p.Exec, p.Stdin, p.Reply})
	return fmt.Sprintf("%x", sha256.Sum256(identity))
}
```

In `PrepareSession`, after `WithSession`, compute `provider, _ := SessionProvider(p, label)` and use `provider` wherever `label` appeared below it: `sessionArgs(p.Exec, provider)`, the provider check and its message, `validSessionID(provider, …)`, the `agy`/`kimi` resume-flag choice, the `claude`/`grok` new-ID branch, and `s.parser.provider = provider`. Replace the fingerprint block and the resume checks with:

```go
	fingerprint := sessionFingerprint(p, provider)
	s.record = sessionRecord{Version: sessionRecordVersion, Provider: provider, Preset: fingerprint}
	data, err := fsutil.ReadFile(s.path, 16<<10)
	resume := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read session: %w", err)
	}
	migrated := false
	if resume {
		if err := json.Unmarshal(data, &s.record); err != nil {
			return nil, fmt.Errorf("read session: %w", err)
		}
		// Phase 1 records carry no version and fingerprint only the command;
		// they migrate when nothing an account profile adds has changed.
		if s.record.Version == 0 && s.record.Provider == provider && s.record.Preset == legacySessionFingerprint(p) &&
			len(p.Env) == 0 && len(p.EnvUnset) == 0 {
			s.record.Version, s.record.Preset, migrated = sessionRecordVersion, fingerprint, true
		}
		if s.record.Provider != provider {
			return nil, fmt.Errorf("listener session belongs to %q, not %q; stop and reset the listener first", s.record.Provider, provider)
		}
		if s.record.Preset != fingerprint {
			return nil, errors.New("listener preset changed since its session was created; stop and reset the listener explicitly")
		}
		if !s.record.Ready || !validSessionID(provider, s.record.ID) {
			return nil, errors.New("previous session initialization was interrupted; inspect the previous run, then stop and reset the listener explicitly")
		}
		flag := "--resume"
		if provider == "agy" {
			flag = "--conversation"
		} else if provider == "kimi" {
			flag = "--session"
		}
		argv = appendSessionOptions(argv, flag, s.record.ID)
	} else if provider == "claude" || provider == "grok" {
```

Keep the rest of the function. Change the final save to `if !resume || migrated { if err := s.save(); … }`.

`sessionArgs(argv []string, label string)`: rename the parameter to `provider`. It is already only compared against provider names.

In `internal/host/preset.go`, rename the `PostProcess` parameter from `label` to `provider` and update its comment ("a kimi provider additionally drops…"). In `internal/host/serve.go` `handle`, replace `return PostProcess(o.Label, body)` with:

```go
	provider, _ := SessionProvider(o.Preset, o.Label)
	return PostProcess(provider, body)
```

In `internal/mcpserver/server.go` `presetsTool`:

```go
		provider, _ := host.SessionProvider(p, name)
		out.Presets = append(out.Presets, PresetView{Name: name, Available: available, SessionSupported: provider != ""})
```

In `PROTOCOL.md` "Conversations and reset", replace the paragraph that starts "The `session` field in a preset…" with:

```markdown
The `session` field in a preset can be `persistent` or `stateless`. An omitted
field selects persistent mode when the preset's provider is known: its
`provider` field, or the base name of its executable (`claude`, `grok`, `agy`,
`kimi`). So `claude-personal` running `claude` keeps its conversation. A custom
wrapper stays stateless unless it sets `provider`; an ad hoc `--exec` opts in
with `--session persistent`. Persistent adapters manage output/resume flags, so
remove conflicting `--continue`, `--resume` or session-ID flags from the
configured command.
```

In the paragraph after the provider table, replace "Each pointer records its provider and preset identity" with: "Each pointer records its provider and a fingerprint of the command and account profile (`env`, `env_unset`); changing those requires an explicit reset. Pointers written before account profiles migrate automatically when nothing else changed."

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/host ./internal/mcpserver ./internal/dispatch ./internal/cli ./internal/thread ./internal/web`
Expected: PASS. The existing `TestSessionPoliciesAndCustomExecutables` must pass unchanged: a wrapper under label `claude` stays stateless, and explicit persistent is accepted through the label fallback.

- [ ] **Step 5: Commit**

```bash
git add internal/host/session.go internal/host/serve.go internal/host/preset.go internal/host/session_test.go internal/host/serve_test.go internal/mcpserver/server.go internal/mcpserver/server_test.go PROTOCOL.md
git commit -m "Choose session adapters by provider and fingerprint account profiles"
```

---

### Task 4: Private preset hand-off from `up` to its daemon

**Files:**
- Modify: `internal/host/lifecycle.go` (`Up` writes the hand-off file; the new helpers)
- Modify: `internal/cli/host.go:164-190` (`cmdServe`: `--resolved-preset-file` replaces `--resolved-preset`)
- Modify: `PROTOCOL.md` ("Files under the room" section)
- Test: `internal/host/lifecycle_test.go`, `internal/cli/host_test.go`

**Interfaces:**
- Consumes: `Preset` JSON with private fields (Task 1). `Preset.normalize()` makes empty collections nil.
- Produces: `func presetFileDir(room, name string) string`, which returns `<Dir(room)>/config/<name>`.
- Produces: `func writePresetFile(room, name, owner string, p Preset) (string, error)`.
- Produces: `func ReadPresetFile(room, name, path string) (Preset, error)`, which reads the file, removes it, validates it and returns the preset.
- Produces: `func removeStalePresetFiles(room, name string) error`.
- Produces: the internal serve flag `--resolved-preset-file <path>`. `--resolved-preset` is removed.

- [ ] **Step 1: Write the failing tests.** Add to `internal/host/lifecycle_test.go` (imports `encoding/json`, `os`, `path/filepath`, `reflect`, `runtime`, `strings`):

```go
func TestPresetFileHandOff(t *testing.T) {
	room := canonicalTempDir(t)
	p := Preset{Exec: []string{"codex"}, Stdin: "none", Reply: "stdout", Env: map[string]string{"CODEX_HOME": "/secret"}}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	path, err := writePresetFile(room, "agent", "owner1", p)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != presetFileDir(room, "agent") {
		t.Fatalf("hand-off file outside its directory: %s", path)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("hand-off file mode: %v %v", info, err)
		}
		dir, err := os.Stat(presetFileDir(room, "agent"))
		if err != nil || dir.Mode().Perm() != 0o700 {
			t.Fatalf("hand-off dir mode: %v %v", dir, err)
		}
	}
	got, err := ReadPresetFile(room, "agent", path)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("read back %+v %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hand-off file not removed after reading: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(outside, []byte(`{"exec":["x"]}`), 0o600)
	if _, err := ReadPresetFile(room, "agent", outside); err == nil {
		t.Fatal("read a preset file outside the listener's hand-off directory")
	}
	stale, _ := writePresetFile(room, "agent", "old", p)
	if err := removeStalePresetFiles(room, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale hand-off file survived")
	}
}
```

Add to `internal/cli/host_test.go`. First extend `fakeAgent` with an env mode, before the final usage error:

```go
	if len(args) > 1 && args[0] == "env" {
		fmt.Printf("%s=%s\n", args[1], os.Getenv(args[1]))
		return 0
	}
```

Then add:

```go
func TestUpHandsPresetEnvPrivately(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached up unsupported")
	}
	fakeAgentConfig(t)
	exe, _ := os.Executable()
	writeAgentsConfig(t, os.Getenv("HOME"), fmt.Sprintf(
		`{"fake":{"exec":[%q,"fake-agent","env","PROFILE_TOKEN"],"env":{"PROFILE_TOKEN":"s3cret-value"},"env_unset":[]}}`, exe))
	room := t.TempDir()
	t.Cleanup(func() { run("down", "fake", "--room", room, "--wait", "5") })
	if code, out, errOut := run("up", "fake", "--room", room, "--wait", "10"); code != ExitOK {
		t.Fatalf("up: %d %q %q", code, out, errOut)
	}
	st, ok, err := host.ReadState(room, "fake")
	if err != nil || !ok {
		t.Fatalf("state: %v %v", ok, err)
	}
	args, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(st.PID)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "s3cret-value") || strings.Contains(string(args), "--resolved-preset ") {
		t.Fatalf("serve argv exposes the preset: %s", args)
	}
	if entries, _ := os.ReadDir(filepath.Join(host.Dir(room), "config", "fake")); len(entries) != 0 {
		t.Fatalf("hand-off file left behind: %v", entries)
	}
	if code, out, errOut := run("up", "fake", "--room", room, "--wait", "10"); code != ExitOK || !strings.HasPrefix(out, "already up") {
		t.Fatalf("identical relaunch: %d %q %q", code, out, errOut)
	}
	code, out, errOut := run("ask", "--room", room, "--to", "fake", "--from", "orch", "--body", "hi", "--timeout", "20")
	if code != ExitOK || !strings.Contains(out, "PROFILE_TOKEN=s3cret-value") {
		t.Fatalf("ask: %d %q %q", code, out, errOut)
	}
}
```


- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/host -run TestPresetFileHandOff ; go test ./internal/cli -run TestUpHandsPresetEnvPrivately`
Expected: the host test fails to compile (`undefined: writePresetFile`). The CLI test fails because serve's argv contains `s3cret-value`.

- [ ] **Step 3: Implement.** Add to `internal/host/lifecycle.go` (imports `github.com/c0ze/tincan/v2/internal/fsutil`):

```go
// presetFileDir holds the private hand-off of a launch's resolved preset from
// Up to the daemon it starts (spec §5.2). One directory per listener, because
// listener names may contain dots.
func presetFileDir(room, name string) string { return filepath.Join(Dir(room), "config", name) }

// writePresetFile stores p for the daemon of lifetime owner: 0700 directory,
// 0600 file, written atomically. The argv carries only the path.
func writePresetFile(room, name, owner string, p Preset) (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	path := filepath.Join(presetFileDir(room, name), owner+".json")
	if err := fsutil.WriteFileAtomic(path, data); err != nil {
		return "", fmt.Errorf("write preset hand-off: %w", err)
	}
	return path, nil
}

// ReadPresetFile loads and removes the hand-off file at path, which must be in
// listener name's hand-off directory of room.
func ReadPresetFile(room, name, path string) (Preset, error) {
	if err := spool.ValidName(name); err != nil {
		return Preset{}, err
	}
	room, err := canonicalRoom(room)
	if err != nil {
		return Preset{}, err
	}
	dir := presetFileDir(room, name)
	if filepath.Dir(filepath.Clean(path)) != dir {
		return Preset{}, fmt.Errorf("preset hand-off %s is not in %s", path, dir)
	}
	data, err := fsutil.ReadFile(path, 1<<20)
	if err != nil {
		return Preset{}, fmt.Errorf("read preset hand-off: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Preset{}, err
	}
	_ = fsutil.SyncDir(dir)
	var p Preset
	if err := json.Unmarshal(data, &p); err != nil {
		return Preset{}, fmt.Errorf("read preset hand-off: %w", err)
	}
	if err := p.normalize(); err != nil {
		return Preset{}, err
	}
	return p, nil
}

// removeStalePresetFiles empties name's hand-off directory. Callers hold the
// launch lock with the lifetime lock free, so no daemon is reading one.
func removeStalePresetFiles(room, name string) error {
	dir := presetFileDir(room, name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
```

In `Up`, replace the `json.Marshal(o.Preset)` block and the `args` line with:

```go
	owner := envelope.NewID()
	if err := removeStalePresetFiles(room, o.Name); err != nil {
		return UpResult{}, err
	}
	presetFile, err := writePresetFile(room, o.Name, owner, o.Preset)
	if err != nil {
		return UpResult{}, err
	}
	// serve removes the file once it has read it; this covers a daemon that
	// never got that far.
	defer os.Remove(presetFile)
	args := []string{"serve", o.Name, "--room", room, "--daemon", "--owner", owner, "--resolved-label", o.Label, "--resolved-preset-file", presetFile}
```

`encoding/json` is still used by the helpers, so keep the import.

In `internal/cli/host.go` `cmdServe`:
- rename the `resolved` variable to `presetFile`;
- register `fs.StringVar(&presetFile, "resolved-preset-file", "", "internal: private file holding the resolved preset")` and drop `--resolved-preset`;
- compute `room` before resolving the preset. Move the `room, err := filepath.Abs(h.room)` block above the `var p host.Preset` block;
- replace the unmarshal branch with:

```go
	var p host.Preset
	if presetFile != "" {
		if p, err = host.ReadPresetFile(room, h.name, presetFile); err != nil {
			fmt.Fprintf(stderr, "tincan serve: %v\n", err)
			return ExitError
		}
	} else {
```

The `json` import in `internal/cli/host.go` stays, because `cmdPresets` uses it.

In `PROTOCOL.md` "Files under the room", add a line for `.tincan/hosts/config/<name>/<owner>.json`: "a launch's resolved preset (0600), read and removed by the daemon at startup".

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/host ./internal/cli ./internal/mcpserver ./internal/dispatch`
Expected: PASS.

- [ ] **Step 5: Run the whole suite, vet and format**

Run: `gofmt -l . ; go vet ./... && go test ./...`
Expected: no gofmt output, and every package passes. `internal/thread/prompt_test.go` was not gofmt-clean before this plan (a deferred minor from phase 1). If it is the only file listed, run `gofmt -w internal/thread/prompt_test.go` and include it in this commit.

- [ ] **Step 6: Commit**

```bash
git add internal/host/lifecycle.go internal/host/lifecycle_test.go internal/cli/host.go internal/cli/host_test.go PROTOCOL.md
git commit -m "Hand the resolved preset to serve through a private file"
```
