# tincan committees 2b-3: reviews Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make committees usable end to end. From MCP (`tincan_review`), the CLI (`tincan review`) or the web UI, the owner or an agent asks committee X to review a change. `tincan web` coordinates the members on both machines until each has a result. Every member's review then comes back as one bundle.

**Architecture:**
- **`internal/review`** owns reviews in the room (`.tincan/reviews/`):
  - IDs, the store, prompts, the bundle and publication;
  - a `Coordinator` that advances each review's member obligations (submit, collect, acknowledge, cancel, each bounded by the member's expiry), then closes and settles the review.
- **A `Transport` interface** is the coordinator's only view of members. `tincan web` implements it: local members go to the in-process `reviewjob.Service`, remote members go through the outbound peer client.
- **The coordinator runs inside each room pass**, on the dispatcher that owns the room.
- **Entry points** (MCP, CLI, web API) publish reviews and request cancellation. They never contact peers.
- **Quota skipping** is decided on the member's machine (`quota.Decide`, config-first).

Thread integration (`@committee`, results in threads, synthesis) is 2b-4.

**Tech Stack:** Go 1.25 standard library, the MCP Go SDK already in use, plain JS UI.

**Spec:** `docs/superpowers/specs/2026-09-28-tincan-committees-design.md`: §6.2 (layout, IDs, `review.json`), §6.3 (publication), §6.4 (placement, member obligations, close, cancel, settle), §6.8 (coordinator prompt), §6.9 (bundle), §6.10 (MCP and CLI, standalone), §7, §8 (quota skip), §12.

## Global Constraints

- **Review ID:** `rv-` + the first 16 hex digits of `sha256(machine + "\0" + canonical room + "\0" + key)`. The key is the request ID; without one, the ID is random (`rv-` + 16 random hex digits). Member job IDs are `<review_id>-<index>`.
- **Replay identity** (`input.json`):
  - committee name, version and ordered member list;
  - question SHA-256, scope, `included_tree`, origin.
  - Equal → return the existing review; different → error `conflict`.
- **Layout:** `<room>/.tincan/reviews/{staging/<id>.<nonce>/, <id>/{input.json, packet.json, prompts/<n>.txt, review.json, results/<n>.md, bundle.md, lock}}`. `results/<n>.md` is written once and never overwritten. `review.json` is rewritten atomically under `lock`.
- **Publication:**
  - requires a fresh heartbeat (< 30 s, except when called from `tincan web` itself);
  - `rooms.Touch` plus confirmed registry membership (excluded rooms refused);
  - a per-attempt staging directory; the rename is the only commit point.
  - Staging directories older than 1 h are removed by the coordinator.
- **Deadline** = created + `deadline_minutes`. A member's `expires_at` = deadline + 30 min.
- **Obligations:**
  - **Submit:** persist `submitting` before the call; retry every 15 s until the deadline, then `unreachable`.
    - 409/400/404/422 → `error` (permanent); 410 → `expired`; 5xx or transport failure → retry.
  - **Collect:** poll every 5 s for `submitted`/`running`/`unreachable`. Past `expires_at` + 5 min, stop; a member still without a result becomes `expired`, or stays `unreachable`.
  - **Acknowledge:** acknowledge a recorded result until 2xx/404/410, or until `expires_at` + 5 min.
  - **Cancel:** after `cancel_requested`, `planned` → `cancelled`; any other non-final member → cancel every 15 s until acknowledged, or until `expires_at`.
- **Close** once, when every member is final or the deadline passes: persist `closing` with an immutable `closure` snapshot, render `bundle.md` from the snapshot alone, then persist `closed`. **Settle** once every obligation has ended.
- **Bundle** ≤ 1 MiB. Header: committee, version, scope, base/included tree, close time, late members. Each member's result appears inline up to `min(200 KiB, 896 KiB / members)`, truncated with a pointer to `results/<n>.md`.
- **Prompt** ≤ 128 KiB. Mode-neutral text, as in spec §6.8, listing omitted paths (at most 200 lines, then "and N more"). Adds a partial-review note when the packet is incomplete, the committee's instructions, and the question.
- **Quota skip** only when `skip_exhausted` is set, decided by the member's machine (spec §8). Any failure means "not exhausted".
- **MCP:** `tincan_review`, `tincan_review_wait` (0–30 s; the bundle is inline when ≤ 256 KiB, else its path), `tincan_review_cancel`.
- **CLI:** `tincan review` with `--committee`, `--question`/`--question-file`, `--scope`, `--request-id`, `--room` and `--wait`, plus `--wait <id>` and `--cancel <id>`. `--wait` exits 4 when the review closed with late or unreachable members (3 is the CLI's existing `ExitTimeout`).
- **Transport caps:** status responses may carry a 1 MiB result that grows with JSON escaping, so the client allows 8 MiB. Job creation allows 60 s; every other call 10 s.
- No new dependencies. gofmt, `go vet ./...` and `GOOS=windows go vet ./...` must stay clean. Never use `git stash`. Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A peer goes offline mid-review.** Its members end `unreachable` or `expired` within their bounds. The review closes at its deadline and eventually settles. Nothing is retried forever, and the review is never left open. Pinned in Task 6.
2. **The coordinator crashes between `closing` and `closed`.** The next pass re-renders a byte-identical bundle from the closure snapshot. A late result arriving after close is recorded but does not change the bundle. Pinned in Task 6.
3. **The same `request_id` is published twice**, concurrently or after a crash. There is one review, and the losing staging directory is removed. A different question with the same key is a conflict. Pinned in Task 5.
4. **Cancel after close while a member is still running (late).** The member is cancelled remotely, and its result is kept if it had already finished. Pinned in Task 6.
5. **`tincan_review` when `tincan web` is not running.** A clear error and nothing written, never a review that no coordinator will advance. Pinned in Task 5 and Task 9.

---

### Task 1: Machine-local quota skip decision

**Files:**
- Create: `internal/quota/decide.go`
- Modify: `internal/web/api.go` (the route `GET /api/presets/{preset}/quota-status`)
- Test: `internal/quota/decide_test.go`, `internal/web/api_test.go`

**Interfaces:**
- Produces `type Decision struct { Exhausted bool; Window string; ResetAt *time.Time; Reason string }`, with JSON tags `exhausted`, `window,omitempty`, `reset_at,omitempty`, `reason`.
- Produces `func Decide(cacheDir, configPath, preset string, now time.Time) Decision`.

- [ ] **Step 1: Write the failing test.** Create `internal/quota/decide_test.go`:

```go
package quota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecideSkipsOnlyWhenEveryConditionHolds(t *testing.T) {
	now := time.Now()
	future := ftoa(unix(now.Add(2 * time.Hour)))
	fetched := ftoa(unix(now.Add(-time.Minute)))
	full := `{"percent": 100, "reset_at": ` + future + `, "short_percent": 20, "short_reset_at": ` + future + `, "fetched_at": ` + fetched + `}`
	shortFull := `{"percent": 40, "reset_at": ` + future + `, "short_percent": 100, "short_reset_at": ` + future + `, "fetched_at": ` + fetched + `}`
	cases := []struct {
		name, config, cache string
		exhausted           bool
		reason              string
	}{
		{"weekly exhausted", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, full, true, ""},
		{"either takes short", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "either"}}`, shortFull, true, ""},
		{"short not blocking weekly", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, shortFull, false, "not exhausted"},
		{"no blocking", `{"codex-gmail": {"presets": ["codex-gmail"]}}`, full, false, "blocking"},
		{"unmapped", `{}`, full, false, "no quota entry"},
		{"ambiguous with missing cache", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}, "other": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, full, false, "several"},
		{"malformed config", `{not json`, full, false, "malformed"},
		{"no cache", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, "", false, "no cached"},
		{"stale", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, `{"percent": 100, "reset_at": ` + future + `, "fetched_at": ` + ftoa(unix(now.Add(-2*time.Hour))) + `}`, false, "stale"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "quotas.json")
		os.WriteFile(cfg, []byte(c.config), 0o600)
		if c.cache != "" {
			os.WriteFile(filepath.Join(dir, "codex-quota-gmail.json"), []byte(c.cache), 0o600)
		}
		d := Decide(dir, cfg, "codex-gmail", now)
		if d.Exhausted != c.exhausted || (c.reason != "" && !strings.Contains(d.Reason, c.reason)) {
			t.Errorf("%s: %+v", c.name, d)
		}
		if d.Exhausted && (d.ResetAt == nil || d.Window == "") {
			t.Errorf("%s: exhausted without window/reset: %+v", c.name, d)
		}
	}
}
```

The cache file name must map to the entry ID `codex-gmail`. Check `Files` in `quota.go` for the exact naming pattern (`<provider>-quota[-<profile>].json` → ID `<provider>-<profile>`), and adjust the fixture name so the ID matches. `ftoa` and `unix` are existing helpers in `quota_test.go`. The staleness window follows the existing `state` function; if two hours is not stale there, use the age the existing stale test uses.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/quota -run TestDecide`
Expected: build failure (`undefined: Decide`).

- [ ] **Step 3: Implement.** Create `internal/quota/decide.go`:

```go
package quota

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// Decision is a machine-local answer to "is this preset's quota
// exhausted?" (committees spec §8). Anything uncertain is not exhausted.
type Decision struct {
	Exhausted bool       `json:"exhausted"`
	Window    string     `json:"window,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	Reason    string     `json:"reason"`
}

