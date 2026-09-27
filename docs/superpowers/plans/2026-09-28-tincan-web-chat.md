# tincan web chat Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `tincan web`, a tailnet-only chat UI where the owner talks to tincan-hosted agents in per-project threads, @mentions pull agents in, agents hand work to each other within a chain budget, and one hub page also shows a peer machine.

**Architecture:** Threads are an append-only journal in each room (`.tincan/threads/<tid>/events.jsonl`) driven by a per-room dispatcher that turns @mentions into durable intents and submits them through a send path shared with the MCP server. `internal/web` serves a JSON API, SSE change notifications and an embedded plain-JS UI over a unix socket that `tailscale serve` exposes; a hub reverse-proxies a peer instance.

**Tech Stack:** Go 1.25 standard library (`net/http` pattern routing, `httputil.ReverseProxy`, `embed`, `html`), existing modules only (`github.com/fsnotify/fsnotify`), plain HTML/CSS/JS with no build step.

**Spec:** `docs/superpowers/specs/2026-09-28-tincan-web-chat-design.md`

## Global Constraints

- Go floor: `go 1.25.0` (go.mod); CI matrix Linux/macOS/Windows × Go 1.25.x and stable.
- No new module dependencies; UI has no build step and no framework.
- Existing CLI, MCP tool and skill behaviour must not change; existing tests pass unchanged.
- Thread IDs: 8 lowercase hex. Thread listener name: `<preset>.<tid>`; every generated name must pass `envelope.ValidComponent`.
- Request ID for a turn: `<tid>-<message id>`; message ID: `m` + 12 lowercase hex.
- Mention syntax: `@name`, name `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`, `@` at start or after whitespace, `(`, `[`, `,`; ignored inside fenced or inline code; `@you` reserved and never dispatched.
- Prompt limits: total 256 KiB UTF-8; header ≤ 2 KiB; trigger message cut at 128 KiB; posted text ≤ 128 KiB.
- Default chain budget 6 automatic executions per user message (user's own targets do not count); `--idle-stop 30m`; reconcile every 2 s.
- Security: default listen `unix:$XDG_RUNTIME_DIR/tincan/web.sock` (fallback `<state dir>/web.sock`), dir 0700, socket 0600; TCP only on loopback; every request needs `Tailscale-User-Login` == owner; POST/PATCH need `X-Tincan-Request: 1` and a matching `Origin` when present; CSP `default-src 'self'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`.
- Registry: `$TINCAN_STATE_DIR` > `$XDG_STATE_HOME/tincan` > `%LocalAppData%\tincan` (Windows) > `~/.local/state/tincan`; file `rooms.json`; room ID = first 12 hex of SHA-256 of the canonical path. Registry failures never fail a CLI command.
- Tests that start detached hosts skip on Windows (`runtime.GOOS == "windows"`), as existing tests do.

## Review Focus

- Agent reply containing HTML or `<script>` must render as inert text in the UI — pinned in Task 9 (`TestRenderMarkdownEscapesHTML`).
- Room paths with spaces and non-ASCII characters (e.g. `…/Application Support/Claude/scratch-…`) must register, list and dispatch — pinned in Task 1 (`TestRegistryHandlesSpacesAndUnicode`) and Task 6 (`TestDispatchInRoomWithSpaces`).
- A room directory deleted while `tincan web` runs must not break the loop or other rooms — pinned in Task 10 (`TestLoopSkipsMissingRoom`).
- An agent that prints nothing must produce a `done` message with empty text, not a stuck `running` card — pinned in Task 6 (`TestEmptyReplyCompletes`).
- Creating a thread whose primary agent is not available on the machine must fail with a clear 400, not create a dead thread — pinned in Task 9 (`TestCreateThreadRejectsUnavailablePrimary`).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/rooms/registry.go` | Room registry: state dir, canonical IDs, touch/add/hide/list/import, `TooBroad`. |
| `internal/rooms/registry_test.go` | Registry tests. |
| `internal/cli/mcp.go` (modify) | `inferRoom` uses `rooms.TooBroad`; `Touch` on start. |
| `internal/cli/cli.go`, `internal/cli/host.go` (modify) | `Touch` from `send`, `ask`, `recv`, `up`, `serve`; usage text. |
| `internal/cli/host_test.go`, `internal/mcpserver/server_test.go` (modify) | Isolate registry with `TINCAN_STATE_DIR`. |
| `internal/dispatch/dispatch.go` | Shared launch/send path (name and preset separate). |
| `internal/dispatch/dispatch_test.go` | Launch/send tests. |
| `internal/mcpserver/server.go` (modify) | Call `internal/dispatch`. |
| `internal/thread/model.go` | Event/Message/Meta types, constants, folding. |
| `internal/thread/store.go` | Thread create/open/list, locked journal transactions, prompt files. |
| `internal/thread/mention.go` | Mention parsing. |
| `internal/thread/resolve.go` | Target resolution. |
| `internal/thread/prompt.go` | Prompt building. |
| `internal/thread/dispatcher.go` | Post, reconcile (submit/collect/handoffs), budget. |
| `internal/thread/control.go` | Stop, archive, retry, janitor. |
| `internal/thread/*_test.go` | Unit and integration tests (fixture agent). |
| `internal/web/server.go` | Config, listener, middleware, routes, background loop. |
| `internal/web/owner.go` | Tailscale owner detection. |
| `internal/web/api.go` | JSON handlers. |
| `internal/web/markdown.go` | Safe Markdown-subset renderer. |
| `internal/web/events.go` | SSE hub and change scanner/watcher. |
| `internal/web/proxy.go` | Peer proxy and health. |
| `internal/web/ui/index.html`, `app.js`, `app.css` | Embedded UI. |
| `internal/web/*_test.go` | Web tests. |
| `internal/cli/web.go` | `tincan web` command. |
| `docs/web.md`, `deploy/systemd/tincan-web.service`, `deploy/launchd/net.tincan.web.plist` | Docs and service files. |
| `README.md`, `PROTOCOL.md`, `docs/README.md` (modify) | Links and command reference. |

---

### Task 1: Room registry

**Files:**
- Create: `internal/rooms/registry.go`, `internal/rooms/registry_test.go`
- Modify: `internal/cli/mcp.go` (`inferRoom`, `cmdMCP`), `internal/cli/cli.go` (`cmdSend`, `cmdRecv`, `cmdAsk`), `internal/cli/host.go` (`cmdServe`, `cmdUp`), `internal/cli/host_test.go` (`TestMain`), `internal/mcpserver/server_test.go` (`TestMain`)

**Interfaces:**
- Produces:
  - `type rooms.Room struct { ID, Path, Name string; Hidden bool; LastUsed time.Time; Missing bool }`
  - `func rooms.StateDir() string`
  - `func rooms.Default() *rooms.Registry`, `func rooms.Open(path string) *rooms.Registry`
  - `func (r *Registry) Touch(room string) error`, `List() ([]Room, error)`, `Get(id string) (Room, bool, error)`, `Add(dir string) (Room, error)`, `SetHidden(id string, hidden bool) error`, `Import(dirs []string, depth int) (int, error)`
  - `func rooms.ID(canonical string) string`, `func rooms.Canonical(dir string) (string, error)`, `func rooms.TooBroad(dir string) bool`, `func rooms.TouchQuiet(room string, stderr io.Writer)`

- [ ] **Step 1: Write the failing tests**

```go
// internal/rooms/registry_test.go
package rooms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempRegistry(t *testing.T) *Registry {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), "state", "rooms.json"))
}

func mkroom(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{t.TempDir()}, parts...)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTouchIsIdempotentAndStableID(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "proj")
	for i := 0; i < 2; i++ {
		if err := r.Touch(room); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Path != room || list[0].ID != ID(room) || list[0].Name != "proj" {
		t.Fatalf("list = %+v", list)
	}
	if len(ID(room)) != 12 {
		t.Fatalf("ID length %d", len(ID(room)))
	}
}

func TestListDisambiguatesDuplicateBaseNames(t *testing.T) {
	r := tempRegistry(t)
	a := mkroom(t, "one", "app")
	b := mkroom(t, "two", "app")
	r.Touch(a)
	r.Touch(b)
	list, _ := r.List()
	names := []string{list[0].Name, list[1].Name}
	if names[0] == names[1] || !strings.HasSuffix(names[0], "/app") || !strings.HasSuffix(names[1], "/app") {
		t.Fatalf("names not disambiguated: %q", names)
	}
}

func TestAddRejectsHomeRootAndFiles(t *testing.T) {
	r := tempRegistry(t)
	home := mkroom(t, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, err := r.Add(home); err == nil {
		t.Fatal("home accepted")
	}
	if _, err := r.Add(string(filepath.Separator)); err == nil {
		t.Fatal("filesystem root accepted")
	}
	file := filepath.Join(mkroom(t, "x"), "f")
	os.WriteFile(file, nil, 0o600)
	if _, err := r.Add(file); err == nil {
		t.Fatal("file accepted")
	}
}

func TestSetHiddenAndGet(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "proj")
	got, err := r.Add(room)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetHidden(got.ID, true); err != nil {
		t.Fatal(err)
	}
	g, ok, err := r.Get(got.ID)
	if err != nil || !ok || !g.Hidden {
		t.Fatalf("Get = %+v %v %v", g, ok, err)
	}
	if err := r.SetHidden("000000000000", true); err == nil {
		t.Fatal("unknown id accepted")
	}
}

func TestImportFindsTincanDirs(t *testing.T) {
	r := tempRegistry(t)
	root := mkroom(t, "projects")
	for _, p := range []string{"a/.tincan", "games/b/.tincan", "deep/x/y/z/.tincan", "node_modules/c/.tincan"} {
		os.MkdirAll(filepath.Join(root, p), 0o700)
	}
	n, err := r.Import([]string{root}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("imported %d, want 2 (a, games/b)", n)
	}
}

func TestRegistryHandlesSpacesAndUnicode(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "Application Support", "scratch-ü 1")
	if err := r.Touch(room); err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.Get(ID(room))
	if err != nil || !ok || got.Path != room || got.Name != "scratch-ü 1" {
		t.Fatalf("Get = %+v %v %v", got, ok, err)
	}
}

func TestListMarksMissingRooms(t *testing.T) {
	r := tempRegistry(t)
	room := mkroom(t, "gone")
	r.Touch(room)
	os.RemoveAll(room)
	list, _ := r.List()
	if len(list) != 1 || !list[0].Missing {
		t.Fatalf("list = %+v", list)
	}
}

func TestEmptyPathRegistryIsNoop(t *testing.T) {
	r := Open("")
	if err := r.Touch(t.TempDir()); err != nil {
		t.Fatalf("Touch without state dir: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/rooms/ -count=1`
Expected: FAIL — package has no non-test Go files / undefined: `Open`.

- [ ] **Step 3: Implement the registry**

```go
// internal/rooms/registry.go

// Package rooms records the project rooms tincan has been used in, so the web
// UI can list them. The registry is advisory: failures never fail a command.
package rooms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

type Room struct {
	ID       string    `json:"id"`
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Hidden   bool      `json:"hidden,omitempty"`
	LastUsed time.Time `json:"last_used"`
	Missing  bool      `json:"missing,omitempty"` // computed by List, never stored
}

type file struct {
	Rooms []Room `json:"rooms"`
}

// StateDir is where per-user tincan state lives; "" when it cannot be found.
func StateDir() string {
	if d := os.Getenv("TINCAN_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tincan")
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LocalAppData"); d != "" {
			return filepath.Join(d, "tincan")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "tincan")
}

type Registry struct{ path string }

func Open(path string) *Registry { return &Registry{path: path} }

func Default() *Registry {
	d := StateDir()
	if d == "" {
		return Open("")
	}
	return Open(filepath.Join(d, "rooms.json"))
}

func ID(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:12]
}

func Canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// TooBroad reports whether a canonical directory is a filesystem root, the
// home directory, or one of its ancestors: never a project room.
func TooBroad(dir string) bool {
	if filepath.Dir(dir) == dir {
		return true
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return false
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	rel, err := filepath.Rel(dir, home)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TouchQuiet records room in the default registry, reporting (not returning)
// any failure.
func TouchQuiet(room string, stderr io.Writer) {
	if err := Default().Touch(room); err != nil {
		fmt.Fprintf(stderr, "tincan: room registry: %v\n", err)
	}
}

func (r *Registry) read() (file, error) {
	var f file
	data, err := os.ReadFile(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", r.path, err)
	}
	return f, nil
}

func (r *Registry) update(fn func(*file) error) error {
	if err := fsutil.MkdirPrivate(filepath.Dir(r.path)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := filelock.Acquire(ctx, r.path+".lock")
	if err != nil {
		return err
	}
	defer l.Close()
	f, err := r.read()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(r.path, append(data, '\n'))
}

func upsert(f *file, path string, used time.Time) {
	id := ID(path)
	for i := range f.Rooms {
		if f.Rooms[i].ID == id {
			if used.After(f.Rooms[i].LastUsed) {
				f.Rooms[i].LastUsed = used
			}
			return
		}
	}
	f.Rooms = append(f.Rooms, Room{ID: id, Path: path, LastUsed: used})
}

func (r *Registry) Touch(room string) error {
	if r.path == "" {
		return nil
	}
	path, err := Canonical(room)
	if err != nil {
		return err
	}
	return r.update(func(f *file) error { upsert(f, path, time.Now().UTC()); return nil })
}

func (r *Registry) Add(dir string) (Room, error) {
	path, err := Canonical(dir)
	if err != nil {
		return Room{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Room{}, err
	}
	if !info.IsDir() {
		return Room{}, fmt.Errorf("%s is not a directory", path)
	}
	if TooBroad(path) {
		return Room{}, fmt.Errorf("%s is the home directory, an ancestor of it, or a filesystem root", path)
	}
	if r.path == "" {
		return Room{}, errors.New("no state directory for the room registry")
	}
	if err := r.update(func(f *file) error { upsert(f, path, time.Now().UTC()); return nil }); err != nil {
		return Room{}, err
	}
	got, _, err := r.Get(ID(path))
	return got, err
}

func (r *Registry) SetHidden(id string, hidden bool) error {
	return r.update(func(f *file) error {
		for i := range f.Rooms {
			if f.Rooms[i].ID == id {
				f.Rooms[i].Hidden = hidden
				return nil
			}
		}
		return fmt.Errorf("unknown room %q", id)
	})
}

func (r *Registry) List() ([]Room, error) {
	if r.path == "" {
		return nil, nil
	}
	f, err := r.read()
	if err != nil {
		return nil, err
	}
	out := append([]Room(nil), f.Rooms...)
	counts := map[string]int{}
	for _, room := range out {
		counts[filepath.Base(room.Path)]++
	}
	for i := range out {
		base := filepath.Base(out[i].Path)
		out[i].Name = base
		if counts[base] > 1 {
			out[i].Name = filepath.Base(filepath.Dir(out[i].Path)) + "/" + base
		}
		if info, err := os.Stat(out[i].Path); err != nil || !info.IsDir() {
			out[i].Missing = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *Registry) Get(id string) (Room, bool, error) {
	list, err := r.List()
	if err != nil {
		return Room{}, false, err
	}
	for _, room := range list {
		if room.ID == id {
			return room, true, nil
		}
	}
	return Room{}, false, nil
}

// Import registers every directory containing a .tincan directory at most
// depth levels below each of dirs. node_modules and dot-directories are skipped.
func (r *Registry) Import(dirs []string, depth int) (int, error) {
	var found []string
	for _, dir := range dirs {
		root, err := Canonical(dir)
		if err != nil {
			continue
		}
		base := strings.Count(root, string(filepath.Separator))
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			name := d.Name()
			if p != root && (name == "node_modules" || (strings.HasPrefix(name, ".") && name != ".tincan")) {
				return fs.SkipDir
			}
			if name == ".tincan" {
				parent := filepath.Dir(p)
				if !TooBroad(parent) {
					found = append(found, parent)
				}
				return fs.SkipDir
			}
			if strings.Count(p, string(filepath.Separator))-base >= depth {
				return fs.SkipDir
			}
			return nil
		})
	}
	if len(found) == 0 || r.path == "" {
		return 0, nil
	}
	added := 0
	err := r.update(func(f *file) error {
		for _, p := range found {
			before := len(f.Rooms)
			info, _ := os.Stat(filepath.Join(p, ".tincan"))
			used := time.Time{}
			if info != nil {
				used = info.ModTime().UTC()
			}
			upsert(f, p, used)
			if len(f.Rooms) > before {
				added++
			}
		}
		return nil
	})
	return added, err
}
```

Note on `Import` depth: a `.tincan` directory is found when its **parent** is at most `depth` levels below the scan root (`a` = 1, `games/b` = 2); `deep/x/y/z` = 4 is skipped.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/rooms/ -count=1 -v`
Expected: PASS for all eight tests.

- [ ] **Step 5: Wire `TooBroad` and `Touch` into the CLI**

In `internal/cli/mcp.go` replace the body of `inferRoom`'s local `tooBroad` closure with the shared helper (keep the cwd canonicalization):

```go
func inferRoom() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if c, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = c
	}
	if rooms.TooBroad(cwd) {
		return "", fmt.Errorf("cannot infer a room from working directory %s; start the client in a project directory or pass --room /absolute/project", cwd)
	}
	for dir := cwd; !rooms.TooBroad(dir); dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
	}
	return cwd, nil
}
```

In `cmdMCP`, after `*room` is final and before `mcpserver.New`: `rooms.TouchQuiet(*room, stderr)`.
In `cmdSend`, `cmdRecv`, `cmdAsk` (`internal/cli/cli.go`), immediately after each `if w := roomRootWarning(*room); w != "" { … }` block: `rooms.TouchQuiet(*room, stderr)`.
In `cmdServe` and `cmdUp` (`internal/cli/host.go`), immediately after their `roomRootWarning(room)` block: `rooms.TouchQuiet(room, stderr)`.
Add the import `"github.com/c0ze/tincan/v2/internal/rooms"` to each file. Remove the now-unused `strings` import from `mcp.go` if the compiler reports it.

Isolate tests from the real registry. In `internal/cli/host_test.go` `TestMain`, before `os.Exit(m.Run())`:

```go
	state, err := os.MkdirTemp("", "tincan-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("TINCAN_STATE_DIR", state)
	code := m.Run()
	os.RemoveAll(state)
	os.Exit(code)
```

Apply the same block in `internal/mcpserver/server_test.go` `TestMain` in place of `os.Exit(m.Run())`.

- [ ] **Step 6: Run the full suite**

Run: `go vet ./... && go test ./... -count=1`
Expected: PASS; `internal/cli` InferRoom tests still pass.

- [ ] **Step 7: Commit**

```bash
git add internal/rooms internal/cli internal/mcpserver/server_test.go
git commit -m "Add room registry and record rooms from CLI and MCP"
```

---

### Task 2: Shared send path

**Files:**
- Create: `internal/dispatch/dispatch.go`, `internal/dispatch/dispatch_test.go`
- Modify: `internal/mcpserver/server.go` (`presets`, `launch`, `sendTool`)

**Interfaces:**
- Consumes: `host.Existing`, `host.Resolve`, `host.WithSession`, `host.Up`, `request.*` (existing).
- Produces:
  - `type dispatch.Options struct { Room, Executable string; Presets map[string]host.Preset }`
  - `func (o Options) PresetMap() (map[string]host.Preset, error)`
  - `type dispatch.LaunchResult struct { State host.State; Already bool }`
  - `func dispatch.Launch(ctx context.Context, o Options, name, preset, sessionMode string) (LaunchResult, error)`
  - `type dispatch.SendSpec struct { Agent, Preset, From, Body, RequestID string }`
  - `func dispatch.Send(ctx context.Context, o Options, s SendSpec) (request.Record, error)`

- [ ] **Step 1: Write the failing tests**

```go
// internal/dispatch/dispatch_test.go
package dispatch_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/cli"
	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
		case "fixture":
			fmt.Print(strings.ToUpper(os.Args[2]))
			os.Exit(0)
		}
	}
	state, _ := os.MkdirTemp("", "tincan-state-")
	os.Setenv("TINCAN_STATE_DIR", state)
	code := m.Run()
	os.RemoveAll(state)
	os.Exit(code)
}

func opts(t *testing.T) dispatch.Options {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts are not supported on Windows")
	}
	exe, _ := os.Executable()
	room, _ := filepath.EvalSymlinks(t.TempDir())
	fixture := host.Preset{Exec: []string{exe, "fixture", "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 30}
	return dispatch.Options{Room: room, Presets: map[string]host.Preset{"fixture": fixture}}
}

func TestSendLaunchesNamedListenerWithSeparatePreset(t *testing.T) {
	o := opts(t)
	name := "fixture.t0000001"
	t.Cleanup(func() { host.Down(context.Background(), o.Room, name, 5*time.Second) })
	r, err := dispatch.Send(context.Background(), o, dispatch.SendSpec{Agent: name, Preset: "fixture", From: "web", Body: "hi", RequestID: "t0000001-m1"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = request.Wait(context.Background(), o.Room, r.ID, 20*time.Second)
	if err != nil || r.Result != "HI" {
		t.Fatalf("result %+v %v", r, err)
	}
	st, ok := host.Existing(context.Background(), o.Room, name)
	if !ok || st.Preset != "fixture" {
		t.Fatalf("listener state %+v %v", st, ok)
	}
}

func TestSendSameIDAndBodyDoesNotExecuteTwice(t *testing.T) {
	o := opts(t)
	name := "fixture.t0000002"
	t.Cleanup(func() { host.Down(context.Background(), o.Room, name, 5*time.Second) })
	spec := dispatch.SendSpec{Agent: name, Preset: "fixture", From: "web", Body: "once", RequestID: "t0000002-m1"}
	first, err := dispatch.Send(context.Background(), o, spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatch.Send(context.Background(), o, spec)
	if err != nil || second.ID != first.ID {
		t.Fatalf("second send %+v %v", second, err)
	}
	spec.Body = "different"
	if _, err := dispatch.Send(context.Background(), o, spec); err == nil {
		t.Fatal("same ID with a different body was accepted")
	}
}

func TestLaunchUnknownPresetFails(t *testing.T) {
	o := opts(t)
	if _, err := dispatch.Launch(context.Background(), o, "x.t0000003", "nope", ""); err == nil {
		t.Fatal("unknown preset accepted")
	}
}

func TestLaunchThreadListenerUsesPresetSessionMode(t *testing.T) {
	o := opts(t)
	// A fake executable named like a session-capable provider selects the
	// persistent adapter; each thread listener has its own session file.
	dir := t.TempDir()
	exe, _ := os.Executable()
	claude := filepath.Join(dir, "claude")
	if err := os.Symlink(exe, claude); err != nil {
		t.Skip(err)
	}
	o.Presets["claude"] = host.Preset{Exec: []string{claude, "fixture", "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 30}
	name := "claude.t0000004"
	t.Cleanup(func() { host.Down(context.Background(), o.Room, name, 5*time.Second) })
	res, err := dispatch.Launch(context.Background(), o, name, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.State.Session != "persistent" || res.State.Preset != "claude" {
		t.Fatalf("state %+v", res.State)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dispatch/ -count=1`
Expected: FAIL — undefined: `dispatch.Options`.

- [ ] **Step 3: Implement `internal/dispatch`**

```go
// internal/dispatch/dispatch.go

// Package dispatch is the single path by which tincan front ends (the MCP
// server and the web chat) submit work and start listeners, so routing,
// interactive-listener detection and idempotency stay identical.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
)

type Options struct {
	Room       string                 // canonical room directory
	Executable string                 // tincan executable for detached hosts; "" = current
	Presets    map[string]host.Preset // nil loads the user's effective presets
}

func (o Options) PresetMap() (map[string]host.Preset, error) {
	if o.Presets != nil {
		return o.Presets, nil
	}
	return host.Effective(host.ConfigPath())
}

type LaunchResult struct {
	State   host.State
	Already bool
}

// Launch starts or reconnects listener name. preset selects the configuration
// ("" means the name itself, or the live listener's own preset).
func Launch(ctx context.Context, o Options, name, preset, sessionMode string) (LaunchResult, error) {
	if err := spool.ValidName(name); err != nil {
		return LaunchResult{}, err
	}
	if st, alive := host.Existing(ctx, o.Room, name); alive && preset == "" {
		if sessionMode == "" {
			return LaunchResult{State: st, Already: true}, nil
		}
		// An alias keeps its existing provider when only the session mode is
		// specified. Up still rejects changing a live configuration.
		if st.Preset != "" {
			preset = st.Preset
		}
	}
	all, err := o.PresetMap()
	if err != nil {
		return LaunchResult{}, err
	}
	label, p, err := host.Resolve(all, name, preset, host.Overrides{ExecTimeoutSec: -1})
	if err != nil {
		return LaunchResult{}, err
	}
	p, err = host.WithSession(p, label, sessionMode)
	if err != nil {
		return LaunchResult{}, err
	}
	res, err := host.Up(ctx, host.UpOptions{Room: o.Room, Name: name, Label: label, Preset: p, Wait: 10 * time.Second, Executable: o.Executable})
	if err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{State: res.State, Already: res.Already}, nil
}

type SendSpec struct {
	Agent     string // listener name
	Preset    string // preset used if the listener must be launched; "" = Agent
	From      string // sender name; "" = "mcp"
	Body      string
	RequestID string // optional idempotency key
}

// Send submits a durable request and launches its hosted listener when absent.
// A saved request stays retrievable when the launch fails.
func Send(ctx context.Context, o Options, s SendSpec) (request.Record, error) {
	if s.From == "" {
		s.From = "mcp"
	}
	if err := spool.ValidName(s.Agent); err != nil {
		return request.Record{}, err
	}
	if err := spool.ValidName(s.From); err != nil {
		return request.Record{}, err
	}
	if s.RequestID != "" {
		if err := request.ValidateID(s.RequestID); err != nil {
			return request.Record{}, err
		}
	}
	if len(s.Body) > request.MaxBodyBytes {
		return request.Record{}, fmt.Errorf("prompt exceeds %d bytes", request.MaxBodyBytes)
	}
	// A cached result must remain retrievable even when the worker is stopped,
	// uninstalled, or named with an alias that is not itself a preset.
	if s.RequestID != "" {
		existing, err := request.Get(o.Room, s.RequestID)
		if err == nil && existing.Terminal() {
			return request.Submit(ctx, o.Room, s.Agent, s.From, s.Body, s.RequestID)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return request.Record{}, err
		}
	}
	route, err := request.LockRoute(ctx, o.Room, s.Agent)
	if err != nil {
		return request.Record{}, err
	}
	defer route.Close()
	state, alive := host.Existing(ctx, o.Room, s.Agent)
	interactive := alive && state.Owner == ""
	if !alive {
		interactive, err = request.InteractivePending(ctx, o.Room, s.Agent)
		if err != nil {
			return request.Record{}, err
		}
	}
	submit := request.Submit
	if interactive {
		submit = request.SubmitInteractive
	}
	r, err := submit(ctx, o.Room, s.Agent, s.From, s.Body, s.RequestID)
	if err != nil {
		return r, err
	}
	if !r.Terminal() && !alive && !interactive {
		if _, err := Launch(ctx, o, s.Agent, s.Preset, ""); err != nil {
			return r, fmt.Errorf("request %s is saved; launch its listener or retry this same request_id: %w", r.ID, err)
		}
	}
	return r, nil
}
```

- [ ] **Step 4: Point the MCP server at it**

In `internal/mcpserver/server.go`:

```go
func (s *service) opts() dispatch.Options {
	return dispatch.Options{Room: s.Room, Executable: s.Executable, Presets: s.Presets}
}

func (s *service) presets() (map[string]host.Preset, error) { return s.opts().PresetMap() }

func agentView(name string, st host.State) AgentView {
	return AgentView{Name: name, Preset: st.Preset, Alive: true, Busy: st.State == "busy", PID: st.PID, CurrentRequest: st.CurrentID, SessionMode: st.Session}
}

func (s *service) launch(ctx context.Context, in LaunchInput) (LaunchOutput, error) {
	res, err := dispatch.Launch(ctx, s.opts(), in.Name, in.Preset, in.SessionMode)
	if err != nil {
		return LaunchOutput{}, err
	}
	return LaunchOutput{Agent: agentView(in.Name, res.State), Already: res.Already}, nil
}
```

Replace the body of `sendTool` after its signature with:

```go
	r, err := dispatch.Send(ctx, s.opts(), dispatch.SendSpec{Agent: in.Agent, From: in.From, Body: in.Body, RequestID: in.RequestID})
	if r.ID == "" {
		return nil, RequestView{}, err
	}
	return nil, view(r), err
```

Remove imports that become unused (`errors`, `os` if unused). `view(r)` must not be called with a zero record, hence the `r.ID` guard, matching the old behaviour of returning `RequestView{}` on validation errors.

- [ ] **Step 5: Run tests**

Run: `go vet ./... && go test ./internal/dispatch/ ./internal/mcpserver/ -race -count=1`
Expected: PASS; all pre-existing `internal/mcpserver` tests pass unchanged.

- [ ] **Step 6: Commit**

```bash
git add internal/dispatch internal/mcpserver/server.go
git commit -m "Extract shared launch and send path from the MCP server"
```

---

### Task 3: Thread store and journal

**Files:**
- Create: `internal/thread/model.go`, `internal/thread/store.go`, `internal/thread/store_test.go`

**Interfaces:**
- Produces:
  - Constants: `StatusOpen, StatusStopping, StatusArchiving, StatusArchived`; `KindMessage, KindIntent, KindState, KindHandoffs, KindChain, KindThread`; `RoleUser, RoleAgent, RoleSystem`; `StatePending, StateRunning, StateDone, StateError, StateCancelled, StateSuggested, StateUncollectable`; `OpReserve, OpStop`.
  - `type Meta struct { ID, Title, Primary, Status, ClientID string; Created time.Time; Budget int; Listeners map[string]string }` (listener name → preset)
  - `type Event struct` (fields below), `type Message struct` (fields below), `type Chain struct { Used int; Stopped bool }`, `type Snapshot struct { Meta Meta; Messages []Message; Chains map[string]*Chain; MaxSeq, NextN int64 }`
  - `func (s *Snapshot) Message(id string) (Message, bool)`, `func (s *Snapshot) ByClientID(id string) (Message, bool)`
  - `func Root(room string) string`
  - `func Create(room, title, primary, clientID string, budget int) (*Thread, error)`, `func Open(room, id string) (*Thread, error)`, `func List(room string) ([]Meta, error)`
  - `func (t *Thread) Snapshot() (Snapshot, error)`, `func (t *Thread) Update(ctx context.Context, fn func(*Tx) error) error`
  - `type Tx struct { Meta Meta; Snap *Snapshot }`, `func (tx *Tx) Append(e Event) Event`, `func (tx *Tx) SaveMeta()`
  - `func (t *Thread) WritePrompt(requestID, prompt string) error`, `func (t *Thread) ReadPrompt(requestID string) (string, error)`
  - `var ErrNotFound`

- [ ] **Step 1: Write the failing tests**

```go
// internal/thread/store_test.go
package thread

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func room(t *testing.T) string {
	t.Helper()
	r, _ := filepath.EvalSymlinks(t.TempDir())
	return r
}

func TestCreateOpenList(t *testing.T) {
	r := room(t)
	th, err := Create(r, "audit", "claude", "c1", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.ID) != 8 {
		t.Fatalf("id %q", th.ID)
	}
	if _, err := Open(r, th.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r, "../../etc"); err == nil {
		t.Fatal("invalid id accepted")
	}
	list, err := List(r)
	if err != nil || len(list) != 1 || list[0].Title != "audit" || list[0].Status != StatusOpen || list[0].Budget != 6 {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestAppendAssignsSeqAndDisplayOrder(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	var ids []string
	err := th.Update(context.Background(), func(tx *Tx) error {
		ids = append(ids, tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "a"}).ID)
		ids = append(ids, tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "b"}).ID)
		tx.Append(Event{Kind: KindState, Message: ids[0], State: StateDone, Text: "a2"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := th.Snapshot()
	if snap.MaxSeq != 3 || len(snap.Messages) != 2 {
		t.Fatalf("snap %+v", snap)
	}
	if snap.Messages[0].ID != ids[0] || snap.Messages[0].Text != "a2" || snap.Messages[0].Seq != 3 || snap.Messages[1].N != 2 {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestConcurrentUpdatesKeepSeqStrict(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				th2, _ := Open(th.Room, th.ID) // separate handle, as another process would have
				th2.Update(context.Background(), func(tx *Tx) error {
					tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "x"})
					return nil
				})
			}
		}()
	}
	wg.Wait()
	evs, err := th.events()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 80 {
		t.Fatalf("%d events", len(evs))
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
}

func TestTornTailIsIgnoredThenRepaired(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	th.Update(context.Background(), func(tx *Tx) error {
		tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "whole"})
		return nil
	})
	f, _ := os.OpenFile(th.eventsPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"seq":2,"kind":"mess`)
	f.Close()
	snap, err := th.Snapshot()
	if err != nil || len(snap.Messages) != 1 {
		t.Fatalf("torn tail not ignored: %+v %v", snap, err)
	}
	if err := th.Update(context.Background(), func(tx *Tx) error {
		tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "next"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap, err = th.Snapshot()
	if err != nil || len(snap.Messages) != 2 || snap.Messages[1].Seq != 2 {
		t.Fatalf("after repair: %+v %v", snap.Messages, err)
	}
}

func TestClientIDLookupAndMetaSave(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	th.Update(context.Background(), func(tx *Tx) error {
		tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: "x", ClientID: "k1"})
		tx.Meta.Status = StatusStopping
		tx.SaveMeta()
		return nil
	})
	snap, _ := th.Snapshot()
	if _, ok := snap.ByClientID("k1"); !ok || snap.Meta.Status != StatusStopping {
		t.Fatalf("snap %+v", snap)
	}
}

func TestPromptFilesRoundTrip(t *testing.T) {
	th, _ := Create(room(t), "t", "claude", "", 6)
	if err := th.WritePrompt("abc-m1", "hello ü"); err != nil {
		t.Fatal(err)
	}
	got, err := th.ReadPrompt("abc-m1")
	if err != nil || got != "hello ü" {
		t.Fatalf("%q %v", got, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/thread/ -count=1`
Expected: FAIL — undefined: `Create`.

- [ ] **Step 3: Implement the model**

```go
// internal/thread/model.go

// Package thread stores tincan web chat threads as append-only journals in
// the room and dispatches their agent turns.
package thread

import (
	"sort"
	"time"
)

const (
	StatusOpen      = "open"
	StatusStopping  = "stopping"
	StatusArchiving = "archiving"
	StatusArchived  = "archived"

	KindMessage  = "message"
	KindIntent   = "intent"
	KindState    = "state"
	KindHandoffs = "handoffs"
	KindChain    = "chain"
	KindThread   = "thread"

	RoleUser   = "user"
	RoleAgent  = "agent"
	RoleSystem = "system"

	StatePending       = "pending"
	StateRunning       = "running"
	StateDone          = "done"
	StateError         = "error"
	StateCancelled     = "cancelled"
	StateSuggested     = "suggested"
	StateUncollectable = "uncollectable"

	OpReserve = "reserve"
	OpStop    = "stop"
)

type Meta struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Primary   string            `json:"primary"`
	Created   time.Time         `json:"created"`
	Status    string            `json:"status"`
	Budget    int               `json:"budget"`
	ClientID  string            `json:"client_id,omitempty"`
	Listeners map[string]string `json:"listeners,omitempty"` // thread listener → preset
}

// Event is one journal line. Fields are used by kind:
// message: ID N Author Role Text ReplyTo Chain ClientID; intent: Message
// Listener Preset RequestID PromptSHA; state: Message State Text; handoffs:
// Message Produced; chain: Chain Op; thread: (none, marks a meta change).
type Event struct {
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	Time      time.Time `json:"time"`
	ID        string    `json:"id,omitempty"`
	N         int64     `json:"n,omitempty"`
	Author    string    `json:"author,omitempty"`
	Role      string    `json:"role,omitempty"`
	Text      string    `json:"text,omitempty"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Chain     string    `json:"chain,omitempty"`
	ClientID  string    `json:"client_id,omitempty"`
	Message   string    `json:"message,omitempty"`
	Listener  string    `json:"listener,omitempty"`
	Preset    string    `json:"preset,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	PromptSHA string    `json:"prompt_sha256,omitempty"`
	State     string    `json:"state,omitempty"`
	Produced  []string  `json:"produced,omitempty"`
	Op        string    `json:"op,omitempty"`
}

type Message struct {
	ID        string    `json:"id"`
	N         int64     `json:"n"`
	Seq       int64     `json:"seq"`
	Time      time.Time `json:"time"`
	Author    string    `json:"author"`
	Role      string    `json:"role"`
	Text      string    `json:"text"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Chain     string    `json:"chain,omitempty"`
	ClientID  string    `json:"client_id,omitempty"`
	State     string    `json:"state,omitempty"`
	Listener  string    `json:"listener,omitempty"`
	Preset    string    `json:"preset,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	Handoffs  bool      `json:"handoffs,omitempty"`
	Updated   time.Time `json:"updated"`
}

type Chain struct {
	Used    int  `json:"used"`
	Stopped bool `json:"stopped"`
}

type Snapshot struct {
	Meta     Meta
	Messages []Message
	Chains   map[string]*Chain
	MaxSeq   int64
	NextN    int64
	index    map[string]int
}

func newSnapshot(m Meta) *Snapshot {
	return &Snapshot{Meta: m, Chains: map[string]*Chain{}, NextN: 1, index: map[string]int{}}
}

func (s *Snapshot) apply(e Event) {
	if e.Seq > s.MaxSeq {
		s.MaxSeq = e.Seq
	}
	switch e.Kind {
	case KindMessage:
		s.index[e.ID] = len(s.Messages)
		s.Messages = append(s.Messages, Message{ID: e.ID, N: e.N, Seq: e.Seq, Time: e.Time, Author: e.Author, Role: e.Role, Text: e.Text, ReplyTo: e.ReplyTo, Chain: e.Chain, ClientID: e.ClientID, Updated: e.Time})
		if e.N >= s.NextN {
			s.NextN = e.N + 1
		}
	case KindIntent, KindState, KindHandoffs:
		i, ok := s.index[e.Message]
		if !ok {
			return
		}
		m := &s.Messages[i]
		m.Seq, m.Updated = e.Seq, e.Time
		switch e.Kind {
		case KindIntent:
			m.Listener, m.Preset, m.RequestID = e.Listener, e.Preset, e.RequestID
		case KindState:
			m.State = e.State
			if e.Text != "" || e.State == StateDone {
				m.Text = e.Text
			}
		case KindHandoffs:
			m.Handoffs = true
		}
	case KindChain:
		c := s.Chains[e.Chain]
		if c == nil {
			c = &Chain{}
			s.Chains[e.Chain] = c
		}
		switch e.Op {
		case OpReserve:
			c.Used++
		case OpStop:
			c.Stopped = true
		}
	}
}

func (s *Snapshot) sortMessages() {
	sort.SliceStable(s.Messages, func(i, j int) bool { return s.Messages[i].N < s.Messages[j].N })
	for i, m := range s.Messages {
		s.index[m.ID] = i
	}
}

func (s *Snapshot) Message(id string) (Message, bool) {
	i, ok := s.index[id]
	if !ok {
		return Message{}, false
	}
	return s.Messages[i], true
}

func (s *Snapshot) ByClientID(id string) (Message, bool) {
	for _, m := range s.Messages {
		if m.ClientID == id && m.Role == RoleUser {
			return m, true
		}
	}
	return Message{}, false
}

// Active reports agent messages that are not yet terminal.
func (m Message) Active() bool {
	return m.Role == RoleAgent && (m.State == StatePending || m.State == StateRunning)
}
```

- [ ] **Step 4: Implement the store**

```go
// internal/thread/store.go
package thread

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/request"
)

var ErrNotFound = errors.New("thread not found")

func Root(room string) string { return filepath.Join(room, ".tincan", "threads") }

type Thread struct {
	Room string
	ID   string
	dir  string
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func validTID(id string) bool {
	if len(id) != 8 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (t *Thread) metaPath() string   { return filepath.Join(t.dir, "thread.json") }
func (t *Thread) eventsPath() string { return filepath.Join(t.dir, "events.jsonl") }
func (t *Thread) lockPath() string   { return filepath.Join(t.dir, "lock") }

func Create(room, title, primary, clientID string, budget int) (*Thread, error) {
	id := randHex(4)
	t := &Thread{Room: room, ID: id, dir: filepath.Join(Root(room), id)}
	if err := fsutil.MkdirPrivate(t.dir); err != nil {
		return nil, err
	}
	m := Meta{ID: id, Title: title, Primary: primary, Created: time.Now().UTC(), Status: StatusOpen, Budget: budget, ClientID: clientID}
	if err := t.writeMeta(m); err != nil {
		return nil, err
	}
	return t, nil
}

func Open(room, id string) (*Thread, error) {
	if !validTID(id) {
		return nil, ErrNotFound
	}
	t := &Thread{Room: room, ID: id, dir: filepath.Join(Root(room), id)}
	if _, err := os.Stat(t.metaPath()); err != nil {
		return nil, ErrNotFound
	}
	return t, nil
}

func List(room string) ([]Meta, error) {
	entries, err := os.ReadDir(Root(room))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() || !validTID(e.Name()) {
			continue
		}
		t := &Thread{Room: room, ID: e.Name(), dir: filepath.Join(Root(room), e.Name())}
		m, err := t.readMeta()
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

func (t *Thread) readMeta() (Meta, error) {
	var m Meta
	data, err := os.ReadFile(t.metaPath())
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data, &m)
}

func (t *Thread) writeMeta(m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(t.metaPath(), append(data, '\n'))
}

// events reads complete journal lines; an unterminated final line (a torn
// write) is ignored. It returns the byte length of the complete prefix.
func (t *Thread) readEvents() ([]Event, int64, error) {
	data, err := os.ReadFile(t.eventsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	valid := int64(bytes.LastIndexByte(data, '\n') + 1)
	var out []Event
	for _, line := range bytes.Split(data[:valid], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, 0, fmt.Errorf("%s: corrupt journal line: %w", t.eventsPath(), err)
		}
		out = append(out, e)
	}
	return out, valid, nil
}

func (t *Thread) events() ([]Event, error) {
	evs, _, err := t.readEvents()
	return evs, err
}

func (t *Thread) load() (*Snapshot, int64, error) {
	m, err := t.readMeta()
	if err != nil {
		return nil, 0, err
	}
	evs, valid, err := t.readEvents()
	if err != nil {
		return nil, 0, err
	}
	s := newSnapshot(m)
	for _, e := range evs {
		s.apply(e)
	}
	s.sortMessages()
	return s, valid, nil
}

// Snapshot reads the thread without locking; it never sees a torn line.
func (t *Thread) Snapshot() (Snapshot, error) {
	s, _, err := t.load()
	if err != nil {
		return Snapshot{}, err
	}
	return *s, nil
}

type Tx struct {
	Meta     Meta
	Snap     *Snapshot
	appended []Event
	metaSave bool
	nextSeq  int64
}

// Append assigns seq and time (and N and ID for messages), applies the event
// to Snap so later logic in the same transaction sees it, and stages it.
func (tx *Tx) Append(e Event) Event {
	e.Seq = tx.nextSeq
	tx.nextSeq++
	e.Time = time.Now().UTC()
	if e.Kind == KindMessage {
		if e.ID == "" {
			e.ID = "m" + randHex(6)
		}
		e.N = tx.Snap.NextN
	}
	tx.Snap.apply(e)
	tx.appended = append(tx.appended, e)
	return e
}

// SaveMeta persists tx.Meta at commit and records a thread event.
func (tx *Tx) SaveMeta() {
	if !tx.metaSave {
		tx.metaSave = true
		tx.Append(Event{Kind: KindThread})
	}
}

// Update runs fn under the thread lock against a fresh snapshot and commits
// its staged events (after repairing any torn tail) and meta.
func (t *Thread) Update(ctx context.Context, fn func(*Tx) error) error {
	l, err := filelock.Acquire(ctx, t.lockPath())
	if err != nil {
		return err
	}
	defer l.Close()
	s, valid, err := t.load()
	if err != nil {
		return err
	}
	tx := &Tx{Meta: s.Meta, Snap: s, nextSeq: s.MaxSeq + 1}
	if err := fn(tx); err != nil {
		return err
	}
	if len(tx.appended) > 0 {
		if err := t.appendEvents(valid, tx.appended); err != nil {
			return err
		}
	}
	if tx.metaSave {
		return t.writeMeta(tx.Meta)
	}
	return nil
}

func (t *Thread) appendEvents(valid int64, evs []Event) error {
	var buf bytes.Buffer
	for _, e := range evs {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	f, err := fsutil.OpenFile(t.eventsPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > valid {
		if err := f.Truncate(valid); err != nil {
			return err
		}
	}
	if _, err := f.Seek(valid, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return err
	}
	return f.Sync()
}

func (t *Thread) promptPath(requestID string) (string, error) {
	if err := request.ValidateID(requestID); err != nil {
		return "", err
	}
	return filepath.Join(t.dir, "prompts", requestID+".txt"), nil
}

func (t *Thread) WritePrompt(requestID, prompt string) error {
	p, err := t.promptPath(requestID)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(p, []byte(prompt))
}

func (t *Thread) ReadPrompt(requestID string) (string, error) {
	p, err := t.promptPath(requestID)
	if err != nil {
		return "", err
	}
	data, err := fsutil.ReadFile(p, request.MaxBodyBytes)
	return string(data), err
}
```

Check `fsutil.OpenFile` and `fsutil.ReadFile` signatures before relying on them (`OpenFile(path string, flags int, mode os.FileMode) (*os.File, error)`, `ReadFile(path string, maxBytes int64) ([]byte, error)`); `request.MaxBodyBytes` is an untyped constant, so `ReadFile(p, request.MaxBodyBytes)` compiles.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/thread/ -race -count=1 -v`
Expected: PASS for all six tests.

- [ ] **Step 6: Commit**

```bash
git add internal/thread
git commit -m "Add thread journal store with locked appends and tail repair"
```

---

### Task 4: Mention parsing and target resolution

**Files:**
- Create: `internal/thread/mention.go`, `internal/thread/resolve.go`, `internal/thread/mention_test.go`

**Interfaces:**
- Consumes: `Meta` (Task 3), `host.Preset`, `host.State`, `host.ResolveExecutable`, `envelope.ValidComponent`.
- Produces:
  - `func ParseMentions(text string) []string` — distinct names in order of first appearance, `you` included (callers skip it).
  - `type Target struct { Mention, Listener, Preset string; Existing bool }`
  - `type Resolver struct { Room string; Presets map[string]host.Preset; Alive func(name string) (host.State, bool) }`
  - `func (r Resolver) Resolve(meta Meta, names []string) (targets []Target, unresolved []string)`

- [ ] **Step 1: Write the failing tests**

```go
// internal/thread/mention_test.go
package thread

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/host"
)

func TestParseMentions(t *testing.T) {
	cases := map[string][]string{
		"@codex review this":                    {"codex"},
		"hey @claude, then @codex.":             {"claude", "codex"},
		"(@grok) and [@agy]":                    {"grok", "agy"},
		"mail me at a@b.com":                    nil,
		"`@codex` in code":                      nil,
		"```\n@codex\n```\n@claude":             {"claude"},
		"@you note to self":                     {"you"},
		"@codex @codex twice":                   {"codex"},
		"@codex-audit: status?":                 {"codex-audit"},
		"@" + strings.Repeat("a", 70) + " long": {strings.Repeat("a", 64)},
	}
	for in, want := range cases {
		if got := ParseMentions(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseMentions(%q) = %q, want %q", in, got, want)
		}
	}
}

func testResolver(t *testing.T, alive map[string]host.State) Resolver {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	return Resolver{
		Room: dir,
		Presets: map[string]host.Preset{
			"claude":  {Exec: []string{bin}},
			"codex":   {Exec: []string{bin}},
			"missing": {Exec: []string{filepath.Join(dir, "not-installed")}},
		},
		Alive: func(name string) (host.State, bool) { st, ok := alive[name]; return st, ok },
	}
}

func TestResolveOrderAndCanonicalNames(t *testing.T) {
	r := testResolver(t, map[string]host.State{"codex-audit": {Preset: "codex", Owner: "o"}})
	meta := Meta{ID: "t7f2a9c1", Listeners: map[string]string{"claude.t7f2a9c1": "claude"}}
	got, unresolved := r.Resolve(meta, []string{"claude", "claude.t7f2a9c1", "codex-audit", "missing", "nobody", "you"})
	want := []Target{
		{Mention: "claude", Listener: "claude.t7f2a9c1", Preset: "claude"},
		{Mention: "codex-audit", Listener: "codex-audit", Preset: "codex", Existing: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(unresolved, []string{"missing", "nobody"}) {
		t.Fatalf("unresolved %q", unresolved)
	}
}

func TestResolveIgnoresInteractiveListeners(t *testing.T) {
	r := testResolver(t, map[string]host.State{"alice": {Owner: ""}})
	got, unresolved := r.Resolve(Meta{ID: "t7f2a9c1"}, []string{"alice"})
	if len(got) != 0 || !reflect.DeepEqual(unresolved, []string{"alice"}) {
		t.Fatalf("got %+v unresolved %q", got, unresolved)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/thread/ -run 'Mention|Resolve' -count=1`
Expected: FAIL — undefined: `ParseMentions`.

- [ ] **Step 3: Implement parsing**

```go
// internal/thread/mention.go
package thread

import "strings"

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isNameByte(c byte) bool { return isNameStart(c) || c == '.' || c == '_' || c == '-' }

func mentionBoundary(prev byte) bool {
	return prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' || prev == '(' || prev == '[' || prev == ','
}

// stripCode blanks fenced blocks and inline code spans so mentions inside
// them are ignored; newlines are kept so boundaries stay intact.
func stripCode(text string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			b.WriteString("\n")
			continue
		}
		if inFence {
			b.WriteString("\n")
			continue
		}
		inCode := false
		for i := 0; i < len(line); i++ {
			if line[i] == '`' {
				inCode = !inCode
				b.WriteByte(' ')
				continue
			}
			if inCode && line[i] != '\n' {
				b.WriteByte(' ')
				continue
			}
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

func ParseMentions(text string) []string {
	s := stripCode(text)
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(s); i++ {
		if s[i] != '@' || (i > 0 && !mentionBoundary(s[i-1])) || i+1 >= len(s) || !isNameStart(s[i+1]) {
			continue
		}
		j := i + 1
		for j < len(s) && isNameByte(s[j]) {
			j++
		}
		name := strings.TrimRight(s[i+1:j], "._-")
		if len(name) > 64 {
			name = name[:64]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}
```

The trailing `.`, `,`, `:`, `;`, `)`, `]` rule is satisfied because `,:;)]` are not name bytes and trailing `._-` is trimmed.

- [ ] **Step 4: Implement resolution**

```go
// internal/thread/resolve.go
package thread

import (
	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/host"
)

type Target struct {
	Mention  string // as written
	Listener string // canonical listener name
	Preset   string
	Existing bool // an existing room listener, not owned by the thread
}

type Resolver struct {
	Room    string
	Presets map[string]host.Preset
	// Alive reports a live listener; hosted listeners have a non-empty Owner.
	Alive func(name string) (host.State, bool)
}

func (r Resolver) available(name string) bool {
	p, ok := r.Presets[name]
	if !ok || len(p.Exec) == 0 {
		return false
	}
	_, err := host.ResolveExecutable(p.Exec[0], r.Room)
	return err == nil
}

// Resolve maps mention names to canonical targets (spec §6.1). "you" is
// skipped silently; duplicates by canonical listener are dropped.
func (r Resolver) Resolve(meta Meta, names []string) ([]Target, []string) {
	var out []Target
	var unresolved []string
	seen := map[string]bool{}
	add := func(t Target) {
		if !seen[t.Listener] {
			seen[t.Listener] = true
			out = append(out, t)
		}
	}
	for _, name := range names {
		switch {
		case name == "you":
		case r.available(name):
			listener := name + "." + meta.ID
			if envelope.ValidComponent(listener) != nil {
				unresolved = append(unresolved, name)
				continue
			}
			add(Target{Mention: name, Listener: listener, Preset: name})
		case meta.Listeners[name] != "":
			add(Target{Mention: name, Listener: name, Preset: meta.Listeners[name]})
		default:
			if _, isPreset := r.Presets[name]; !isPreset && r.Alive != nil {
				if st, ok := r.Alive(name); ok && st.Owner != "" {
					add(Target{Mention: name, Listener: name, Preset: st.Preset, Existing: true})
					continue
				}
			}
			unresolved = append(unresolved, name)
		}
	}
	return out, unresolved
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/thread/ -run 'Mention|Resolve' -count=1 -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/thread/mention.go internal/thread/resolve.go internal/thread/mention_test.go
git commit -m "Parse thread mentions and resolve them to canonical listeners"
```

---

### Task 5: Prompt building

**Files:**
- Create: `internal/thread/prompt.go`, `internal/thread/prompt_test.go`

**Interfaces:**
- Consumes: `Message` (Task 3).
- Produces:
  - `const MaxPromptBytes = 256 << 10`, `const MaxTriggerBytes = 128 << 10`, `const MaxPostBytes = 128 << 10`
  - `type PromptInput struct { Listener, Title, Room string; Participants []string; Transcript []Message; Trigger Message }`
  - `func BuildPrompt(in PromptInput) string`
  - `func transcriptOf(s *Snapshot, excludeID string) []Message` (unexported helper used by Task 6)

- [ ] **Step 1: Write the failing tests**

```go
// internal/thread/prompt_test.go
package thread

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func msg(author, text string) Message { return Message{Author: author, Role: RoleAgent, Text: text, State: StateDone} }

func TestPromptContainsHeaderTranscriptAndTrigger(t *testing.T) {
	p := BuildPrompt(PromptInput{
		Listener: "codex.t7f2a9c1", Title: "audit", Room: "/w/app",
		Participants: []string{"claude.t7f2a9c1", "codex.t7f2a9c1"},
		Transcript:   []Message{{Author: "you", Role: RoleUser, Text: "fix it"}, msg("claude.t7f2a9c1", "fixed")},
		Trigger:      msg("claude.t7f2a9c1", "@codex review"),
	})
	for _, want := range []string{"You are codex.t7f2a9c1", "'audit'", "/w/app", "git diff", "@you", "you: fix it", "claude.t7f2a9c1: fixed", "claude.t7f2a9c1: @codex review"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestPromptTruncatesOldestAndCountsOmitted(t *testing.T) {
	var tr []Message
	for i := 0; i < 400; i++ {
		tr = append(tr, msg("claude.t", strings.Repeat("x", 1024)))
	}
	p := BuildPrompt(PromptInput{Listener: "a", Title: "t", Room: "/r", Transcript: tr, Trigger: msg("you", "go")})
	if len(p) > MaxPromptBytes {
		t.Fatalf("prompt %d bytes", len(p))
	}
	if !strings.Contains(p, "earlier messages omitted]") {
		t.Fatal("no omitted marker")
	}
	if !strings.HasSuffix(strings.TrimSpace(p), "you: go") {
		t.Fatal("trigger not last")
	}
}

func TestPromptCutsOversizedTriggerAtRuneBoundary(t *testing.T) {
	big := strings.Repeat("ü", MaxTriggerBytes) // 2 bytes each
	p := BuildPrompt(PromptInput{Listener: "a", Title: "t", Room: "/r", Trigger: Message{ID: "m1", Author: "you", Text: big}})
	if len(p) > MaxPromptBytes || !utf8.ValidString(p) || !strings.Contains(p, "[… truncated") {
		t.Fatalf("len %d valid %v", len(p), utf8.ValidString(p))
	}
}

func TestTranscriptOfSkipsUnfinishedAndTrigger(t *testing.T) {
	s := newSnapshot(Meta{})
	s.Messages = []Message{
		{ID: "1", Author: "you", Role: RoleUser, Text: "a"},
		{ID: "2", Author: "claude.t", Role: RoleAgent, State: StateRunning},
		{ID: "3", Author: "codex.t", Role: RoleAgent, State: StateError, Text: "ERROR boom"},
		{ID: "4", Author: "you", Role: RoleUser, Text: "trigger"},
		{ID: "5", Author: "system", Role: RoleSystem, State: StateSuggested, Text: "@x please"},
	}
	got := transcriptOf(s, "4")
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("transcript %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/thread/ -run 'Prompt|Transcript' -count=1`
Expected: FAIL — undefined: `BuildPrompt`.

- [ ] **Step 3: Implement**

```go
// internal/thread/prompt.go
package thread

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxPromptBytes  = 256 << 10
	MaxTriggerBytes = 128 << 10
	MaxPostBytes    = 128 << 10
	maxHeaderBytes  = 2 << 10
)

type PromptInput struct {
	Listener     string
	Title        string
	Room         string
	Participants []string
	Transcript   []Message
	Trigger      Message
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func line(m Message) string {
	text := m.Text
	if m.State == StateError {
		text = "[error] " + text
	}
	return m.Author + ": " + text + "\n"
}

func BuildPrompt(in PromptInput) string {
	header := fmt.Sprintf("You are %s in tincan thread '%s' (room %s). Participants: you (the owner, @you), %s. "+
		"All participants work in this same directory; use `git status` and `git diff` to see changes others made. "+
		"Mention another participant with @name to hand them work. Your reply is posted to the thread.\n\n",
		in.Listener, in.Title, in.Room, strings.Join(in.Participants, ", "))
	header = cutUTF8(header, maxHeaderBytes)

	trigger := in.Trigger.Text
	if len(trigger) > MaxTriggerBytes {
		trigger = cutUTF8(trigger, MaxTriggerBytes) + fmt.Sprintf("\n[… truncated, full text in thread message %s]", in.Trigger.ID)
	}
	tail := "Message to answer:\n" + in.Trigger.Author + ": " + trigger + "\n"

	budget := MaxPromptBytes - len(header) - len(tail) - 128
	lines := make([]string, len(in.Transcript))
	for i, m := range in.Transcript {
		lines[i] = line(m)
	}
	start, size := len(lines), 0
	for start > 0 && size+len(lines[start-1]) <= budget {
		start--
		size += len(lines[start])
	}
	var b strings.Builder
	b.WriteString(header)
	if len(lines) > 0 {
		b.WriteString("Thread so far:\n")
		if start > 0 {
			fmt.Fprintf(&b, "[%d earlier messages omitted]\n", start)
		}
		for _, l := range lines[start:] {
			b.WriteString(l)
		}
		b.WriteString("\n")
	}
	b.WriteString(tail)
	return b.String()
}

// transcriptOf lists finished messages (user and system text, completed or
// failed agent turns) except excludeID, in display order.
func transcriptOf(s *Snapshot, excludeID string) []Message {
	var out []Message
	for _, m := range s.Messages {
		if m.ID == excludeID || m.State == StateSuggested {
			continue
		}
		if m.Role == RoleAgent && m.State != StateDone && m.State != StateError {
			continue
		}
		out = append(out, m)
	}
	return out
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/thread/ -run 'Prompt|Transcript' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/thread/prompt.go internal/thread/prompt_test.go
git commit -m "Build bounded agent prompts from thread transcripts"
```

---

### Task 6: Dispatcher — post, submit, collect, handoffs, budget, crash safety

**Files:**
- Create: `internal/thread/dispatcher.go`, `internal/thread/dispatcher_test.go`

**Interfaces:**
- Consumes: Tasks 2–5 (`dispatch.Options`, `dispatch.Send`, `Thread`, `Tx`, `Resolver`, `BuildPrompt`, `transcriptOf`), `request.Poll`, `host.Existing`.
- Produces:
  - `type Dispatcher struct { Opts dispatch.Options; From string; Hook func(point string) error }`
  - `var ErrNotOwner`
  - `func New(o dispatch.Options) *Dispatcher`
  - `func (d *Dispatcher) Acquire() error`, `func (d *Dispatcher) Close()`
  - `func (d *Dispatcher) Post(ctx context.Context, tid, text, clientID string) (Message, error)`
  - `func (d *Dispatcher) Reconcile(ctx context.Context) error`
  - `func (d *Dispatcher) Resolver() Resolver`
  - Hook points: `"before-submit"`, `"after-submit"`, `"after-terminal"`, `"after-handoffs"`.

- [ ] **Step 1: Write the failing tests (fixture harness + behaviour)**

```go
// internal/thread/dispatcher_test.go
package thread_test

import (
	"context"
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
	for _, m := range snap.Messages {
		if m.State == thread.StateSuggested {
			suggested++
		}
	}
	if total != 7 || suggested != 3 {
		t.Fatalf("executions %d (want 1 user + 6 automatic), suggested %d (want 3)", total, suggested)
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
	e := newEnv(t, "a")
	e.script("a", "reply", "")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, quiescent)
	done := agentMessages(snap, thread.StateDone)
	if len(done) != 1 || done[0].Text != "" {
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/thread/ -run 'RoundTrip|Primary|Unresolved|Budget|Self|Empty|Spaces|LockHolder|Crash' -count=1`
Expected: FAIL — undefined: `thread.New`.

- [ ] **Step 3: Implement the dispatcher**

```go
// internal/thread/dispatcher.go
package thread

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
)

var ErrNotOwner = errors.New("another tincan web process dispatches this room")

type Dispatcher struct {
	Opts dispatch.Options
	From string // request sender name, default "web"
	// Hook, when set, is called at named points and aborts the pass when it
	// returns an error; tests use it to simulate crashes.
	Hook func(point string) error

	mu   sync.Mutex
	lock *filelock.Lock
}

func New(o dispatch.Options) *Dispatcher { return &Dispatcher{Opts: o, From: "web"} }

func (d *Dispatcher) Acquire() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lock != nil {
		return nil
	}
	if err := fsutil.MkdirPrivate(Root(d.Opts.Room)); err != nil {
		return err
	}
	l, err := filelock.Try(filepath.Join(Root(d.Opts.Room), "dispatcher.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return ErrNotOwner
	}
	if err != nil {
		return err
	}
	d.lock = l
	return nil
}

func (d *Dispatcher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lock != nil {
		d.lock.Close()
		d.lock = nil
	}
}

func (d *Dispatcher) hook(point string) error {
	if d.Hook != nil {
		return d.Hook(point)
	}
	return nil
}

func (d *Dispatcher) Resolver() Resolver {
	presets, _ := d.Opts.PresetMap()
	return Resolver{Room: d.Opts.Room, Presets: presets, Alive: func(name string) (host.State, bool) {
		return host.Existing(context.Background(), d.Opts.Room, name)
	}}
}

func (d *Dispatcher) system(tx *Tx, text, chain string) {
	tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, Text: text, Chain: chain})
}

func unresolvedNote(names []string) string {
	at := make([]string, len(names))
	for i, n := range names {
		at[i] = "@" + n
	}
	return "Not dispatched: " + strings.Join(at, ", ") + " (unknown in this room or not available on this machine)."
}

// Post appends an owner message and plans its agent turns. Any process may
// call it; only the lock holder submits the planned turns.
func (d *Dispatcher) Post(ctx context.Context, tid, text, clientID string) (Message, error) {
	if strings.TrimSpace(text) == "" {
		return Message{}, errors.New("message is empty")
	}
	if len(text) > MaxPostBytes {
		return Message{}, fmt.Errorf("message exceeds %d bytes", MaxPostBytes)
	}
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return Message{}, err
	}
	res := d.Resolver()
	var out Message
	err = t.Update(ctx, func(tx *Tx) error {
		if clientID != "" {
			if m, ok := tx.Snap.ByClientID(clientID); ok {
				out = m
				return nil
			}
		}
		if tx.Meta.Status != StatusOpen {
			return fmt.Errorf("thread is %s", tx.Meta.Status)
		}
		chain := "c" + randHex(6)
		ev := tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: text, Chain: chain, ClientID: clientID})
		out, _ = tx.Snap.Message(ev.ID)
		names := ParseMentions(text)
		if len(names) == 0 {
			names = []string{tx.Meta.Primary}
		}
		targets, unresolved := res.Resolve(tx.Meta, names)
		if len(unresolved) > 0 {
			d.system(tx, unresolvedNote(unresolved), chain)
		}
		for _, tg := range targets {
			if _, err := d.planTurn(t, tx, tg, out, chain, false); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func participants(tx *Tx, extra string) []string {
	set := map[string]bool{extra: true}
	for l := range tx.Meta.Listeners {
		set[l] = true
	}
	for _, m := range tx.Snap.Messages {
		if m.Role == RoleAgent && m.Listener != "" {
			set[m.Listener] = true
		}
	}
	var out []string
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// planTurn records one agent turn durably: message, prompt file, intent and
// pending state. Automatic turns reserve chain budget or become suggestions.
// It returns the new message ID ("" when nothing was planned).
func (d *Dispatcher) planTurn(t *Thread, tx *Tx, tg Target, trigger Message, chain string, auto bool) (string, error) {
	if auto {
		c := tx.Snap.Chains[chain]
		if c != nil && c.Stopped {
			return "", nil
		}
		if c != nil && c.Used >= tx.Meta.Budget {
			ev := tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, ReplyTo: trigger.ID, Chain: chain,
				Text: fmt.Sprintf("@%s please pick up the handoff from %s above.", tg.Mention, trigger.Author)})
			tx.Append(Event{Kind: KindState, Message: ev.ID, State: StateSuggested})
			return ev.ID, nil
		}
		tx.Append(Event{Kind: KindChain, Chain: chain, Op: OpReserve})
	}
	msg := tx.Append(Event{Kind: KindMessage, Author: tg.Listener, Role: RoleAgent, ReplyTo: trigger.ID, Chain: chain})
	rid := tx.Meta.ID + "-" + msg.ID
	if !tg.Existing {
		if tx.Meta.Listeners == nil {
			tx.Meta.Listeners = map[string]string{}
		}
		if tx.Meta.Listeners[tg.Listener] == "" {
			tx.Meta.Listeners[tg.Listener] = tg.Preset
			tx.SaveMeta()
		}
	}
	prompt := BuildPrompt(PromptInput{
		Listener: tg.Listener, Title: tx.Meta.Title, Room: t.Room,
		Participants: participants(tx, tg.Listener),
		Transcript:   transcriptOf(tx.Snap, trigger.ID),
		Trigger:      trigger,
	})
	if err := t.WritePrompt(rid, prompt); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(prompt))
	tx.Append(Event{Kind: KindIntent, Message: msg.ID, Listener: tg.Listener, Preset: tg.Preset, RequestID: rid, PromptSHA: hex.EncodeToString(sum[:])})
	tx.Append(Event{Kind: KindState, Message: msg.ID, State: StatePending})
	return msg.ID, nil
}

// Reconcile advances every open thread one step: finishes Stop/archive,
// submits planned turns, collects results and processes handoffs.
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	d.mu.Lock()
	owned := d.lock != nil
	d.mu.Unlock()
	if !owned {
		return ErrNotOwner
	}
	metas, err := List(d.Opts.Room)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range metas {
		if m.Status == StatusArchived {
			continue
		}
		if err := d.reconcileThread(ctx, m.ID); err != nil {
			errs = append(errs, fmt.Errorf("thread %s: %w", m.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (d *Dispatcher) reconcileThread(ctx context.Context, tid string) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	snap, err := t.Snapshot()
	if err != nil {
		return err
	}
	if snap.Meta.Status == StatusStopping || snap.Meta.Status == StatusArchiving {
		return d.finishStop(ctx, t, snap)
	}
	for _, m := range snap.Messages {
		if m.Role != RoleAgent {
			continue
		}
		var err error
		switch {
		case m.State == StatePending && m.RequestID != "":
			err = d.submit(ctx, t, m)
		case m.State == StateRunning:
			err = d.collect(ctx, t, m)
		case m.State == StateDone && !m.Handoffs:
			err = d.handoffs(ctx, t, m)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) submit(ctx context.Context, t *Thread, m Message) error {
	ok := false
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		c := tx.Snap.Chains[cur.Chain]
		ok = tx.Meta.Status == StatusOpen && cur.State == StatePending && (c == nil || !c.Stopped)
		return nil
	}); err != nil || !ok {
		return err
	}
	if err := d.hook("before-submit"); err != nil {
		return err
	}
	prompt, err := t.ReadPrompt(m.RequestID)
	var sendErr error
	if err != nil {
		sendErr = fmt.Errorf("prompt: %w", err)
	} else {
		_, sendErr = dispatch.Send(ctx, d.Opts, dispatch.SendSpec{Agent: m.Listener, Preset: m.Preset, From: d.From, Body: prompt, RequestID: m.RequestID})
	}
	if err := d.hook("after-submit"); err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		if cur, _ := tx.Snap.Message(m.ID); cur.State != StatePending {
			return nil
		}
		if sendErr != nil {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StateError, Text: "ERROR submit: " + sendErr.Error()})
			return nil
		}
		tx.Append(Event{Kind: KindState, Message: m.ID, State: StateRunning})
		return nil
	})
}

// outcome maps a terminal request record to a message state and text.
func outcome(r request.Record) (string, string) {
	switch r.Status {
	case "completed":
		if strings.HasPrefix(r.Result, "ERROR") {
			return StateError, r.Result
		}
		return StateDone, r.Result
	case "canceled":
		return StateCancelled, r.Result
	default:
		return StateError, r.Result
	}
}

func (d *Dispatcher) collect(ctx context.Context, t *Thread, m Message) error {
	r, err := request.Poll(ctx, d.Opts.Room, m.RequestID)
	if errors.Is(err, os.ErrNotExist) {
		return t.Update(ctx, func(tx *Tx) error {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StateUncollectable, Text: "result expired: the request record no longer exists"})
			return nil
		})
	}
	if err != nil {
		return err
	}
	if !r.Terminal() {
		return nil
	}
	state, text := outcome(r)
	if err := t.Update(ctx, func(tx *Tx) error {
		if cur, _ := tx.Snap.Message(m.ID); cur.State != StateRunning {
			return nil
		}
		tx.Append(Event{Kind: KindState, Message: m.ID, State: state, Text: text})
		return nil
	}); err != nil {
		return err
	}
	return d.hook("after-terminal")
}

func (d *Dispatcher) handoffs(ctx context.Context, t *Thread, m Message) error {
	names := ParseMentions(m.Text)
	res := d.Resolver()
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		if cur.Handoffs {
			return nil
		}
		var produced []string
		c := tx.Snap.Chains[cur.Chain]
		if tx.Meta.Status == StatusOpen && (c == nil || !c.Stopped) && len(names) > 0 {
			targets, unresolved := res.Resolve(tx.Meta, names)
			if len(unresolved) > 0 {
				d.system(tx, unresolvedNote(unresolved), cur.Chain)
			}
			for _, tg := range targets {
				if tg.Listener == cur.Listener {
					continue
				}
				id, err := d.planTurn(t, tx, tg, cur, cur.Chain, true)
				if err != nil {
					return err
				}
				if id != "" {
					produced = append(produced, id)
				}
			}
		}
		tx.Append(Event{Kind: KindHandoffs, Message: cur.ID, Produced: produced})
		return nil
	}); err != nil {
		return err
	}
	return d.hook("after-handoffs")
}
```

`finishStop` is added in Task 7; until then add this stub at the end of `dispatcher.go` so the package compiles:

```go
func (d *Dispatcher) finishStop(ctx context.Context, t *Thread, snap Snapshot) error { return nil }
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/thread/ -race -count=1 -v -run 'RoundTrip|Primary|Unresolved|Budget|Self|Empty|Spaces|LockHolder|Crash'`
Expected: PASS. If `TestCrashAtEveryBoundaryRunsEachTurnOnce/after-submit` fails with a second execution, the request ID or prompt is not stable across retries — fix the code, not the test.

- [ ] **Step 5: Commit**

```bash
git add internal/thread/dispatcher.go internal/thread/dispatcher_test.go
git commit -m "Dispatch thread turns durably with chain budget and handoffs"
```

---

### Task 7: Stop, archive, retry and janitor

**Files:**
- Create: `internal/thread/control.go`, `internal/thread/control_test.go`
- Modify: `internal/thread/dispatcher.go` (delete the `finishStop` stub)

**Interfaces:**
- Consumes: Task 6.
- Produces:
  - `func (d *Dispatcher) RequestStop(ctx context.Context, tid string) error` — sets the barrier; returns immediately.
  - `func (d *Dispatcher) SetArchived(ctx context.Context, tid string, archived bool) error`
  - `func (d *Dispatcher) Retry(ctx context.Context, tid, mid string) error`
  - `func (d *Dispatcher) Janitor(ctx context.Context, idle time.Duration) error`
  - `func WaitStatus(ctx context.Context, t *Thread, notIn ...string) (Meta, error)` — polls until the status is not in `notIn` (used by the API).

- [ ] **Step 1: Write the failing tests**

```go
// internal/thread/control_test.go
package thread_test

import (
	"context"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/thread"
)

func TestStopCancelsRunningTurn(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "sleep", "30s")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) == 1 })
	if err := d.RequestStop(context.Background(), th.ID); err != nil {
		t.Fatal(err)
	}
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusOpen })
	if got := agentMessages(snap, ""); len(got) != 1 || (got[0].State != thread.StateCancelled && got[0].State != thread.StateError) {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestStopRacingCompletionPreventsHandoff(t *testing.T) {
	e := newEnv(t, "a", "b")
	e.script("a", "reply", "@b next")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := thread.New(e.opts)
	if err := d.Acquire(); err != nil {
		t.Fatal(err)
	}
	stopHere := true
	d.Hook = func(p string) error {
		if p == "after-terminal" && stopHere {
			stopHere = false
			return context.Canceled // leave a done message with unprocessed handoffs
		}
		return nil
	}
	d.Post(context.Background(), th.ID, "@a go", "")
	deadline := time.Now().Add(30 * time.Second)
	for stopHere && time.Now().Before(deadline) {
		d.Reconcile(context.Background())
		time.Sleep(100 * time.Millisecond)
	}
	if err := d.RequestStop(context.Background(), th.ID); err != nil {
		t.Fatal(err)
	}
	e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusOpen && quiescent(s) })
	time.Sleep(time.Second)
	if e.count("b") != 0 {
		t.Fatalf("b ran %d times after Stop", e.count("b"))
	}
}

func TestArchiveCancelsQueuedWorkAndStopsListeners(t *testing.T) {
	e := newEnv(t, "a")
	e.script("a", "sleep", "3s")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a one", "")
	d.Post(context.Background(), th.ID, "@a two", "")
	e.settle(d, th, func(s thread.Snapshot) bool { return len(agentMessages(s, thread.StateRunning)) >= 1 })
	if err := d.SetArchived(context.Background(), th.ID, true); err != nil {
		t.Fatal(err)
	}
	e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusArchived })
	if _, alive := host.Existing(context.Background(), e.room, "a."+th.ID); alive {
		t.Fatal("thread listener still running after archive")
	}
	if err := d.SetArchived(context.Background(), th.ID, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	if e.count("a") > 1 {
		t.Fatalf("a ran %d times; queued work survived archive", e.count("a"))
	}
}

func TestRetryResubmitsFailedTurn(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	e.script("a", "reply", "ERROR simulated failure")
	d.Post(context.Background(), th.ID, "@a go", "")
	snap := e.settle(d, th, quiescent)
	failed := agentMessages(snap, thread.StateError)
	if len(failed) != 1 {
		t.Fatalf("messages %+v", snap.Messages)
	}
	e.script("a", "reply", "fine now")
	if err := d.Retry(context.Background(), th.ID, failed[0].ID); err != nil {
		t.Fatal(err)
	}
	snap = e.settle(d, th, quiescent)
	if done := agentMessages(snap, thread.StateDone); len(done) != 1 || done[0].Text != "fine now" {
		t.Fatalf("messages %+v", snap.Messages)
	}
}

func TestJanitorStopsIdleThreadListeners(t *testing.T) {
	e := newEnv(t, "a")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d := e.dispatcher()
	d.Post(context.Background(), th.ID, "@a go", "")
	e.settle(d, th, quiescent)
	if err := d.Janitor(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, alive := host.Existing(context.Background(), e.room, "a."+th.ID); alive {
		t.Fatal("idle listener not stopped")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/thread/ -run 'Stop|Archive|Retry|Janitor' -count=1`
Expected: FAIL — undefined: `(*Dispatcher).RequestStop`.

- [ ] **Step 3: Implement control**

Delete the `finishStop` stub from `dispatcher.go`, then:

```go
// internal/thread/control.go
package thread

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
)

// barrier stops every chain and moves the thread to status. From this point
// no turn is submitted and no handoff is planned (both check under the lock).
func barrier(tx *Tx, status, note string) {
	for id, c := range tx.Snap.Chains {
		if !c.Stopped {
			tx.Append(Event{Kind: KindChain, Chain: id, Op: OpStop})
		}
	}
	for _, m := range tx.Snap.Messages {
		if c := tx.Snap.Chains[m.Chain]; m.Chain != "" && c == nil {
			tx.Append(Event{Kind: KindChain, Chain: m.Chain, Op: OpStop})
		}
	}
	tx.Meta.Status = status
	tx.SaveMeta()
	tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, Text: note})
}

func (d *Dispatcher) RequestStop(ctx context.Context, tid string) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		if tx.Meta.Status != StatusOpen {
			return nil
		}
		barrier(tx, StatusStopping, "Stopped by you.")
		return nil
	})
}

func (d *Dispatcher) SetArchived(ctx context.Context, tid string, archived bool) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		switch {
		case archived && (tx.Meta.Status == StatusOpen || tx.Meta.Status == StatusStopping):
			barrier(tx, StatusArchiving, "Archived.")
		case !archived && tx.Meta.Status == StatusArchived:
			tx.Meta.Status = StatusOpen
			tx.SaveMeta()
			tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, Text: "Unarchived; agents start fresh conversations."})
		case !archived:
			return fmt.Errorf("thread is %s", tx.Meta.Status)
		}
		return nil
	})
}

// finishStop runs on the dispatcher: cancel unsubmitted turns, cancel and
// await submitted ones, then reopen or finish archiving.
func (d *Dispatcher) finishStop(ctx context.Context, t *Thread, snap Snapshot) error {
	room := d.Opts.Room
	type result struct{ id, state, text string }
	var settled []result
	pending := false
	for _, m := range snap.Messages {
		if !m.Active() {
			continue
		}
		r, err := request.Cancel(ctx, room, m.RequestID)
		if errors.Is(err, os.ErrNotExist) {
			settled = append(settled, result{m.ID, StateCancelled, "cancelled before it started"})
			continue
		}
		if err != nil {
			return err
		}
		if !r.Terminal() {
			host.Cancel(ctx, room, m.Listener, m.RequestID) // best effort; polling decides
			pending = true
			continue
		}
		state, text := outcome(r)
		settled = append(settled, result{m.ID, state, text})
	}
	if err := t.Update(ctx, func(tx *Tx) error {
		for _, s := range settled {
			if cur, _ := tx.Snap.Message(s.id); cur.Active() {
				tx.Append(Event{Kind: KindState, Message: s.id, State: s.state, Text: s.text})
			}
		}
		// Completed turns never hand off after a Stop.
		for _, m := range tx.Snap.Messages {
			if m.Role == RoleAgent && m.State == StateDone && !m.Handoffs {
				tx.Append(Event{Kind: KindHandoffs, Message: m.ID})
			}
		}
		return nil
	}); err != nil || pending {
		return err
	}
	if snap.Meta.Status == StatusArchiving {
		for name := range snap.Meta.Listeners {
			if err := host.Down(ctx, room, name, 10*time.Second); err != nil {
				return err
			}
			if err := host.ClearSession(ctx, room, name); err != nil {
				return err
			}
		}
	}
	return t.Update(ctx, func(tx *Tx) error {
		switch tx.Meta.Status {
		case StatusStopping:
			tx.Meta.Status = StatusOpen
		case StatusArchiving:
			tx.Meta.Status = StatusArchived
		default:
			return nil
		}
		tx.SaveMeta()
		return nil
	})
}

// Retry re-runs a failed turn: the same request when it was never saved or
// never ran, otherwise a fresh turn for the same listener and trigger.
func (d *Dispatcher) Retry(ctx context.Context, tid, mid string) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		if tx.Meta.Status != StatusOpen {
			return fmt.Errorf("thread is %s", tx.Meta.Status)
		}
		m, ok := tx.Snap.Message(mid)
		if !ok || m.Role != RoleAgent || (m.State != StateError && m.State != StateCancelled) {
			return errors.New("only failed or cancelled agent turns can be retried")
		}
		if r, err := request.Get(d.Opts.Room, m.RequestID); errors.Is(err, os.ErrNotExist) || (err == nil && !r.Terminal()) {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StatePending})
			return nil
		}
		trigger, ok := tx.Snap.Message(m.ReplyTo)
		if !ok {
			return errors.New("the message this turn answered is gone")
		}
		_, existing := tx.Meta.Listeners[m.Listener]
		_, err := d.planTurn(t, tx, Target{Mention: m.Listener, Listener: m.Listener, Preset: m.Preset, Existing: !existing}, trigger, m.Chain, false)
		return err
	})
}

// Janitor stops thread-owned listeners with no active turn whose last
// journal activity is older than idle. Sessions are kept.
func (d *Dispatcher) Janitor(ctx context.Context, idle time.Duration) error {
	metas, err := List(d.Opts.Room)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-idle)
	for _, meta := range metas {
		t, err := Open(d.Opts.Room, meta.ID)
		if err != nil {
			continue
		}
		snap, err := t.Snapshot()
		if err != nil {
			continue
		}
		last := map[string]time.Time{}
		busy := map[string]bool{}
		for _, m := range snap.Messages {
			if m.Listener == "" {
				continue
			}
			if m.Updated.After(last[m.Listener]) {
				last[m.Listener] = m.Updated
			}
			if m.Active() {
				busy[m.Listener] = true
			}
		}
		for name := range snap.Meta.Listeners {
			if busy[name] || last[name].After(cutoff) {
				continue
			}
			if _, alive := host.Existing(ctx, d.Opts.Room, name); alive {
				host.Down(ctx, d.Opts.Room, name, 10*time.Second)
			}
		}
	}
	return nil
}

func WaitStatus(ctx context.Context, t *Thread, notIn ...string) (Meta, error) {
	for {
		snap, err := t.Snapshot()
		if err != nil {
			return Meta{}, err
		}
		blocked := false
		for _, s := range notIn {
			if snap.Meta.Status == s {
				blocked = true
			}
		}
		if !blocked {
			return snap.Meta, nil
		}
		select {
		case <-ctx.Done():
			return snap.Meta, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
```

Note: the spec's per-listener "session generation" is not needed: v1 sends the full transcript every turn (spec §6.4) and archive clears each thread listener's session, so an unarchived thread starts fresh without tracking generations.

Note: queued requests that were cancelled cannot run later: `request.Start` returns a terminal record for a `CancelRequested` request, so the host skips it (`internal/request/store.go`, `Start`). No inbox draining is needed.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/thread/ -race -count=1 -v`
Expected: PASS for the whole package.

- [ ] **Step 5: Commit**

```bash
git add internal/thread
git commit -m "Add Stop and archive barriers, retry and idle janitor to threads"
```

---

### Task 8: Web server core — config, listener, owner and CSRF middleware, `tincan web`

**Files:**
- Create: `internal/web/server.go`, `internal/web/owner.go`, `internal/web/ui/index.html` (placeholder shell, finished in Task 12), `internal/web/ui/app.js` (empty file), `internal/web/ui/app.css` (empty file), `internal/web/server_test.go`, `internal/cli/web.go`
- Modify: `internal/cli/cli.go` (`Run` switch, `usageText`)

**Interfaces:**
- Consumes: `rooms.Registry`, `dispatch.Options`, `buildinfo.Current()`.
- Produces:
  - `type web.Peer struct { Name, URL string }`
  - `type web.Config struct { PublicPath, Owner, Origin, Machine string; Peers []Peer; ChainBudget int; IdleStop, Tick time.Duration; Registry *rooms.Registry; Dispatch dispatch.Options }`
  - `func web.New(cfg Config) (*Server, error)`, `func (s *Server) Handler() http.Handler`, `func (s *Server) Run(ctx context.Context, ln net.Listener) error`
  - `func web.Listen(spec string) (net.Listener, error)`, `func web.DefaultListen() string`
  - `func web.DetectOwner(ctx context.Context) (string, error)`
  - Internal: `s.mux *http.ServeMux`, `func (s *Server) writeJSON(w, status, v)`, `func (s *Server) fail(w, status, err)` (used by Tasks 9–11)

- [ ] **Step 1: Write the failing tests**

```go
// internal/web/server_test.go
package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

const owner = "owner@example.com"

func testServer(t *testing.T) *Server {
	t.Helper()
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	s, err := New(Config{Owner: owner, PublicPath: "/tincan", Machine: "testbox", Registry: reg, ChainBudget: 6, Dispatch: dispatch.Options{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ownerHdr() map[string]string { return map[string]string{"Tailscale-User-Login": owner} }

func TestOwnerCheck(t *testing.T) {
	h := testServer(t).Handler()
	if rec := do(t, h, "GET", "/api/self", "", nil); rec.Code != 403 {
		t.Fatalf("no identity: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/self", "", map[string]string{"Tailscale-User-Login": "intruder@example.com"}); rec.Code != 403 {
		t.Fatalf("wrong identity: %d", rec.Code)
	}
	rec := do(t, h, "GET", "/api/self", "", ownerHdr())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"machine":"testbox"`) {
		t.Fatalf("owner: %d %s", rec.Code, rec.Body)
	}
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy"} {
		if rec.Header().Get(k) == "" {
			t.Errorf("missing header %s", k)
		}
	}
}

func TestMutationsNeedCSRFHeaderAndSameOrigin(t *testing.T) {
	h := testServer(t).Handler()
	hdr := ownerHdr()
	if rec := do(t, h, "POST", "/api/rooms", `{"path":"/nope"}`, hdr); rec.Code != 403 {
		t.Fatalf("without X-Tincan-Request: %d", rec.Code)
	}
	hdr["X-Tincan-Request"] = "1"
	hdr["Origin"] = "https://evil.example"
	if rec := do(t, h, "POST", "/api/rooms", `{"path":"/nope"}`, hdr); rec.Code != 403 {
		t.Fatalf("foreign origin: %d", rec.Code)
	}
	hdr["Origin"] = "http://example.com" // httptest's default Host
	if rec := do(t, h, "POST", "/api/rooms", `{"path":"/nope"}`, hdr); rec.Code == 403 {
		t.Fatalf("same origin refused: %s", rec.Body)
	}
}

func TestIndexUsesPublicPath(t *testing.T) {
	h := testServer(t).Handler()
	rec := do(t, h, "GET", "/", "", ownerHdr())
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.Contains(string(body), `content="/tincan/"`) || !strings.Contains(string(body), `src="/tincan/app.js"`) {
		t.Fatalf("index: %d %s", rec.Code, body)
	}
}

func TestListenRefusesNonLoopbackTCPAndSecuresSocket(t *testing.T) {
	if _, err := Listen("0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback TCP accepted")
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if runtime.GOOS == "windows" {
		return
	}
	dir := filepath.Join(t.TempDir(), "run")
	sock := filepath.Join(dir, "web.sock")
	ln, err = Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for path, want := range map[string]os.FileMode{dir: 0o700, sock: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode %v %v", path, info.Mode().Perm(), err)
		}
	}
	// A stale socket from a crashed run is replaced.
	ln.Close()
	ln2, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln2.Close()
}

func TestParseOwnerFromStatusJSON(t *testing.T) {
	got, err := parseOwner([]byte(`{"Self":{"UserID":42},"User":{"42":{"LoginName":"me@example.com"}}}`))
	if err != nil || got != "me@example.com" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := parseOwner([]byte(`{"Self":{"UserID":1},"User":{}}`)); err == nil {
		t.Fatal("missing user accepted")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ -count=1`
Expected: FAIL — package has no Go files / undefined: `New`.

- [ ] **Step 3: Implement owner detection**

```go
// internal/web/owner.go
package web

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
)

var tailscaleCLIs = []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"}

func parseOwner(data []byte) (string, error) {
	var st struct {
		Self struct {
			UserID int64 `json:"UserID"`
		} `json:"Self"`
		User map[string]struct {
			LoginName string `json:"LoginName"`
		} `json:"User"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", err
	}
	login := st.User[strconv.FormatInt(st.Self.UserID, 10)].LoginName
	if login == "" {
		return "", errors.New("tailscale status has no login for this node's owner (is the node tagged?)")
	}
	return login, nil
}

// DetectOwner asks the local Tailscale CLI which user owns this node.
func DetectOwner(ctx context.Context) (string, error) {
	for _, name := range tailscaleCLIs {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, path, "status", "--json").Output()
		if err != nil {
			return "", err
		}
		return parseOwner(out)
	}
	return "", errors.New("tailscale CLI not found; pass --owner <login>")
}
```

- [ ] **Step 4: Implement the server core**

```go
// internal/web/server.go

// Package web serves the tincan chat UI and its JSON API. It trusts only the
// owner's Tailscale identity as set by `tailscale serve`.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/buildinfo"
	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
)

//go:embed ui
var uiFS embed.FS

type Peer struct {
	Name string
	URL  string
}

type Config struct {
	PublicPath  string // URL prefix the browser sees, e.g. "/tincan"
	Owner       string // required Tailscale-User-Login
	Origin      string // optional pinned origin for mutations
	Machine     string
	Peers       []Peer
	ChainBudget int
	IdleStop    time.Duration
	Tick        time.Duration
	Registry    *rooms.Registry
	Dispatch    dispatch.Options // Executable and Presets; Room is set per room
}

type Server struct {
	cfg   Config
	base  string // public path with trailing slash
	mux   *http.ServeMux
	index *template.Template
	hub   *hub
	peers map[string]*peer

	mu          sync.Mutex
	dispatchers map[string]*thread.Dispatcher // room ID → owned dispatcher
	lastJanitor time.Time                     // touched only by tick
}

func New(cfg Config) (*Server, error) {
	if cfg.Owner == "" {
		return nil, errors.New("owner login is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("room registry is required")
	}
	if cfg.ChainBudget <= 0 {
		cfg.ChainBudget = 6
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 2 * time.Second
	}
	if cfg.IdleStop <= 0 {
		cfg.IdleStop = 30 * time.Minute
	}
	base := "/" + strings.Trim(cfg.PublicPath, "/") + "/"
	if base == "//" {
		base = "/"
	}
	idx, err := template.ParseFS(uiFS, "ui/index.html")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, base: base, mux: http.NewServeMux(), index: idx, hub: newHub(), peers: map[string]*peer{}, dispatchers: map[string]*thread.Dispatcher{}}
	for _, p := range cfg.Peers {
		pp, err := newPeer(p)
		if err != nil {
			return nil, err
		}
		s.peers[p.Name] = pp
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.serveIndex)
	s.mux.HandleFunc("GET /app.js", s.serveAsset("ui/app.js", "text/javascript; charset=utf-8"))
	s.mux.HandleFunc("GET /app.css", s.serveAsset("ui/app.css", "text/css; charset=utf-8"))
	s.mux.HandleFunc("GET /api/self", s.apiSelf)
	s.apiRoutes()                                  // Task 9
	s.mux.HandleFunc("GET /api/events", s.apiEvents) // Task 10
	s.mux.HandleFunc("/api/peers/{peer}/{rest...}", s.proxyPeer) // Task 11
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.index.Execute(w, map[string]string{"Base": s.base})
}

func (s *Server) serveAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := uiFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)
	}
}

func (s *Server) apiSelf(w http.ResponseWriter, r *http.Request) {
	type peerView struct {
		Name   string `json:"name"`
		Online bool   `json:"online"`
	}
	peers := []peerView{}
	for name, p := range s.peers {
		peers = append(peers, peerView{Name: name, Online: p.online.Load()})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"machine": s.cfg.Machine, "version": buildinfo.Current().Version, "peers": peers})
}

// Handler wraps every route with security headers and the owner/CSRF checks.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Header.Get("Tailscale-User-Login") != s.cfg.Owner {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Tincan-Request") != "1" {
				http.Error(w, "missing X-Tincan-Request", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !s.originOK(o, r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) originOK(origin string, r *http.Request) bool {
	if s.cfg.Origin != "" {
		return strings.EqualFold(strings.TrimRight(origin, "/"), strings.TrimRight(s.cfg.Origin, "/"))
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return strings.EqualFold(u.Host, host)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	s.writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, thread.MaxPostBytes+16<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// DefaultListen is the platform default listen address.
func DefaultListen() string {
	if runtime.GOOS == "windows" {
		return "127.0.0.1:7788"
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return "unix:" + filepath.Join(d, "tincan", "web.sock")
	}
	return "unix:" + filepath.Join(rooms.StateDir(), "web.sock")
}

// Listen opens a private unix socket ("unix:<path>") or a loopback TCP
// address. Non-loopback TCP is refused: remote access is via tailscale serve.
func Listen(spec string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(spec, "unix:"); ok {
		if err := fsutil.MkdirPrivate(filepath.Dir(path)); err != nil {
			return nil, err
		}
		if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s is in use by another tincan web", path)
		}
		os.Remove(path)
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			ln.Close()
			return nil, err
		}
		return ln, nil
	}
	host, _, err := net.SplitHostPort(spec)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("refusing to listen on %s: use a unix socket or a loopback address with tailscale serve", spec)
	}
	return net.Listen("tcp", spec)
}

// Run serves until ctx is done, running the dispatch loop and peer checks.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	go s.loop(ctx)       // Task 10
	go s.watchPeers(ctx) // Task 11
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
```

Until Tasks 9–11 exist, add temporary stubs so the package compiles (each is replaced by its task):

```go
// internal/web/stubs.go — delete in Task 11
package web

import (
	"context"
	"net/http"
	"sync/atomic"
)

type hub struct{}

func newHub() *hub { return &hub{} }

type peer struct{ online atomic.Bool }

func newPeer(Peer) (*peer, error)                                  { return &peer{}, nil }
func (s *Server) apiRoutes()                                       {}
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {}
func (s *Server) proxyPeer(w http.ResponseWriter, r *http.Request) {}
func (s *Server) loop(ctx context.Context)                         {}
func (s *Server) watchPeers(ctx context.Context)                   {}
```

`POST /api/rooms` is registered by Task 9; until then `TestMutationsNeedCSRFHeaderAndSameOrigin`'s last assertion only requires "not 403" (a 404/405 from the mux is fine).

UI shell (finished in Task 12):

```html
<!-- internal/web/ui/index.html -->
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="tincan-base" content="{{.Base}}">
<title>tincan</title>
<link rel="stylesheet" href="{{.Base}}app.css">
<script defer src="{{.Base}}app.js"></script>
</head>
<body></body>
</html>
```

Create empty `internal/web/ui/app.js` and `internal/web/ui/app.css`.

- [ ] **Step 5: Add the `tincan web` command**

```go
// internal/cli/web.go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/web"
)

func cmdWeb(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", web.DefaultListen(), "unix:<path> or a loopback host:port")
	public := fs.String("public-path", "/", "URL path the browser uses, e.g. /tincan")
	owner := fs.String("owner", "", "Tailscale login allowed to use the UI (default: this node's owner)")
	origin := fs.String("origin", "", "pinned browser origin for mutations, e.g. https://host.tailnet.ts.net")
	budget := fs.Int("chain-budget", 6, "automatic agent executions per user message")
	idle := fs.Duration("idle-stop", 30*time.Minute, "stop idle thread listeners after this long")
	var peers, scans stringList
	fs.Var(&peers, "peer", "name=https://peer/tincan/ (repeatable)")
	fs.Var(&scans, "scan", "directory to scan for existing rooms (repeatable; default ~/projects)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "tincan web: unexpected arguments")
		return ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *owner == "" {
		detected, err := web.DetectOwner(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "tincan web: %v\n", err)
			return ExitUsage
		}
		*owner = detected
	}
	var peerList []web.Peer
	for _, p := range peers {
		name, u, ok := strings.Cut(p, "=")
		if !ok || name == "" || u == "" {
			fmt.Fprintf(stderr, "tincan web: --peer %q: expected name=url\n", p)
			return ExitUsage
		}
		peerList = append(peerList, web.Peer{Name: name, URL: u})
	}
	if len(scans) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			scans = stringList{filepath.Join(home, "projects")}
		}
	}
	reg := rooms.Default()
	if n, err := reg.Import(scans, 3); err != nil {
		fmt.Fprintf(stderr, "tincan web: room import: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(stderr, "tincan web: imported %d rooms\n", n)
	}
	machine, _ := os.Hostname()
	machine, _, _ = strings.Cut(machine, ".")
	srv, err := web.New(web.Config{PublicPath: *public, Owner: *owner, Origin: *origin, Machine: machine, Peers: peerList,
		ChainBudget: *budget, IdleStop: *idle, Registry: reg, Dispatch: dispatch.Options{}})
	if err != nil {
		fmt.Fprintf(stderr, "tincan web: %v\n", err)
		return ExitError
	}
	ln, err := web.Listen(*listen)
	if err != nil {
		fmt.Fprintf(stderr, "tincan web: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stderr, "tincan web: serving %s for %s on %s\n", *public, *owner, *listen)
	if err := srv.Run(ctx, ln); err != nil {
		fmt.Fprintf(stderr, "tincan web: %v\n", err)
		return ExitError
	}
	return ExitOK
}
```

In `internal/cli/cli.go`: add `case "web": return cmdWeb(args[1:], stdout, stderr)` to the `Run` switch, and add to `usageText` under the MCP line:

```
  tincan web [--listen unix:<path>|127.0.0.1:<port>] [--public-path /tincan] [--peer name=url]...
                                 chat UI for agents; expose with tailscale serve (see docs/web.md)
```

- [ ] **Step 6: Run tests**

Run: `go vet ./... && go test ./internal/web/ ./internal/cli/ -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web internal/cli
git commit -m "Add tincan web server core with owner and CSRF checks"
```

---

### Task 9: JSON API and safe Markdown rendering

**Files:**
- Create: `internal/web/api.go`, `internal/web/markdown.go`, `internal/web/api_test.go`, `internal/web/markdown_test.go`
- Modify: `internal/web/stubs.go` (delete `apiRoutes`)

**Interfaces:**
- Consumes: Tasks 1, 3, 6, 7, 8; `request.Progress`, `request.Get`, `host.ListStates`, `spool.Open(...).ListPresence`, `host.LogPath`.
- Produces:
  - `func RenderMarkdown(src string) string`
  - `func (s *Server) roomByID(id string) (rooms.Room, error)` and `func (s *Server) dispatcherFor(room rooms.Room) *thread.Dispatcher` (non-owning instance for Post/Stop/Archive/Retry; the owning one is in `s.dispatchers`, Task 10)
  - Routes from spec §7.1 plus `GET /api/rooms/{rid}/agents` → `{"presets":[…available…],"listeners":[…hosted alive…]}`.
  - Message JSON: `thread.Message` fields plus `"html"` (rendered) — type `messageView`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/web/markdown_test.go
package web

import (
	"strings"
	"testing"
)

func TestRenderMarkdownEscapesHTML(t *testing.T) {
	out := RenderMarkdown("<script>alert(1)</script> <img src=x onerror=alert(1)>")
	if strings.Contains(out, "<script") || strings.Contains(out, "<img") {
		t.Fatalf("raw HTML rendered: %s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("not escaped: %s", out)
	}
}

func TestRenderMarkdownSubset(t *testing.T) {
	out := RenderMarkdown("Hi @codex, see `x < y`.\n\n- one\n- two\n\n```go\nfmt.Println(\"<b>\")\n```\n[docs](https://example.com/a?b=1&c=2) [bad](javascript:alert(1))")
	for _, want := range []string{
		`<span class="mention">@codex</span>`,
		`<code>x &lt; y</code>`,
		`<ul><li>one</li><li>two</li></ul>`,
		`<pre><code>fmt.Println(&#34;&lt;b&gt;&#34;)</code></pre>`,
		`<a href="https://example.com/a?b=1&amp;c=2" rel="noopener noreferrer" target="_blank">docs</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "javascript:") && strings.Contains(out, "href=\"javascript") {
		t.Fatalf("javascript link rendered: %s", out)
	}
}
```

```go
// internal/web/api_test.go
package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func apiServer(t *testing.T) (*Server, string) {
	t.Helper()
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	room, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(t.TempDir(), "agent")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	presets := map[string]host.Preset{
		"claude":  {Exec: []string{bin}, Stdin: "none", Reply: "stdout"},
		"missing": {Exec: []string{filepath.Join(t.TempDir(), "absent")}},
	}
	s, err := New(Config{Owner: owner, Machine: "box", Registry: reg, ChainBudget: 6, Dispatch: dispatch.Options{Presets: presets}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := reg.Add(room)
	if err != nil {
		t.Fatal(err)
	}
	return s, r.ID
}

func mut() map[string]string {
	return map[string]string{"Tailscale-User-Login": owner, "X-Tincan-Request": "1"}
}

func decode(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}

func TestThreadLifecycleOverAPI(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"audit","primary":"claude","client_id":"c1"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var meta struct{ ID string }
	decode(t, rec.Body.String(), &meta)
	again := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"audit","primary":"claude","client_id":"c1"}`, mut())
	var meta2 struct{ ID string }
	decode(t, again.Body.String(), &meta2)
	if meta2.ID != meta.ID {
		t.Fatal("client_id did not dedupe thread creation")
	}
	rec = do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages", `{"text":"hello <b>","client_id":"m1"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages?after=0", "", ownerHdr())
	var page struct {
		Seq      int64
		Messages []struct{ Role, Text, HTML, State string }
	}
	decode(t, rec.Body.String(), &page)
	if page.Seq == 0 || len(page.Messages) != 2 || page.Messages[0].HTML != "<p>hello &lt;b&gt;</p>" || page.Messages[1].State != "pending" {
		t.Fatalf("page %+v", page)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages?after="+itoa(page.Seq), "", ownerHdr())
	decode(t, rec.Body.String(), &page)
	if len(page.Messages) != 0 {
		t.Fatalf("cursor returned old messages: %+v", page.Messages)
	}
}

func TestCreateThreadRejectsUnavailablePrimary(t *testing.T) {
	s, rid := apiServer(t)
	rec := do(t, s.Handler(), "POST", "/api/rooms/"+rid+"/threads", `{"title":"x","primary":"missing"}`, mut())
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "missing") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUnknownRoomAndThreadAre404(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	if rec := do(t, h, "GET", "/api/rooms/000000000000/threads", "", ownerHdr()); rec.Code != 404 {
		t.Fatalf("room: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/rooms/"+rid+"/threads/deadbeef/messages", "", ownerHdr()); rec.Code != 404 {
		t.Fatalf("thread: %d", rec.Code)
	}
}

func TestAgentsListsAvailablePresetsOnly(t *testing.T) {
	s, rid := apiServer(t)
	rec := do(t, s.Handler(), "GET", "/api/rooms/"+rid+"/agents", "", ownerHdr())
	var got struct{ Presets []string }
	decode(t, rec.Body.String(), &got)
	if len(got.Presets) != 1 || got.Presets[0] != "claude" {
		t.Fatalf("presets %q", got.Presets)
	}
}

func TestAgentLogRejectsTraversal(t *testing.T) {
	s, rid := apiServer(t)
	if rec := do(t, s.Handler(), "GET", "/api/rooms/"+rid+"/agents/..%2F..%2Fetc/log", "", ownerHdr()); rec.Code != 400 && rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}
```

Add a tiny helper to `api_test.go`: `func itoa(n int64) string { return strconv.FormatInt(n, 10) }` (import `strconv`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ -count=1`
Expected: FAIL — undefined: `RenderMarkdown`; thread routes 404.

- [ ] **Step 3: Implement the Markdown renderer**

The spec places the Markdown renderer in the UI; this plan renders on the server instead (`html` field per message) so the escaping is covered by Go tests, and the browser only inserts that pre-escaped HTML.

```go
// internal/web/markdown.go
package web

import (
	"html"
	"regexp"
	"strings"
)

var (
	linkRE    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	mentionRE = regexp.MustCompile(`(^|[\s(\[,])@([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)
)

func isItem(l string) bool {
	t := strings.TrimLeft(l, " ")
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
}

func itemText(l string) string { return strings.TrimLeft(l, " ")[2:] }

// text escapes s and decorates links and mentions; nothing from s is ever
// emitted unescaped.
func text(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range linkRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(mentions(html.EscapeString(s[last:m[0]])))
		b.WriteString(`<a href="` + html.EscapeString(s[m[4]:m[5]]) + `" rel="noopener noreferrer" target="_blank">` + html.EscapeString(s[m[2]:m[3]]) + `</a>`)
		last = m[1]
	}
	b.WriteString(mentions(html.EscapeString(s[last:])))
	return b.String()
}

func mentions(escaped string) string {
	return mentionRE.ReplaceAllString(escaped, `$1<span class="mention">@$2</span>`)
}

func inline(s string) string {
	parts := strings.Split(s, "`")
	if len(parts)%2 == 0 { // unbalanced: treat the last backtick literally
		parts[len(parts)-2] += "`" + parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString("<code>" + html.EscapeString(p) + "</code>")
		} else {
			b.WriteString(text(p))
		}
	}
	return b.String()
}

// RenderMarkdown renders paragraphs, fenced and inline code, bullet lists,
// http(s) links and @mentions. Raw HTML is always escaped.
func RenderMarkdown(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), "```"):
			j := i + 1
			var code []string
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				code = append(code, lines[j])
				j++
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>")
			i = j + 1
		case strings.TrimSpace(l) == "":
			i++
		case isItem(l):
			b.WriteString("<ul>")
			for i < len(lines) && isItem(lines[i]) {
				b.WriteString("<li>" + inline(itemText(lines[i])) + "</li>")
				i++
			}
			b.WriteString("</ul>")
		default:
			var para []string
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" && !isItem(lines[i]) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				para = append(para, inline(lines[i]))
				i++
			}
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
		}
	}
	return b.String()
}
```

- [ ] **Step 4: Implement the API**

Delete `apiRoutes` from `stubs.go`, then:

```go
// internal/web/api.go
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/spool"
	"github.com/c0ze/tincan/v2/internal/thread"
)

var errNotFound = errors.New("not found")

func (s *Server) apiRoutes() {
	s.mux.HandleFunc("GET /api/rooms", s.listRooms)
	s.mux.HandleFunc("POST /api/rooms", s.addRoom)
	s.mux.HandleFunc("PATCH /api/rooms/{rid}", s.patchRoom)
	s.mux.HandleFunc("GET /api/rooms/{rid}/agents", s.listAgents)
	s.mux.HandleFunc("GET /api/rooms/{rid}/threads", s.listThreads)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads", s.createThread)
	s.mux.HandleFunc("GET /api/rooms/{rid}/threads/{tid}/messages", s.listMessages)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/messages", s.postMessage)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/stop", s.stopThread)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/archive", s.archiveThread)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/messages/{mid}/retry", s.retryMessage)
	s.mux.HandleFunc("GET /api/rooms/{rid}/activity", s.activity)
	s.mux.HandleFunc("GET /api/rooms/{rid}/requests/{id}/progress", s.progress)
	s.mux.HandleFunc("GET /api/rooms/{rid}/agents/{name}/log", s.agentLog)
}

func (s *Server) roomByID(id string) (rooms.Room, error) {
	room, ok, err := s.cfg.Registry.Get(id)
	if err != nil {
		return rooms.Room{}, err
	}
	if !ok || room.Missing {
		return rooms.Room{}, errNotFound
	}
	return room, nil
}

func (s *Server) opts(room rooms.Room) dispatch.Options {
	o := s.cfg.Dispatch
	o.Room = room.Path
	return o
}

// dispatcherFor returns a non-owning dispatcher for journal writes (post,
// stop, archive, retry); submission is left to the room's lock holder.
func (s *Server) dispatcherFor(room rooms.Room) *thread.Dispatcher { return thread.New(s.opts(room)) }

func (s *Server) room(w http.ResponseWriter, r *http.Request) (rooms.Room, bool) {
	room, err := s.roomByID(r.PathValue("rid"))
	if errors.Is(err, errNotFound) {
		s.fail(w, http.StatusNotFound, fmt.Errorf("room %s not found", r.PathValue("rid")))
		return room, false
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return room, false
	}
	return room, true
}

func (s *Server) threadOf(w http.ResponseWriter, r *http.Request) (rooms.Room, *thread.Thread, bool) {
	room, ok := s.room(w, r)
	if !ok {
		return room, nil, false
	}
	t, err := thread.Open(room.Path, r.PathValue("tid"))
	if err != nil {
		s.fail(w, http.StatusNotFound, errors.New("thread not found"))
		return room, nil, false
	}
	return room, t, true
}

type roomView struct {
	rooms.Room
	Running int `json:"running"`
}

func running(roomPath string) int {
	states, _ := host.ListStates(roomPath)
	n := 0
	for _, st := range states {
		if st.State == "busy" {
			n++
		}
	}
	return n
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Registry.List()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []roomView{}
	for _, room := range list {
		v := roomView{Room: room}
		if !room.Missing {
			v.Running = running(room.Path)
		}
		out = append(out, v)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) addRoom(w http.ResponseWriter, r *http.Request) {
	var in struct{ Path string }
	if err := readJSON(r, &in); err != nil || !filepath.IsAbs(in.Path) {
		s.fail(w, http.StatusBadRequest, errors.New("expected {\"path\": \"/absolute/dir\"}"))
		return
	}
	room, err := s.cfg.Registry.Add(in.Path)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, room)
}

func (s *Server) patchRoom(w http.ResponseWriter, r *http.Request) {
	var in struct{ Hidden bool }
	if err := readJSON(r, &in); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.cfg.Registry.SetHidden(r.PathValue("rid"), in.Hidden); err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"hidden": in.Hidden})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	res := s.dispatcherFor(room).Resolver()
	presets := []string{}
	for _, name := range host.Names(res.Presets) {
		if targets, _ := res.Resolve(thread.Meta{ID: "00000000"}, []string{name}); len(targets) == 1 {
			presets = append(presets, name)
		}
	}
	listeners := []string{}
	states, _ := host.ListStates(room.Path)
	for name, st := range states {
		if !strings.Contains(name, ".") && st.Owner != "" && st.Ready(r.Context()) {
			listeners = append(listeners, name)
		}
	}
	sort.Strings(listeners)
	s.writeJSON(w, http.StatusOK, map[string][]string{"presets": presets, "listeners": listeners})
}

type threadView struct {
	thread.Meta
	Running  int       `json:"running"`
	LastTime time.Time `json:"last_time"`
}

func (s *Server) listThreads(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	metas, err := thread.List(room.Path)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []threadView{}
	for _, m := range metas {
		v := threadView{Meta: m, LastTime: m.Created}
		if t, err := thread.Open(room.Path, m.ID); err == nil {
			if snap, err := t.Snapshot(); err == nil {
				for _, msg := range snap.Messages {
					if msg.Active() {
						v.Running++
					}
					if msg.Updated.After(v.LastTime) {
						v.LastTime = msg.Updated
					}
				}
			}
		}
		out = append(out, v)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) createThread(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	var in struct{ Title, Primary, ClientID string }
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Title) == "" || in.Primary == "" {
		s.fail(w, http.StatusBadRequest, errors.New("expected {\"title\", \"primary\"}"))
		return
	}
	if in.ClientID != "" {
		metas, _ := thread.List(room.Path)
		for _, m := range metas {
			if m.ClientID == in.ClientID {
				s.writeJSON(w, http.StatusCreated, m)
				return
			}
		}
	}
	if targets, _ := s.dispatcherFor(room).Resolver().Resolve(thread.Meta{ID: "00000000"}, []string{in.Primary}); len(targets) == 0 {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("agent %q is not available on %s", in.Primary, s.cfg.Machine))
		return
	}
	t, err := thread.Create(room.Path, strings.TrimSpace(in.Title), in.Primary, in.ClientID, s.cfg.ChainBudget)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	snap, _ := t.Snapshot()
	s.hub.publish(note{Kind: "thread", Room: room.ID, Thread: t.ID})
	s.writeJSON(w, http.StatusCreated, snap.Meta)
}

type messageView struct {
	thread.Message
	HTML string `json:"html"`
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	_, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	snap, err := t.Snapshot()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []messageView{}
	for _, m := range snap.Messages {
		if m.Seq > after {
			out = append(out, messageView{Message: m, HTML: RenderMarkdown(m.Text)})
		}
	}
	chains := map[string]*thread.Chain{}
	for id, c := range snap.Chains {
		chains[id] = c
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"seq": snap.MaxSeq, "meta": snap.Meta, "messages": out, "chains": chains})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	var in struct{ Text, ClientID string }
	if err := readJSON(r, &in); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	m, err := s.dispatcherFor(room).Post(r.Context(), t.ID, in.Text, in.ClientID)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "messages", Room: room.ID, Thread: t.ID})
	s.writeJSON(w, http.StatusCreated, messageView{Message: m, HTML: RenderMarkdown(m.Text)})
}

func (s *Server) stopThread(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	if err := s.dispatcherFor(room).RequestStop(r.Context(), t.ID); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "messages", Room: room.ID, Thread: t.ID})
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	meta, err := thread.WaitStatus(ctx, t, thread.StatusStopping)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, meta)
}

func (s *Server) archiveThread(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	var in struct{ Archived bool }
	if err := readJSON(r, &in); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.dispatcherFor(room).SetArchived(r.Context(), t.ID, in.Archived); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "thread", Room: room.ID, Thread: t.ID})
	snap, _ := t.Snapshot()
	s.writeJSON(w, http.StatusOK, snap.Meta)
}

func (s *Server) retryMessage(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	if err := s.dispatcherFor(room).Retry(r.Context(), t.ID, r.PathValue("mid")); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "messages", Room: room.ID, Thread: t.ID})
	s.writeJSON(w, http.StatusOK, map[string]bool{"retried": true})
}

type listenerView struct {
	Name    string `json:"name"`
	Preset  string `json:"preset,omitempty"`
	Mode    string `json:"mode"` // hosted | interactive
	Busy    bool   `json:"busy"`
	Current string `json:"current,omitempty"`
	Since   string `json:"since,omitempty"`
}

type requestView struct {
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	From    string    `json:"from"`
	Status  string    `json:"status"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Result  string    `json:"result,omitempty"`
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	listeners := []listenerView{}
	states, _ := host.ListStates(room.Path)
	for name, st := range states {
		if st.Ready(r.Context()) {
			listeners = append(listeners, listenerView{Name: name, Preset: st.Preset, Mode: "hosted", Busy: st.State == "busy", Current: st.CurrentID})
		}
	}
	if sp, err := spool.Open(room.Path); err == nil {
		if present, err := sp.ListPresence(); err == nil {
			for _, p := range present {
				if _, hosted := states[p.Name]; !hosted && p.Alive {
					listeners = append(listeners, listenerView{Name: p.Name, Mode: "interactive", Since: p.Since.Format(time.RFC3339)})
				}
			}
		}
	}
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].Name < listeners[j].Name })
	entries, _ := os.ReadDir(request.Dir(room.Path))
	var reqs []requestView
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		rec, err := request.Get(room.Path, id)
		if err != nil {
			continue
		}
		v := requestView{ID: rec.ID, Agent: rec.Agent, Status: rec.Status, Created: rec.Created, Updated: rec.Updated, Result: rec.Result}
		if rec.Envelope != nil {
			v.From = rec.Envelope.From
		}
		if len(v.Result) > 2048 {
			v.Result = v.Result[:2048] + "…"
		}
		reqs = append(reqs, v)
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Updated.After(reqs[j].Updated) })
	if len(reqs) > 50 {
		reqs = reqs[:50]
	}
	if reqs == nil {
		reqs = []requestView{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"listeners": listeners, "requests": reqs})
}

func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := request.ValidateID(id); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	events, next, err := request.Progress(room.Path, id, cursor, 200)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_cursor": next})
}

func (s *Server) agentLog(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := spool.ValidName(name); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	data, err := fsutil.ReadFile(host.LogPath(room.Path, name), 64<<20)
	if err != nil {
		s.fail(w, http.StatusNotFound, errors.New("no log for this agent"))
		return
	}
	if len(data) > 64<<10 {
		data = data[len(data)-64<<10:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}
```

`fsutil.ReadFile` refuses symlinks and FIFOs and enforces the 64 MiB ceiling; only the last 64 KiB is returned.

`note` and `s.hub.publish` come from Task 10. Until then, add to `stubs.go`:

```go
type note struct{ Kind, Room, Thread string }

func (h *hub) publish(note) {}
```

- [ ] **Step 5: Run tests**

Run: `go vet ./... && go test ./internal/web/ -count=1 -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web
git commit -m "Add tincan web JSON API and safe Markdown rendering"
```

---

### Task 10: Live updates and the dispatch loop

**Files:**
- Create: `internal/web/events.go`, `internal/web/events_test.go`
- Modify: `internal/web/stubs.go` (delete `hub`, `newHub`, `note`, `publish`, `apiEvents`, `loop`)

**Interfaces:**
- Consumes: Tasks 6–9; `github.com/fsnotify/fsnotify`.
- Produces:
  - `type note struct { Kind, Room, Thread string; Seq int64 }` (JSON: `kind`, `room`, `thread`, `seq`)
  - `type hub`, `func newHub() *hub`, `func (h *hub) publish(n note)`, `func (h *hub) subscribe() (chan note, func())`
  - `func (s *Server) apiEvents(w, r)` (SSE)
  - `func (s *Server) loop(ctx context.Context)` — every `cfg.Tick`: scan registry rooms, acquire/keep a dispatcher per room, `Reconcile`, janitor each minute, publish notes on fingerprint changes; fsnotify triggers an early scan.
  - `func (s *Server) tick(ctx context.Context)` — one iteration (exported to tests in-package).

- [ ] **Step 1: Write the failing tests**

```go
// internal/web/events_test.go
package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/thread"
)

func TestSSEStreamsNotes(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	req.Header.Set("Tailscale-User-Login", owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.hub.publish(note{Kind: "messages", Room: "r1", Thread: "t1", Seq: 3})
	}()
	sc := bufio.NewScanner(resp.Body)
	deadline := time.After(5 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, `"thread":"t1"`) {
				return
			}
		case <-deadline:
			t.Fatal("no note received")
		}
	}
}

func TestTickPublishesThreadChanges(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	th, _ := thread.Create(room.Path, "t", "claude", "", 6)
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	s.tick(context.Background()) // baseline
	drain(ch)
	th.Update(context.Background(), func(tx *thread.Tx) error {
		tx.Append(thread.Event{Kind: thread.KindMessage, Author: "you", Role: thread.RoleUser, Text: "x"})
		return nil
	})
	s.tick(context.Background())
	select {
	case n := <-ch:
		if n.Kind != "messages" || n.Room != rid || n.Thread != th.ID {
			t.Fatalf("note %+v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no note after journal change")
	}
}

func TestLoopSkipsMissingRoom(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	os.RemoveAll(room.Path)
	s.tick(context.Background()) // must not panic or block
	if rec := do(t, s.Handler(), "GET", "/api/rooms", "", ownerHdr()); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"missing":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func drain(ch chan note) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ -run 'SSE|Tick|Loop' -count=1`
Expected: FAIL — `s.hub.subscribe` undefined.

- [ ] **Step 3: Implement**

Delete `hub`, `newHub`, `note`, `publish`, `apiEvents` and `loop` from `stubs.go`, then:

```go
// internal/web/events.go
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
	"github.com/fsnotify/fsnotify"
)

type note struct {
	Kind   string `json:"kind"` // thread | messages | activity | peer
	Room   string `json:"room,omitempty"`
	Thread string `json:"thread,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
}

type hub struct {
	mu   sync.Mutex
	subs map[chan note]struct{}
	fp   map[string]string // fingerprints from the last scan
}

func newHub() *hub { return &hub{subs: map[chan note]struct{}{}, fp: map[string]string{}} }

func (h *hub) subscribe() (chan note, func()) {
	ch := make(chan note, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// publish never blocks: a slow client misses notes and refetches on reconnect.
func (h *hub) publish(n note) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- n:
		default:
		}
	}
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case n := <-ch:
			data, _ := json.Marshal(n)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func statFP(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// scan compares per-room fingerprints with the last scan and publishes notes.
func (s *Server) scan(list []rooms.Room) {
	next := map[string]string{}
	var notes []note
	for _, room := range list {
		if room.Missing {
			continue
		}
		act := statFP(filepath.Join(room.Path, ".tincan", "requests")) + statFP(filepath.Join(room.Path, ".tincan", "hosts")) + statFP(filepath.Join(room.Path, ".tincan", "present"))
		next["a:"+room.ID] = act
		next["r:"+room.ID] = statFP(thread.Root(room.Path))
		metas, _ := thread.List(room.Path)
		for _, m := range metas {
			dir := filepath.Join(thread.Root(room.Path), m.ID)
			next["t:"+room.ID+":"+m.ID] = statFP(filepath.Join(dir, "events.jsonl")) + statFP(filepath.Join(dir, "thread.json"))
		}
	}
	s.hub.mu.Lock()
	prev := s.hub.fp
	s.hub.fp = next
	s.hub.mu.Unlock()
	if len(prev) == 0 {
		return // first scan is the baseline
	}
	for key, fp := range next {
		if prev[key] == fp {
			continue
		}
		switch key[0] {
		case 'a':
			notes = append(notes, note{Kind: "activity", Room: key[2:]})
		case 'r':
			notes = append(notes, note{Kind: "thread", Room: key[2:]})
		case 't':
			rid, tid := key[2:14], key[15:]
			notes = append(notes, note{Kind: "messages", Room: rid, Thread: tid})
		}
	}
	for _, n := range notes {
		s.hub.publish(n)
	}
}

func (s *Server) dispatcher(room rooms.Room) *thread.Dispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.dispatchers[room.ID]; ok {
		return d
	}
	d := thread.New(s.opts(room))
	if err := d.Acquire(); err != nil {
		return nil // another process owns this room; retry next tick
	}
	s.dispatchers[room.ID] = d
	return d
}

// tick runs one dispatch-and-notify iteration over all registered rooms.
func (s *Server) tick(ctx context.Context) {
	list, err := s.cfg.Registry.List()
	if err != nil {
		return
	}
	janitor := time.Since(s.lastJanitor) > time.Minute
	for _, room := range list {
		if room.Missing {
			continue
		}
		if _, err := os.Stat(thread.Root(room.Path)); err != nil {
			continue // no threads yet: nothing to dispatch
		}
		d := s.dispatcher(room)
		if d == nil {
			continue
		}
		if err := d.Reconcile(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "tincan web: %s: %v\n", room.Name, err)
		}
		if janitor {
			d.Janitor(ctx, s.cfg.IdleStop)
		}
	}
	if janitor {
		s.lastJanitor = time.Now()
	}
	s.scan(list)
}

func (s *Server) loop(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	var events chan fsnotify.Event
	if err == nil {
		defer watcher.Close()
		events = watcher.Events
	}
	watched := map[string]bool{}
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	for {
		s.tick(ctx)
		if watcher != nil {
			list, _ := s.cfg.Registry.List()
			for _, room := range list {
				dirs := []string{thread.Root(room.Path), filepath.Join(room.Path, ".tincan", "requests"), filepath.Join(room.Path, ".tincan", "hosts"), filepath.Join(room.Path, ".tincan", "present")}
				metas, _ := thread.List(room.Path)
				for _, m := range metas {
					dirs = append(dirs, filepath.Join(thread.Root(room.Path), m.ID))
				}
				for _, d := range dirs {
					if !watched[d] && watcher.Add(d) == nil {
						watched[d] = true
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, d := range s.dispatchers {
				d.Close()
			}
			s.mu.Unlock()
			return
		case <-ticker.C:
		case <-events:
			time.Sleep(100 * time.Millisecond) // coalesce bursts
			for len(events) > 0 {
				<-events
			}
		}
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/web/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web
git commit -m "Run thread dispatch from tincan web and stream change notes"
```

---

### Task 11: Peer proxy and health

**Files:**
- Create: `internal/web/proxy.go`, `internal/web/proxy_test.go`
- Delete: `internal/web/stubs.go`

**Interfaces:**
- Consumes: Task 8 `Config.Peers`, `Server.peers`, `hub.publish`.
- Produces: `type peer struct { name string; base *url.URL; online atomic.Bool; proxy *httputil.ReverseProxy; client *http.Client }`, `func newPeer(p Peer) (*peer, error)`, `func (s *Server) proxyPeer(w, r)`, `func (s *Server) watchPeers(ctx)`, `func (s *Server) checkPeers(ctx)`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/web/proxy_test.go
package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/rooms"
)

// servedLike emulates `tailscale serve --set-path /tincan`: it strips the
// mount prefix and overwrites the identity header for the calling device.
func servedLike(h http.Handler, login string) http.Handler {
	return http.StripPrefix("/tincan", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Tailscale-User-Login", login)
		h.ServeHTTP(w, r)
	}))
}

func peerPair(t *testing.T) (*Server, *http.Request, *httptest.Server) {
	t.Helper()
	peerReg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	peerReg.Add(t.TempDir())
	peerServer, err := New(Config{Owner: owner, Machine: "macmini", Registry: peerReg})
	if err != nil {
		t.Fatal(err)
	}
	last := &http.Request{}
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*last = *r.Clone(context.Background())
		peerServer.Handler().ServeHTTP(w, r)
	})
	peerHTTP := httptest.NewServer(servedLike(record, owner))
	t.Cleanup(peerHTTP.Close)
	hubServer, err := New(Config{Owner: owner, Machine: "cachyos", Registry: rooms.Open(filepath.Join(t.TempDir(), "rooms.json")),
		Peers: []Peer{{Name: "macmini", URL: peerHTTP.URL + "/tincan/"}}})
	if err != nil {
		t.Fatal(err)
	}
	return hubServer, last, peerHTTP
}

func TestProxyForwardsReadsWithPathJoinedOnce(t *testing.T) {
	hub, seen, _ := peerPair(t)
	rec := do(t, hub.Handler(), "GET", "/api/peers/macmini/rooms", "", ownerHdr())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if seen.URL.Path != "/api/rooms" {
		t.Fatalf("peer saw path %q", seen.URL.Path)
	}
}

func TestProxyMutationUsesHubHeaders(t *testing.T) {
	hub, seen, _ := peerPair(t)
	hdr := mut()
	hdr["Origin"] = "http://example.com"
	hdr["Cookie"] = "secret=1"
	rec := do(t, hub.Handler(), "POST", "/api/peers/macmini/rooms", `{"path":"/nonexistent"}`, hdr)
	if rec.Code == 403 {
		t.Fatalf("mutation refused: %s", rec.Body)
	}
	if seen.Header.Get("Origin") != "" || seen.Header.Get("Cookie") != "" || seen.Header.Get("X-Tincan-Request") != "1" {
		t.Fatalf("peer headers %v", seen.Header)
	}
}

func TestProxyRejectsTraversalAndUnknownPeer(t *testing.T) {
	hub, _, _ := peerPair(t)
	if rec := do(t, hub.Handler(), "GET", "/api/peers/macmini/../../comics", "", ownerHdr()); rec.Code != 400 && rec.Code != 404 && rec.Code != 301 {
		t.Fatalf("traversal: %d", rec.Code)
	}
	if rec := do(t, hub.Handler(), "GET", "/api/peers/nobody/rooms", "", ownerHdr()); rec.Code != 404 {
		t.Fatalf("unknown peer: %d", rec.Code)
	}
}

func TestPeerHealthTracksOffline(t *testing.T) {
	hub, _, peerSrv := peerPair(t)
	hub.checkPeers(context.Background())
	if !hub.peers["macmini"].online.Load() {
		t.Fatal("peer not online")
	}
	peerSrv.Close()
	hub.checkPeers(context.Background())
	if hub.peers["macmini"].online.Load() {
		t.Fatal("peer still online after close")
	}
	rec := do(t, hub.Handler(), "GET", "/api/peers/macmini/rooms", "", ownerHdr())
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("offline peer: %d %s", rec.Code, body)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ -run 'Proxy|PeerHealth' -count=1`
Expected: FAIL — `hub.checkPeers` undefined.

- [ ] **Step 3: Implement**

Delete `internal/web/stubs.go`, then:

```go
// internal/web/proxy.go
package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type peer struct {
	name   string
	base   *url.URL
	online atomic.Bool
	proxy  *httputil.ReverseProxy
	client *http.Client
}

func newPeer(p Peer) (*peer, error) {
	base, err := url.Parse(p.URL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return nil, errors.New("peer " + p.Name + ": URL must be http(s)://host/path/")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	pp := &peer{name: p.Name, base: base, client: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	pp.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // stream SSE
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := pr.In.PathValue("rest")
			pr.Out.URL = base.ResolveReference(&url.URL{Path: "api/" + rest, RawQuery: pr.In.URL.RawQuery})
			pr.Out.Host = base.Host
			for _, h := range []string{"Origin", "Cookie", "Referer", "Tailscale-User-Login", "Tailscale-User-Name", "Tailscale-User-Profile-Pic", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-Tincan-Request", "1")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			pp.online.Store(false)
			http.Error(w, "peer "+p.Name+" is offline", http.StatusBadGateway)
		},
	}
	return pp, nil
}

func (s *Server) proxyPeer(w http.ResponseWriter, r *http.Request) {
	p, ok := s.peers[r.PathValue("peer")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	rest := r.PathValue("rest")
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." || seg == "." {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}
	if strings.HasPrefix(rest, "peers/") {
		http.Error(w, "peers are not chained", http.StatusBadRequest)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

func (s *Server) checkPeers(ctx context.Context) {
	for _, p := range s.peers {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", p.base.ResolveReference(&url.URL{Path: "api/self"}).String(), nil)
		resp, err := p.client.Do(req)
		online := err == nil && resp.StatusCode == http.StatusOK
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		if p.online.Swap(online) != online {
			s.hub.publish(note{Kind: "peer"})
		}
	}
}

func (s *Server) watchPeers(ctx context.Context) {
	if len(s.peers) == 0 {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.checkPeers(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
```

The proxy's upstream request reaches the peer's `tailscale serve`, which sets the peer-side identity header for the hub device's owner; in tests `servedLike` plays that role.

- [ ] **Step 4: Run tests**

Run: `go vet ./... && go test ./internal/web/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A internal/web
git commit -m "Proxy a peer tincan web with health tracking"
```

---

### Task 12: UI

**Files:**
- Modify: `internal/web/ui/index.html`, `internal/web/ui/app.js`, `internal/web/ui/app.css`
- Create: `internal/web/ui_test.go`

**Interfaces:**
- Consumes: every API route of Tasks 8–11; SSE notes `{kind, room, thread, seq}`.
- Produces: the browser UI (layout B). URL state in `location.hash`: `#/<machine>/<rid>/<tid|activity>` where `<machine>` is `local` or a peer name.

- [ ] **Step 1: Write the failing test (UI contract)**

```go
// internal/web/ui_test.go
package web

import (
	"strings"
	"testing"
)

// The UI must build every URL from the <meta> base, send the CSRF header on
// mutations, and never inject server text with innerHTML except the
// server-rendered "html" field.
func TestUIContract(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{`meta[name="tincan-base"]`, `"X-Tincan-Request"`, "new EventSource(", "client_id", "api/peers/"} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if n := strings.Count(src, "innerHTML"); n != 1 {
		t.Errorf("innerHTML used %d times; only the server-rendered message html may use it", n)
	}
	html, _ := uiFS.ReadFile("ui/index.html")
	if strings.Contains(string(html), "<script>") || strings.Contains(string(html), "style=") {
		t.Error("index.html has inline script or style (blocked by CSP)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run UIContract -count=1`
Expected: FAIL — app.js is empty.

- [ ] **Step 3: Write `index.html`**

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="tincan-base" content="{{.Base}}">
<title>tincan</title>
<link rel="stylesheet" href="{{.Base}}app.css">
<script defer src="{{.Base}}app.js"></script>
</head>
<body>
<div id="app">
  <aside id="sidebar">
    <div class="brand"><button id="close-drawer" class="icon" aria-label="Close menu">✕</button>tincan</div>
    <nav id="machines"></nav>
    <label class="toggle"><input type="checkbox" id="show-hidden"> show hidden rooms</label>
  </aside>
  <main id="main">
    <header id="top">
      <button id="open-drawer" class="icon" aria-label="Menu">☰</button>
      <h1 id="title">Pick a room</h1>
      <div id="chips"></div>
      <div class="actions">
        <button id="archive" class="secondary" hidden>Archive</button>
        <button id="stop" class="danger" hidden>■ Stop</button>
      </div>
    </header>
    <section id="view"></section>
    <form id="composer" hidden>
      <div id="suggest" hidden></div>
      <textarea id="input" rows="2" placeholder="Message… (@ to mention an agent, Enter to send, Shift+Enter for a new line)"></textarea>
      <button type="submit">Send</button>
    </form>
  </main>
</div>
<dialog id="new-thread">
  <form method="dialog" id="new-thread-form">
    <h2>New thread</h2>
    <label>Title <input id="nt-title" required maxlength="120"></label>
    <label>Primary agent <select id="nt-primary"></select></label>
    <menu><button value="cancel" formnovalidate class="secondary">Cancel</button><button value="ok">Create</button></menu>
  </form>
</dialog>
</body>
</html>
```

- [ ] **Step 4: Write `app.css`**

```css
:root {
  --bg: #ffffff; --fg: #1d1d1f; --muted: #6e6e73; --line: #e2e2e6; --side: #f5f5f7;
  --accent: #2f6fec; --accent-bg: #e6eefc; --busy: #d99a00; --ok: #2f9e44; --err: #d6336c; --code: #f1f1f4;
}
@media (prefers-color-scheme: dark) {
  :root { --bg: #17171a; --fg: #ececf1; --muted: #9a9aa3; --line: #2c2c31; --side: #1e1e22;
          --accent: #6d9bff; --accent-bg: #22304d; --code: #24242a; }
}
* { box-sizing: border-box; }
html, body { margin: 0; height: 100%; background: var(--bg); color: var(--fg);
  font: 15px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif; }
button { font: inherit; border: 1px solid var(--line); background: var(--bg); color: var(--fg);
  border-radius: 8px; padding: 6px 12px; cursor: pointer; }
button.danger { color: var(--err); border-color: var(--err); }
button.secondary { color: var(--muted); }
button.icon { border: 0; padding: 4px 8px; }
#app { display: flex; height: 100%; }
#sidebar { width: 260px; flex-shrink: 0; background: var(--side); border-right: 1px solid var(--line);
  overflow-y: auto; padding: 12px; }
.brand { font-weight: 700; margin-bottom: 12px; display: flex; gap: 6px; align-items: center; }
#close-drawer, #open-drawer { display: none; }
.machine { margin-bottom: 14px; }
.machine > .name { font-size: 11px; text-transform: uppercase; letter-spacing: .05em; color: var(--muted);
  display: flex; align-items: center; gap: 6px; margin: 6px 0; }
.machine.offline { opacity: .5; }
.dot { width: 8px; height: 8px; border-radius: 50%; display: inline-block; background: var(--muted); }
.dot.on { background: var(--ok); } .dot.busy { background: var(--busy); }
.room > .name { font-weight: 600; padding: 4px 6px; border-radius: 6px; cursor: pointer; }
.item { padding: 3px 6px 3px 18px; border-radius: 6px; cursor: pointer; color: var(--fg);
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.item.on, .room > .name.on { background: var(--accent-bg); }
.item.new { color: var(--muted); }
.item .count { color: var(--busy); font-size: 12px; }
.toggle { font-size: 12px; color: var(--muted); }
#main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
#top { display: flex; align-items: center; gap: 10px; padding: 10px 16px; border-bottom: 1px solid var(--line); }
#title { font-size: 16px; margin: 0; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
#chips { display: flex; gap: 6px; flex-wrap: wrap; flex: 1; }
.chip { border: 1px solid var(--line); border-radius: 12px; padding: 1px 8px; font-size: 12px;
  display: inline-flex; align-items: center; gap: 5px; }
.actions { display: flex; gap: 8px; }
#view { flex: 1; overflow-y: auto; padding: 16px; }
.msg { margin: 0 0 14px; max-width: 900px; }
.msg .who { font-weight: 600; font-size: 13px; }
.msg .who .when { font-weight: 400; color: var(--muted); margin-left: 6px; }
.msg.user .who { color: var(--accent); }
.msg.system { color: var(--muted); font-size: 13px; }
.msg .body p { margin: 4px 0; }
.msg .body pre { background: var(--code); padding: 8px 10px; border-radius: 8px; overflow-x: auto; }
.msg .body code { background: var(--code); padding: 1px 4px; border-radius: 4px; font-size: 13px; }
.msg .body pre code { padding: 0; background: none; }
.mention { color: var(--accent); font-weight: 600; }
.working { color: var(--busy); font-size: 13px; }
details.live { margin-top: 4px; }
details.live pre { max-height: 240px; overflow: auto; font-size: 12px; background: var(--code);
  padding: 8px; border-radius: 8px; white-space: pre-wrap; }
.msg.error .body { border-left: 3px solid var(--err); padding-left: 10px; color: var(--err); }
.msg .tools { display: flex; gap: 8px; margin-top: 6px; }
#composer { display: flex; gap: 8px; padding: 10px 16px; border-top: 1px solid var(--line); position: relative; }
#composer[hidden] { display: none; }
#input { flex: 1; resize: vertical; font: inherit; padding: 8px 10px; border: 1px solid var(--line);
  border-radius: 8px; background: var(--bg); color: var(--fg); }
#suggest { position: absolute; bottom: 100%; left: 16px; background: var(--bg); border: 1px solid var(--line);
  border-radius: 8px; padding: 4px; min-width: 200px; box-shadow: 0 4px 16px rgba(0,0,0,.15); }
#suggest div { padding: 4px 8px; border-radius: 6px; cursor: pointer; }
#suggest div.on { background: var(--accent-bg); }
table.activity { border-collapse: collapse; width: 100%; font-size: 13px; }
table.activity td, table.activity th { border-bottom: 1px solid var(--line); padding: 6px; text-align: left; vertical-align: top; }
.muted { color: var(--muted); }
dialog { border: 1px solid var(--line); border-radius: 12px; background: var(--bg); color: var(--fg); min-width: 320px; }
dialog label { display: block; margin: 10px 0; }
dialog input, dialog select { display: block; width: 100%; margin-top: 4px; font: inherit; padding: 6px;
  background: var(--bg); color: var(--fg); border: 1px solid var(--line); border-radius: 6px; }
dialog menu { display: flex; justify-content: flex-end; gap: 8px; padding: 0; }
@media (max-width: 768px) {
  #sidebar { position: fixed; inset: 0 auto 0 0; z-index: 10; transform: translateX(-100%); transition: transform .2s; }
  #app.drawer #sidebar { transform: none; }
  #close-drawer, #open-drawer { display: inline-block; }
  #view { padding: 12px; }
}
```

- [ ] **Step 5: Write `app.js`**

```js
// tincan web UI. Plain JS; every URL is built from the <meta> base. The only
// HTML injected is the server-rendered, escaped message body (renderBody).
"use strict";
const BASE = document.querySelector('meta[name="tincan-base"]').content;
const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

const state = {
  machines: [],        // [{key, name, online}]
  rooms: {},           // key -> [room]
  threads: {},         // key/rid -> [thread]
  current: null,       // {key, rid, tid}  tid === "activity" for the activity view
  messages: new Map(), // id -> message (current thread)
  meta: null,
  chains: {},
  seq: 0,
  agents: { presets: [], listeners: [] },
  progress: new Map(), // request id -> {cursor, text}
  sources: {},
};

function prefix(key) { return key === "local" ? "api/" : `api/peers/${encodeURIComponent(key)}/`; }

async function api(key, path, opts = {}) {
  const init = { method: opts.method || "GET", headers: {} };
  if (init.method !== "GET") {
    init.headers["X-Tincan-Request"] = "1";
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body || {});
  }
  const res = await fetch(BASE + prefix(key) + path, init);
  const ct = res.headers.get("Content-Type") || "";
  const data = ct.includes("json") ? await res.json() : await res.text();
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

function clientId() { return (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random()).replace(/[^a-z0-9-]/gi, ""); }

function parseHash() {
  const [key, rid, tid] = location.hash.replace(/^#\//, "").split("/").map(decodeURIComponent);
  return key && rid ? { key, rid, tid: tid || "activity" } : null;
}

function go(key, rid, tid) {
  location.hash = `#/${encodeURIComponent(key)}/${rid}/${tid}`;
  $("app").classList.remove("drawer");
}

// ---------- sidebar ----------
async function loadMachines() {
  const self = await api("local", "self");
  state.machines = [{ key: "local", name: self.machine, online: true },
    ...self.peers.map((p) => ({ key: p.name, name: p.name, online: p.online }))];
  await Promise.all(state.machines.map(loadRooms));
  renderSidebar();
}

async function loadRooms(m) {
  if (!m.online) { state.rooms[m.key] = []; return; }
  try {
    state.rooms[m.key] = await api(m.key, "rooms");
    await Promise.all(state.rooms[m.key].filter((r) => !r.missing).map((r) => loadThreads(m.key, r.id)));
  } catch (e) { m.online = false; state.rooms[m.key] = []; }
}

async function loadThreads(key, rid) {
  state.threads[key + "/" + rid] = await api(key, `rooms/${rid}/threads`);
}

function renderSidebar() {
  const nav = $("machines");
  nav.replaceChildren();
  const showHidden = $("show-hidden").checked;
  const cur = state.current || {};
  for (const m of state.machines) {
    const box = el("div", "machine" + (m.online ? "" : " offline"));
    const name = el("div", "name");
    name.append(el("span", "dot" + (m.online ? " on" : "")), document.createTextNode(m.name + (m.online ? "" : " (offline)")));
    box.append(name);
    for (const r of state.rooms[m.key] || []) {
      if ((r.hidden && !showHidden) || r.missing) continue;
      const room = el("div", "room");
      const rn = el("div", "name" + (cur.key === m.key && cur.rid === r.id ? " on" : ""), "# " + r.name);
      rn.title = r.path;
      rn.onclick = () => go(m.key, r.id, "activity");
      room.append(rn);
      for (const t of state.threads[m.key + "/" + r.id] || []) {
        if (t.status === "archived" && !(cur.tid === t.id)) continue;
        const it = el("div", "item" + (cur.tid === t.id ? " on" : ""), "› " + t.title);
        if (t.running) it.append(el("span", "count", ` ${t.running}●`));
        it.onclick = () => go(m.key, r.id, t.id);
        room.append(it);
      }
      const act = el("div", "item" + (cur.key === m.key && cur.rid === r.id && cur.tid === "activity" ? " on" : ""),
        `› Activity${r.running ? ` (${r.running} running)` : ""}`);
      act.onclick = () => go(m.key, r.id, "activity");
      const add = el("div", "item new", "+ new thread");
      add.onclick = () => newThread(m.key, r.id);
      room.append(act, add);
      box.append(room);
    }
    nav.append(box);
  }
}

// ---------- new thread ----------
async function newThread(key, rid) {
  const agents = await api(key, `rooms/${rid}/agents`);
  const sel = $("nt-primary");
  sel.replaceChildren(...[...agents.presets, ...agents.listeners].map((a) => el("option", "", a)));
  $("nt-title").value = "";
  const dlg = $("new-thread");
  dlg.returnValue = "";
  dlg.showModal();
  dlg.onclose = async () => {
    if (dlg.returnValue !== "ok") return;
    try {
      const t = await api(key, `rooms/${rid}/threads`, { method: "POST",
        body: { title: $("nt-title").value, primary: sel.value, client_id: clientId() } });
      await loadThreads(key, rid);
      go(key, rid, t.id);
    } catch (e) { alert(e.message); }
  };
}

// ---------- thread view ----------
async function openCurrent() {
  const cur = parseHash();
  state.current = cur;
  state.messages = new Map();
  state.progress = new Map();
  state.seq = 0;
  state.meta = null;
  renderSidebar();
  $("view").replaceChildren();
  $("composer").hidden = true;
  $("stop").hidden = $("archive").hidden = true;
  $("chips").replaceChildren();
  if (!cur) { $("title").textContent = "Pick a room"; return; }
  if (cur.tid === "activity") return showActivity();
  state.agents = await api(cur.key, `rooms/${cur.rid}/agents`).catch(() => ({ presets: [], listeners: [] }));
  await refreshMessages();
}

async function refreshMessages() {
  const cur = state.current;
  if (!cur || cur.tid === "activity") return;
  const page = await api(cur.key, `rooms/${cur.rid}/threads/${cur.tid}/messages?after=${state.seq}`);
  state.meta = page.meta;
  state.chains = page.chains || {};
  state.seq = page.seq;
  for (const m of page.messages) state.messages.set(m.id, m);
  renderThread();
}

function sorted() { return [...state.messages.values()].sort((a, b) => a.n - b.n); }

function renderThread() {
  const meta = state.meta;
  $("title").textContent = meta.title;
  const archived = meta.status === "archived";
  $("composer").hidden = archived;
  $("stop").hidden = archived;
  $("archive").hidden = false;
  $("archive").textContent = archived ? "Unarchive" : "Archive";
  const chips = new Map();
  for (const name of Object.keys(meta.listeners || {})) chips.set(name, "idle");
  for (const m of sorted()) if (m.role === "agent" && m.listener) {
    if (m.state === "running" || m.state === "pending") chips.set(m.listener, "busy");
    else if (!chips.has(m.listener)) chips.set(m.listener, "idle");
  }
  $("chips").replaceChildren(...[...chips].map(([name, s]) => {
    const c = el("span", "chip");
    c.append(el("span", "dot" + (s === "busy" ? " busy" : " on")), document.createTextNode(name));
    return c;
  }));
  const view = $("view");
  const nearBottom = view.scrollHeight - view.scrollTop - view.clientHeight < 80;
  view.replaceChildren(...sorted().map(renderMessage));
  if (nearBottom) view.scrollTop = view.scrollHeight;
}

function renderBody(m) {
  const body = el("div", "body");
  body.innerHTML = m.html; // server-rendered with every input escaped (RenderMarkdown)
  return body;
}

function renderMessage(m) {
  const box = el("div", `msg ${m.role}` + (m.state === "error" || m.state === "uncollectable" ? " error" : ""));
  const who = el("div", "who", m.author);
  who.append(el("span", "when", new Date(m.time).toLocaleTimeString()));
  box.append(who);
  if (m.state === "pending" || m.state === "running") {
    const c = state.chains[m.chain];
    const used = c ? c.used : 0;
    box.append(el("div", "working", `working… (${used}/${state.meta.budget} handoffs used)`));
    const live = el("details", "live");
    live.append(el("summary", "", "live output"));
    const pre = el("pre", "", (state.progress.get(m.request_id) || {}).text || "");
    live.append(pre);
    box.append(live);
    return box;
  }
  if (m.state === "suggested") {
    box.append(el("div", "", "Handoff not sent: chain budget reached."));
    const send = el("button", "", "Send");
    send.onclick = () => post(m.text);
    box.append(el("div", "muted", m.text), el("div", "tools")).lastChild.append(send);
    return box;
  }
  if (m.state === "uncollectable") { box.append(el("div", "body", "result expired")); return box; }
  box.append(m.text ? renderBody(m) : el("div", "body muted", m.role === "agent" ? "(no output)" : ""));
  if (m.state === "error" || m.state === "cancelled") {
    const tools = el("div", "tools");
    const retry = el("button", "", "Retry");
    retry.onclick = () => act(`messages/${m.id}/retry`);
    const log = el("button", "secondary", "Open log");
    log.onclick = () => openLog(m.listener);
    tools.append(retry, log);
    box.append(tools);
  }
  return box;
}

async function pollProgress() {
  const cur = state.current;
  if (!cur || cur.tid === "activity") return;
  let changed = false;
  for (const m of state.messages.values()) {
    if (m.state !== "running" || !m.request_id) continue;
    const p = state.progress.get(m.request_id) || { cursor: 0, text: "" };
    try {
      const r = await api(cur.key, `rooms/${cur.rid}/requests/${m.request_id}/progress?cursor=${p.cursor}`);
      if (r.events && r.events.length) {
        p.text = (p.text + r.events.map((e) => e.text).join("")).slice(-20000);
        changed = true;
      }
      p.cursor = r.next_cursor;
      state.progress.set(m.request_id, p);
    } catch (e) { /* next poll retries */ }
  }
  if (changed) renderThread();
}

