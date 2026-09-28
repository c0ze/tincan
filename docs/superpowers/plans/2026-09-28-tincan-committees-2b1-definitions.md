# tincan committees 2b-1: definitions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the owner define committees (named groups of `preset@machine` reviewers) in the web UI, stored on a hub machine and cached by its peer, with every machine publishing a redacted preset catalogue and a coordinator heartbeat.

**Architecture:** A new `internal/committee` package holds the model, validation, the hub's file store and the peer's cache. `tincan web` gains four things:
- a heartbeat file;
- a direct peer client for machine-to-machine calls (separate from the browser proxy);
- the `api/presets` catalogue;
- the `api/committees` routes, with a polling cache when started with `--committees-from <peer>`.

The UI gets a Committees page. Nothing runs reviews yet; that is 2b-2 to 2b-4.

**Tech Stack:** Go 1.25 (standard library, existing `internal/*` packages), plain JS in `internal/web/ui`.

**Spec:** `docs/superpowers/specs/2026-09-28-tincan-committees-design.md`: §6.1 (definitions), §6.3 step 1 (heartbeat), §6.7 (`api/presets`, `api/committees`, outbound peer client, change notes), §9.

## Roadmap for phase 2b

Each plan ships on its own, and each later plan is written after the previous one lands, against the real code.

| Plan | Delivers | Spec |
|---|---|---|
| **2b-1 definitions (this plan)** | Committees store/cache/API/UI, `api/presets`, heartbeat, peer client, machine names | §6.1, §6.3.1, §6.7 |
| 2b-2 packets and jobs | Packet builder (scopes, snapshot, omissions, limits), receiver jobs (identity, workspace materialization in checkout and packet-only modes, path policy, pinned executables, create/status/cancel/ack, janitor), job API | §6.5, §6.6, §6.8 |
| 2b-3 reviews | Publication, per-room review reconcile (obligations, close, bundle, settle), MCP `tincan_review*`, CLI `tincan review`, review API and UI list, quota skip | §6.2–§6.4, §6.9, §6.10 (MCP/CLI), §8 |
| 2b-4 threads | `@committee` mentions, keyed reservations, results in threads, Stop/archive branch, synthesis turn, review cards | §6.10 (threads), §10 |

## Global Constraints

- Committee `name`: `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`. It must not be `you` and must not equal a preset name on any known machine.
- `members`: 1–8 unique `preset@machine`. `machine` is this machine or a configured peer. Each member must resolve, be available, and have a bare-name or absolute executable on its machine at save time.
- Members whose argv contains a known permission bypass (`--dangerously-skip-permissions`, `--always-approve`, `--yolo`, `-s danger-full-access`, `--sandbox danger-full-access`) are accepted with a visible warning.
- `deadline_minutes`: 1–240, default 30. `instructions`: ≤ 8 KiB. `version` increments on every save. `skip_exhausted` defaults to false.
- Hub storage is `<state>/committees.json`, under a file lock with atomic replace. The peer cache is `<state>/committees-cache.json`, carrying `fetched_at`.
- A peer refreshes every 60 s, and on `GET api/committees` when its cache is older than 10 s. Edits go to the hub only; a peer answers 409.
- `<state>/web.json` carries `pid`, `machine`, `started` and `updated`. It is written at least every 10 s and removed on clean shutdown. It counts as fresh when `updated` is under 30 s old.
- Machine name: the `--machine` flag, else the first label of this node's Tailscale DNS name, else the first label of the host name.
- Peer client:
  - calls the peer's configured public URL;
  - sets `X-Tincan-Request: 1`;
  - never sends `Origin`, `Tailscale-*`, `X-Forwarded-*`, `Cookie` or `Authorization`;
  - uses a 5 s dial and 30 s response-header timeout, a per-call deadline of 10 s unless the caller sets one, and caps the response body.
- `api/presets` never includes env values.
- `Config.StateDir == ""` disables the heartbeat, the store and the cache, so existing tests keep writing nothing to the real state directory.
- The UI keeps the single `innerHTML` sink, builds every URL from the base meta tag and sends `X-Tincan-Request` on mutations.
- No new dependencies. gofmt, `go vet ./...` and the Windows CI matrix must all stay clean. Never use `git stash`. Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Peer offline while saving.** A committee with a member on an unreachable peer must fail to save with a clear message naming the peer. It must never save unvalidated members. Pinned in Task 5.
2. **Concurrent saves** from two browser tabs must both persist, with versions `n+1` and `n+2`. No lost update, no torn file. Pinned in Task 1.
3. **Both machines configured as peers of each other** (each with `--committees-from` the other). The cache must report "X is not a committees hub" rather than loop or cache an empty list as valid. Pinned in Task 6.
4. **Default machine names** must match the peer names already in use (`macmini`, `cachyos`), not `Mac-mini` or `cachyos-desktop`. Pinned in Task 6.
5. **A malformed `committees.json`** (hand-edited) must make `GET api/committees` return 500 with the file path. It must never be silently treated as empty and then overwritten by the next save. Pinned in Task 1.

---

### Task 1: `internal/committee` — model, validation, hub store, peer cache

**Files:**
- Create: `internal/committee/committee.go`
- Create: `internal/committee/store.go`
- Test: `internal/committee/committee_test.go`, `internal/committee/store_test.go`

**Interfaces:**
- Produces the type `Committee`:

  ```go
  type Committee struct {
      Name            string   `json:"name"`
      Version         int      `json:"version"`
      Members         []string `json:"members"`
      DeadlineMinutes int      `json:"deadline_minutes"`
      Instructions    string   `json:"instructions,omitempty"`
      SkipExhausted   bool     `json:"skip_exhausted"`
  }
  ```

- Produces `type Member struct{ Preset, Machine string }`, plus:
  - `func ParseMember(s string) (Member, error)`
  - `func ValidName(name string) error`
  - `func (c *Committee) Normalize() error`
- Produces the hub store:
  - `func NewStore(stateDir string) *Store`
  - `(*Store).Path() string`
  - `(*Store).List() ([]Committee, error)`
  - `(*Store).Put(ctx context.Context, c Committee) (Committee, error)`
  - `(*Store).Delete(ctx context.Context, name string) (bool, error)`
- Produces the peer cache:
  - `type Cache struct { From string; FetchedAt time.Time; AttemptedAt time.Time; Error string; Committees []Committee }`, with JSON tags `from`, `fetched_at`, `attempted_at`, `error,omitempty` and `committees`
  - `func CachePath(stateDir string) string`
  - `func LoadCache(stateDir string) (Cache, bool, error)`
  - `func SaveCache(stateDir string, c Cache) error`
- Constants: `MaxMembers = 8`, `MaxInstructions = 8 << 10`, `DefaultDeadlineMinutes = 30`, `MaxDeadlineMinutes = 240`, `MaxCommittees = 64`.

- [ ] **Step 1: Write the failing tests.** Create `internal/committee/committee_test.go`:

```go
package committee

import (
	"strings"
	"testing"
)

func TestNormalizeDefaultsAndValidates(t *testing.T) {
	c := Committee{Name: "reviewers", Members: []string{"codex-gmail@cachyos", "claude-personal@macmini"}}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.DeadlineMinutes != DefaultDeadlineMinutes || c.SkipExhausted {
		t.Fatalf("defaults: %+v", c)
	}
	for name, bad := range map[string]Committee{
		"empty name":        {Name: "", Members: []string{"a@b"}},
		"reserved you":      {Name: "you", Members: []string{"a@b"}},
		"dot in name":       {Name: "a.b", Members: []string{"a@b"}},
		"leading dash":      {Name: "-a", Members: []string{"a@b"}},
		"long name":         {Name: strings.Repeat("a", 65), Members: []string{"a@b"}},
		"no members":        {Name: "a"},
		"too many":          {Name: "a", Members: []string{"a@m", "b@m", "c@m", "d@m", "e@m", "f@m", "g@m", "h@m", "i@m"}},
		"duplicate":         {Name: "a", Members: []string{"x@m", "x@m"}},
		"no machine":        {Name: "a", Members: []string{"codex"}},
		"bad machine":       {Name: "a", Members: []string{"codex@mac.mini"}},
		"deadline zero":     {Name: "a", Members: []string{"a@b"}, DeadlineMinutes: -1},
		"deadline too long": {Name: "a", Members: []string{"a@b"}, DeadlineMinutes: 241},
		"instructions":      {Name: "a", Members: []string{"a@b"}, Instructions: strings.Repeat("x", MaxInstructions+1)},
	} {
		c := bad
		if err := c.Normalize(); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}
}

func TestParseMember(t *testing.T) {
	m, err := ParseMember("claude-personal@macmini")
	if err != nil || m.Preset != "claude-personal" || m.Machine != "macmini" {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{"", "@m", "p@", "p@m@n", "../x@m", "p@M-1."} {
		if _, err := ParseMember(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
```