// Decide counts mappings over the whole quotas.json (including entries whose
// cache file is absent) before reading any cache: exactly one explicit
// mapping with a blocking window, a fresh successful reading, and that
// window at 100% with a future reset are all required.
func Decide(cacheDir, configPath, preset string, now time.Time) Decision {
	no := func(reason string) Decision { return Decision{Reason: reason} }
	maps := map[string]mapping{}
	data, err := os.ReadFile(configPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return no("no quota entry maps this preset (no quotas.json)")
	case err != nil:
		return no("quotas.json unreadable: " + err.Error())
	}
	if err := json.Unmarshal(data, &maps); err != nil {
		return no("quotas.json is malformed")
	}
	var id string
	var m mapping
	n := 0
	for eid, em := range maps {
		for _, p := range em.Presets {
			if p == preset {
				id, m = eid, em
				n++
				break
			}
		}
	}
	switch {
	case n == 0:
		return no("no quota entry maps this preset")
	case n > 1:
		return no("several quota entries map this preset")
	case m.Blocking == "":
		return no("the quota entry does not declare blocking")
	}
	files, err := Files(cacheDir)
	if err != nil {
		return no("quota caches unreadable: " + err.Error())
	}
	path, ok := files[id]
	if !ok {
		return no("no cached quota reading for " + id)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return no("quota cache unreadable: " + err.Error())
	}
	e, err := Parse(id, raw, now)
	if err != nil {
		return no("quota cache malformed")
	}
	if e.State != StateOK {
		return no("quota reading is " + e.State)
	}
	check := func(window string, pct *float64, reset *time.Time) (Decision, bool) {
		if pct != nil && *pct >= 100 && reset != nil && reset.After(now) {
			return Decision{Exhausted: true, Window: window, ResetAt: reset, Reason: window + " quota exhausted"}, true
		}
		return Decision{}, false
	}
	if m.Blocking == "weekly" || m.Blocking == "either" {
		if d, ok := check("weekly", e.Percent, e.ResetAt); ok {
			return d
		}
	}
	if m.Blocking == "short" || m.Blocking == "either" {
		if d, ok := check("short", e.ShortPercent, e.ShortResetAt); ok {
			return d
		}
	}
	return no("quota not exhausted")
}
```

In `internal/web/api.go` `apiRoutes`, add `s.mux.HandleFunc("GET /api/presets/{preset}/quota-status", s.quotaStatus)`, and:

```go
func (s *Server) quotaStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, quota.Decide(s.cfg.QuotaDir, s.cfg.QuotaConfig, r.PathValue("preset"), time.Now()))
}
```

Add to `internal/web/api_test.go`:

```go
func TestQuotaStatusEndpoint(t *testing.T) {
	s, _ := apiServer(t)
	rec := do(t, s.Handler(), "GET", "/api/presets/claude/quota-status", "", ownerHdr())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"exhausted":false`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/quota ./internal/web`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/quota internal/web
git commit -m "Decide quota skips on the member's machine, counting mappings config-first"
```

---

### Task 2: Heartbeat names the committee source; committee lookup

**Files:**
- Modify: `internal/rooms/heartbeat.go` (`Heartbeat.CommitteesFrom`, `ReadHeartbeat`)
- Modify: `internal/web/events.go` (`tick` writes `CommitteesFrom`)
- Modify: `internal/committee/store.go` (`Lookup`)
- Test: `internal/rooms/heartbeat_test.go`, `internal/committee/store_test.go`

**Interfaces:**
- Produces the field `rooms.Heartbeat.CommitteesFrom string` (`json:"committees_from,omitempty"`).
- Produces `func ReadHeartbeat(stateDir string) (Heartbeat, error)` (the exported form of `readHeartbeat`).
- Produces `func committee.Lookup(stateDir, name string, fromPeer bool) (Committee, error)`. It reads the peer cache when `fromPeer`, else the hub store. It errors with "committee %q not found" plus the cache age for a peer.

- [ ] **Step 1: Write the failing tests.** Add to `internal/rooms/heartbeat_test.go`:

```go
func TestHeartbeatCarriesCommitteeSource(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	WriteHeartbeat(dir, Heartbeat{PID: 1, Machine: "cachyos", CommitteesFrom: "macmini", Updated: time.Now()})
	hb, err := ReadHeartbeat(dir)
	if err != nil || hb.CommitteesFrom != "macmini" || hb.Machine != "cachyos" {
		t.Fatalf("%+v %v", hb, err)
	}
}
```

Add to `internal/committee/store_test.go`:

```go
func TestLookupHubAndPeer(t *testing.T) {
	dir := stateDir(t)
	NewStore(dir).Put(context.Background(), Committee{Name: "hubbed", Members: []string{"x@m"}})
	SaveCache(dir, Cache{From: "macmini", FetchedAt: time.Now(), Committees: []Committee{{Name: "cached", Version: 4, Members: []string{"y@m"}, DeadlineMinutes: 30}}})
	if c, err := Lookup(dir, "hubbed", false); err != nil || c.Version != 1 {
		t.Fatalf("hub lookup: %+v %v", c, err)
	}
	if c, err := Lookup(dir, "cached", true); err != nil || c.Version != 4 {
		t.Fatalf("peer lookup: %+v %v", c, err)
	}
	if _, err := Lookup(dir, "hubbed", true); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("peer must not read the hub store: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/rooms ./internal/committee`
Expected: build failures (`unknown field CommitteesFrom`, `undefined: Lookup`).

- [ ] **Step 3: Implement.** In `heartbeat.go`, add `CommitteesFrom string \`json:"committees_from,omitempty"\`` to `Heartbeat` and rename `readHeartbeat` to the exported `ReadHeartbeat`, updating its callers. In `events.go` `tick`, include `CommitteesFrom: s.cfg.CommitteesFrom` in the `rooms.Heartbeat` literal. Add to `internal/committee/store.go`:

```go
// Lookup finds a committee on this machine: in the peer cache when this
// machine reads committees from a hub, else in the hub store.
func Lookup(stateDir, name string, fromPeer bool) (Committee, error) {
	var list []Committee
	where := "on this machine (the committees hub)"
	if fromPeer {
		c, ok, err := LoadCache(stateDir)
		if err != nil {
			return Committee{}, err
		}
		if !ok {
			return Committee{}, fmt.Errorf("committee %q not found: no committees fetched from the hub yet", name)
		}
		list = c.Committees
		where = fmt.Sprintf("in the copy fetched from %s at %s", c.From, c.FetchedAt.Format(time.RFC3339))
	} else {
		var err error
		if list, err = NewStore(stateDir).List(); err != nil {
			return Committee{}, err
		}
	}
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
	}
	return Committee{}, fmt.Errorf("committee %q not found %s", name, where)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/rooms ./internal/committee ./internal/web`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/rooms internal/committee internal/web/events.go
git commit -m "Record the committee source in the heartbeat and look committees up locally"
```

---

### Task 3: `internal/review` — model, IDs and the room store

**Files:**
- Create: `internal/review/model.go`, `internal/review/store.go`
- Test: `internal/review/store_test.go`

**Interfaces:**
- Produces the type `Input`:

  ```go
  type Input struct {
      ReviewID     string              `json:"review_id"`
      Committee    committee.Committee `json:"committee"`
      Question     string              `json:"question"`
      QuestionSHA  string              `json:"question_sha256"`
      Scope        string              `json:"scope"`
      IncludedTree string              `json:"included_tree,omitempty"`
      Origin       string              `json:"origin"`
      Requester    string              `json:"requester"`
      Created      time.Time           `json:"created"`
      Deadline     time.Time           `json:"deadline"`
      PacketSHA    string              `json:"packet_sha256"`
  }
  ```

- Produces the type `Member`:

  ```go
  type Member struct {
      Member      string    `json:"member"`
      Index       int       `json:"index"`
      JobID       string    `json:"job_id"`
      ExpiresAt   time.Time `json:"expires_at"`
      State       string    `json:"state"`
      Late        bool      `json:"late,omitempty"`
      Posted      bool      `json:"posted,omitempty"`
      Acked       bool      `json:"acked,omitempty"`
      CancelAcked bool      `json:"cancel_acked,omitempty"`
      Mode        string    `json:"mode,omitempty"`
      Started     time.Time `json:"started,omitempty"`
      Finished    time.Time `json:"finished,omitempty"`
      Note        string    `json:"note,omitempty"`
  }
  ```

- Produces the types `Closure` and `State`:

  ```go
  type ClosedMember struct {
      Index    int    `json:"index"`
      State    string `json:"state"`
      Late     bool   `json:"late,omitempty"`
      Included bool   `json:"included"`
  }
  type Closure struct {
      At      time.Time      `json:"at"`
      Members []ClosedMember `json:"members"`
  }
  type State struct {
      ReviewID        string     `json:"review_id"`
      Status          string     `json:"status"` // running|closing|closed|cancelled
      Settled         bool       `json:"settled"`
      CancelRequested bool       `json:"cancel_requested,omitempty"`
      ClosedAt        *time.Time `json:"closed_at,omitempty"`
      Closure         *Closure   `json:"closure,omitempty"`
      ThreadDone      bool       `json:"thread_done,omitempty"`
      Members         []Member   `json:"members"`
  }
  ```

- Produces:
  - `func (in Input) SameIdentity(o Input) bool`
  - `func Final(state string) bool`: done, error, skipped, cancelled, expired.
  - `func NewID(machine, room, key string) string`
  - `func Root(room string) string`, which returns `<room>/.tincan/reviews`
  - `func Dir(room, id string) string`
  - `func List(room string) ([]string, error)`: published IDs, sorted.
  - `func ReadInput(room, id string) (Input, error)`
  - `func ReadPacket(room, id string) (*packet.Packet, error)`
  - `func ReadPrompt(room, id string, n int) (string, error)`
  - `func ReadState(room, id string) (State, error)`
  - `func WriteState(room, id string, st State) error`
  - `func Lock(ctx context.Context, room, id string) (*filelock.Lock, error)`
  - `func WriteResult(room, id string, n int, text string) error`: write-once, a no-op if present.
  - `func ReadResult(room, id string, n int) (string, bool, error)`
  - `func ReadBundle(room, id string) (string, bool, error)`
- Constants: `ContentLimit = 1 << 20` (result and bundle file reads).

- [ ] **Step 1: Write the failing test.** Create `internal/review/store_test.go`:

```go
package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
)

func roomDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewIDIsDeterministicPerMachineRoomAndKey(t *testing.T) {
	a := NewID("macmini", "/r", "k1")
	if a != NewID("macmini", "/r", "k1") || !strings.HasPrefix(a, "rv-") || len(a) != 19 {
		t.Fatalf("id %q", a)
	}
	for _, other := range []string{NewID("cachyos", "/r", "k1"), NewID("macmini", "/s", "k1"), NewID("macmini", "/r", "k2")} {
		if other == a {
			t.Fatal("IDs collide across machine, room or key")
		}
	}
	if r1, r2 := NewID("m", "/r", ""), NewID("m", "/r", ""); r1 == r2 {
		t.Fatal("random IDs repeat")
	}
}

func TestStoreStateResultsAndIdentity(t *testing.T) {
	room := roomDir(t)
	id := NewID("m", room, "k")
	os.MkdirAll(filepath.Join(Dir(room, id), "results"), 0o700)
	st := State{ReviewID: id, Status: "running", Members: []Member{{Member: "codex@m", Index: 0, JobID: id + "-0", State: "planned"}}}
	if err := WriteState(room, id, st); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(room, id)
	if err != nil || got.Members[0].JobID != id+"-0" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := WriteResult(room, id, 0, "first"); err != nil {
		t.Fatal(err)
	}
	WriteResult(room, id, 0, "second")
	if s, ok, _ := ReadResult(room, id, 0); !ok || s != "first" {
		t.Fatalf("result overwritten: %q", s)
	}
	l, err := Lock(context.Background(), room, id)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	c := committee.Committee{Name: "r", Version: 2, Members: []string{"a@m", "b@m"}}
	in := Input{ReviewID: id, Committee: c, QuestionSHA: "q", Scope: "none", Origin: "mcp", Created: time.Now()}
	same := in
	same.Created = time.Now().Add(time.Hour)
	if !in.SameIdentity(same) {
		t.Fatal("created time is not part of the identity")
	}
	other := in
	other.Committee.Members = []string{"b@m", "a@m"}
	if in.SameIdentity(other) {
		t.Fatal("member order ignored")
	}
	ids, _ := List(room)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("list: %v", ids)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review`
Expected: build failure.

- [ ] **Step 3: Implement.** Create `internal/review/model.go`:

```go
// Package review owns committee reviews in a room (committees spec §6.2–§6.4,
// §6.8–§6.10): publication by entry points, and a coordinator that advances
// each member through submit, collect, acknowledge and cancel until the
// review closes and settles.
package review

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
)

type Input struct {
	ReviewID     string              `json:"review_id"`
	Committee    committee.Committee `json:"committee"`
	Question     string              `json:"question"`
	QuestionSHA  string              `json:"question_sha256"`
	Scope        string              `json:"scope"`
	IncludedTree string              `json:"included_tree,omitempty"`
	Origin       string              `json:"origin"`
	Requester    string              `json:"requester"`
	Created      time.Time           `json:"created"`
	Deadline     time.Time           `json:"deadline"`
	PacketSHA    string              `json:"packet_sha256"`
}

// SameIdentity compares what a replay must match (committees §6.2).
func (in Input) SameIdentity(o Input) bool {
	return in.Committee.Name == o.Committee.Name && in.Committee.Version == o.Committee.Version &&
		slices.Equal(in.Committee.Members, o.Committee.Members) && in.QuestionSHA == o.QuestionSHA &&
		in.Scope == o.Scope && in.IncludedTree == o.IncludedTree && in.Origin == o.Origin
}

type Member struct {
	Member      string    `json:"member"`
	Index       int       `json:"index"`
	JobID       string    `json:"job_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	State       string    `json:"state"`
	Late        bool      `json:"late,omitempty"`
	Posted      bool      `json:"posted,omitempty"`
	Acked       bool      `json:"acked,omitempty"`
	CancelAcked bool      `json:"cancel_acked,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Started     time.Time `json:"started,omitempty"`
	Finished    time.Time `json:"finished,omitempty"`
	Note        string    `json:"note,omitempty"`
}

type ClosedMember struct {
	Index    int    `json:"index"`
	State    string `json:"state"`
	Late     bool   `json:"late,omitempty"`
	Included bool   `json:"included"`
}

type Closure struct {
	At      time.Time      `json:"at"`
	Members []ClosedMember `json:"members"`
}

type State struct {
	ReviewID        string     `json:"review_id"`
	Status          string     `json:"status"`
	Settled         bool       `json:"settled"`
	CancelRequested bool       `json:"cancel_requested,omitempty"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	Closure         *Closure   `json:"closure,omitempty"`
	ThreadDone      bool       `json:"thread_done,omitempty"`
	Members         []Member   `json:"members"`
}

// Final reports member states that need no further collection.
func Final(state string) bool {
	switch state {
	case "done", "error", "skipped", "cancelled", "expired":
		return true
	}
	return false
}

// NewID derives a review ID from the requesting machine, room and key; an
// empty key yields a random ID (committees §6.2).
func NewID(machine, room, key string) string {
	if key == "" {
		var b [8]byte
		rand.Read(b[:])
		return "rv-" + hex.EncodeToString(b[:])
	}
	sum := sha256.Sum256([]byte(machine + "\x00" + room + "\x00" + key))
	return "rv-" + hex.EncodeToString(sum[:])[:16]
}
```

Create `internal/review/store.go`:

```go
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/packet"
)

const ContentLimit = 1 << 20

func Root(room string) string { return filepath.Join(room, ".tincan", "reviews") }

func validID(id string) error {
	if !strings.HasPrefix(id, "rv-") || len(id) != 19 || strings.ContainsAny(id[3:], "./\\") {
		return fmt.Errorf("invalid review id %q", id)
	}
	return nil
}

func Dir(room, id string) string { return filepath.Join(Root(room), id) }

func List(room string) ([]string, error) {
	entries, err := os.ReadDir(Root(room))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && validID(e.Name()) == nil {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func readJSON(path string, limit int64, v any) error {
	data, err := fsutil.ReadFile(path, limit)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data)
}

func ReadInput(room, id string) (Input, error) {
	var in Input
	if err := validID(id); err != nil {
		return in, err
	}
	return in, readJSON(filepath.Join(Dir(room, id), "input.json"), 1<<20, &in)
}

func ReadPacket(room, id string) (*packet.Packet, error) {
	var p packet.Packet
	if err := validID(id); err != nil {
		return nil, err
	}
	return &p, readJSON(filepath.Join(Dir(room, id), "packet.json"), packet.MaxEncoded+(1<<20), &p)
}

func ReadPrompt(room, id string, n int) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	data, err := fsutil.ReadFile(filepath.Join(Dir(room, id), "prompts", strconv.Itoa(n)+".txt"), 256<<10)
	return string(data), err
}

func ReadState(room, id string) (State, error) {
	var st State
	if err := validID(id); err != nil {
		return st, err
	}
	return st, readJSON(filepath.Join(Dir(room, id), "review.json"), 4<<20, &st)
}

func WriteState(room, id string, st State) error {
	if err := validID(id); err != nil {
		return err
	}
	return writeJSON(filepath.Join(Dir(room, id), "review.json"), st)
}

func Lock(ctx context.Context, room, id string) (*filelock.Lock, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	return filelock.Acquire(ctx, filepath.Join(Dir(room, id), "lock"))
}

func resultPath(room, id string, n int) string {
	return filepath.Join(Dir(room, id), "results", strconv.Itoa(n)+".md")
}

// WriteResult records member n's result once; an existing result is kept.
func WriteResult(room, id string, n int, text string) error {
	p := resultPath(room, id, n)
	if _, err := os.Lstat(p); err == nil {
		return nil
	}
	if err := fsutil.MkdirPrivate(filepath.Dir(p)); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(p, []byte(text))
}

func ReadResult(room, id string, n int) (string, bool, error) {
	data, err := fsutil.ReadFile(resultPath(room, id, n), ContentLimit+64)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return string(data), err == nil, err
}

func ReadBundle(room, id string) (string, bool, error) {
	data, err := fsutil.ReadFile(filepath.Join(Dir(room, id), "bundle.md"), ContentLimit+64)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	return string(data), err == nil, err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/review`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/review
git commit -m "Add the review model, IDs and room store"
```

---

### Task 4: Coordinator prompt and bundle

**Files:**
- Create: `internal/review/render.go`
- Test: `internal/review/render_test.go`

**Interfaces:**
- Produces:
  - `func BuildPrompt(c committee.Committee, member string, p *packet.Packet) string`
  - `func RenderBundle(in Input, st State, results map[int]string) []byte`, which uses only `st.Closure`
- Constants: `MaxPrompt = 128 << 10`, `MaxBundle = 1 << 20`.

- [ ] **Step 1: Write the failing test.** Create `internal/review/render_test.go`:

```go
package review

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/packet"
)

func TestPromptIsModeNeutralAndBounded(t *testing.T) {
	c := committee.Committee{Name: "reviewers", Instructions: "Cite file:line."}
	p := &packet.Packet{Question: "Is it safe?", Manifest: packet.Manifest{Complete: false}}
	for i := 0; i < 500; i++ {
		p.Manifest.Changes = append(p.Manifest.Changes, packet.Change{Path: fmt.Sprintf("secret%d.env", i), Reason: "secret-looking path"})
	}
	got := BuildPrompt(c, "codex@cachyos", p)
	for _, want := range []string{"member `codex@cachyos` of committee `reviewers`", "Review only", "Cite file:line.", "Is it safe?", "partial", "and 300 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, mode := range []string{"git diff --cached", "packet/"} {
		if strings.Contains(got, mode) {
			t.Errorf("prompt mentions workspace mode %q", mode)
		}
	}
	huge := &packet.Packet{Question: strings.Repeat("q", packet.MaxQuestion)}
	c.Instructions = strings.Repeat("i", committee.MaxInstructions)
	if n := len(BuildPrompt(c, "m@x", huge)); n > MaxPrompt {
		t.Fatalf("prompt %d bytes", n)
	}
}

func TestBundleRendersOnlyTheClosureAndIsBounded(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	in := Input{Committee: committee.Committee{Name: "reviewers", Version: 3}, Scope: "uncommitted", IncludedTree: "abc"}
	st := State{Members: []Member{{Member: "a@m", Index: 0, Started: at.Add(-4 * time.Minute), Finished: at}, {Member: "b@m", Index: 1}, {Member: "c@m", Index: 2, Note: "quota exhausted"}},
		Closure: &Closure{At: at, Members: []ClosedMember{{Index: 0, State: "done", Included: true}, {Index: 1, State: "running", Late: true}, {Index: 2, State: "skipped"}}}}
	results := map[int]string{0: "LGTM", 1: "arrived after close"}
	b := string(RenderBundle(in, st, results))
	for _, want := range []string{"reviewers", "v3", "## a@m — done in 4m0s", "LGTM", "## b@m — late (still running)", "## c@m — skipped: quota exhausted"} {
		if !strings.Contains(b, want) {
			t.Errorf("bundle lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "arrived after close") {
		t.Fatal("bundle includes a result not in the closure")
	}
	if string(RenderBundle(in, st, results)) != b {
		t.Fatal("bundle not deterministic")
	}
	big := map[int]string{0: strings.Repeat("x", 3<<20)}
	if n := len(RenderBundle(in, st, big)); n > MaxBundle {
		t.Fatalf("bundle %d bytes", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review -run 'TestPrompt|TestBundle'`
Expected: build failure.

- [ ] **Step 3: Implement.** Create `internal/review/render.go`:

```go
package review

import (
	"fmt"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/packet"
)

const (
	MaxPrompt = 128 << 10
	MaxBundle = 1 << 20
)

// BuildPrompt is the coordinator's mode-neutral prompt for one member
// (committees §6.8); the receiver prepends its own workspace note.
func BuildPrompt(c committee.Committee, member string, p *packet.Packet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are reviewing a change for the owner as member `%s` of committee `%s`. Review only: do not modify, create or delete files, and do not run commands that change state. Cite file:line.\n", member, c.Name)
	var omitted []packet.Change
	for _, ch := range p.Manifest.Changes {
		if !ch.Included {
			omitted = append(omitted, ch)
		}
	}
	if len(omitted) > 0 {
		b.WriteString("\nThese changed paths were not sent to you:\n")
		for i, ch := range omitted {
			if i == 200 {
				fmt.Fprintf(&b, "- … and %d more\n", len(omitted)-200)
				break
			}
			fmt.Fprintf(&b, "- %s (%s)\n", ch.Path, ch.Reason)
		}
	}
	if !p.Manifest.Complete {
		b.WriteString("\nThis is a partial review: the full patch was too large, so only some post-change file contents are available.\n")
	}
	if c.Instructions != "" {
		b.WriteString("\nCommittee instructions:\n" + c.Instructions + "\n")
	}
	b.WriteString("\nQuestion:\n" + p.Question + "\n")
	s := b.String()
	if len(s) > MaxPrompt {
		s = s[:MaxPrompt-32] + "\n[prompt truncated]\n"
	}
	return s
}

// RenderBundle renders bundle.md from the closure snapshot alone, so a
// re-render after a crash is byte-identical (committees §6.4, §6.9).
func RenderBundle(in Input, st State, results map[int]string) []byte {
	if st.Closure == nil {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Committee `%s` (v%d) review\n\n", in.Committee.Name, in.Committee.Version)
	fmt.Fprintf(&b, "Scope: %s", in.Scope)
	if in.IncludedTree != "" {
		fmt.Fprintf(&b, " · included tree %s", in.IncludedTree)
	}
	fmt.Fprintf(&b, "\nClosed: %s\n", st.Closure.At.UTC().Format(time.RFC3339))
	var late []string
	for _, cm := range st.Closure.Members {
		if cm.Late {
			late = append(late, st.Members[cm.Index].Member)
		}
	}
	if len(late) > 0 {
		fmt.Fprintf(&b, "Late (still running at close): %s\n", strings.Join(late, ", "))
	}
	n := len(st.Closure.Members)
	per := 200 << 10
	if n > 0 && (896<<10)/n < per {
		per = (896 << 10) / n
	}
	for _, cm := range st.Closure.Members {
		m := st.Members[cm.Index]
		switch {
		case cm.Late:
			fmt.Fprintf(&b, "\n## %s — late (still running)\n", m.Member)
		case cm.State == "done":
			fmt.Fprintf(&b, "\n## %s — done in %s\n", m.Member, m.Finished.Sub(m.Started).Round(time.Second))
		case cm.State == "skipped" || cm.State == "unreachable" || cm.State == "error" || cm.State == "expired":
			fmt.Fprintf(&b, "\n## %s — %s", m.Member, cm.State)
			if m.Note != "" {
				fmt.Fprintf(&b, ": %s", m.Note)
			}
			b.WriteString("\n")
		default:
			fmt.Fprintf(&b, "\n## %s — %s\n", m.Member, cm.State)
		}
		if cm.Included {
			r := results[cm.Index]
			if len(r) > per {
				r = r[:per] + fmt.Sprintf("\n\n[truncated; full result in results/%d.md]", cm.Index)
			}
			b.WriteString("\n" + r + "\n")
		}
	}
	out := b.String()
	if len(out) > MaxBundle {
		out = out[:MaxBundle-40] + "\n[bundle truncated]\n"
	}
	return []byte(out)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/review`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/review
git commit -m "Render member prompts and the review bundle"
```

---

### Task 5: Publication and cancellation requests

**Files:**
- Create: `internal/review/publish.go`
- Test: `internal/review/publish_test.go`

**Interfaces:**
- Consumes: `rooms.CoordinatorAlive`, `rooms.ReadHeartbeat`, `rooms.Registry`, `committee.Lookup`, `packet.Build`, `packet.Hash`, `BuildPrompt`.
- Produces:

  ```go
  type PublishRequest struct {
      StateDir       string
      Room           string
      Committee      string
      Question       string
      Scope          string
      RequestID      string
      Origin         string          // mcp | cli | web
      Machine        string          // "" = from the heartbeat
      CommitteesFrom *string         // nil = from the heartbeat
      InCoordinator  bool            // called by tincan web itself: no heartbeat check
      Registry       *rooms.Registry // nil = rooms.Default()
      Now            func() time.Time
  }
  ```

- Produces `func Publish(ctx context.Context, req PublishRequest) (Input, State, error)`.
- Produces `func RequestCancel(ctx context.Context, room, id string) (State, error)`.
- Produces the error values `ErrNoCoordinator` and `ErrConflict`.

- [ ] **Step 1: Write the failing test.** Create `internal/review/publish_test.go`:

```go
package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func publishFixture(t *testing.T) (PublishRequest, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	state := roomDir(t)
	t.Setenv("TINCAN_STATE_DIR", state)
	room := roomDir(t)
	cmd := exec.Command("git", "init", "-q", "-b", "main")
	cmd.Dir = room
	cmd.Run()
	os.WriteFile(filepath.Join(room, "a.txt"), []byte("a\n"), 0o644)
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "reviewers", Members: []string{"codex@macmini", "grok@cachyos"}, DeadlineMinutes: 20})
	rooms.WriteHeartbeat(state, rooms.Heartbeat{PID: 1, Machine: "macmini", Updated: time.Now()})
	return PublishRequest{StateDir: state, Room: room, Committee: "reviewers", Question: "safe?", Origin: "mcp",
		Registry: rooms.Open(filepath.Join(state, "rooms.json"))}, room
}

func TestPublishCreatesACompleteReview(t *testing.T) {
	req, room := publishFixture(t)
	in, st, err := Publish(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if in.Requester != "macmini" || in.Committee.Version != 1 || in.Deadline.Sub(in.Created) != 20*time.Minute || in.Scope != "uncommitted" {
		t.Fatalf("input: %+v", in)
	}
	if st.Status != "running" || len(st.Members) != 2 || st.Members[1].JobID != in.ReviewID+"-1" || !st.Members[0].ExpiresAt.Equal(in.Deadline.Add(30*time.Minute)) {
		t.Fatalf("state: %+v", st)
	}
	for _, f := range []string{"input.json", "packet.json", "prompts/0.txt", "prompts/1.txt", "review.json"} {
		if _, err := os.Stat(filepath.Join(Dir(room, in.ReviewID), f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if prompt, _ := ReadPrompt(room, in.ReviewID, 1); !strings.Contains(prompt, "grok@cachyos") {
		t.Fatalf("prompt: %q", prompt)
	}
	if list, _ := req.Registry.List(); len(list) != 1 {
		t.Fatalf("room not registered: %+v", list)
	}
}

func TestPublishIsIdempotentPerRequestID(t *testing.T) {
	req, room := publishFixture(t)
	req.RequestID = "k1"
	var wg sync.WaitGroup
	ids := make([]string, 4)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, _, err := Publish(context.Background(), req)
			if err != nil {
				t.Error(err)
			}
			ids[i] = in.ReviewID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent publications diverged: %v", ids)
		}
	}
	if list, _ := List(room); len(list) != 1 {
		t.Fatalf("reviews: %v", list)
	}
	if entries, _ := os.ReadDir(filepath.Join(Root(room), "staging")); len(entries) != 0 {
		t.Fatalf("staging left behind: %d", len(entries))
	}
	req.Question = "different"
	if _, _, err := Publish(context.Background(), req); !errors.Is(err, ErrConflict) {
		t.Fatalf("different question: %v", err)
	}
}

func TestPublishRefusals(t *testing.T) {
	req, room := publishFixture(t)
	stale := req
	rooms.WriteHeartbeat(req.StateDir, rooms.Heartbeat{PID: 1, Machine: "macmini", Updated: time.Now().Add(-time.Hour)})
	if _, _, err := Publish(context.Background(), stale); !errors.Is(err, ErrNoCoordinator) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	if _, err := os.Stat(Root(room)); err == nil {
		t.Fatal("refused publication wrote reviews")
	}
	rooms.WriteHeartbeat(req.StateDir, rooms.Heartbeat{PID: 1, Machine: "macmini", Updated: time.Now()})
	for name, mutate := range map[string]func(*PublishRequest){
		"unknown committee": func(r *PublishRequest) { r.Committee = "nobody" },
		"empty question":    func(r *PublishRequest) { r.Question = "  " },
		"bad scope":         func(r *PublishRequest) { r.Scope = "sideways" },
		"excluded room":     func(r *PublishRequest) { r.Room = filepath.Join(r.StateDir, "reviews", "ws", "x"); os.MkdirAll(r.Room, 0o700) },
	} {
		r := req
		mutate(&r)
		if _, _, err := Publish(context.Background(), r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRequestCancel(t *testing.T) {
	req, room := publishFixture(t)
	in, _, _ := Publish(context.Background(), req)
	st, err := RequestCancel(context.Background(), room, in.ReviewID)
	if err != nil || !st.CancelRequested || st.Status != "cancelled" {
		t.Fatalf("%+v %v", st, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/review -run 'TestPublish|TestRequestCancel'`
Expected: build failure.

- [ ] **Step 3: Implement.** Create `internal/review/publish.go`:

```go
package review

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

var (
	ErrNoCoordinator = errors.New("tincan web is not running on this machine; start it (see docs/web.md)")
	ErrConflict      = errors.New("a different review already uses this request_id")
)

type PublishRequest struct {
	StateDir       string
	Room           string
	Committee      string
	Question       string
	Scope          string
	RequestID      string
	Origin         string
	Machine        string
	CommitteesFrom *string
	InCoordinator  bool
	Registry       *rooms.Registry
	Now            func() time.Time
}

// Publish creates a review in the room (committees §6.3): coordinator check,
// registry membership, replay, per-attempt staging, then one rename.
func Publish(ctx context.Context, req PublishRequest) (Input, State, error) {
	now := time.Now
	if req.Now != nil {
		now = req.Now
	}
	if strings.TrimSpace(req.Question) == "" {
		return Input{}, State{}, errors.New("the question is empty")
	}
	if req.StateDir == "" {
		return Input{}, State{}, errors.New("no tincan state directory")
	}
	machine, fromPeer := req.Machine, false
	if req.CommitteesFrom != nil {
		fromPeer = *req.CommitteesFrom != ""
	}
	if !req.InCoordinator {
		if !rooms.CoordinatorAlive(req.StateDir, now()) {
			return Input{}, State{}, ErrNoCoordinator
		}
		hb, err := rooms.ReadHeartbeat(req.StateDir)
		if err != nil {
			return Input{}, State{}, ErrNoCoordinator
		}
		if machine == "" {
			machine = hb.Machine
		}
		if req.CommitteesFrom == nil {
			fromPeer = hb.CommitteesFrom != ""
		}
	}
	if machine == "" {
		return Input{}, State{}, errors.New("this machine's name is unknown")
	}
	c, err := committee.Lookup(req.StateDir, req.Committee, fromPeer)
	if err != nil {
		return Input{}, State{}, err
	}
	room, err := rooms.Canonical(req.Room)
	if err != nil {
		return Input{}, State{}, err
	}
	reg := req.Registry
	if reg == nil {
		reg = rooms.Default()
	}
	if err := reg.Touch(room); err != nil {
		return Input{}, State{}, fmt.Errorf("room registry: %w", err)
	}
	if r, ok, err := reg.Get(rooms.ID(room)); err != nil || !ok || r.Missing {
		return Input{}, State{}, fmt.Errorf("%s cannot be coordinated (excluded, too broad, or the room registry is unavailable)", room)
	}
	id := NewID(machine, room, req.RequestID)
	p, err := packet.Build(ctx, room, req.Scope, req.Question)
	if err != nil {
		return Input{}, State{}, err
	}
	sum := sha256.Sum256([]byte(req.Question))
	created := now().UTC()
	in := Input{ReviewID: id, Committee: c, Question: req.Question, QuestionSHA: hex.EncodeToString(sum[:]), Scope: p.Manifest.Scope,
		IncludedTree: p.Manifest.IncludedTree, Origin: req.Origin, Requester: machine, Created: created,
		Deadline: created.Add(time.Duration(c.DeadlineMinutes) * time.Minute), PacketSHA: packet.Hash(p)}
	if existing, err := ReadInput(room, id); err == nil {
		return replay(room, id, existing, in)
	}
	st := State{ReviewID: id, Status: "running", Members: make([]Member, len(c.Members))}
	for i, m := range c.Members {
		st.Members[i] = Member{Member: m, Index: i, JobID: id + "-" + strconv.Itoa(i), ExpiresAt: in.Deadline.Add(30 * time.Minute), State: "planned"}
	}
	var nonce [6]byte
	rand.Read(nonce[:])
	staging := filepath.Join(Root(room), "staging", id+"."+hex.EncodeToString(nonce[:]))
	if err := fsutil.MkdirPrivate(filepath.Join(staging, "prompts")); err != nil {
		return Input{}, State{}, err
	}
	defer os.RemoveAll(staging)
	if err := fsutil.MkdirPrivate(filepath.Join(staging, "results")); err != nil {
		return Input{}, State{}, err
	}
	for i, m := range c.Members {
		if err := fsutil.WriteFileAtomic(filepath.Join(staging, "prompts", strconv.Itoa(i)+".txt"), []byte(BuildPrompt(c, m, p))); err != nil {
			return Input{}, State{}, err
		}
	}
	for name, v := range map[string]any{"input.json": in, "packet.json": p, "review.json": st} {
		if err := writeJSON(filepath.Join(staging, name), v); err != nil {
			return Input{}, State{}, err
		}
	}
	if err := os.Rename(staging, Dir(room, id)); err != nil {
		if existing, rerr := ReadInput(room, id); rerr == nil {
			return replay(room, id, existing, in)
		}
		return Input{}, State{}, err
	}
	fsutil.SyncDir(Root(room))
	return in, st, nil
}

func replay(room, id string, existing, want Input) (Input, State, error) {
	if !existing.SameIdentity(want) {
		return Input{}, State{}, ErrConflict
	}
	st, err := ReadState(room, id)
	return existing, st, err
}

// RequestCancel records cancellation (committees §6.4); the coordinator
// then cancels members. A running review becomes cancelled at once.
func RequestCancel(ctx context.Context, room, id string) (State, error) {
	l, err := Lock(ctx, room, id)
	if err != nil {
		return State{}, err
	}
	defer l.Close()
	st, err := ReadState(room, id)
	if err != nil {
		return st, err
	}
	if st.Settled || st.CancelRequested {
		return st, nil
	}
	st.CancelRequested = true
	if st.Status == "running" {
		st.Status = "cancelled"
	}
	return st, WriteState(room, id, st)
}
```

Notes for the implementer:
- `os.Rename` onto an existing non-empty directory fails on every platform, which is what makes the rename the commit point.
- `rooms.Canonical` must exist. Check `internal/rooms/registry.go`: `Canonical(dir string) (string, error)` does. Confirm that `Touch` on an excluded path is a silent no-op, so the `Get` check is what refuses it.
- The `staging/<id>.<nonce>` directory must be under `Root(room)`, which `MkdirPrivate` creates, including `.tincan`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/review`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/review
git commit -m "Publish reviews idempotently and record cancellation"
```

---

### Task 6: The coordinator — obligations, close, settle

**Files:**
- Create: `internal/review/coordinator.go`
- Modify: `internal/reviewjob/job.go` (`(*Error).HTTPStatus`), `internal/web/peerclient.go` (`(*PeerError).HTTPStatus`)
- Test: `internal/review/coordinator_test.go`

**Interfaces:**
- Produces:

  ```go
  type JobStatus struct { State, Result, Mode string } // creating|running|done|error|cancelled
  type Transport interface {
      Create(ctx context.Context, machine string, req reviewjob.CreateRequest) (JobStatus, error)
      Status(ctx context.Context, machine, jobID string) (JobStatus, error)
      Ack(ctx context.Context, machine, jobID string) error
      Cancel(ctx context.Context, machine, jobID string, expiresAt time.Time) (JobStatus, bool, error)
      QuotaStatus(ctx context.Context, machine, preset string) (quota.Decision, error)
  }
  type Coordinator struct {
      Transport Transport
      Now       func() time.Time
      // rate limits live in memory, keyed by job and operation
  }
  ```

- Produces `func (c *Coordinator) Reconcile(ctx context.Context, room string) error`: one pass over unsettled reviews, plus staging cleanup.
- Produces `func Classify(err error) string`, returning `permanent|gone|transient`. It uses `interface{ HTTPStatus() int }`: 409, 400, 404 and 422 are permanent; 410 is gone; anything else is transient.
- The receiving `reviewjob.Error` and `web.PeerError` gain `HTTPStatus() int`.

- [ ] **Step 1: Write the failing tests.** Create `internal/review/coordinator_test.go`:

```go
package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

type statusErr int

func (e statusErr) Error() string   { return fmt.Sprintf("status %d", int(e)) }
func (e statusErr) HTTPStatus() int { return int(e) }

// fakeTransport scripts member machines. down[machine] makes every call fail
// transiently; jobs hold each job's current status.
type fakeTransport struct {
	mu        sync.Mutex
	down      map[string]bool
	createErr map[string]error
	jobs      map[string]*JobStatus
	creates   map[string]int
	acks      map[string]int
	cancels   map[string]int
	exhausted map[string]bool
}

func newFake() *fakeTransport {
	return &fakeTransport{down: map[string]bool{}, createErr: map[string]error{}, jobs: map[string]*JobStatus{}, creates: map[string]int{}, acks: map[string]int{}, cancels: map[string]int{}, exhausted: map[string]bool{}}
}

func (f *fakeTransport) Create(ctx context.Context, machine string, req reviewjob.CreateRequest) (JobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[machine] {
		return JobStatus{}, errors.New("connection refused")
	}
	if err := f.createErr[req.JobID]; err != nil {
		return JobStatus{}, err
	}
	f.creates[req.JobID]++
	if f.jobs[req.JobID] == nil {
		f.jobs[req.JobID] = &JobStatus{State: "running", Mode: "packet"}
	}
	return *f.jobs[req.JobID], nil
}

func (f *fakeTransport) Status(ctx context.Context, machine, id string) (JobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[machine] {
		return JobStatus{}, errors.New("connection refused")
	}
	if j := f.jobs[id]; j != nil {
		return *j, nil
	}
	return JobStatus{}, statusErr(404)
}

func (f *fakeTransport) Ack(ctx context.Context, machine, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[machine] {
		return errors.New("connection refused")
	}
	f.acks[id]++
	return nil
}

func (f *fakeTransport) Cancel(ctx context.Context, machine, id string, exp time.Time) (JobStatus, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[machine] {
		return JobStatus{}, false, errors.New("connection refused")
	}
	f.cancels[id]++
	j := f.jobs[id]
	if j == nil {
		return JobStatus{State: "cancelled"}, true, nil
	}
	if j.State == "running" || j.State == "creating" {
		j.State = "cancelled"
	}
	return *j, true, nil
}

func (f *fakeTransport) QuotaStatus(ctx context.Context, machine, preset string) (quota.Decision, error) {
	if f.exhausted[preset] {
		reset := time.Now().Add(time.Hour)
		return quota.Decision{Exhausted: true, Window: "weekly", ResetAt: &reset}, nil
	}
	return quota.Decision{}, nil
}

func (f *fakeTransport) finish(id, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id] = &JobStatus{State: "done", Result: result, Mode: "checkout"}
}

// published writes a review directly (no git or heartbeat needed).
func published(t *testing.T, members []string, deadline time.Duration, skip bool) (string, Input) {
	t.Helper()
	room := roomDir(t)
	id := NewID("macmini", room, "k")
	created := time.Now().UTC()
	in := Input{ReviewID: id, Committee: committee.Committee{Name: "reviewers", Version: 1, Members: members, DeadlineMinutes: 30, SkipExhausted: skip},
		Question: "q", Scope: "none", Origin: "mcp", Requester: "macmini", Created: created, Deadline: created.Add(deadline)}
	st := State{ReviewID: id, Status: "running"}
	for i, m := range members {
		st.Members = append(st.Members, Member{Member: m, Index: i, JobID: id + "-" + string(rune('0'+i)), ExpiresAt: in.Deadline.Add(30 * time.Minute), State: "planned"})
	}
	dir := Dir(room, id)
	os.MkdirAll(filepath.Join(dir, "prompts"), 0o700)
	os.MkdirAll(filepath.Join(dir, "results"), 0o700)
	for i := range members {
		os.WriteFile(filepath.Join(dir, "prompts", string(rune('0'+i))+".txt"), []byte("prompt"), 0o600)
	}
	writeJSON(filepath.Join(dir, "input.json"), in)
	writeJSON(filepath.Join(dir, "packet.json"), map[string]any{"manifest": map[string]any{"scope": "none", "base_kind": "none", "changes": []any{}}, "question": "q"})
	WriteState(room, id, st)
	return room, in
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time           { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestHappyPathSubmitsCollectsClosesAndSettles(t *testing.T) {
	room, in := published(t, []string{"codex@macmini", "grok@cachyos"}, 30*time.Minute, false)
	f := newFake()
	clk := &clock{time.Now()}
	c := &Coordinator{Transport: f, Now: clk.now}
	ctx := context.Background()
	c.Reconcile(ctx, room)
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[0].State != "running" || st.Members[1].State != "running" || f.creates[st.Members[0].JobID] != 1 {
		t.Fatalf("after submit: %+v", st.Members)
	}
	f.finish(st.Members[0].JobID, "codex says ok")
	f.finish(st.Members[1].JobID, "grok says ok")
	clk.advance(6 * time.Second)
	c.Reconcile(ctx, room)
	st, _ = ReadState(room, in.ReviewID)
	if st.Status != "closed" || st.Closure == nil {
		t.Fatalf("not closed: %+v", st)
	}
	b, ok, _ := ReadBundle(room, in.ReviewID)
	if !ok || !strings.Contains(b, "codex says ok") || !strings.Contains(b, "grok says ok") {
		t.Fatalf("bundle: %q", b)
	}
	clk.advance(16 * time.Second)
	c.Reconcile(ctx, room)
	st, _ = ReadState(room, in.ReviewID)
	if !st.Settled || f.acks[st.Members[0].JobID] == 0 || f.acks[st.Members[1].JobID] == 0 {
		t.Fatalf("not settled/acked: %+v acks=%v", st, f.acks)
	}
	c.Reconcile(ctx, room) // settled reviews are left alone
	if f.creates[st.Members[0].JobID] != 1 {
		t.Fatal("settled review resubmitted")
	}
}

func TestOfflinePeerEndsWithinBounds(t *testing.T) {
	room, in := published(t, []string{"codex@macmini", "grok@cachyos"}, 10*time.Minute, false)
	f := newFake()
	f.down["cachyos"] = true
	clk := &clock{time.Now()}
	c := &Coordinator{Transport: f, Now: clk.now}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c.Reconcile(ctx, room)
		clk.advance(20 * time.Second)
	}
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[1].State != "submitting" {
		t.Fatalf("offline member: %+v", st.Members[1])
	}
	f.finish(st.Members[0].JobID, "ok")
	clk.t = in.Deadline.Add(time.Second)
	c.Reconcile(ctx, room)
	st, _ = ReadState(room, in.ReviewID)
	if st.Members[1].State != "unreachable" || st.Status != "closed" {
		t.Fatalf("at deadline: %+v", st)
	}
	clk.t = st.Members[1].ExpiresAt.Add(6 * time.Minute)
	c.Reconcile(ctx, room)
	st, _ = ReadState(room, in.ReviewID)
	if !st.Settled {
		t.Fatalf("never settled: %+v", st)
	}
}

func TestLostCreateResponseIsAdoptedByPolling(t *testing.T) {
	room, in := published(t, []string{"grok@cachyos"}, 10*time.Minute, false)
	f := newFake()
	clk := &clock{time.Now()}
	c := &Coordinator{Transport: f, Now: clk.now}
	ctx := context.Background()
	f.down["cachyos"] = true
	c.Reconcile(ctx, room)
	// The peer accepted a job whose response was lost, then went quiet.
	f.jobs[in.ReviewID+"-0"] = &JobStatus{State: "running"}
	clk.t = in.Deadline.Add(time.Second)
	c.Reconcile(ctx, room)
	f.down["cachyos"] = false
	f.finish(in.ReviewID+"-0", "late but real")
	clk.advance(6 * time.Second)
	c.Reconcile(ctx, room)
	if r, ok, _ := ReadResult(room, in.ReviewID, 0); !ok || r != "late but real" {
		t.Fatalf("unreachable member's result not adopted: %q %v", r, ok)
	}
}

func TestPermanentAndGoneCreateErrors(t *testing.T) {
	room, in := published(t, []string{"a@macmini", "b@macmini"}, 10*time.Minute, false)
	f := newFake()
	f.createErr[in.ReviewID+"-0"] = statusErr(404)
	f.createErr[in.ReviewID+"-1"] = statusErr(410)
	c := &Coordinator{Transport: f, Now: time.Now}
	c.Reconcile(context.Background(), room)
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[0].State != "error" || st.Members[1].State != "expired" {
		t.Fatalf("%+v", st.Members)
	}
}

func TestCrashBetweenClosingAndClosedRerendersIdentically(t *testing.T) {
	room, in := published(t, []string{"a@macmini"}, 10*time.Minute, false)
	f := newFake()
	clk := &clock{time.Now()}
	c := &Coordinator{Transport: f, Now: clk.now}
	ctx := context.Background()
	c.Reconcile(ctx, room)
	f.finish(in.ReviewID+"-0", "result")
	clk.advance(6 * time.Second)
	c.Reconcile(ctx, room)
	first, _, _ := ReadBundle(room, in.ReviewID)
	st, _ := ReadState(room, in.ReviewID)
	st.Status = "closing" // as if the process died before persisting "closed"
	WriteState(room, in.ReviewID, st)
	os.Remove(filepath.Join(Dir(room, in.ReviewID), "bundle.md"))
	clk.advance(time.Hour)
	c.Reconcile(ctx, room)
	second, _, _ := ReadBundle(room, in.ReviewID)
	if first == "" || first != second {
		t.Fatalf("bundle changed:\n%s\n---\n%s", first, second)
	}
}

func TestCancelAfterCloseStopsLateMemberButKeepsResults(t *testing.T) {
	room, in := published(t, []string{"a@macmini", "b@cachyos"}, time.Minute, false)
	f := newFake()
	clk := &clock{time.Now()}
	c := &Coordinator{Transport: f, Now: clk.now}
	ctx := context.Background()
	c.Reconcile(ctx, room)
	f.finish(in.ReviewID+"-0", "on time")
	clk.t = in.Deadline.Add(time.Second)
	c.Reconcile(ctx, room)
	st, _ := ReadState(room, in.ReviewID)
	if st.Status != "closed" || !st.Members[1].Late {
		t.Fatalf("late member: %+v", st)
	}
	RequestCancel(ctx, room, in.ReviewID)
	clk.advance(16 * time.Second)
	c.Reconcile(ctx, room)
	st, _ = ReadState(room, in.ReviewID)
	if f.cancels[in.ReviewID+"-1"] == 0 || st.Members[1].State != "cancelled" || st.Members[0].State != "done" {
		t.Fatalf("cancel after close: %+v cancels=%v", st.Members, f.cancels)
	}
	if st.Status != "closed" {
		t.Fatalf("closed review changed status: %s", st.Status)
	}
}

func TestCancelBeforeSubmitNeverRuns(t *testing.T) {
	room, in := published(t, []string{"a@macmini"}, 10*time.Minute, false)
	f := newFake()
	RequestCancel(context.Background(), room, in.ReviewID)
	(&Coordinator{Transport: f, Now: time.Now}).Reconcile(context.Background(), room)
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[0].State != "cancelled" || f.creates[in.ReviewID+"-0"] != 0 || !st.Settled {
		t.Fatalf("%+v creates=%v", st, f.creates)
	}
}

func TestSkipExhaustedMembers(t *testing.T) {
	room, in := published(t, []string{"codex@macmini", "grok@cachyos"}, 10*time.Minute, true)
	f := newFake()
	f.exhausted["codex"] = true
	(&Coordinator{Transport: f, Now: time.Now}).Reconcile(context.Background(), room)
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[0].State != "skipped" || !strings.Contains(st.Members[0].Note, "weekly") || st.Members[1].State != "running" {
		t.Fatalf("%+v", st.Members)
	}
}

func TestStaleStagingRemoved(t *testing.T) {
	room, _ := published(t, []string{"a@macmini"}, 10*time.Minute, false)
	old := filepath.Join(Root(room), "staging", "rv-0000000000000000.abc")
	os.MkdirAll(old, 0o700)
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, past, past)
	(&Coordinator{Transport: newFake(), Now: time.Now}).Reconcile(context.Background(), room)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale staging kept")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/review -run 'TestHappy|TestOffline|TestLost|TestPermanent|TestCrash|TestCancel|TestSkip|TestStale'`
Expected: build failure (`undefined: Coordinator`).

- [ ] **Step 3: Implement.** Add to `internal/reviewjob/job.go`: `func (e *Error) HTTPStatus() int { return e.Status }`. Add to `internal/web/peerclient.go`: `func (e *PeerError) HTTPStatus() int { return e.Status }`. Create `internal/review/coordinator.go`:

```go
package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

type JobStatus struct {
	State  string `json:"state"`
	Result string `json:"result,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

type Transport interface {
	Create(ctx context.Context, machine string, req reviewjob.CreateRequest) (JobStatus, error)
	Status(ctx context.Context, machine, jobID string) (JobStatus, error)
	Ack(ctx context.Context, machine, jobID string) error
	Cancel(ctx context.Context, machine, jobID string, expiresAt time.Time) (JobStatus, bool, error)
	QuotaStatus(ctx context.Context, machine, preset string) (quota.Decision, error)
}

const (
	submitEvery = 15 * time.Second
	pollEvery   = 5 * time.Second
	cancelEvery = 15 * time.Second
	ackEvery    = 15 * time.Second
	collectTail = 5 * time.Minute
)

// Coordinator advances reviews (committees §6.4). It runs in tincan web's
// room pass on the dispatcher that owns the room; rate limits live in
// memory and reset harmlessly on restart.
type Coordinator struct {
	Transport Transport
	Now       func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

// Classify maps a transport error to permanent, gone or transient.
func Classify(err error) string {
	var hs interface{ HTTPStatus() int }
	if errors.As(err, &hs) {
		switch hs.HTTPStatus() {
		case 400, 404, 409, 422:
			return "permanent"
		case 410:
			return "gone"
		}
	}
	return "transient"
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// due reports whether op on key may run again, and records the attempt.
func (c *Coordinator) due(key string, every time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]time.Time{}
	}
	now := c.now()
	if t, ok := c.last[key]; ok && now.Sub(t) < every {
		return false
	}
	c.last[key] = now
	return true
}

// Reconcile advances every unsettled review in room one step and removes
// stale staging directories.
func (c *Coordinator) Reconcile(ctx context.Context, room string) error {
	c.cleanStaging(room)
	ids, err := List(room)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := c.reconcile(ctx, room, id); err != nil {
			errs = append(errs, fmt.Errorf("review %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Coordinator) cleanStaging(room string) {
	dir := filepath.Join(Root(room), "staging")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && c.now().Sub(info.ModTime()) > time.Hour {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func (c *Coordinator) reconcile(ctx context.Context, room, id string) error {
	st, err := ReadState(room, id)
	if err != nil || st.Settled {
		return err
	}
	lock, err := Lock(ctx, room, id)
	if err != nil {
		return err
	}
	defer lock.Close()
	if st, err = ReadState(room, id); err != nil || st.Settled {
		return err
	}
	in, err := ReadInput(room, id)
	if err != nil {
		return err
	}
	save := func() error { return WriteState(room, id, st) }
	for i := range st.Members {
		if err := c.member(ctx, room, in, &st, i, save); err != nil {
			return err
		}
	}
	if err := c.close(room, in, &st, save); err != nil {
		return err
	}
	if c.settled(room, in, st) {
		st.Settled = true
		return save()
	}
	return nil
}

func split(member string) (preset, machine string) {
	m, _ := committee.ParseMember(member)
	return m.Preset, m.Machine
}

func (c *Coordinator) adopt(room, id string, m *Member, js JobStatus) error {
	now := c.now()
	if js.Mode != "" {
		m.Mode = js.Mode
	}
	switch js.State {
	case "creating":
		if m.State == "submitting" || m.State == "unreachable" {
			m.State = "submitted"
		}
	case "running":
		m.State = "running"
	case "done", "error", "cancelled":
		if js.Result != "" {
			if err := WriteResult(room, id, m.Index, js.Result); err != nil {
				return err
			}
		}
		m.State, m.Finished = js.State, now
	}
	return nil
}

func (c *Coordinator) member(ctx context.Context, room string, in Input, st *State, i int, save func() error) error {
	m := &st.Members[i]
	preset, machine := split(m.Member)
	now := c.now()
	key := m.JobID + "/"
	switch {
	case st.CancelRequested && !m.CancelAcked:
		switch {
		case m.State == "planned":
			m.State, m.CancelAcked, m.Finished = "cancelled", true, now
			return save()
		case Final(m.State):
			m.CancelAcked = true
			return save()
		case now.After(m.ExpiresAt):
			m.CancelAcked = true // the job cannot be running past its expiry
			return save()
		case c.due(key+"cancel", cancelEvery):
			js, acked, err := c.Transport.Cancel(ctx, machine, m.JobID, m.ExpiresAt)
			if err != nil {
				return nil
			}
			if err := c.adopt(room, in.ReviewID, m, js); err != nil {
				return err
			}
			if acked {
				m.CancelAcked = true
				if !Final(m.State) {
					m.State, m.Finished = "cancelled", now
				}
			}
			return save()
		}
		return nil
	case m.State == "planned" && !st.CancelRequested:
		if in.Committee.SkipExhausted {
			if d, err := c.Transport.QuotaStatus(ctx, machine, preset); err == nil && d.Exhausted {
				m.State, m.Finished = "skipped", now
				m.Note = "quota exhausted (" + d.Window + ")"
				if d.ResetAt != nil {
					m.Note += " until " + d.ResetAt.UTC().Format(time.RFC3339)
				}
				return save()
			}
		}
		m.State, m.Started = "submitting", now
		if err := save(); err != nil { // persisted before the call (committees §6.4)
			return err
		}
		return c.submit(ctx, room, in, m, machine, preset, save)
	case m.State == "submitting":
		if now.After(in.Deadline) {
			m.State, m.Note = "unreachable", "peer not reachable"
			return save()
		}
		return c.submit(ctx, room, in, m, machine, preset, save)
	case m.State == "submitted" || m.State == "running" || m.State == "unreachable":
		if now.After(m.ExpiresAt.Add(collectTail)) {
			if m.State != "unreachable" {
				m.State, m.Note = "expired", "result not retrievable"
			}
			m.Finished = now
			return save()
		}
		if !c.due(key+"poll", pollEvery) {
			return nil
		}
		js, err := c.Transport.Status(ctx, machine, m.JobID)
		if err != nil {
			return nil
		}
		before := *m
		if err := c.adopt(room, in.ReviewID, m, js); err != nil {
			return err
		}
		if *m != before {
			return save()
		}
		return nil
	}
	return c.ack(ctx, room, in, m, machine, save)
}

func (c *Coordinator) submit(ctx context.Context, room string, in Input, m *Member, machine, preset string, save func() error) error {
	if !c.due(m.JobID+"/submit", submitEvery) {
		return nil
	}
	p, err := ReadPacket(room, in.ReviewID)
	if err != nil {
		return err
	}
	prompt, err := ReadPrompt(room, in.ReviewID, m.Index)
	if err != nil {
		return err
	}
	sum := sha256Hex(prompt)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	js, err := c.Transport.Create(cctx, machine, reviewjob.CreateRequest{JobID: m.JobID, ReviewID: in.ReviewID, Requester: in.Requester,
		Preset: preset, ExpiresAt: m.ExpiresAt, Packet: p, Prompt: prompt, PromptSHA: sum})
	switch {
	case err == nil:
		if m.State == "submitting" {
			m.State = "submitted"
		}
		if err := c.adopt(room, in.ReviewID, m, js); err != nil {
			return err
		}
	case Classify(err) == "permanent":
		m.State, m.Note, m.Finished = "error", err.Error(), c.now()
	case Classify(err) == "gone":
		m.State, m.Note, m.Finished = "expired", err.Error(), c.now()
	default:
		return nil // transient: stay submitting, retry later
	}
	return save()
}

func (c *Coordinator) ack(ctx context.Context, room string, in Input, m *Member, machine string, save func() error) error {
	if m.Acked || (m.State != "done" && m.State != "error" && m.State != "cancelled") {
		return nil
	}
	if _, ok, _ := ReadResult(room, in.ReviewID, m.Index); !ok {
		return nil
	}
	if c.now().After(m.ExpiresAt.Add(collectTail)) {
		m.Acked = true
		return save()
	}
	if !c.due(m.JobID+"/ack", ackEvery) {
		return nil
	}
	err := c.Transport.Ack(ctx, machine, m.JobID)
	if err == nil || Classify(err) != "transient" {
		m.Acked = true
		return save()
	}
	return nil
}

// close finishes a running review once (committees §6.4): closing with an
// immutable closure, the bundle rendered from it, then closed.
func (c *Coordinator) close(room string, in Input, st *State, save func() error) error {
	now := c.now()
	if st.Status == "running" {
		all := true
		for _, m := range st.Members {
			if !Final(m.State) {
				all = false
			}
		}
		if !all && !now.After(in.Deadline) {
			return nil
		}
		cl := &Closure{At: now.UTC()}
		for i := range st.Members {
			m := &st.Members[i]
			_, has, _ := ReadResult(room, in.ReviewID, m.Index)
			late := !Final(m.State) && m.State != "unreachable"
			m.Late = late
			cl.Members = append(cl.Members, ClosedMember{Index: m.Index, State: m.State, Late: late, Included: has})
		}
		st.Status, st.Closure = "closing", cl
		at := cl.At
		st.ClosedAt = &at
		if err := save(); err != nil {
			return err
		}
	}
	if st.Status != "closing" {
		return nil
	}
	results := map[int]string{}
	for _, cm := range st.Closure.Members {
		if cm.Included {
			r, _, _ := ReadResult(room, in.ReviewID, cm.Index)
			results[cm.Index] = r
		}
	}
	if err := writeFile(filepath.Join(Dir(room, in.ReviewID), "bundle.md"), RenderBundle(in, *st, results)); err != nil {
		return err
	}
	st.Status = "closed"
	return save()
}

// settled reports whether every obligation of every member has ended
// (committees §6.4): collected or past its tail, acknowledged or nothing to
// acknowledge, cancelled or no cancel requested.
func (c *Coordinator) settled(room string, in Input, st State) bool {
	if st.Status != "closed" && st.Status != "cancelled" {
		return false
	}
	now := c.now()
	for _, m := range st.Members {
		pastTail := now.After(m.ExpiresAt.Add(collectTail))
		_, hasResult, _ := ReadResult(room, in.ReviewID, m.Index)
		collected := Final(m.State) || pastTail
		acked := m.Acked || pastTail || !hasResult
		cancelled := !st.CancelRequested || m.CancelAcked || Final(m.State) || now.After(m.ExpiresAt)
		if !collected || !acked || !cancelled {
			return false
		}
	}
	return true
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeFile(path string, data []byte) error { return fsutil.WriteFileAtomic(path, data) }
```

Nothing in `close` handles a cancelled review: `RequestCancel` already moved it to `cancelled`, so it never closes and gets no bundle. That matches the spec.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/review`
Expected: PASS. Rate limits are keyed per job and operation and use the injected clock, so tests advance `clk` past each interval before expecting another call.

- [ ] **Step 5: Commit**

```bash
git add internal/review internal/reviewjob/job.go internal/web/peerclient.go
git commit -m "Coordinate review members through bounded obligations, close and settle"
```

---

### Task 7: The web transport, the coordinator in room passes, review notes

**Files:**
- Create: `internal/web/reviews.go` (transport and coordinator wiring)
- Modify: `internal/web/server.go` (`Server.coord *review.Coordinator`, created in `initJobs`), `internal/web/events.go` (`runRoomPass` calls the coordinator after `d.Reconcile`; `scan` fingerprints reviews and publishes `reviews` notes)
- Test: `internal/web/reviews_test.go`

**Interfaces:**
- Produces `type transport struct{ s *Server }`, which implements `review.Transport`:
  - local machine (`machine == s.cfg.Machine`) → `s.jobs`;
  - peers → `(*peer).call`. `Create` uses a 60 s deadline and a 1 MiB response cap; status, ack and cancel responses allow 8 MiB.
  - Unknown machine → a `*reviewjob.Error{404}`.
- Produces the note kind `reviews` (with `Room`). Its fingerprint is `v:<room id>`: the `.tincan/reviews` directory and every `review.json`.

- [ ] **Step 1: Write the failing test.** Create `internal/web/reviews_test.go`: an end-to-end review of a local member and a remote member, using `peerPair`:

```go
package web

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func TestReviewAcrossTwoMachines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts")
	}
	hub, _, _, member := peerPair(t) // hub = cachyos (requester), member = macmini
	bin := buildTincan(t)
	script := filepath.Join(t.TempDir(), "reviewer")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"REVIEWED on $(basename $(dirname $PWD))\"\n"), 0o755)
	presets := map[string]host.Preset{"reviewer": {Exec: []string{script, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}}
	for _, s := range []*Server{hub, member} {
		state, _ := filepath.EvalSymlinks(t.TempDir())
		s.cfg.StateDir = state
		s.cfg.Dispatch.Presets = presets
		s.cfg.Dispatch.Executable = bin
		s.initJobs()
	}
	t.Setenv("TINCAN_STATE_DIR", hub.cfg.StateDir)
	committee.NewStore(hub.cfg.StateDir).Put(context.Background(), committee.Committee{Name: "pair", Members: []string{"reviewer@cachyos", "reviewer@macmini"}})
	room, _ := filepath.EvalSymlinks(t.TempDir())
	hub.cfg.Registry = rooms.Open(filepath.Join(hub.cfg.StateDir, "rooms.json"))
	from := ""
	in, _, err := review.Publish(context.Background(), review.PublishRequest{StateDir: hub.cfg.StateDir, Room: room, Committee: "pair",
		Question: "ok?", Origin: "web", Machine: "cachyos", CommitteesFrom: &from, InCoordinator: true, Registry: hub.cfg.Registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range []*Server{hub, member} {
			s.jobs.Wait()
			jobs, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "reviews", "ws"))
			for _, j := range jobs {
				host.Down(context.Background(), filepath.Join(s.cfg.StateDir, "reviews", "ws", j.Name()), "reviewer", 5*time.Second)
			}
		}
	})
	deadline := time.Now().Add(40 * time.Second)
	var st review.State
	for time.Now().Before(deadline) {
		hub.coord.Reconcile(context.Background(), room)
		st, _ = review.ReadState(room, in.ReviewID)
		if st.Status == "closed" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if st.Status != "closed" {
		t.Fatalf("review did not close: %+v", st)
	}
	b, _, _ := review.ReadBundle(room, in.ReviewID)
	if strings.Count(b, "REVIEWED") != 2 {
		t.Fatalf("bundle:\n%s", b)
	}
}

func TestReviewsNote(t *testing.T) {
	s, rid := apiServer(t)
	r, _, _ := s.cfg.Registry.Get(rid)
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	list, _ := s.cfg.Registry.List()
	s.scan(list)
	drain(ch)
	os.MkdirAll(filepath.Join(review.Root(r.Path), "rv-0000000000000000"), 0o700)
	os.WriteFile(filepath.Join(review.Root(r.Path), "rv-0000000000000000", "review.json"), []byte(`{}`), 0o600)
	s.scan(list)
	for {
		select {
		case n := <-ch:
			if n.Kind == "reviews" && n.Room == rid {
				return
			}
		default:
			t.Fatal("no reviews note")
		}
	}
}
```

The member job receives `$PWD` as its workspace, so the script's output just proves the member ran. Both machines run `reviewer`, so the bundle holds two results.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web -run 'TestReviewAcross|TestReviewsNote'`
Expected: build failure (`hub.coord undefined`).

- [ ] **Step 3: Implement.** Create `internal/web/reviews.go`:

```go
package web

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

// transport carries coordinator calls to members: this machine's jobs
// in-process, a peer's over the machine link (committees §6.7).
type transport struct{ s *Server }

func (t transport) peerFor(machine string) (*peer, error) {
	if p, ok := t.s.peers[machine]; ok {
		return p, nil
	}
	return nil, &reviewjob.Error{Status: 404, Msg: fmt.Sprintf("unknown machine %q", machine)}
}

func jobStatus(j reviewjob.Job) review.JobStatus {
	return review.JobStatus{State: j.State, Result: j.Result, Mode: j.Mode}
}

func (t transport) Create(ctx context.Context, machine string, req reviewjob.CreateRequest) (review.JobStatus, error) {
	if machine == t.s.cfg.Machine {
		j, err := t.s.jobs.Create(ctx, req)
		return jobStatus(j), err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return review.JobStatus{}, err
	}
	var v jobView
	err = p.call(ctx, "POST", "review-jobs", req, 1<<20, &v)
	return review.JobStatus{State: v.State, Result: v.Result, Mode: v.Mode}, err
}

func (t transport) Status(ctx context.Context, machine, id string) (review.JobStatus, error) {
	if machine == t.s.cfg.Machine {
		j, err := t.s.jobs.Status(id)
		return jobStatus(j), err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return review.JobStatus{}, err
	}
	var v jobView
	err = p.call(ctx, "GET", "review-jobs/"+url.PathEscape(id), nil, 8<<20, &v)
	return review.JobStatus{State: v.State, Result: v.Result, Mode: v.Mode}, err
}

func (t transport) Ack(ctx context.Context, machine, id string) error {
	if machine == t.s.cfg.Machine {
		_, err := t.s.jobs.Ack(ctx, id)
		return err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return err
	}
	return p.call(ctx, "POST", "review-jobs/"+url.PathEscape(id)+"/ack", map[string]string{}, 8<<20, nil)
}

func (t transport) Cancel(ctx context.Context, machine, id string, exp time.Time) (review.JobStatus, bool, error) {
	if machine == t.s.cfg.Machine {
		j, acked, err := t.s.jobs.Cancel(ctx, id, exp)
		return jobStatus(j), acked, err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return review.JobStatus{}, false, err
	}
	var out struct {
		Job   jobView `json:"job"`
		Acked bool    `json:"acked"`
	}
	err = p.call(ctx, "POST", "review-jobs/"+url.PathEscape(id)+"/cancel", map[string]any{"expires_at": exp}, 8<<20, &out)
	return review.JobStatus{State: out.Job.State, Result: out.Job.Result, Mode: out.Job.Mode}, out.Acked, err
}

func (t transport) QuotaStatus(ctx context.Context, machine, preset string) (quota.Decision, error) {
	if machine == t.s.cfg.Machine {
		return quota.Decide(t.s.cfg.QuotaDir, t.s.cfg.QuotaConfig, preset, time.Now()), nil
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return quota.Decision{}, err
	}
	var d quota.Decision
	err = p.call(ctx, "GET", "presets/"+url.PathEscape(preset)+"/quota-status", nil, 64<<10, &d)
	return d, err
}
```

In `jobs.go` `initJobs`, after creating `s.jobs`, add `s.coord = &review.Coordinator{Transport: transport{s}}`, and add the field `coord *review.Coordinator` to `Server`. In `events.go` `runRoomPass`, after the `d.Reconcile` block:

```go
	if s.coord != nil {
		if err := s.coord.Reconcile(ctx, room.Path); err != nil {
			s.logOnce(room.ID+"/reviews", fmt.Sprintf("tincan web: %s: reviews: %v", room.Name, err))
		}
	}
```

In `scan`, inside the per-room loop, add:

```go
		rfp := statFP(review.Root(room.Path))
		if ids, err := review.List(room.Path); err == nil {
			for _, id := range ids {
				rfp += statFP(filepath.Join(review.Dir(room.Path, id), "review.json"))
			}
		}
		next["v:"+room.ID] = rfp
```

In the `switch key[0]`, add `case 'v': notes = append(notes, note{Kind: "reviews", Room: key[2:]})`. Update the `note.Kind` comment. Also add the reviews directory to the fsnotify watch list in `loop`: append `review.Root(room.Path)` to `dirs`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web ./internal/review`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web
git commit -m "Coordinate reviews in tincan web's room passes across both machines"
```

---

### Task 8: Review API routes

**Files:**
- Modify: `internal/web/reviews.go` (handlers), `internal/web/api.go` (routes)
- Test: `internal/web/reviews_test.go`

**Interfaces:**
- Produces the routes:
  - `GET /api/rooms/{rid}/reviews` → `[]reviewSummary`
  - `GET /api/rooms/{rid}/reviews/{id}` → `reviewDetail`
  - `POST /api/rooms/{rid}/reviews` (80 KiB) `{committee, question, scope?, request_id?}` → 201 `reviewSummary`
  - `POST /api/rooms/{rid}/reviews/{id}/cancel` → `reviewSummary`
- Produces `type reviewSummary struct`, with fields and JSON tags:
  - `ReviewID` (`review_id`), `Committee` (`committee`), `Status` (`status`), `Settled` (`settled`), `Created` (`created`), `Deadline` (`deadline`);
  - `Members []memberView` (`members`), where `memberView` is `{member, state, late, note}`.
- Produces `type reviewDetail struct` = `reviewSummary` plus `Question string`, `Scope string`, `Results map[int]string` (`results`) and `Bundle string` (`bundle,omitempty`).
- Errors: `review.ErrConflict` → 409. A committee that is not found → 404. Other publication errors → 400.

- [ ] **Step 1: Write the failing test.** Add to `internal/web/reviews_test.go`:

```go
func TestReviewAPI(t *testing.T) {
	s, rid := apiServer(t) // machine "box", preset "claude" available
	state, _ := filepath.EvalSymlinks(t.TempDir())
	s.cfg.StateDir = state
	s.initJobs()
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"claude@box"}})
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/reviews", `{"committee":"solo","question":"ok?","scope":"none","request_id":"k"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var sum reviewSummary
	decode(t, rec.Body.String(), &sum)
	if sum.Committee != "solo" || len(sum.Members) != 1 || sum.Status != "running" {
		t.Fatalf("summary: %+v", sum)
	}
	if rec := do(t, h, "POST", "/api/rooms/"+rid+"/reviews", `{"committee":"solo","question":"different","scope":"none","request_id":"k"}`, mut()); rec.Code != 409 {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/rooms/"+rid+"/reviews", `{"committee":"nobody","question":"q"}`, mut()); rec.Code != 404 {
		t.Fatalf("unknown committee: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/reviews", "", ownerHdr())
	var list []reviewSummary
	decode(t, rec.Body.String(), &list)
	if len(list) != 1 || list[0].ReviewID != sum.ReviewID {
		t.Fatalf("list: %s", rec.Body)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/reviews/"+sum.ReviewID, "", ownerHdr())
	var det reviewDetail
	decode(t, rec.Body.String(), &det)
	if det.Question != "ok?" || det.Scope != "none" {
		t.Fatalf("detail: %s", rec.Body)
	}
	rec = do(t, h, "POST", "/api/rooms/"+rid+"/reviews/"+sum.ReviewID+"/cancel", "", mut())
	decode(t, rec.Body.String(), &sum)
	if rec.Code != 200 || sum.Status != "cancelled" {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/rooms/"+rid+"/reviews/rv-../../x", "", ownerHdr()); rec.Code == 200 {
		t.Fatal("traversal accepted")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/web -run TestReviewAPI`
Expected: build failure (`undefined: reviewSummary`).

- [ ] **Step 3: Implement.** Append to `internal/web/reviews.go`:

```go
type memberView struct {
	Member string `json:"member"`
	State  string `json:"state"`
	Late   bool   `json:"late,omitempty"`
	Note   string `json:"note,omitempty"`
}

type reviewSummary struct {
	ReviewID  string       `json:"review_id"`
	Committee string       `json:"committee"`
	Status    string       `json:"status"`
	Settled   bool         `json:"settled"`
	Created   time.Time    `json:"created"`
	Deadline  time.Time    `json:"deadline"`
	Members   []memberView `json:"members"`
}

type reviewDetail struct {
	reviewSummary
	Question string         `json:"question"`
	Scope    string         `json:"scope"`
	Results  map[int]string `json:"results"`
	Bundle   string         `json:"bundle,omitempty"`
}

func summarize(in review.Input, st review.State) reviewSummary {
	s := reviewSummary{ReviewID: in.ReviewID, Committee: in.Committee.Name, Status: st.Status, Settled: st.Settled, Created: in.Created, Deadline: in.Deadline, Members: []memberView{}}
	for _, m := range st.Members {
		s.Members = append(s.Members, memberView{Member: m.Member, State: m.State, Late: m.Late, Note: m.Note})
	}
	return s
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	ids, err := review.List(room.Path)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []reviewSummary{}
	for i := len(ids) - 1; i >= 0; i-- {
		in, err1 := review.ReadInput(room.Path, ids[i])
		st, err2 := review.ReadState(room.Path, ids[i])
		if err1 == nil && err2 == nil {
			out = append(out, summarize(in, st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) getReview(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	in, err := review.ReadInput(room.Path, id)
	if err != nil {
		s.fail(w, http.StatusNotFound, fmt.Errorf("review %s not found", id))
		return
	}
	st, err := review.ReadState(room.Path, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	d := reviewDetail{reviewSummary: summarize(in, st), Question: in.Question, Scope: in.Scope, Results: map[int]string{}}
	for _, m := range st.Members {
		if res, ok, _ := review.ReadResult(room.Path, id, m.Index); ok {
			d.Results[m.Index] = res
		}
	}
	d.Bundle, _, _ = review.ReadBundle(room.Path, id)
	s.writeJSON(w, http.StatusOK, d)
}

func (s *Server) startReview(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	if s.cfg.StateDir == "" {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return
	}
	var in struct {
		Committee string `json:"committee"`
		Question  string `json:"question"`
		Scope     string `json:"scope"`
		RequestID string `json:"request_id"`
	}
	if err := readJSON(w, r, 80<<10, &in); err != nil {
		s.failBody(w, err)
		return
	}
	from := s.cfg.CommitteesFrom
	rin, st, err := review.Publish(r.Context(), review.PublishRequest{StateDir: s.cfg.StateDir, Room: room.Path, Committee: in.Committee,
		Question: in.Question, Scope: in.Scope, RequestID: in.RequestID, Origin: "web", Machine: s.cfg.Machine,
		CommitteesFrom: &from, InCoordinator: true, Registry: s.cfg.Registry})
	switch {
	case errors.Is(err, review.ErrConflict):
		s.fail(w, http.StatusConflict, err)
	case err != nil && strings.Contains(err.Error(), "not found"):
		s.fail(w, http.StatusNotFound, err)
	case err != nil:
		s.fail(w, http.StatusBadRequest, err)
	default:
		s.writeJSON(w, http.StatusCreated, summarize(rin, st))
	}
}

func (s *Server) cancelReview(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	in, err := review.ReadInput(room.Path, id)
	if err != nil {
		s.fail(w, http.StatusNotFound, fmt.Errorf("review %s not found", id))
		return
	}
	st, err := review.RequestCancel(r.Context(), room.Path, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, summarize(in, st))
}
```

Routes in `apiRoutes`:

```go
	s.mux.HandleFunc("GET /api/rooms/{rid}/reviews", s.listReviews)
	s.mux.HandleFunc("POST /api/rooms/{rid}/reviews", s.startReview)
	s.mux.HandleFunc("GET /api/rooms/{rid}/reviews/{id}", s.getReview)
	s.mux.HandleFunc("POST /api/rooms/{rid}/reviews/{id}/cancel", s.cancelReview)
```

Add the needed imports (`errors`, `net/http`, `sort`, `strings`) to `reviews.go`. `apiServer`'s registry is not `rooms.Default()`; the handler passes `s.cfg.Registry`, so the test's registry confirms membership.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web
git commit -m "Serve reviews over the room API: list, detail, start and cancel"
```

---

### Task 9: MCP tools

**Files:**
- Modify: `internal/mcpserver/server.go` (three tools; `Options.StateDir`)
- Test: `internal/mcpserver/review_test.go`

**Interfaces:**
- Produces `Options.StateDir string` ("" = `rooms.StateDir()`).
- Produces the tool `tincan_review {committee, question, scope?, request_id?}` → `ReviewOutput{review_id, status, members[{member,state}]}`.
- Produces the tool `tincan_review_wait {review_id, timeout_seconds}` → `ReviewWaitOutput{review_id, status, closed, settled, members[{member,state,late}], bundle?, bundle_path?}`.
- Produces the tool `tincan_review_cancel {review_id}` → `ReviewOutput`.

- [ ] **Step 1: Write the failing test.** Create `internal/mcpserver/review_test.go`:

```go
package mcpserver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/mcpserver"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectState(t *testing.T, room, state string) *mcp.ClientSession {
	t.Helper()
	server, err := mcpserver.New(mcpserver.Options{Room: room, StateDir: state, Presets: map[string]host.Preset{}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return cs
}

func TestReviewToolsPublishWaitAndCancel(t *testing.T) {
	state, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TINCAN_STATE_DIR", state)
	room, _ := filepath.EvalSymlinks(t.TempDir())
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"claude@box"}})
	cs := connectState(t, room, state)

	res, _ := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "tincan_review", Arguments: map[string]any{"committee": "solo", "question": "q"}})
	if !res.IsError || !strings.Contains(textOf(res), "tincan web is not running") {
		t.Fatalf("without a coordinator: %+v", res)
	}
	if _, err := os.Stat(review.Root(room)); err == nil {
		t.Fatal("refused review wrote files")
	}

	rooms.WriteHeartbeat(state, rooms.Heartbeat{PID: 1, Machine: "box", Updated: time.Now()})
	out := call(t, cs, "tincan_review", map[string]any{"committee": "solo", "question": "q", "scope": "none", "request_id": "k"})
	id, _ := out["review_id"].(string)
	if !strings.HasPrefix(id, "rv-") || out["status"] != "running" {
		t.Fatalf("review: %v", out)
	}
	waited := call(t, cs, "tincan_review_wait", map[string]any{"review_id": id, "timeout_seconds": 0})
	if waited["closed"] != false {
		t.Fatalf("wait: %v", waited)
	}
	cancelled := call(t, cs, "tincan_review_cancel", map[string]any{"review_id": id})
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancel: %v", cancelled)
	}
}

func textOf(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
```

`call` is the existing helper in `server_test.go`, which lives in the same `mcpserver_test` package. The test's room is not a Git repository, so `scope` defaults to `none`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mcpserver -run TestReviewTools`
Expected: build failure (`unknown field StateDir`).

- [ ] **Step 3: Implement.** In `server.go`: add `StateDir string` to `Options`. In `New`, default it with `if o.StateDir == "" { o.StateDir = rooms.StateDir() }`. Register the tools:

```go
	add(server, "tincan_review", "Ask a committee (a named group of agents on this and other machines) to review the current change, or answer a question. Returns a review_id immediately; members run in parallel. Needs tincan web running on this machine.", false, true, true, s.reviewTool)
	add(server, "tincan_review_wait", "Wait up to 30 seconds for a committee review; returns member states and, once it closes, the bundle of every member's review. Keep waiting while it is running; synthesize the bundle yourself.", true, true, false, s.reviewWaitTool)
	add(server, "tincan_review_cancel", "Cancel a committee review; members still running are stopped.", false, true, false, s.reviewCancelTool)
```

Add the handlers (a new file `internal/mcpserver/review.go` keeps `server.go` readable):

```go
package mcpserver

import (
	"context"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReviewInput struct {
	Committee string `json:"committee" jsonschema:"Committee name"`
	Question  string `json:"question" jsonschema:"What the reviewers should answer; the change itself is attached automatically"`
	Scope     string `json:"scope,omitempty" jsonschema:"uncommitted (default in a Git room), branch, commit:<rev>, range:<a>..<b>, or none"`
	RequestID string `json:"request_id,omitempty" jsonschema:"Optional idempotency key"`
}

type ReviewMember struct {
	Member string `json:"member"`
	State  string `json:"state"`
	Late   bool   `json:"late,omitempty"`
}

type ReviewOutput struct {
	ReviewID string         `json:"review_id"`
	Status   string         `json:"status"`
	Members  []ReviewMember `json:"members"`
}

type ReviewWaitInput struct {
	ReviewID       string `json:"review_id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"0 polls; 1–30 waits"`
}

type ReviewWaitOutput struct {
	ReviewOutput
	Closed     bool   `json:"closed"`
	Settled    bool   `json:"settled"`
	Bundle     string `json:"bundle,omitempty"`
	BundlePath string `json:"bundle_path,omitempty"`
}

func reviewOut(st review.State) ReviewOutput {
	o := ReviewOutput{ReviewID: st.ReviewID, Status: st.Status, Members: []ReviewMember{}}
	for _, m := range st.Members {
		o.Members = append(o.Members, ReviewMember{Member: m.Member, State: m.State, Late: m.Late})
	}
	return o
}

func (s *service) reviewTool(ctx context.Context, req *mcp.CallToolRequest, in ReviewInput) (*mcp.CallToolResult, ReviewOutput, error) {
	_, st, err := review.Publish(ctx, review.PublishRequest{StateDir: s.StateDir, Room: s.Room, Committee: in.Committee,
		Question: in.Question, Scope: in.Scope, RequestID: in.RequestID, Origin: "mcp"})
	if err != nil {
		return nil, ReviewOutput{}, err
	}
	return nil, reviewOut(st), nil
}

func (s *service) reviewWaitTool(ctx context.Context, req *mcp.CallToolRequest, in ReviewWaitInput) (*mcp.CallToolResult, ReviewWaitOutput, error) {
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 30 {
		return nil, ReviewWaitOutput{}, fmt.Errorf("timeout_seconds must be between 0 and 30")
	}
	deadline := time.Now().Add(time.Duration(in.TimeoutSeconds) * time.Second)
	for {
		st, err := review.ReadState(s.Room, in.ReviewID)
		if err != nil {
			return nil, ReviewWaitOutput{}, err
		}
		closed := st.Status == "closed" || st.Status == "cancelled"
		if closed || !time.Now().Before(deadline) {
			out := ReviewWaitOutput{ReviewOutput: reviewOut(st), Closed: closed, Settled: st.Settled}
			if b, ok, _ := review.ReadBundle(s.Room, in.ReviewID); ok {
				if len(b) <= 256<<10 {
					out.Bundle = b
				} else {
					out.BundlePath = filepath.Join(review.Dir(s.Room, in.ReviewID), "bundle.md")
				}
			}
			return nil, out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ReviewWaitOutput{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *service) reviewCancelTool(ctx context.Context, req *mcp.CallToolRequest, in ReviewWaitInput) (*mcp.CallToolResult, ReviewOutput, error) {
	st, err := review.RequestCancel(ctx, s.Room, in.ReviewID)
	if err != nil {
		return nil, ReviewOutput{}, err
	}
	return nil, reviewOut(st), nil
}
```

Add the `fmt` import. The existing test `TestToolsAndDurableReconnect` asserts `len(listed.Tools) != 8`; update it to 11. Update the server `Instructions` string to mention committee reviews in one clause: "tincan_review asks a committee of agents on this and other machines to review the change; wait for its bundle and synthesize it."

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/mcpserver`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver
git commit -m "Add tincan_review, tincan_review_wait and tincan_review_cancel"
```

---

### Task 10: `tincan review` CLI

**Files:**
- Create: `internal/cli/review.go`
- Modify: `internal/cli/cli.go` (the `case "review":` dispatch; usage text)
- Test: `internal/cli/review_test.go`

**Interfaces:**
- Produces `func cmdReview(args []string, stdout, stderr io.Writer) int`.
- Flags: `--committee`, `--question`, `--question-file`, `--scope`, `--request-id`, `--room` (default `.`), `--wait` (bool), `--timeout` (seconds, default 3600) and `--cancel` (bool).
- Forms:
  - `tincan review --committee X --question Q [--wait]`
  - `tincan review --wait <id>`
  - `tincan review --cancel <id>`
- Output: the `review_id` on its own line. With `--wait`, member states go to stderr and the bundle to stdout.
- Exit codes: `ExitOK`; 4 when closed with late or unreachable members; `ExitUsage`; `ExitError`; and `ExitTimeout` (3) on timeout.

- [ ] **Step 1: Write the failing test.** Create `internal/cli/review_test.go`:

```go
package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func TestReviewCLI(t *testing.T) {
	state := os.Getenv("TINCAN_STATE_DIR") // set by TestMain
	room, _ := filepath.EvalSymlinks(t.TempDir())
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"claude@box"}})
	os.Remove(rooms.HeartbeatPath(state))
	if code, _, errOut := run("review", "--room", room, "--committee", "solo", "--question", "q"); code != ExitError || !strings.Contains(errOut, "tincan web is not running") {
		t.Fatalf("no coordinator: %d %q", code, errOut)
	}
	rooms.WriteHeartbeat(state, rooms.Heartbeat{PID: 1, Machine: "box", Updated: time.Now()})
	code, out, errOut := run("review", "--room", room, "--committee", "solo", "--question", "q", "--scope", "none", "--request-id", "k")
	id := strings.TrimSpace(out)
	if code != ExitOK || !strings.HasPrefix(id, "rv-") {
		t.Fatalf("review: %d %q %q", code, out, errOut)
	}
	// Simulate the coordinator closing it with a late member.
	st, _ := review.ReadState(room, id)
	st.Status, st.Members[0].Late, st.Members[0].State = "closed", true, "running"
	review.WriteState(room, id, st)
	os.WriteFile(filepath.Join(review.Dir(room, id), "bundle.md"), []byte("# bundle\n"), 0o600)
	code, out, _ = run("review", "--room", room, "--wait", id, "--timeout", "5")
	if code != 4 || !strings.Contains(out, "# bundle") {
		t.Fatalf("wait: %d %q", code, out)
	}
	if code, _, errOut := run("review", "--room", room, "--cancel", id); code != ExitOK {
		t.Fatalf("cancel: %d %q", code, errOut)
	}
	if code, _, _ := run("review", "--room", room); code != ExitUsage {
		t.Fatal("missing --committee accepted")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli -run TestReviewCLI`
Expected: FAIL (unknown command `review`).

- [ ] **Step 3: Implement.** Create `internal/cli/review.go`:

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func cmdReview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		committee = fs.String("committee", "", "committee name")
		question  = fs.String("question", "", "what reviewers should answer")
		qfile     = fs.String("question-file", "", "read the question from a file")
		scope     = fs.String("scope", "", "uncommitted (default in Git), branch, commit:<rev>, range:<a>..<b>, none")
		requestID = fs.String("request-id", "", "idempotency key")
		room      = fs.String("room", ".", "room directory")
		wait      = fs.Bool("wait", false, "wait for the review to close and print its bundle")
		cancel    = fs.Bool("cancel", false, "cancel the review named by the argument")
		timeout   = fs.Int("timeout", 3600, "seconds to wait with --wait")
	)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	ctx := context.Background()
	if *cancel {
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "tincan review: --cancel takes a review id")
			return ExitUsage
		}
		if _, err := review.RequestCancel(ctx, *room, fs.Arg(0)); err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ExitError
		}
		return ExitOK
	}
	id := ""
	if *wait && fs.NArg() == 1 && *committee == "" {
		id = fs.Arg(0)
	} else {
		if *committee == "" || fs.NArg() != 0 {
			fmt.Fprintln(stderr, "tincan review: --committee and --question (or --question-file) are required")
			return ExitUsage
		}
		q, ec, err := bodyFrom(*question, *qfile)
		if err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ec
		}
		in, _, err := review.Publish(ctx, review.PublishRequest{StateDir: rooms.StateDir(), Room: *room, Committee: *committee,
			Question: q, Scope: *scope, RequestID: *requestID, Origin: "cli"})
		if err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ExitError
		}
		id = in.ReviewID
		fmt.Fprintln(stdout, id)
		if !*wait {
			return ExitOK
		}
	}
	return waitReview(ctx, *room, id, time.Duration(*timeout)*time.Second, stdout, stderr)
}

func waitReview(ctx context.Context, room, id string, timeout time.Duration, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(timeout)
	for {
		st, err := review.ReadState(room, id)
		if err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ExitError
		}
		if st.Status == "closed" || st.Status == "cancelled" {
			degraded := false
			for _, m := range st.Members {
				fmt.Fprintf(stderr, "%s: %s%s\n", m.Member, m.State, map[bool]string{true: " (late)"}[m.Late])
				degraded = degraded || m.Late || m.State == "unreachable"
			}
			if b, ok, _ := review.ReadBundle(room, id); ok {
				fmt.Fprint(stdout, b)
			}
			if degraded {
				return 4 // closed with late or unreachable members
			}
			return ExitOK
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "tincan review: %s still %s; reattach with tincan review --wait %s\n", id, st.Status, id)
			return ExitTimeout
		}
		time.Sleep(time.Second)
	}
}

```

`bodyFrom` is the existing helper that `cmdAsk` uses; `ExitTimeout` (3) is the existing timeout code. In `cli.go`, add `case "review": return cmdReview(args[1:], stdout, stderr)` and a usage line: `review      ask a committee to review the change (see docs/web.md)`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli
git commit -m "Add tincan review"
```

---

### Task 11: Reviews in the UI, and docs

**Files:**
- Modify: `internal/web/ui/app.js`, `internal/web/ui/app.css`, `internal/web/ui_test.go`
- Modify: `docs/web.md`, `PROTOCOL.md` (MCP tools table and CLI section)

**Interfaces:**
- Consumes: `GET/POST api/rooms/{rid}/reviews`, `GET api/rooms/{rid}/reviews/{id}`, `POST …/cancel`, `GET api/committees` (from the room's machine), and the `reviews` note.
- Produces: an Activity section "Reviews" (a table plus the "Start a review" form), `showReview(key, rid, id)`, and handling of the `n.kind === "reviews"` note.

- [ ] **Step 1: Write the failing test.** In `TestUIContract`, add to the second `want` list:

```go
		// Reviews (committees spec §6.10, 2b-3).
		"showReview(",
		"Start a review",
		`n.kind === "reviews"`,
		`/reviews/${encodeURIComponent(`,
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/web -run TestUIContract`
Expected: FAIL listing the missing strings.

- [ ] **Step 3: Implement.** In `app.js`:

1. In `showActivity`, after the requests table, fetch reviews and append a section:

```js
  const reviews = await api(cur.key, `rooms/${encodeURIComponent(cur.rid)}/reviews`).catch(() => []);
  const vt = el("table", "activity");
  vt.append(row("th", ["Review", "Committee", "Status", "Members", "Created"]));
  for (const r of reviews) {
    const tr = row("td", [r.review_id, r.committee, r.status + (r.settled ? "" : " …"),
      r.members.map((m) => `${m.member}: ${m.state}${m.late ? " (late)" : ""}`).join(", "), new Date(r.created).toLocaleString()]);
    tr.classList.add("clickable");
    tr.onclick = () => showReview(cur.key, cur.rid, r.review_id);
    vt.append(tr);
  }
  view.append(el("h3", "", "Reviews"), reviews.length ? vt : el("p", "muted", "No reviews."), reviewForm(cur.key, cur.rid));
```

`view.replaceChildren(...)` at the end of `showActivity` currently sets the listeners and requests sections. Keep that call, and append the reviews section after it, so the existing order is unchanged.

2. Add:

```js
function reviewForm(key, rid) {
  const form = el("form", "review-form");
  const committee = el("select");
  api(key, "committees").then((d) => {
    for (const c of d.committees) committee.append(Object.assign(el("option", "", c.name), { value: c.name }));
  }).catch(() => {});
  const scope = el("select");
  for (const s of ["uncommitted", "branch", "none"]) scope.append(Object.assign(el("option", "", s), { value: s }));
  const question = el("textarea");
  question.rows = 3;
  question.placeholder = "What should the committee check?";
  const status = el("p", "muted");
  const go = el("button", "", "Request review");
  go.type = "button";
  go.onclick = async () => {
    try {
      const r = await api(key, `rooms/${encodeURIComponent(rid)}/reviews`, { method: "POST", body: { committee: committee.value, scope: scope.value, question: question.value, request_id: clientId() } });
      showReview(key, rid, r.review_id);
    } catch (e) { status.textContent = e.message; }
  };
  const label = (t, i) => { const l = el("label", "", t + " "); l.append(i); return l; };
  form.append(el("h3", "", "Start a review"), label("Committee", committee), label("Scope", scope), question, go, status);
  return form;
}

async function showReview(key, rid, id) {
  const d = await api(key, `rooms/${encodeURIComponent(rid)}/reviews/${encodeURIComponent(id)}`);
  const view = $("view");
  const box = el("div", "review");
  box.append(el("h3", "", `${d.committee} · ${d.status}${d.settled ? "" : " …"}`), el("p", "muted", d.question));
  for (const [i, m] of d.members.entries()) {
    const sec = el("section", "member");
    sec.append(el("h4", "", `${m.member} — ${m.state}${m.late ? " (late)" : ""}${m.note ? ": " + m.note : ""}`));
    if (d.results[i]) sec.append(el("pre", "result", d.results[i]));
    box.append(sec);
  }
  if (d.status === "running" || (d.status === "closed" && !d.settled)) {
    const cancel = el("button", "danger", "Cancel review");
    cancel.onclick = async () => { await api(key, `rooms/${encodeURIComponent(rid)}/reviews/${encodeURIComponent(id)}/cancel`, { method: "POST" }); showReview(key, rid, id); };
    box.append(cancel);
  }
  const back = el("button", "secondary", "Back to activity");
  back.onclick = showActivity;
  box.append(back);
  state.reviewOpen = { key, rid, id };
  view.replaceChildren(box);
}
```

Add `reviewOpen: null` to `state`, and reset it to `null` in `openCurrent`.

3. In `connect`'s `onmessage`, before the generic room handling:

```js
    if (n.kind === "reviews") {
      const cur = state.current;
      if (state.reviewOpen && state.reviewOpen.key === key && state.reviewOpen.rid === n.room) showReview(key, n.room, state.reviewOpen.id);
      else if (cur && cur.key === key && cur.rid === n.room && cur.tid === "activity") showActivity();
      return;
    }
```

`showActivity` resets `state.reviewOpen = null` at its start.

In `app.css`, add styles for `.review .member`, `.review pre.result` (pre-wrap, code background), `.review-form` (a column layout like `.committee-edit`) and `tr.clickable { cursor: pointer }`.

In `docs/web.md`, add a "Reviews" section:
- start a review from a room's Activity page, from `tincan review`, or from an agent through `tincan_review`;
- `tincan web` on the requesting machine coordinates it: each member runs as a reviewer job on its machine;
- results appear as they arrive; the review closes when all members finish or at the deadline (members still running are marked late);
- the bundle collects every result; cancel stops the members;
- quota skipping needs `skip_exhausted` and a `quotas.json` entry with `blocking`.

In `PROTOCOL.md`, add the three MCP tools to the tools list and a `tincan review` entry to the CLI section, in the same style as the existing entries.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web && node --check internal/web/ui/app.js`
Expected: PASS, and no syntax errors.

- [ ] **Step 5: Run the whole suite, vet and format**

Run: `gofmt -l . ; go vet ./... && GOOS=windows go vet ./... && go test ./...`
Expected: no gofmt output; every package passes.

- [ ] **Step 6: Commit**

```bash
git add internal/web/ui internal/web/ui_test.go docs/web.md PROTOCOL.md
git commit -m "Show and start committee reviews in the web UI; document reviews"
```