async function act(path, body) {
  const cur = state.current;
  try {
    await api(cur.key, `rooms/${cur.rid}/threads/${cur.tid}/${path}`, { method: "POST", body });
    await refreshMessages();
  } catch (e) { alert(e.message); }
}

async function post(text) {
  const cur = state.current;
  if (!text.trim()) return;
  try {
    await api(cur.key, `rooms/${cur.rid}/threads/${cur.tid}/messages`, { method: "POST", body: { text, client_id: clientId() } });
    await refreshMessages();
  } catch (e) { alert(e.message); }
}

async function openLog(name) {
  const cur = state.current;
  try {
    const text = await api(cur.key, `rooms/${cur.rid}/agents/${encodeURIComponent(name)}/log`);
    const w = window.open("", "_blank");
    if (w) { const pre = w.document.createElement("pre"); pre.textContent = text; w.document.body.append(pre); }
  } catch (e) { alert(e.message); }
}

// ---------- activity ----------
async function showActivity() {
  const cur = state.current;
  const room = (state.rooms[cur.key] || []).find((r) => r.id === cur.rid);
  $("title").textContent = (room ? room.name : "") + " · Activity";
  const data = await api(cur.key, `rooms/${cur.rid}/activity`);
  const view = $("view");
  const lt = el("table", "activity");
  lt.append(row("th", ["Listener", "Mode", "State", ""]));
  for (const l of data.listeners) {
    const tr = row("td", [l.name, l.mode + (l.preset ? ` · ${l.preset}` : ""), l.busy ? "busy" : "idle", ""]);
    if (l.mode === "hosted" && !l.name.includes(".")) {
      const b = el("button", "", "Message this agent");
      b.onclick = async () => {
        const t = await api(cur.key, `rooms/${cur.rid}/threads`, { method: "POST", body: { title: `with ${l.name}`, primary: l.name, client_id: clientId() } });
        await loadThreads(cur.key, cur.rid);
        go(cur.key, cur.rid, t.id);
      };
      tr.lastChild.append(b);
    }
    lt.append(tr);
  }
  const rt = el("table", "activity");
  rt.append(row("th", ["Request", "From → agent", "Status", "Updated", "Result"]));
  for (const r of data.requests) {
    rt.append(row("td", [r.id, `${r.from || "?"} → ${r.agent}`, r.status, new Date(r.updated).toLocaleString(), r.result || ""]));
  }
  view.replaceChildren(el("h3", "", "Listeners"), data.listeners.length ? lt : el("p", "muted", "No listeners."),
    el("h3", "", "Recent requests"), data.requests.length ? rt : el("p", "muted", "No journaled requests."));
}