Create `internal/committee/store_test.go`:

```go
package committee

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorePutListDeleteVersions(t *testing.T) {
	s := NewStore(t.TempDir())
	ctx := context.Background()
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty store: %v %v", list, err)
	}
	c, err := s.Put(ctx, Committee{Name: "reviewers", Members: []string{"codex@cachyos"}})
	if err != nil || c.Version != 1 || c.DeadlineMinutes != DefaultDeadlineMinutes {
		t.Fatalf("first put: %+v %v", c, err)
	}
	c, err = s.Put(ctx, Committee{Name: "reviewers", Version: 99, Members: []string{"codex@cachyos", "grok@macmini"}})
	if err != nil || c.Version != 2 {
		t.Fatalf("second put must ignore the client's version: %+v %v", c, err)
	}
	s.Put(ctx, Committee{Name: "alpha", Members: []string{"kimi@cachyos"}})
	list, _ := s.List()
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "reviewers" {
		t.Fatalf("list not sorted: %+v", list)
	}
	if ok, err := s.Delete(ctx, "reviewers"); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := s.Delete(ctx, "reviewers"); ok {
		t.Fatal("deleted twice")
	}
	c, _ = s.Put(ctx, Committee{Name: "reviewers", Members: []string{"codex@cachyos"}})
	if c.Version != 1 {
		t.Fatalf("recreated committee version = %d", c.Version)
	}
}

func TestStoreConcurrentPutsKeepEveryVersion(t *testing.T) {
	s := NewStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(context.Background(), Committee{Name: "reviewers", Members: []string{"codex@cachyos"}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Version != 8 {
		t.Fatalf("lost update: %+v %v", list, err)
	}
}

func TestStoreMalformedFileIsAnErrorNotEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	os.WriteFile(s.Path(), []byte("{not json"), 0o600)
	if _, err := s.List(); err == nil || !strings.Contains(err.Error(), s.Path()) {
		t.Fatalf("malformed file: %v", err)
	}
	if _, err := s.Put(context.Background(), Committee{Name: "a", Members: []string{"x@m"}}); err == nil {
		t.Fatal("put overwrote a malformed file")
	}
	if data, _ := os.ReadFile(s.Path()); string(data) != "{not json" {
		t.Fatalf("file changed: %q", data)
	}
}

func TestStoreRejectsInvalidAndTooMany(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Put(context.Background(), Committee{Name: "you", Members: []string{"x@m"}}); err == nil {
		t.Fatal("stored an invalid committee")
	}
	for i := 0; i < MaxCommittees; i++ {
		if _, err := s.Put(context.Background(), Committee{Name: "c" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Members: []string{"x@m"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put(context.Background(), Committee{Name: "overflow", Members: []string{"x@m"}}); err == nil {
		t.Fatal("stored more than MaxCommittees")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadCache(dir); ok || err != nil {
		t.Fatalf("empty cache: %v %v", ok, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	want := Cache{From: "macmini", FetchedAt: now, AttemptedAt: now, Committees: []Committee{{Name: "a", Version: 3, Members: []string{"x@m"}, DeadlineMinutes: 30}}}
	if err := SaveCache(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadCache(dir)
	if err != nil || !ok || got.From != "macmini" || !got.FetchedAt.Equal(now) || len(got.Committees) != 1 || got.Committees[0].Version != 3 {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/committee`
Expected: build failure (`no non-test Go files` or `undefined: Committee`).

- [ ] **Step 3: Implement.** Create `internal/committee/committee.go`:

```go
// Package committee defines review committees — named groups of
// preset@machine reviewers — and stores them on the hub machine, with a
// cached copy on its peer (committees spec §6.1).
package committee

import (
	"errors"
	"fmt"
	"regexp"
)

const (
	MaxMembers             = 8
	MaxInstructions        = 8 << 10
	DefaultDeadlineMinutes = 30
	MaxDeadlineMinutes     = 240
	MaxCommittees          = 64
)

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	presetRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	machineRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Committee is one owner-defined group of reviewers.
type Committee struct {
	Name            string   `json:"name"`
	Version         int      `json:"version"`
	Members         []string `json:"members"`
	DeadlineMinutes int      `json:"deadline_minutes"`
	Instructions    string   `json:"instructions,omitempty"`
	SkipExhausted   bool     `json:"skip_exhausted"`
}

// Member is one parsed preset@machine reference.
type Member struct{ Preset, Machine string }

// ParseMember splits and validates a preset@machine reference. Presets may
// contain dots (thread listeners never become members); machine names are
// single DNS-style labels.
func ParseMember(s string) (Member, error) {
	var m Member
	at := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '@' {
			if at >= 0 {
				return m, fmt.Errorf("member %q has more than one @", s)
			}
			at = i
		}
	}
	if at < 0 {
		return m, fmt.Errorf("member %q must be preset@machine", s)
	}
	m.Preset, m.Machine = s[:at], s[at+1:]
	if !presetRe.MatchString(m.Preset) || m.Preset[len(m.Preset)-1] == '.' {
		return m, fmt.Errorf("member %q: invalid preset name", s)
	}
	if !machineRe.MatchString(m.Machine) {
		return m, fmt.Errorf("member %q: invalid machine name", s)
	}
	return m, nil
}

// ValidName checks a committee name: no dots (reserved for thread
// listeners) and not "you" (the owner's author name in threads).
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("committee name %q must match %s", name, nameRe)
	}
	if name == "you" {
		return errors.New(`committee name "you" is reserved`)
	}
	return nil
}

// Normalize fills defaults and validates the committee's own shape. Whether
// members exist on their machines is checked by the web layer at save time.
func (c *Committee) Normalize() error {
	if err := ValidName(c.Name); err != nil {
		return err
	}
	if len(c.Members) == 0 || len(c.Members) > MaxMembers {
		return fmt.Errorf("a committee needs 1 to %d members", MaxMembers)
	}
	seen := map[string]bool{}
	for _, s := range c.Members {
		if _, err := ParseMember(s); err != nil {
			return err
		}
		if seen[s] {
			return fmt.Errorf("member %s is listed twice", s)
		}
		seen[s] = true
	}
	if c.DeadlineMinutes == 0 {
		c.DeadlineMinutes = DefaultDeadlineMinutes
	}
	if c.DeadlineMinutes < 1 || c.DeadlineMinutes > MaxDeadlineMinutes {
		return fmt.Errorf("deadline_minutes must be 1 to %d", MaxDeadlineMinutes)
	}
	if len(c.Instructions) > MaxInstructions {
		return fmt.Errorf("instructions exceed %d bytes", MaxInstructions)
	}
	return nil
}
```

Create `internal/committee/store.go`:

```go
package committee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

const maxFileBytes = 1 << 20

type storeFile struct {
	Committees []Committee `json:"committees"`
}

// Store is the hub's committees.json in the shared state directory.
type Store struct{ dir string }

func NewStore(stateDir string) *Store { return &Store{dir: stateDir} }

func (s *Store) Path() string { return filepath.Join(s.dir, "committees.json") }

func (s *Store) lockPath() string { return filepath.Join(s.dir, "committees.lock") }

// List returns the committees sorted by name. A missing file is empty; a
// malformed one is an error naming the file, never an empty list.
func (s *Store) List() ([]Committee, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	return f.Committees, nil
}

func (s *Store) read() (storeFile, error) {
	var f storeFile
	data, err := fsutil.ReadFile(s.Path(), maxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return storeFile{Committees: []Committee{}}, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", s.Path(), err)
	}
	if f.Committees == nil {
		f.Committees = []Committee{}
	}
	sort.Slice(f.Committees, func(i, j int) bool { return f.Committees[i].Name < f.Committees[j].Name })
	return f, nil
}

func (s *Store) update(ctx context.Context, fn func(*storeFile) error) error {
	if err := fsutil.MkdirPrivate(s.dir); err != nil {
		return err
	}
	lock, err := filelock.Acquire(ctx, s.lockPath())
	if err != nil {
		return err
	}
	defer lock.Close()
	f, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	sort.Slice(f.Committees, func(i, j int) bool { return f.Committees[i].Name < f.Committees[j].Name })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.Path(), data)
}

// Put creates or replaces the committee named c.Name. The stored version is
// the previous version plus one (1 for a new committee); the caller's
// Version is ignored.
func (s *Store) Put(ctx context.Context, c Committee) (Committee, error) {
	if err := c.Normalize(); err != nil {
		return Committee{}, err
	}
	err := s.update(ctx, func(f *storeFile) error {
		c.Version = 1
		for i, old := range f.Committees {
			if old.Name == c.Name {
				c.Version = old.Version + 1
				f.Committees[i] = c
				return nil
			}
		}
		if len(f.Committees) >= MaxCommittees {
			return fmt.Errorf("at most %d committees", MaxCommittees)
		}
		f.Committees = append(f.Committees, c)
		return nil
	})
	if err != nil {
		return Committee{}, err
	}
	return c, nil
}

// Delete removes the named committee and reports whether it existed.
func (s *Store) Delete(ctx context.Context, name string) (bool, error) {
	found := false
	err := s.update(ctx, func(f *storeFile) error {
		for i, c := range f.Committees {
			if c.Name == name {
				f.Committees = append(f.Committees[:i], f.Committees[i+1:]...)
				found = true
				return nil
			}
		}
		return nil
	})
	return found, err
}

// Cache is a peer's last copy of the hub's committees.
type Cache struct {
	From        string      `json:"from"`
	FetchedAt   time.Time   `json:"fetched_at"`
	AttemptedAt time.Time   `json:"attempted_at"`
	Error       string      `json:"error,omitempty"`
	Committees  []Committee `json:"committees"`
}

func CachePath(stateDir string) string { return filepath.Join(stateDir, "committees-cache.json") }

// LoadCache reads the peer cache; ok is false when there is none yet.
func LoadCache(stateDir string) (Cache, bool, error) {
	var c Cache
	data, err := fsutil.ReadFile(CachePath(stateDir), maxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, false, fmt.Errorf("%s: %w", CachePath(stateDir), err)
	}
	if c.Committees == nil {
		c.Committees = []Committee{}
	}
	return c, true, nil
}

// SaveCache atomically replaces the peer cache.
func SaveCache(stateDir string, c Cache) error {
	if err := fsutil.MkdirPrivate(stateDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(CachePath(stateDir), data)
}
```

The test's `t.TempDir()` is not canonical on macOS (`/var` → `/private/var`), and `fsutil.MkdirPrivate`/`ReadFile` reject symlinked ancestors. If the store tests fail with an unsafe-path error, canonicalize the directory in the tests with `filepath.EvalSymlinks(t.TempDir())`, through a helper `stateDir(t)` in `store_test.go`. Do not change the fsutil checks.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/committee`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/committee
git commit -m "Add committee definitions with a hub store and a peer cache"
```

---

### Task 2: Coordinator heartbeat (`<state>/web.json`)

**Files:**
- Create: `internal/rooms/heartbeat.go`
- Modify: `internal/web/server.go` (`Config.StateDir`; clean-shutdown removal in `Run`)
- Modify: `internal/web/events.go` (`tick` writes the heartbeat)
- Test: `internal/rooms/heartbeat_test.go`, `internal/web/events_test.go`

**Interfaces:**
- Produces `type Heartbeat struct { PID int; Machine string; Started, Updated time.Time }`, with JSON tags `pid`, `machine`, `started`, `updated`.
- Produces:
  - `func HeartbeatPath(stateDir string) string`
  - `func WriteHeartbeat(stateDir string, hb Heartbeat) error`
  - `func RemoveHeartbeat(stateDir string, pid int) error` (removes only when the file's PID matches)
  - `func CoordinatorAlive(stateDir string, now time.Time) bool`
  - `const HeartbeatFresh = 30 * time.Second`
- Produces the field `web.Config.StateDir string`. `""` disables the heartbeat and every other state-directory feature in this plan.

- [ ] **Step 1: Write the failing tests.** Create `internal/rooms/heartbeat_test.go`:

```go
package rooms

import (
	"os"
	"testing"
	"time"
)