function row(cell, values) {
  const tr = el("tr");
  for (const v of values) tr.append(el(cell, "", v));
  return tr;
}

// ---------- composer + @ autocomplete ----------
function mentionAtCursor(input) {
  const upto = input.value.slice(0, input.selectionStart);
  const m = upto.match(/(^|[\s(\[,])@([A-Za-z0-9._-]*)$/);
  return m ? { start: upto.length - m[2].length, word: m[2] } : null;
}

function updateSuggest() {
  const input = $("input");
  const box = $("suggest");
  const at = mentionAtCursor(input);
  const names = [...state.agents.presets, ...Object.keys((state.meta || {}).listeners || {}), ...state.agents.listeners, "you"];
  const hits = at ? [...new Set(names)].filter((n) => n.startsWith(at.word)).slice(0, 8) : [];
  box.hidden = hits.length === 0;
  box.replaceChildren(...hits.map((n, i) => {
    const d = el("div", i === 0 ? "on" : "", "@" + n);
    d.onmousedown = (e) => { e.preventDefault(); complete(n); };
    return d;
  }));
}

function complete(name) {
  const input = $("input");
  const at = mentionAtCursor(input);
  if (!at) return;
  input.value = input.value.slice(0, at.start) + name + " " + input.value.slice(input.selectionStart);
  $("suggest").hidden = true;
  input.focus();
}

function wireComposer() {
  const input = $("input");
  input.addEventListener("input", updateSuggest);
  input.addEventListener("keydown", (e) => {
    const box = $("suggest");
    if (!box.hidden && (e.key === "Tab" || e.key === "Enter")) {
      e.preventDefault();
      complete(box.querySelector(".on").textContent.slice(1));
      return;
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      $("composer").requestSubmit();
    }
  });
  $("composer").addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = input.value;
    input.value = "";
    $("suggest").hidden = true;
    await post(text);
  });
  $("stop").onclick = () => act("stop");
  $("archive").onclick = () => act("archive", { archived: state.meta.status !== "archived" });
  $("open-drawer").onclick = () => $("app").classList.add("drawer");
  $("close-drawer").onclick = () => $("app").classList.remove("drawer");
  $("show-hidden").onchange = renderSidebar;
}

// ---------- live updates ----------
function connect(key) {
  if (state.sources[key]) state.sources[key].close();
  const src = new EventSource(BASE + prefix(key) + "events");
  state.sources[key] = src;
  src.onopen = () => { if (key === "local") loadMachines().then(openCurrent); };
  src.onmessage = async (ev) => {
    const n = JSON.parse(ev.data);
    const cur = state.current;
    if (n.kind === "peer") { await loadMachines(); state.machines.forEach((m) => m.key !== "local" && m.online && connect(m.key)); return; }
    if (n.room) await loadThreads(key, n.room).catch(() => {});
    if (n.kind === "activity" || n.kind === "thread") {
      const m = state.machines.find((x) => x.key === key);
      if (m) await loadRooms(m);
    }
    renderSidebar();
    if (!cur || cur.key !== key || cur.rid !== n.room) return;
    if (cur.tid === "activity" && n.kind === "activity") showActivity();
    if (n.thread === cur.tid) refreshMessages();
  };
}

async function start() {
  wireComposer();
  window.addEventListener("hashchange", openCurrent);
  await loadMachines();
  connect("local");
  for (const m of state.machines) if (m.key !== "local" && m.online) connect(m.key);
  await openCurrent();
  setInterval(pollProgress, 1500);
}

start().catch((e) => { $("title").textContent = "tincan: " + e.message; });
```

- [ ] **Step 6: Run the UI contract test and the full package**

Run: `go test ./internal/web/ -count=1`
Expected: PASS (`TestUIContract` finds exactly one `innerHTML`, the required strings, and no inline script/style in `index.html`).

- [ ] **Step 7: Manual smoke test in a real browser**

```bash
go build -o /tmp/tincan-web-smoke ./cmd/tincan
TINCAN_STATE_DIR=$(mktemp -d) /tmp/tincan-web-smoke web --listen 127.0.0.1:7799 --owner smoke@local --scan "$PWD/.." &
```

A browser cannot add the `Tailscale-User-Login` header itself, so for this local smoke test run a small header-injecting proxy in front of it:

```bash
python3 - <<'EOF' &
import http.server, urllib.request
class P(http.server.BaseHTTPRequestHandler):
    def _go(self):
        body = self.rfile.read(int(self.headers.get('Content-Length') or 0)) or None
        req = urllib.request.Request('http://127.0.0.1:7799' + self.path, data=body, method=self.command)
        for k, v in self.headers.items():
            if k.lower() not in ('host',): req.add_header(k, v)
        req.add_header('Tailscale-User-Login', 'smoke@local')
        try: r = urllib.request.urlopen(req, timeout=300)
        except urllib.error.HTTPError as e: r = e
        self.send_response(r.status)
        for k, v in r.headers.items():
            if k.lower() not in ('transfer-encoding', 'connection'): self.send_header(k, v)
        self.end_headers()
        while True:
            chunk = r.read1(65536) if hasattr(r, 'read1') else r.read(65536)
            if not chunk: break
            self.wfile.write(chunk); self.wfile.flush()
    do_GET = do_POST = do_PATCH = _go