func TestHeartbeatFreshness(t *testing.T) {
	dir := t.TempDir()
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
```

Add to `internal/web/events_test.go`:

```go
func TestTickWritesHeartbeat(t *testing.T) {
	s := testServer(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	s.cfg.StateDir = state
	s.tick(context.Background())
	hb, err := os.ReadFile(rooms.HeartbeatPath(state))
	if err != nil || !strings.Contains(string(hb), `"machine": "testbox"`) {
		t.Fatalf("heartbeat: %s %v", hb, err)
	}
	if !rooms.CoordinatorAlive(state, time.Now()) {
		t.Fatal("tick did not make the coordinator alive")
	}
}
```

Add any missing imports (`context`, `os`, `path/filepath`, `strings`, `time`, `internal/rooms`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/rooms -run TestHeartbeatFreshness; go test ./internal/web -run TestTickWritesHeartbeat`
Expected: compile failures (`undefined: CoordinatorAlive`, `s.cfg.StateDir undefined`).

- [ ] **Step 3: Implement.** Create `internal/rooms/heartbeat.go`:

```go
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
```

In `internal/web/server.go`, add to `Config` (after `QuotaConfig`):

```go
	// StateDir is the shared tincan state directory (rooms.StateDir()).
	// Empty disables the heartbeat and committee storage (tests).
	StateDir string
```

and add to `Server`:

```go
	started       time.Time // for the heartbeat
	lastHeartbeat time.Time // touched only by tick
```

Set `started: time.Now()` in `New`'s `&Server{…}` literal. In `Run`, before `err := srv.Serve(ln)`, add:

```go
	defer func() {
		if s.cfg.StateDir != "" {
			rooms.RemoveHeartbeat(s.cfg.StateDir, os.Getpid())
		}
	}()
```

In `internal/web/events.go` `tick`, before `list, err := s.cfg.Registry.List()`:

```go
	if s.cfg.StateDir != "" && time.Since(s.lastHeartbeat) >= 10*time.Second {
		now := time.Now()
		if err := rooms.WriteHeartbeat(s.cfg.StateDir, rooms.Heartbeat{PID: os.Getpid(), Machine: s.cfg.Machine, Started: s.started, Updated: now}); err != nil {
			s.logOnce("heartbeat", fmt.Sprintf("tincan web: heartbeat: %v", err))
		} else {
			s.lastHeartbeat = now
		}
	}
```

In `internal/cli/web.go`, pass `StateDir: rooms.StateDir()` in the `web.Config` literal.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/rooms ./internal/web ./internal/cli`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rooms/heartbeat.go internal/rooms/heartbeat_test.go internal/web/server.go internal/web/events.go internal/web/events_test.go internal/cli/web.go
git commit -m "Write a coordinator heartbeat from tincan web"
```

---

### Task 3: Outbound peer client

**Files:**
- Create: `internal/web/peerclient.go`
- Modify: `internal/web/proxy.go` (`peer` gains an `rpc` client)
- Test: `internal/web/peerclient_test.go`

**Interfaces:**
- Produces `type PeerError struct { Status int; Msg string }`, which implements `error`.
- Produces `func (p *peer) call(ctx context.Context, method, path string, in any, limit int64, out any) error`. `path` is relative to the peer's `api/`, e.g. `"presets"`. A non-2xx response returns `*PeerError`. A transport failure returns a wrapped error that is not a `*PeerError`.
- Produces `func IsTransient(err error) bool`: true for transport errors and `PeerError` with `Status >= 500`.

- [ ] **Step 1: Write the failing test.** First change `peerPair` in `internal/web/proxy_test.go` to also return the peer's `*Server` as a fourth value, `func peerPair(t *testing.T) (*Server, *http.Request, *httptest.Server, *Server)`, returning `peerServer` last. Update its existing callers in `proxy_test.go` to ignore it (`hub, last, peerHTTP, _ := peerPair(t)`); Tasks 5 and 6 use it. Then create `internal/web/peerclient_test.go`:

```go
package web

import (
	"context"
	"errors"
	"testing"
)

func TestPeerCallPassesThePeersChecks(t *testing.T) {
	hub, last, peerHTTP, _ := peerPair(t)
	p := hub.peers["macmini"]
	var self struct {
		Machine string `json:"machine"`
	}
	if err := p.call(context.Background(), "GET", "self", nil, 1<<20, &self); err != nil || self.Machine != "macmini" {
		t.Fatalf("GET self: %+v %v", self, err)
	}
	// A mutation reaches the handler (400 for a bad body), proving the
	// Host, owner and CSRF checks passed rather than a 403 refusal.
	err := p.call(context.Background(), "POST", "rooms", map[string]string{"path": ""}, 1<<20, nil)
	var pe *PeerError
	if !errors.As(err, &pe) || pe.Status != 400 {
		t.Fatalf("POST rooms: %v", err)
	}
	if last.Header.Get("X-Tincan-Request") != "1" || last.Header.Get("Origin") != "" || last.Header.Get("X-Forwarded-Host") != "" {
		t.Fatalf("headers: %v", last.Header)
	}
	if last.Host != peerHTTP.Listener.Addr().String() {
		t.Fatalf("Host = %q", last.Host)
	}
	if IsTransient(err) {
		t.Fatal("a 400 is not transient")
	}
}

func TestPeerCallCapsResponseAndReportsOffline(t *testing.T) {
	hub, _, peerHTTP, _ := peerPair(t)
	p := hub.peers["macmini"]
	if err := p.call(context.Background(), "GET", "self", nil, 8, nil); err == nil {
		t.Fatal("oversized response accepted")
	}
	peerHTTP.Close()
	err := p.call(context.Background(), "GET", "self", nil, 1<<20, nil)
	if err == nil || !IsTransient(err) {
		t.Fatalf("offline peer: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/web -run 'TestPeerCall'`
Expected: compile failure (`p.call undefined`).

- [ ] **Step 3: Implement.** In `internal/web/proxy.go` add the field `rpc *http.Client` to `peer`, and in `newPeer` set `rpc: &http.Client{Transport: transport}` in the `pp` literal. There is no client-wide timeout; calls set deadlines. Create `internal/web/peerclient.go`:

```go
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// PeerError is a non-2xx answer from a peer's machine-link API.
type PeerError struct {
	Status int
	Msg    string
}

func (e *PeerError) Error() string { return fmt.Sprintf("peer answered %d: %s", e.Status, e.Msg) }

// IsTransient reports whether a peer call may succeed if retried: transport
// failures and 5xx answers (committees §6.4).
func IsTransient(err error) bool {
	var pe *PeerError
	if errors.As(err, &pe) {
		return pe.Status >= 500
	}
	return err != nil
}

// call sends one machine-link request to the peer's public URL (committees
// §6.7). tailscale serve on the peer attaches this device owner's identity,
// so the peer's Host, forwarded-Host, owner and Funnel checks apply
// unchanged; the client adds only X-Tincan-Request and never forwards
// browser or identity headers. Without a caller deadline it allows 10 s.
func (p *peer) call(ctx context.Context, method, path string, in any, limit int64, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	u := p.base.ResolveReference(&url.URL{Path: "api/" + path})
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-Tincan-Request", "1")
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.rpc.Do(req)
	if err != nil {
		return fmt.Errorf("peer %s: %w", p.name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("peer %s: %w", p.name, err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("peer %s: response exceeds %d bytes", p.name, limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := string(data)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &PeerError{Status: resp.StatusCode, Msg: msg}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("peer %s: %w", p.name, err)
		}
	}
	return nil
}
```

In the test, `peerPair`'s peer is served under `/tincan` with identity injected (`servedLike`), just as `tailscale serve` would. If `last.Host` differs because `servedLike` strips the prefix, assert on the host the peer's `Handler` saw. `peerPair` records the request as the peer's handler receives it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/peerclient.go internal/web/peerclient_test.go internal/web/proxy.go
git commit -m "Add the outbound machine-link peer client"
```

---

### Task 4: `api/presets` machine-level catalogue

**Files:**
- Create: `internal/web/catalogue.go`
- Modify: `internal/web/api.go` (`apiRoutes`: `GET /api/presets`)
- Test: `internal/web/catalogue_test.go`

**Interfaces:**
- Consumes: `dispatch.Options.PresetMap()`, `host.ResolveExecutable`, `host.SessionProvider`, `(host.Preset).Public()`, `quota.Load`.
- Produces `type PresetInfo struct`, with fields and JSON tags:
  - `Name` (`name`), `Available` (`available`), `Executable` (`executable`, the resolved path or `""`);
  - `ExecKind` (`exec_kind`: `bare|absolute|relative`);
  - `Provider` (`provider,omitempty`), `SessionSupported` (`session_supported`);
  - `EnvKeys` (`env_keys,omitempty`), `EnvUnset` (`env_unset,omitempty`);
  - `Bypass` (`bypass,omitempty`, the flag found), `Warnings` (`warnings,omitempty`);
  - `Quota *quota.Entry` (`quota,omitempty`).
- Produces `func (s *Server) catalogue() ([]PresetInfo, error)` (sorted by name) and `func bypassFlag(argv []string) string`.

- [ ] **Step 1: Write the failing test.** Create `internal/web/catalogue_test.go`:

```go
package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func TestPresetsCatalogueIsRedactedAndWarns(t *testing.T) {
	bin := filepath.Join(t.TempDir(), fakeAgentName())
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	qdir := t.TempDir()
	os.WriteFile(filepath.Join(qdir, "claude-quota.json"), []byte(`{"percent": 40, "reset_at": "2099-01-01T00:00:00Z", "fetched_at": "2099-01-01T00:00:00Z"}`), 0o600)
	s, err := New(Config{Owner: owner, AllowedHosts: testHosts, Machine: "box", Registry: rooms.Open(filepath.Join(t.TempDir(), "rooms.json")),
		QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json"),
		Dispatch: dispatch.Options{Presets: map[string]host.Preset{
			"claude":   {Exec: []string{bin, "-p", "{body}", "--dangerously-skip-permissions"}, Stdin: "none", Reply: "stdout", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/secret/dir"}},
			"relative": {Exec: []string{"./reviewer"}, Stdin: "none", Reply: "stdout"},
			"codexy":   {Exec: []string{bin, "exec", "-s", "danger-full-access"}, Stdin: "none", Reply: "stdout"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, s.Handler(), "GET", "/api/presets", "", ownerHdr())
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "/secret/dir") {
		t.Fatalf("catalogue: %d %s", rec.Code, rec.Body)
	}
	var got []PresetInfo
	decode(t, rec.Body.String(), &got)
	by := map[string]PresetInfo{}
	for _, p := range got {
		by[p.Name] = p
	}
	c := by["claude"]
	if !c.Available || c.ExecKind != "absolute" || c.Bypass != "--dangerously-skip-permissions" || len(c.Warnings) == 0 ||
		len(c.EnvKeys) != 1 || c.EnvKeys[0] != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("claude entry: %+v", c)
	}
	if by["relative"].ExecKind != "relative" || by["relative"].Available {
		t.Fatalf("relative entry: %+v", by["relative"])
	}
	if by["codexy"].Bypass != "-s danger-full-access" {
		t.Fatalf("sandbox bypass not detected: %+v", by["codexy"])
	}
}

func TestBypassFlag(t *testing.T) {
	for argv, want := range map[string]string{
		"claude -p {body}":                        "",
		"grok -p {body} --always-approve":         "--always-approve",
		"codex exec --sandbox danger-full-access": "--sandbox danger-full-access",
		"codex exec --sandbox=danger-full-access": "--sandbox=danger-full-access",
		"gemini --yolo":                           "--yolo",
		"codex exec -s read-only":                 "",
	} {
		if got := bypassFlag(strings.Fields(argv)); got != want {
			t.Errorf("%s: %q, want %q", argv, got, want)
		}
	}
}
```

The quota fixture assumes phase 1's `claude-quota.json` cache schema. Check `internal/quota/quota_test.go` for a valid fixture and copy one exactly if the fields differ. The test asserts nothing about quota beyond the request succeeding.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web -run 'TestPresetsCatalogue|TestBypassFlag'`
Expected: compile failure (`undefined: PresetInfo`).

- [ ] **Step 3: Implement.** Create `internal/web/catalogue.go`:

```go
package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/quota"
)

// PresetInfo is one entry of this machine's redacted preset catalogue
// (committees §6.7): enough to pick committee members and warn about them,
// never an env value.
type PresetInfo struct {
	Name             string       `json:"name"`
	Available        bool         `json:"available"`
	Executable       string       `json:"executable"`
	ExecKind         string       `json:"exec_kind"`
	Provider         string       `json:"provider,omitempty"`
	SessionSupported bool         `json:"session_supported"`
	EnvKeys          []string     `json:"env_keys,omitempty"`
	EnvUnset         []string     `json:"env_unset,omitempty"`
	Bypass           string       `json:"bypass,omitempty"`
	Warnings         []string     `json:"warnings,omitempty"`
	Quota            *quota.Entry `json:"quota,omitempty"`
}

// bypassFlag returns the first known permission-bypass argument in argv, as
// written ("-s danger-full-access" for a split sandbox flag), or "".
func bypassFlag(argv []string) string {
	for i, a := range argv {
		switch a {
		case "--dangerously-skip-permissions", "--always-approve", "--yolo", "--sandbox=danger-full-access":
			return a
		case "-s", "--sandbox":
			if i+1 < len(argv) && argv[i+1] == "danger-full-access" {
				return a + " danger-full-access"
			}
		}
	}
	return ""
}

func execKind(name string) string {
	switch {
	case filepath.IsAbs(name):
		return "absolute"
	case strings.ContainsAny(name, `/\`):
		return "relative"
	default:
		return "bare"
	}
}

func (s *Server) catalogue() ([]PresetInfo, error) {
	presets, err := s.cfg.Dispatch.PresetMap()
	if err != nil {
		return nil, err
	}
	entries, _ := quota.Load(s.cfg.QuotaDir, s.cfg.QuotaConfig, time.Now())
	out := make([]PresetInfo, 0, len(presets))
	for _, name := range host.Names(presets) {
		p := presets[name]
		info := PresetInfo{Name: name, ExecKind: execKind(p.Exec[0])}
		if info.ExecKind != "relative" {
			if path, err := host.ResolveExecutable(p.Exec[0], ""); err == nil {
				info.Available, info.Executable = true, path
			}
		}
		info.Provider, _ = host.SessionProvider(p, name)
		info.SessionSupported = info.Provider != ""
		pub := p.Public()
		info.EnvKeys, info.EnvUnset = pub.EnvKeys, pub.EnvUnset
		if info.Bypass = bypassFlag(p.Exec); info.Bypass != "" {
			info.Warnings = append(info.Warnings, fmt.Sprintf("permission bypass (%s): a reviewer could modify files", info.Bypass))
		}
		if info.ExecKind == "relative" {
			info.Warnings = append(info.Warnings, "relative executable: cannot be a committee member")
		}
		info.Quota = quotaFor(entries, name)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// quotaFor mirrors the UI's choice: an explicit mapping wins, else the first
// entry naming the preset.
func quotaFor(entries []quota.Entry, preset string) *quota.Entry {
	var first *quota.Entry
	for i := range entries {
		for _, p := range entries[i].Presets {
			if p != preset {
				continue
			}
			if entries[i].Explicit {
				return &entries[i]
			}
			if first == nil {
				first = &entries[i]
			}
		}
	}
	return first
}

func (s *Server) apiPresets(w http.ResponseWriter, r *http.Request) {
	list, err := s.catalogue()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}
```

In `apiRoutes` add `s.mux.HandleFunc("GET /api/presets", s.apiPresets)`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/catalogue.go internal/web/catalogue_test.go internal/web/api.go
git commit -m "Serve a redacted machine-level preset catalogue"
```

---

### Task 5: Committees API on the hub, with save-time validation and change notes

**Files:**
- Create: `internal/web/committees.go`
- Modify: `internal/web/api.go` (routes), `internal/web/events.go` (`scan` fingerprint and note)
- Test: `internal/web/committees_test.go`

**Interfaces:**
- Consumes: `committee.Store`, `(*peer).call`, `(*Server).catalogue`.
- Produces the routes `GET /api/committees`, `PUT /api/committees/{name}` (16 KiB body) and `DELETE /api/committees/{name}`.
- Produces `type committeesView struct`, with fields and JSON tags:
  - `Role string` (`role`: `hub|peer`);
  - `From string` (`from,omitempty`);
  - `FetchedAt *time.Time` (`fetched_at,omitempty`);
  - `Error string` (`error,omitempty`);
  - `Committees []committee.Committee` (`committees`).
- Produces `func (s *Server) catalogueOf(ctx context.Context, machine string) ([]PresetInfo, error)`.
- Produces `func (s *Server) validateCommittee(ctx context.Context, c committee.Committee) (warnings []string, err error)`.
- Produces the note kind `committees`.

- [ ] **Step 1: Write the failing tests.** Create `internal/web/committees_test.go`:

```go
package web

import (
	"path/filepath"
	"strings"
	"testing"
)

func hubWithState(t *testing.T) (*Server, string) {
	t.Helper()
	s, _ := apiServer(t) // presets: claude (available), missing
	state, _ := filepath.EvalSymlinks(t.TempDir())
	s.cfg.StateDir = state
	return s, state
}

func TestCommitteeCRUDOnHub(t *testing.T) {
	s, _ := hubWithState(t)
	h := s.Handler()
	rec := do(t, h, "PUT", "/api/committees/reviewers", `{"members":["claude@box"],"instructions":"cite file:line"}`, mut())
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	var saved struct {
		Committee struct {
			Version int `json:"version"`
		} `json:"committee"`
	}
	decode(t, rec.Body.String(), &saved)
	if saved.Committee.Version != 1 {
		t.Fatalf("version: %s", rec.Body)
	}
	rec = do(t, h, "GET", "/api/committees", "", ownerHdr())
	var view committeesView
	decode(t, rec.Body.String(), &view)
	if view.Role != "hub" || len(view.Committees) != 1 || view.Committees[0].Instructions != "cite file:line" {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec := do(t, h, "DELETE", "/api/committees/reviewers", "", mut()); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "DELETE", "/api/committees/reviewers", "", mut()); rec.Code != 404 {
		t.Fatalf("second delete: %d", rec.Code)
	}
}

func TestCommitteeValidationAtSave(t *testing.T) {
	s, _ := hubWithState(t)
	h := s.Handler()
	for body, want := range map[string]string{
		`{"members":["missing@box"]}`:    "not available",
		`{"members":["nosuch@box"]}`:     "no preset",
		`{"members":["claude@elsewhere"]}`: "unknown machine",
		`{"members":[]}`:                 "1 to 8",
	} {
		rec := do(t, h, "PUT", "/api/committees/reviewers", body, mut())
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s (want %q)", body, rec.Code, rec.Body, want)
		}
	}
	if rec := do(t, h, "PUT", "/api/committees/claude", `{"members":["claude@box"]}`, mut()); rec.Code != 400 || !strings.Contains(rec.Body.String(), "preset name") {
		t.Errorf("committee named like a preset: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/committees/reviewers", strings.Repeat(" ", 17<<10)+`{}`, mut()); rec.Code != 413 {
		t.Errorf("oversized body: %d", rec.Code)
	}
}

func TestCommitteeMemberOnOfflinePeerIsRefused(t *testing.T) {
	hub, _, peerHTTP, _ := peerPair(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	hub.cfg.StateDir = state
	peerHTTP.Close()
	rec := do(t, hub.Handler(), "PUT", "/api/committees/reviewers", `{"members":["claude@macmini"]}`, mut())
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "macmini") {
		t.Fatalf("offline peer member: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, hub.Handler(), "GET", "/api/committees", "", ownerHdr()); !strings.Contains(rec.Body.String(), `"committees":[]`) {
		t.Fatalf("saved anyway: %s", rec.Body)
	}
}

func TestCommitteesDisabledWithoutStateDir(t *testing.T) {
	s, _ := apiServer(t)
	if rec := do(t, s.Handler(), "GET", "/api/committees", "", ownerHdr()); rec.Code != 503 {
		t.Fatalf("no state dir: %d", rec.Code)
	}
}

func TestMalformedCommitteesFileIs500(t *testing.T) {
	s, state := hubWithState(t)
	writeFile(t, filepath.Join(state, "committees.json"), "{broken")
	rec := do(t, s.Handler(), "GET", "/api/committees", "", ownerHdr())
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "committees.json") {
		t.Fatalf("malformed: %d %s", rec.Code, rec.Body)
	}
}
```

If `writeFile` does not already exist among the web test helpers, add it to `committees_test.go`: `func writeFile(t *testing.T, path, s string) { t.Helper(); if err := os.WriteFile(path, []byte(s), 0o600); err != nil { t.Fatal(err) } }` (import `os`).

In `peerPair` the peer's catalogue has no presets, because its `Dispatch` is empty and it loads the real `agents.json`. The offline test only needs the peer to be unreachable, so this does not matter.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web -run 'TestCommittee|TestMalformedCommittees'`
Expected: compile failure (`undefined: committeesView`).

- [ ] **Step 3: Implement.** Create `internal/web/committees.go`:

```go
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
)

const committeeBodyLimit = 16 << 10

type committeesView struct {
	Role       string                `json:"role"` // hub | peer
	From       string                `json:"from,omitempty"`
	FetchedAt  *time.Time            `json:"fetched_at,omitempty"`
	Error      string                `json:"error,omitempty"`
	Committees []committee.Committee `json:"committees"`
}

var errNoState = errors.New("committees need tincan web's state directory")

func (s *Server) listCommittees(w http.ResponseWriter, r *http.Request) {
	if s.cfg.StateDir == "" {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return
	}
	if s.cfg.CommitteesFrom != "" {
		s.writeJSON(w, http.StatusOK, s.peerCommittees(r.Context())) // Task 6
		return
	}
	list, err := committee.NewStore(s.cfg.StateDir).List()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, committeesView{Role: "hub", Committees: list})
}

func (s *Server) hubOnly(w http.ResponseWriter) bool {
	if s.cfg.StateDir == "" {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return false
	}
	if s.cfg.CommitteesFrom != "" {
		s.fail(w, http.StatusConflict, fmt.Errorf("committees are edited on %s", s.cfg.CommitteesFrom))
		return false
	}
	return true
}

func (s *Server) putCommittee(w http.ResponseWriter, r *http.Request) {
	if !s.hubOnly(w) {
		return
	}
	var in struct {
		Members         []string `json:"members"`
		DeadlineMinutes int      `json:"deadline_minutes"`
		Instructions    string   `json:"instructions"`
		SkipExhausted   bool     `json:"skip_exhausted"`
	}
	if err := readJSON(w, r, committeeBodyLimit, &in); err != nil {
		s.failBody(w, err)
		return
	}
	c := committee.Committee{Name: r.PathValue("name"), Members: in.Members, DeadlineMinutes: in.DeadlineMinutes, Instructions: in.Instructions, SkipExhausted: in.SkipExhausted}
	if err := c.Normalize(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	warnings, err := s.validateCommittee(r.Context(), c)
	if err != nil {
		status := http.StatusBadRequest
		var unreachable *unreachableError
		if errors.As(err, &unreachable) {
			status = http.StatusBadGateway
		}
		s.fail(w, status, err)
		return
	}
	saved, err := committee.NewStore(s.cfg.StateDir).Put(r.Context(), c)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"committee": saved, "warnings": warnings})
}

func (s *Server) deleteCommittee(w http.ResponseWriter, r *http.Request) {
	if !s.hubOnly(w) {
		return
	}
	found, err := committee.NewStore(s.cfg.StateDir).Delete(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		s.fail(w, http.StatusNotFound, errNotFound)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

type unreachableError struct {
	machine string
	err     error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("cannot validate members on %s: %v", e.machine, e.err)
}

// catalogueOf returns machine's preset catalogue: this machine's directly,
// a peer's through the outbound client.
func (s *Server) catalogueOf(ctx context.Context, machine string) ([]PresetInfo, error) {
	if machine == s.cfg.Machine {
		return s.catalogue()
	}
	p, ok := s.peers[machine]
	if !ok {
		return nil, fmt.Errorf("unknown machine %q (this machine is %q; peers are configured with --peer)", machine, s.cfg.Machine)
	}
	var list []PresetInfo
	if err := p.call(ctx, "GET", "presets", nil, 1<<20, &list); err != nil {
		return nil, &unreachableError{machine, err}
	}
	return list, nil
}

// validateCommittee applies the save-time checks of committees §6.1: every
// member resolves, is available and has a bare or absolute executable on its
// machine; the name is no preset on any reachable machine; bypass presets
// produce warnings.
func (s *Server) validateCommittee(ctx context.Context, c committee.Committee) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	catalogues := map[string][]PresetInfo{}
	var warnings []string
	for _, raw := range c.Members {
		m, _ := committee.ParseMember(raw)
		list, ok := catalogues[m.Machine]
		if !ok {
			var err error
			if list, err = s.catalogueOf(ctx, m.Machine); err != nil {
				return nil, err
			}
			catalogues[m.Machine] = list
		}
		var info *PresetInfo
		for i := range list {
			if list[i].Name == m.Preset {
				info = &list[i]
			}
		}
		switch {
		case info == nil:
			return nil, fmt.Errorf("member %s: no preset %q on %s", raw, m.Preset, m.Machine)
		case info.ExecKind == "relative":
			return nil, fmt.Errorf("member %s: relative executables cannot review (use a bare name or an absolute path)", raw)
		case !info.Available:
			return nil, fmt.Errorf("member %s: preset is not available on %s (executable not found)", raw, m.Machine)
		}
		for _, w := range info.Warnings {
			warnings = append(warnings, raw+": "+w)
		}
	}
	machines := append([]string{s.cfg.Machine}, sortedPeerNames(s.peers)...)
	for _, machine := range machines {
		list, ok := catalogues[machine]
		if !ok {
			var err error
			if list, err = s.catalogueOf(ctx, machine); err != nil {
				warnings = append(warnings, fmt.Sprintf("could not check preset names on %s: %v", machine, err))
				continue
			}
		}
		for _, p := range list {
			if p.Name == c.Name {
				return nil, fmt.Errorf("committee name %q is a preset name on %s; mentions would be ambiguous", c.Name, machine)
			}
		}
	}
	return warnings, nil
}
```

`sortedKeys` in `events.go` takes `map[string]string`, so add this next to it:

```go
// sortedPeerNames returns the configured peer names in sorted order.
func sortedPeerNames(m map[string]*peer) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
```

In `apiRoutes` add:

```go
	s.mux.HandleFunc("GET /api/committees", s.listCommittees)
	s.mux.HandleFunc("PUT /api/committees/{name}", s.putCommittee)
	s.mux.HandleFunc("DELETE /api/committees/{name}", s.deleteCommittee)
```

`Config` gains `CommitteesFrom string // peer that hosts committees; "" = this machine is the hub`, used here and implemented in Task 6. In Task 5 `peerCommittees` does not exist yet. Add a stub in `committees.go`, which Task 6 replaces:

```go
func (s *Server) peerCommittees(ctx context.Context) committeesView {
	return committeesView{Role: "peer", From: s.cfg.CommitteesFrom, Error: "not implemented", Committees: []committee.Committee{}}
}
```

In `internal/web/events.go` `scan`, after the quota fingerprint block, add:

```go
	if s.cfg.StateDir != "" {
		next["c:"] = statFP(committee.NewStore(s.cfg.StateDir).Path()) + statFP(committee.CachePath(s.cfg.StateDir))
	}
```

In `scan`'s `switch key[0]` over changed fingerprints, add:

```go
		case 'c':
			notes = append(notes, note{Kind: "committees"})
``` Update the `note.Kind` comment to include `committees`. Add an `events_test.go` case: write `committees.json` between two `scan` calls and assert that a subscriber receives `{"kind":"committees"}`, modelled on the existing quota note test.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web ./internal/committee`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/committees.go internal/web/committees_test.go internal/web/api.go internal/web/events.go internal/web/events_test.go internal/web/server.go
git commit -m "Store and validate committees on the hub"
```

---

### Task 6: Peer cache, `--committees-from`, and machine names

**Files:**
- Modify: `internal/web/committees.go` (replace the `peerCommittees` stub; `refreshCommittees`; `syncCommittees` loop)
- Modify: `internal/web/server.go` (`New` validates `CommitteesFrom`; `Run` starts `syncCommittees`)
- Modify: `internal/cli/web.go` (`--committees-from`, `--machine`, default machine name)
- Modify: `docs/web.md`, `deploy/systemd/tincan-web.service`, `deploy/launchd/net.tincan.web.plist` (comments only)
- Test: `internal/web/committees_test.go`, `internal/cli/web_test.go` (create if missing)

**Interfaces:**
- Consumes: `committee.LoadCache`/`SaveCache`, `(*peer).call`.
- Produces:
  - `func (s *Server) refreshCommittees(ctx context.Context) error`
  - `func (s *Server) syncCommittees(ctx context.Context)`
  - `func machineName(flag, dnsName, hostname string) string` in `internal/cli/web.go`
- Produces a field on `Server`: `committeesMu sync.Mutex`, which serializes refreshes.

- [ ] **Step 1: Write the failing tests.** Add to `internal/web/committees_test.go`:

```go
func TestPeerCachesHubCommittees(t *testing.T) {
	// hub = the httptest peer ("macmini") holding committees; the local
	// server ("cachyos") runs with --committees-from macmini.
	local, _, peerHTTP, hubServer := peerPair(t)
	hubState, _ := filepath.EvalSymlinks(t.TempDir())
	hubServer.cfg.StateDir = hubState
	committee.NewStore(hubState).Put(context.Background(), committee.Committee{Name: "reviewers", Members: []string{"x@macmini"}})
	localState, _ := filepath.EvalSymlinks(t.TempDir())
	local.cfg.StateDir, local.cfg.CommitteesFrom = localState, "macmini"

	rec := do(t, local.Handler(), "GET", "/api/committees", "", ownerHdr())
	var view committeesView
	decode(t, rec.Body.String(), &view)
	if view.Role != "peer" || view.From != "macmini" || view.FetchedAt == nil || len(view.Committees) != 1 || view.Error != "" {
		t.Fatalf("peer view: %s", rec.Body)
	}
	if rec := do(t, local.Handler(), "PUT", "/api/committees/x", `{"members":["x@macmini"]}`, mut()); rec.Code != 409 || !strings.Contains(rec.Body.String(), "edited on macmini") {
		t.Fatalf("edit on peer: %d %s", rec.Code, rec.Body)
	}
	// Hub goes away: the cache is kept and the error is reported.
	peerHTTP.Close()
	c, _, _ := committee.LoadCache(localState)
	c.FetchedAt = c.FetchedAt.Add(-time.Minute) // force a refresh attempt
	committee.SaveCache(localState, c)
	rec = do(t, local.Handler(), "GET", "/api/committees", "", ownerHdr())
	decode(t, rec.Body.String(), &view)
	if len(view.Committees) != 1 || view.Error == "" {
		t.Fatalf("stale cache view: %s", rec.Body)
	}
}

func TestPeerRefusesANonHub(t *testing.T) {
	local, _, _, hubServer := peerPair(t)
	hubServer.cfg.StateDir, _ = filepath.EvalSymlinks(t.TempDir())
	hubServer.cfg.CommitteesFrom = "cachyos" // misconfigured: also a peer
	local.cfg.StateDir, _ = filepath.EvalSymlinks(t.TempDir())
	local.cfg.CommitteesFrom = "macmini"
	rec := do(t, local.Handler(), "GET", "/api/committees", "", ownerHdr())
	if !strings.Contains(rec.Body.String(), "not a committees hub") {
		t.Fatalf("non-hub accepted: %s", rec.Body)
	}
}

func TestNewRejectsUnknownCommitteesFrom(t *testing.T) {
	_, err := New(Config{Owner: owner, Registry: rooms.Open(filepath.Join(t.TempDir(), "rooms.json")), CommitteesFrom: "nowhere"})
	if err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("unknown hub accepted: %v", err)
	}
}
```

`peerPair`'s fourth value (added in Task 3) is the hub-side `*Server` behind `peerHTTP`. In `TestPeerRefusesANonHub` that server is misconfigured *after* construction to read from a peer it does not have; `refreshCommittees` must therefore guard against a missing peer (see Step 3). Add imports `context`, `time`, `internal/committee`, `internal/rooms`.

Create or extend `internal/cli/web_test.go`:

```go
package cli

import "testing"

func TestMachineNameDefaults(t *testing.T) {
	for _, c := range []struct{ flag, dns, host, want string }{
		{"", "macmini.brill-decibel.ts.net", "Mac-mini.local", "macmini"},
		{"", "cachyos.brill-decibel.ts.net", "cachyos-desktop", "cachyos"},
		{"", "", "cachyos-desktop", "cachyos-desktop"},
		{"", "", "Mac-mini.local", "Mac-mini"},
		{"box", "macmini.x.ts.net", "h", "box"},
	} {
		if got := machineName(c.flag, c.dns, c.host); got != c.want {
			t.Errorf("machineName(%q,%q,%q) = %q, want %q", c.flag, c.dns, c.host, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web -run 'TestPeerCaches|TestPeerRefuses|TestNewRejectsUnknown' ; go test ./internal/cli -run TestMachineNameDefaults`
Expected: failures: the stub returns "not implemented", `New` accepts an unknown hub, and `machineName` is undefined.

- [ ] **Step 3: Implement.** In `New` (`server.go`), after the peers loop:

```go
	if cfg.CommitteesFrom != "" {
		if _, ok := s.peers[cfg.CommitteesFrom]; !ok {
			return nil, fmt.Errorf("--committees-from %q is not a configured --peer", cfg.CommitteesFrom)
		}
	}
```

Replace the stub in `committees.go` with:

```go
// peerCommittees serves the cached copy, refreshing it first when it is more
// than 10 s old (committees §6.7); a failed refresh keeps the last valid copy
// and reports the error.
func (s *Server) peerCommittees(ctx context.Context) committeesView {
	c, ok, err := committee.LoadCache(s.cfg.StateDir)
	if err != nil || !ok || time.Since(c.FetchedAt) > 10*time.Second {
		s.refreshCommittees(ctx)
		c, ok, err = committee.LoadCache(s.cfg.StateDir)
	}
	view := committeesView{Role: "peer", From: s.cfg.CommitteesFrom, Committees: []committee.Committee{}}
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if ok {
		view.Committees, view.Error = c.Committees, c.Error
		if !c.FetchedAt.IsZero() {
			t := c.FetchedAt
			view.FetchedAt = &t
		}
	}
	return view
}

// refreshCommittees fetches the hub's committees into the cache. Concurrent
// callers share one fetch at a time.
func (s *Server) refreshCommittees(ctx context.Context) error {
	s.committeesMu.Lock()
	defer s.committeesMu.Unlock()
	old, _, _ := committee.LoadCache(s.cfg.StateDir)
	old.From, old.AttemptedAt = s.cfg.CommitteesFrom, time.Now().UTC()
	var view committeesView
	var err error
	if p := s.peers[s.cfg.CommitteesFrom]; p == nil {
		err = fmt.Errorf("%s is not a configured peer", s.cfg.CommitteesFrom)
	} else {
		err = p.call(ctx, "GET", "committees", nil, 1<<20, &view)
	}
	if err == nil && view.Role != "hub" {
		err = fmt.Errorf("%s is not a committees hub (it reads committees from %q)", s.cfg.CommitteesFrom, view.From)
	}
	if err == nil {
		for i := range view.Committees {
			if nerr := view.Committees[i].Normalize(); nerr != nil {
				err = fmt.Errorf("hub sent an invalid committee: %w", nerr)
				break
			}
		}
	}
	if err != nil {
		old.Error = err.Error()
	} else {
		old.Error, old.FetchedAt, old.Committees = "", old.AttemptedAt, view.Committees
	}
	if old.Committees == nil {
		old.Committees = []committee.Committee{}
	}
	if serr := committee.SaveCache(s.cfg.StateDir, old); serr != nil {
		return serr
	}
	return err
}

// syncCommittees refreshes the peer cache every 60 s.
func (s *Server) syncCommittees(ctx context.Context) {
	if s.cfg.CommitteesFrom == "" || s.cfg.StateDir == "" {
		return
	}
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		if err := s.refreshCommittees(ctx); err != nil {
			s.logOnce("committees.refresh", fmt.Sprintf("tincan web: committees from %s: %v", s.cfg.CommitteesFrom, err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
```

Add `committeesMu sync.Mutex` to `Server`. In `Run`, add `go s.syncCommittees(ctx)` next to `go s.watchPeers(ctx)`.

In `internal/cli/web.go`:
- add the flags `machineFlag := fs.String("machine", "", "this machine's name in committees and peers' --peer (default: first label of its Tailscale DNS name)")` and `from := fs.String("committees-from", "", "peer that hosts committee definitions (default: this machine is the hub)")`;
- change `webIdentity` to also return the DNS name it found (return `owner, hosts, dnsName, err`) and update its caller;
- replace the hostname block with:

```go
	hostname, _ := os.Hostname()
	machine := machineName(*machineFlag, dnsName, hostname)
```

and add:

```go
// machineName picks this machine's name: the flag, else the first label of
// its Tailscale DNS name (what peers' --peer URLs use), else the host name.
func machineName(flag, dnsName, hostname string) string {
	if flag != "" {
		return flag
	}
	if dnsName != "" {
		label, _, _ := strings.Cut(dnsName, ".")
		return label
	}
	label, _, _ := strings.Cut(hostname, ".")
	return label
}
```

Pass `CommitteesFrom: *from` in the `web.Config` literal.

In `docs/web.md`, add a "Committees" section covering:
- committees are defined on the Committees page and stored on the hub, which is the machine without `--committees-from`;
- a peer started with `--committees-from <peer name>` caches them, refreshing every 60 s and on page load, shows the cache age, and sends edits to the hub;
- members are `preset@machine`, and machine names come from `--machine`, by default the first label of the Tailscale DNS name;
- save-time checks (available, not a relative executable, bypass warnings);
- reviews themselves arrive in later releases.

In both deploy files, add a comment line showing the optional `--committees-from macmini` for the Linux unit. Do not enable it: the owner chooses the hub at deploy time.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web ./internal/cli ./internal/committee`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web internal/cli/web.go internal/cli/web_test.go docs/web.md deploy
git commit -m "Cache committees on peers and name machines after their Tailscale DNS label"
```

---

### Task 7: Committees page in the UI

**Files:**
- Modify: `internal/web/ui/app.js`, `internal/web/ui/index.html`, `internal/web/ui/app.css`
- Test: `internal/web/ui_test.go`

**Interfaces:**
- Consumes: `GET api/committees` (local), `GET api/presets` on every online machine, and `PUT`/`DELETE api/committees/{name}` on the hub key (`"local"` when `role === "hub"`, else `from`).
- Produces: the route `#/committees`, the sidebar link "Committees", the functions `showCommittees()` and `editCommittee(hubKey, catalogues, c)`, and handling of the `committees` note.

- [ ] **Step 1: Write the failing test.** In `internal/web/ui_test.go` `TestUIContract`, extend the second `want` list with:

```go
		// Committees page (committees spec §6.1, §6.7).
		`"#/committees"`,
		`api("local", "committees")`,
		`method: "PUT"`,
		`method: "DELETE"`,
		`"presets"`,
		`n.kind === "committees"`,
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/web -run TestUIContract`
Expected: FAIL listing the missing strings.

- [ ] **Step 3: Implement.** In `index.html`, inside `<aside id="sidebar">` after the brand div, add `<a id="committees-link" class="item" href="#/committees">Committees</a>`. There is no inline script or style; the href is a plain fragment.

In `app.js`:

1. In `parseHash`, before splitting:

```js
    if (location.hash === "#/committees") return { view: "committees" };
```

2. In `openCurrent`, after `if (!cur) { … }`:

```js
  if (cur.view === "committees") return showCommittees();
```

3. Add a new section before `// ---------- composer + @ autocomplete ----------`:

```js
// ---------- committees ----------
async function showCommittees() {
  $("title").textContent = "Committees";
  const view = $("view");
  view.replaceChildren(el("p", "muted", "Loading…"));
  let data;
  try { data = await api("local", "committees"); } catch (e) { view.replaceChildren(el("p", "error", e.message)); return; }
  const hubKey = data.role === "peer" ? data.from : "local";
  const catalogues = {};
  await Promise.all(state.machines.filter((m) => m.online).map(async (m) => {
    catalogues[m.name] = { key: m.key, presets: await api(m.key, "presets").catch(() => []) };
  }));
  if (!state.current || state.current.view !== "committees") return; // navigated away
  const box = el("div", "committees");
  if (data.role === "peer") {
    const age = data.fetched_at ? fmtAgo(data.fetched_at) : "never";
    box.append(el("p", "muted", `Definitions are kept on ${data.from}; this copy was fetched ${age}.` + (data.error ? ` Last refresh failed: ${data.error}` : "")));
  }
  for (const c of data.committees) {
    const card = el("div", "committee");
    card.append(el("h3", "", `${c.name} · v${c.version}`), el("p", "", c.members.join(", ")),
      el("p", "muted", `deadline ${c.deadline_minutes}m` + (c.skip_exhausted ? " · skips exhausted members" : "")));
    if (c.instructions) card.append(el("pre", "instructions", c.instructions));
    const edit = el("button", "secondary", "Edit");
    edit.onclick = () => editCommittee(hubKey, catalogues, c);
    card.append(edit);
    box.append(card);
  }
  const add = el("button", "", "+ New committee");
  add.onclick = () => editCommittee(hubKey, catalogues, null);
  box.append(add);
  view.replaceChildren(box);
}

function fmtAgo(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  return s < 60 ? `${Math.round(s)}s ago` : s < 3600 ? `${Math.round(s / 60)}m ago` : `${Math.round(s / 3600)}h ago`;
}

function editCommittee(hubKey, catalogues, c) {
  const view = $("view");
  const form = el("form", "committee-edit");
  const name = el("input");
  name.value = c ? c.name : "";
  name.disabled = !!c;
  name.placeholder = "name";
  const deadline = el("input");
  deadline.type = "number"; deadline.min = 1; deadline.max = 240;
  deadline.value = c ? c.deadline_minutes : 30;
  const skip = el("input");
  skip.type = "checkbox";
  skip.checked = !!(c && c.skip_exhausted);
  const instructions = el("textarea");
  instructions.rows = 4;
  instructions.value = c ? c.instructions || "" : "";
  const chosen = new Set(c ? c.members : []);
  const members = el("div", "members");
  for (const [machine, cat] of Object.entries(catalogues)) {
    const group = el("fieldset");
    group.append(el("legend", "", machine));
    for (const p of cat.presets) {
      const id = `${p.name}@${machine}`;
      const label = el("label", "member" + (p.available ? "" : " unavailable"));
      const box = el("input");
      box.type = "checkbox";
      box.checked = chosen.has(id);
      box.disabled = !p.available || p.exec_kind === "relative";
      box.onchange = () => { box.checked ? chosen.add(id) : chosen.delete(id); };
      const q = p.quota && p.quota.percent != null ? ` · ${Math.round(p.quota.percent)}%` : "";
      label.append(box, document.createTextNode(` ${p.name}${q}`));
      for (const w of p.warnings || []) label.append(el("span", "warning", ` ⚠ ${w}`));
      group.append(label);
    }
    members.append(group);
  }
  const status = el("p", "muted");
  const save = el("button", "", "Save");
  const del = el("button", "danger", "Delete");
  del.type = save.type = "button";
  save.onclick = async () => {
    try {
      const res = await api(hubKey, `committees/${encodeURIComponent(name.value)}`, { method: "PUT", body: {
        members: [...chosen], deadline_minutes: Number(deadline.value), instructions: instructions.value, skip_exhausted: skip.checked } });
      status.textContent = (res.warnings || []).length ? "Saved with warnings: " + res.warnings.join("; ") : "Saved.";
      if (!(res.warnings || []).length) showCommittees();
    } catch (e) { status.textContent = e.message; }
  };
  del.onclick = async () => {
    if (!c || !confirm(`Delete committee ${c.name}?`)) return;
    try { await api(hubKey, `committees/${encodeURIComponent(c.name)}`, { method: "DELETE" }); showCommittees(); }
    catch (e) { status.textContent = e.message; }
  };
  const cancel = el("button", "secondary", "Cancel");
  cancel.type = "button";
  cancel.onclick = showCommittees;
  const row = (text, input) => { const l = el("label", "", text + " "); l.append(input); return l; };
  form.append(el("h3", "", c ? `Edit ${c.name}` : "New committee"), row("Name", name), members,
    row("Deadline (minutes)", deadline), row("Skip members whose quota is exhausted", skip),
    row("Instructions", instructions), save, cancel);
  if (c) form.append(del);
  form.append(status);
  view.replaceChildren(form);
}
```

4. In `connect`'s `src.onmessage`, right after the `quota` branch:

```js
    if (n.kind === "committees") {
      if (state.current && state.current.view === "committees") showCommittees();
      return;
    }
```

5. `renderSidebar` reads `state.current.key`. With the committees view, `cur.key` is undefined, which is harmless. Keep it.

In `app.css`, add minimal styles for `.committee`, `.committee-edit label`, `.members fieldset`, `.warning` (warning colour), `.unavailable` (muted), and `#committees-link`. Follow the existing variables and classes in `app.css`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web`
Expected: PASS.

- [ ] **Step 5: Run the whole suite, vet and format**

Run: `gofmt -l . ; go vet ./... && go test ./...`
Expected: no gofmt output; every package passes.

- [ ] **Step 6: Commit**

```bash
git add internal/web/ui internal/web/ui_test.go
git commit -m "Add the Committees page"
```