http.server.ThreadingHTTPServer(('127.0.0.1', 7800), P).serve_forever()
EOF
```

Then open `http://127.0.0.1:7800/` and check: rooms listed; create a thread with a real preset (e.g. `claude` or `codex`); post "@codex say hi"; live output appears; a reply that mentions another agent hands off; Stop cancels a running turn; Archive/Unarchive; at 375 px width the sidebar is a drawer. Kill both background processes afterwards.

- [ ] **Step 8: Commit**

```bash
git add internal/web/ui internal/web/ui_test.go
git commit -m "Add tincan web chat UI"
```

---

### Task 13: Documentation and service files

**Files:**
- Create: `docs/web.md`, `deploy/systemd/tincan-web.service`, `deploy/launchd/net.tincan.web.plist`
- Modify: `README.md` (feature list + link), `PROTOCOL.md` (command reference), `docs/README.md` (index)

- [ ] **Step 1: Write the service files**

```ini
# deploy/systemd/tincan-web.service — install to ~/.config/systemd/user/
[Unit]
Description=tincan web chat
After=network-online.target

[Service]
ExecStart=%h/.local/bin/tincan web --public-path /tincan --peer macmini=https://macmini.brill-decibel.ts.net/tincan/
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

```xml
<!-- deploy/launchd/net.tincan.web.plist — install to ~/Library/LaunchAgents/ -->
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>net.tincan.web</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/USER/.local/bin/tincan</string>
    <string>web</string>
    <string>--public-path</string><string>/tincan</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/Users/USER/Library/Logs/tincan-web.log</string>
</dict>
</plist>
```

- [ ] **Step 2: Write `docs/web.md`**

Content (complete; keep it in this order):

1. **What it is** — one paragraph: chat with tincan agents per project room; threads, @mentions, handoffs with a chain budget, Stop, Activity; one hub page for two machines.
2. **Threat model** — anyone who can call the API can run agents with your presets' permissions (e.g. `claude --dangerously-skip-permissions`) in registered rooms. `tincan web` listens on a private unix socket (or loopback TCP) and accepts only requests whose `Tailscale-User-Login` equals `--owner`; any process on any of your untagged devices is therefore trusted. Do not tag these machines; do not expose the socket or port any other way.
3. **Run it** — flags table: `--listen`, `--public-path`, `--owner`, `--origin`, `--peer`, `--scan`, `--chain-budget`, `--idle-stop` with defaults from `tincan web -h`.
4. **Expose it with Tailscale** — `tailscale serve --bg --set-path /tincan unix:$XDG_RUNTIME_DIR/tincan/web.sock` (Linux); on macOS use `/Applications/Tailscale.app/Contents/MacOS/Tailscale serve --bg --set-path /tincan unix:$HOME/.local/state/tincan/web.sock`, or `--listen 127.0.0.1:7788` and `… serve --bg --set-path /tincan http://127.0.0.1:7788` if the app cannot reach the socket.
5. **Two machines** — start the hub with `--peer macmini=https://macmini.<tailnet>.ts.net/tincan/`; open `https://<hub>.<tailnet>.ts.net/tincan/`.
6. **Services** — systemd user unit (`systemctl --user enable --now tincan-web`, `loginctl enable-linger $USER` so it runs without a login session) and launchd agent (`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/net.tincan.web.plist`), referencing `deploy/`.
7. **Using threads** — primary agent, `@name` rules (code ignored, `@you`), who can be mentioned (presets on that machine, this thread's listeners, hosted room listeners; not `/listen` participants), full bounded transcript per turn, chain budget and suggestions, Stop and archive semantics, idle stop and resume.
8. **Limits** — no dispatch to `/listen` participants; CLI `send`/`ask` not shown in Activity; no Tailscale Service hostname (tagging breaks identity); Windows thread listeners need a foreground `tincan serve`.

- [ ] **Step 3: Link it**

- `README.md`: add a bullet under the feature list: "**`tincan web`** — a tailnet-only chat UI: threads per project, `@codex review claude's change`, agents hand work to each other, one page for several machines. See [`docs/web.md`](docs/web.md)."
- `PROTOCOL.md`: add `tincan web [flags]` to the command block after `tincan mcp`, with one sentence and a link to `docs/web.md`.
- `docs/README.md`: add `web.md` to the current guides and the spec/plan to the design documents list.

- [ ] **Step 4: Verify and commit**

Run: `go build ./... && go run ./cmd/tincan web -h 2>&1 | head -20`
Expected: flag list matches `docs/web.md`.

```bash
git add docs deploy README.md PROTOCOL.md
git commit -m "Document tincan web and add service files"
```

---

### Task 14: Deploy to cachyos (hub) and macmini (peer) and verify through real Tailscale

This task changes live machines; confirm with the owner before starting it. Every command below is run by the implementer; nothing needs the owner's password except where noted.

- [ ] **Step 1: Build and install on both machines**

On the Mac (this checkout, after merging to main):

```bash
C=$(git rev-parse HEAD); R=~/.local/share/tincan/releases/$C; mkdir -p $R
go build -trimpath -o $R/tincan ./cmd/tincan
for l in ~/.local/bin/tincan ~/go/bin/tincan ~/.local/share/mise/installs/go/*/bin/tincan; do [ -L "$l" ] && ln -sfn "$R/tincan" "$l"; done
tincan version
```

On cachyos:

```bash
ssh cachyos 'cd ~/projects/tincan && git pull --ff-only && go build -trimpath -o ~/go/bin/.tincan.new ./cmd/tincan && mv -f ~/go/bin/.tincan.new ~/go/bin/tincan && tincan version'
```

- [ ] **Step 2: Start the peer on the Mac**

```bash
sed "s#/Users/USER#$HOME#g" deploy/launchd/net.tincan.web.plist > ~/Library/LaunchAgents/net.tincan.web.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/net.tincan.web.plist
TS=/Applications/Tailscale.app/Contents/MacOS/Tailscale
$TS serve --bg --set-path /tincan unix:$HOME/.local/state/tincan/web.sock
```

Verify from cachyos: `ssh cachyos 'curl -sS https://macmini.brill-decibel.ts.net/tincan/api/self'` → JSON with `"machine":"macmini"`. If it fails with a proxy error, the app cannot reach the socket: edit the plist to add `--listen 127.0.0.1:7788`, `launchctl kickstart -k gui/$(id -u)/net.tincan.web`, and `$TS serve --bg --set-path /tincan http://127.0.0.1:7788`; re-verify.

- [ ] **Step 3: Start the hub on cachyos**

```bash
ssh cachyos 'mkdir -p ~/.config/systemd/user && cp ~/projects/tincan/deploy/systemd/tincan-web.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user enable --now tincan-web && loginctl enable-linger $USER; sleep 2; systemctl --user status tincan-web --no-pager | head -5'
ssh cachyos 'tailscale serve --bg --set-path /tincan unix:$XDG_RUNTIME_DIR/tincan/web.sock; tailscale serve status'
```

If `tailscale serve` needs root on cachyos (operator not set), ask the owner to run `sudo tailscale set --operator=$USER` once.

- [ ] **Step 4: Verify identity, mount path and proxying through real Serve**

From the Mac (an owner device):

```bash
curl -sS https://cachyos.brill-decibel.ts.net/tincan/api/self            # machine cachyos, peers [{macmini, online:true}]
curl -sS https://cachyos.brill-decibel.ts.net/tincan/api/peers/macmini/rooms | head -c 300
curl -sS -o /dev/null -w '%{http_code}\n' https://cachyos.brill-decibel.ts.net/tincan/   # 200
curl -sS -o /dev/null -w '%{http_code}\n' -X POST https://cachyos.brill-decibel.ts.net/tincan/api/rooms   # 403 (no CSRF header)
```

A request from a non-owner identity: if a shared-in user or tagged device is available, `curl` from it must return 403; otherwise record that this check was covered by `TestOwnerCheck` only.

- [ ] **Step 5: UI acceptance in a real browser**

Open `https://cachyos.brill-decibel.ts.net/tincan/` in the built-in browser. Check, and record the results:
1. Both machines listed; `heliobane-amiga` and `tincan` rooms present.
2. New thread in a Mac room with primary `codex`; post "say hi in one line" → reply appears; live output visible while running.
3. Post "@claude summarize what codex said, then ask @codex to confirm" → a handoff to codex runs automatically.
4. Stop during a running turn → turn cancelled, status back to open.
5. Archive, then Unarchive.
6. Phone width (375 px): sidebar drawer works.

- [ ] **Step 6: Commit any deployment fixes and report**

```bash
git add -A && git commit -m "Adjust tincan web deployment after live verification" || true
```

Report: URLs, which listen mode each machine uses, and the acceptance results.
