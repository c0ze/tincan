# tincan Phase 1 (core engine) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `tincan` CLI — send / recv / ask / reply over a filesystem spool — with unit and integration tests, so two AI agents can exchange request/reply messages in a repo.

**Architecture:** A maildir-style spool under `<room>/.tincan/`: sending = atomically renaming a JSON file into `inbox/<name>/`; receiving = fsnotify-blocked claim-by-rename of the oldest file. `ask` mints an ephemeral reply inbox `r-<id>` and blocks on it; `reply` is sugar over `send` into that inbox. No daemon, no message store.

**Tech Stack:** Go 1.23, stdlib + `github.com/fsnotify/fsnotify` (only dep). Module: `github.com/c0ze/tincan` (go.mod already exists at repo root).

**Context for workers with zero prior knowledge:**

- Repo root: `/Users/arda/projects/tincan`. Spec: `docs/superpowers/specs/2026-07-03-tincan-design.md`. Read it if confused; this plan is self-contained.
- A **room** is a directory (usually a repo root) whose message state lives under `<room>/.tincan/`. Participants are **names** (`orch`, `codex`); each has inbox `<room>/.tincan/inbox/<name>/`.
- One message = one JSON file, named `<zero-padded-unixnano>-<id>.json` so lexical order = chronological order.
- Delivery must be atomic: write into `<room>/.tincan/tmp/`, then `os.Rename` into the inbox (same filesystem). Claiming must be exactly-once: `os.Rename` out of the inbox (rename fails for all but one claimant; on failure, try the next file).
- **Exit codes (CLI contract):** 0 ok, 1 runtime error, 2 usage error, **3 timeout**.
- All commits: run `gofmt -l .` (expect no output) and `go vet ./...` (expect no output) first.

---

### Task 1: envelope package

**Files:**
- Create: `internal/envelope/envelope.go`
- Test: `internal/envelope/envelope_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/envelope/envelope_test.go`:

```go
package envelope

import (
	"reflect"
	"testing"
	"time"
)

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	e := &Envelope{
		ID:        NewID(),
		CorrID:    "r-abc",
		From:      "orch",
		To:        "codex",
		ReplyTo:   "r-abc",
		TS:        time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		Body:      "review PR 56",
		Artifacts: []string{"out/img.png"},
	}
	data, err := Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(e, got) {
		t.Fatalf("round trip mismatch:\nsent %+v\ngot  %+v", e, got)
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	if _, err := Unmarshal([]byte("not json")); err == nil {
		t.Fatal("expected error for garbage input")
	}
}

func TestFilenameOrdersChronologically(t *testing.T) {
	// ID would sort the other way; TS must dominate the ordering.
	early := &Envelope{ID: "zzz", TS: time.Unix(0, 1000)}
	late := &Envelope{ID: "aaa", TS: time.Unix(0, 2000)}
	if !(Filename(early) < Filename(late)) {
		t.Fatalf("expected %q < %q", Filename(early), Filename(late))
	}
}

func TestNewIDUniqueAndWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if len(id) != 32 {
			t.Fatalf("id %q: want 32 hex chars", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/arda/projects/tincan && go test ./internal/envelope/`
Expected: FAIL (compile error: undefined `Envelope`, `NewID`, `Marshal`, `Unmarshal`, `Filename`)

- [ ] **Step 3: Write the implementation**

Create `internal/envelope/envelope.go`:

```go
// Package envelope defines the tincan message format and its on-disk naming.
package envelope

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Envelope is one tincan message. One envelope = one JSON file in a spool inbox.
type Envelope struct {
	ID        string    `json:"id"`
	CorrID    string    `json:"corr_id,omitempty"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	TS        time.Time `json:"ts"`
	Body      string    `json:"body"`
	Artifacts []string  `json:"artifacts,omitempty"`
}

// NewID returns a 32-char random hex string.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

// Filename returns the spool filename for e: zero-padded UnixNano, then ID,
// so lexical order == chronological order.
func Filename(e *Envelope) string {
	return fmt.Sprintf("%020d-%s.json", e.TS.UnixNano(), e.ID)
}

// Marshal renders e as indented JSON (readable when inspecting a spool by hand).
func Marshal(e *Envelope) ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
}

// Unmarshal parses one envelope.
func Unmarshal(data []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/arda/projects/tincan && go test ./internal/envelope/ -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Commit**

```bash
cd /Users/arda/projects/tincan && gofmt -l . && go vet ./... && \
git add internal/envelope/ && \
git commit -m "feat: envelope message format with chronologically ordered filenames"
```

---

### Task 2: spool Send (atomic delivery, queueing)

**Files:**
- Create: `internal/spool/spool.go`
- Test: `internal/spool/spool_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/spool/spool_test.go`:

```go
package spool

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
)

// msg builds a test envelope with a deterministic timestamp for ordering.
func msg(from, to, body string, nano int64) *envelope.Envelope {
	return &envelope.Envelope{
		ID:   envelope.NewID(),
		From: from,
		To:   to,
		TS:   time.Unix(0, nano).UTC(),
		Body: body,
	}
}

func TestSendDeliversParseableFileToInbox(t *testing.T) {
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := msg("orch", "codex", "hello", 1)
	if err := sp.Send(e); err != nil {
		t.Fatalf("Send: %v", err)
	}
	entries, err := os.ReadDir(sp.InboxDir("codex"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 file in inbox, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(sp.InboxDir("codex"), entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := envelope.Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Body != "hello" || got.From != "orch" || got.To != "codex" {
		t.Fatalf("wrong envelope: %+v", got)
	}
}

func TestSendLeavesNoTempResidue(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	if err := sp.Send(msg("a", "b", "x", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(room, ".tincan", "tmp"))
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp dir not empty after send: %v", entries)
	}
}

func TestSendQueuesMultipleInChronologicalFilenameOrder(t *testing.T) {
	sp, _ := Open(t.TempDir())
	for i, body := range []string{"m0", "m1", "m2"} {
		if err := sp.Send(msg("a", "b", body, int64(i+1))); err != nil {
			t.Fatalf("Send %s: %v", body, err)
		}
	}
	entries, _ := os.ReadDir(sp.InboxDir("b"))
	if len(entries) != 3 {
		t.Fatalf("want 3 queued files, got %d", len(entries))
	}
	var names []string
	for _, ent := range entries {
		names = append(names, ent.Name())
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("inbox filenames not sorted: %v", names)
	}
	// Oldest file must contain m0.
	data, _ := os.ReadFile(filepath.Join(sp.InboxDir("b"), names[0]))
	got, _ := envelope.Unmarshal(data)
	if got.Body != "m0" {
		t.Fatalf("oldest file body = %q, want m0", got.Body)
	}
}

func TestSendRejectsEmptyTo(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(&envelope.Envelope{From: "a", Body: "x"}); err == nil {
		t.Fatal("expected error for empty To")
	}
}

func TestSendFillsIDAndTS(t *testing.T) {
	sp, _ := Open(t.TempDir())
	e := &envelope.Envelope{From: "a", To: "b", Body: "x"}
	if err := sp.Send(e); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if e.ID == "" || e.TS.IsZero() {
		t.Fatalf("Send must fill ID and TS, got %+v", e)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/arda/projects/tincan && go test ./internal/spool/`
Expected: FAIL (compile error: undefined `Open`, `Send`, `InboxDir`)

- [ ] **Step 3: Write the implementation**

Create `internal/spool/spool.go`:

```go
// Package spool implements the tincan filesystem spool: atomic sends into
// per-name inbox directories and blocking receives that claim exactly once.
package spool

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
)

// Spool is a room's message store rooted at <room>/.tincan.
type Spool struct {
	root string // <room>/.tincan
}

// Open returns the spool for a room directory (typically the repo root).
func Open(room string) (*Spool, error) {
	abs, err := filepath.Abs(room)
	if err != nil {
		return nil, err
	}
	return &Spool{root: filepath.Join(abs, ".tincan")}, nil
}

// InboxDir returns the inbox directory for a participant name.
func (s *Spool) InboxDir(name string) string {
	return filepath.Join(s.root, "inbox", name)
}

func (s *Spool) tmpDir() string { return filepath.Join(s.root, "tmp") }
func (s *Spool) logDir() string { return filepath.Join(s.root, "log") }

// Send delivers e into the To inbox atomically: write to tmp/, then rename.
// Messages queue until a receiver claims them. Fills ID and TS if unset.
func (s *Spool) Send(e *envelope.Envelope) error {
	if e.To == "" {
		return errors.New("tincan: send: empty To")
	}
	if e.ID == "" {
		e.ID = envelope.NewID()
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	inbox := s.InboxDir(e.To)
	for _, dir := range []string{inbox, s.tmpDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := envelope.Marshal(e)
	if err != nil {
		return err
	}
	name := envelope.Filename(e)
	tmp := filepath.Join(s.tmpDir(), name)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(inbox, name))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/arda/projects/tincan && go test ./internal/spool/ -v`
Expected: PASS (5 tests). `logDir` is unused so far — that's fine, `go vet` doesn't flag unused methods.

- [ ] **Step 5: Commit**

```bash
cd /Users/arda/projects/tincan && gofmt -l . && go vet ./... && \
git add internal/spool/ && \
git commit -m "feat: spool Send with atomic temp-write-then-rename delivery"
```

---

### Task 3: spool Recv (blocking, timeout, oldest-first claim)

**Files:**
- Modify: `internal/spool/spool.go` (append Recv + claimOldest + ErrTimeout)
- Modify: `internal/spool/spool_test.go` (append tests)
- Modify: `go.mod`/`go.sum` (add fsnotify)

- [ ] **Step 1: Add the fsnotify dependency**

Run: `cd /Users/arda/projects/tincan && go get github.com/fsnotify/fsnotify`
Expected: `go: added github.com/fsnotify/fsnotify v1.x.x` (and golang.org/x/sys indirect)

- [ ] **Step 2: Write the failing tests**

Append to `internal/spool/spool_test.go`:

```go
func TestRecvDrainsQueuedOldestFirst(t *testing.T) {
	sp, _ := Open(t.TempDir())
	for i, body := range []string{"m0", "m1", "m2"} {
		if err := sp.Send(msg("a", "b", body, int64(i+1))); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	for _, want := range []string{"m0", "m1", "m2"} {
		e, err := sp.Recv("b", 2*time.Second, false)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if e.Body != want {
			t.Fatalf("Recv order: got %q, want %q", e.Body, want)
		}
	}
	// Inbox must now be empty.
	entries, _ := os.ReadDir(sp.InboxDir("b"))
	if len(entries) != 0 {
		t.Fatalf("inbox not drained: %v", entries)
	}
}

func TestRecvBlocksUntilSendArrives(t *testing.T) {
	sp, _ := Open(t.TempDir())
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := sp.Send(msg("a", "b", "wake", time.Now().UnixNano())); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()
	start := time.Now()
	e, err := sp.Recv("b", 10*time.Second, false)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if e.Body != "wake" {
		t.Fatalf("got body %q", e.Body)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("Recv returned after %v; want ~200ms (blocked until send)", elapsed)
	}
}

func TestRecvTimesOutWithErrTimeout(t *testing.T) {
	sp, _ := Open(t.TempDir())
	start := time.Now()
	_, err := sp.Recv("b", 300*time.Millisecond, false)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("returned before the timeout elapsed")
	}
}

func TestReArmAfterTimeoutLosesNothing(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if _, err := sp.Recv("b", 100*time.Millisecond, false); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	// Message lands during the "gap" between recv calls.
	if err := sp.Send(msg("a", "b", "queued-in-gap", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	e, err := sp.Recv("b", 2*time.Second, false)
	if err != nil {
		t.Fatalf("re-armed Recv: %v", err)
	}
	if e.Body != "queued-in-gap" {
		t.Fatalf("got %q", e.Body)
	}
}
```

Also add `"errors"` to the test file's import block.

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd /Users/arda/projects/tincan && go test ./internal/spool/`
Expected: FAIL (compile error: undefined `Recv`, `ErrTimeout`)

- [ ] **Step 4: Write the implementation**

Append to `internal/spool/spool.go` (and add `"fmt"`, `"sort"`, `"strings"`, `"github.com/fsnotify/fsnotify"` to imports):

```go
// ErrTimeout is returned by Recv when no message arrives within the timeout.
var ErrTimeout = errors.New("tincan: recv timeout")

// Recv blocks until one message is available in name's inbox, claims it,
// removes it from the spool (or moves it to log/ when logConsumed), and
// returns it. Returns ErrTimeout if nothing arrives within timeout.
func (s *Spool) Recv(name string, timeout time.Duration, logConsumed bool) (*envelope.Envelope, error) {
	inbox := s.InboxDir(name)
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		return nil, err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	defer watcher.Close()
	// Watch before scanning, so a message landing mid-scan is never missed.
	if err := watcher.Add(inbox); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		e, ok, err := s.claimOldest(name, logConsumed)
		if err != nil {
			return nil, err
		}
		if ok {
			return e, nil
		}
		select {
		case <-watcher.Events:
			// Inbox changed; rescan.
		case werr := <-watcher.Errors:
			return nil, werr
		case <-deadline.C:
			return nil, ErrTimeout
		}
	}
}

// claimOldest tries to claim the oldest message in name's inbox. The claim is
// an atomic rename into tmp/, so concurrent receivers process each message
// exactly once; losing a rename race just means trying the next file.
func (s *Spool) claimOldest(name string, logConsumed bool) (*envelope.Envelope, bool, error) {
	entries, err := os.ReadDir(s.InboxDir(name))
	if err != nil {
		return nil, false, err
	}
	var files []string
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") {
			files = append(files, ent.Name())
		}
	}
	sort.Strings(files) // filenames sort chronologically
	for _, f := range files {
		if err := os.MkdirAll(s.tmpDir(), 0o755); err != nil {
			return nil, false, err
		}
		claimed := filepath.Join(s.tmpDir(), fmt.Sprintf("claim-%s-%s", envelope.NewID(), f))
		if err := os.Rename(filepath.Join(s.InboxDir(name), f), claimed); err != nil {
			continue // another receiver claimed it first
		}
		data, err := os.ReadFile(claimed)
		if err != nil {
			return nil, false, err
		}
		e, err := envelope.Unmarshal(data)
		if err != nil {
			return nil, false, fmt.Errorf("tincan: bad message %s: %w", f, err)
		}
		if logConsumed {
			if err := os.MkdirAll(s.logDir(), 0o755); err != nil {
				return nil, false, err
			}
			if err := os.Rename(claimed, filepath.Join(s.logDir(), f)); err != nil {
				return nil, false, err
			}
		} else if err := os.Remove(claimed); err != nil {
			return nil, false, err
		}
		return e, true, nil
	}
	return nil, false, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /Users/arda/projects/tincan && go test ./internal/spool/ -v`
Expected: PASS (9 tests, includes the 5 from Task 2)

- [ ] **Step 6: Commit**

```bash
cd /Users/arda/projects/tincan && gofmt -l . && go vet ./... && \
git add internal/spool/ go.mod go.sum && \
git commit -m "feat: blocking spool Recv with fsnotify wake, timeout, oldest-first claim"
```

---

### Task 4: spool concurrency, log mode, RemoveInbox

**Files:**
- Modify: `internal/spool/spool.go` (append RemoveInbox)
- Modify: `internal/spool/spool_test.go` (append tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/spool/spool_test.go`:

```go
func TestConcurrentReceiversClaimExactlyOnce(t *testing.T) {
	sp, _ := Open(t.TempDir())
	const n = 10
	for i := 0; i < n; i++ {
		if err := sp.Send(msg("a", "b", fmt.Sprintf("m%02d", i), int64(i+1))); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	got := make(chan string, n)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				e, err := sp.Recv("b", 500*time.Millisecond, false)
				if errors.Is(err, ErrTimeout) {
					return // drained
				}
				if err != nil {
					t.Errorf("Recv: %v", err)
					return
				}
				got <- e.Body
			}
		}()
	}
	wg.Wait()
	close(got)
	seen := map[string]bool{}
	for body := range got {
		if seen[body] {
			t.Fatalf("message %q delivered twice", body)
		}
		seen[body] = true
	}
	if len(seen) != n {
		t.Fatalf("delivered %d of %d messages", len(seen), n)
	}
}

func TestLogConsumedKeepsCopyInLog(t *testing.T) {
	room := t.TempDir()
	sp, _ := Open(room)
	if err := sp.Send(msg("a", "b", "keep-me", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := sp.Recv("b", 2*time.Second, true); err != nil {
		t.Fatalf("Recv: %v", err)
	}
	logs, err := os.ReadDir(filepath.Join(room, ".tincan", "log"))
	if err != nil {
		t.Fatalf("ReadDir log: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 logged message, got %d", len(logs))
	}
	inbox, _ := os.ReadDir(sp.InboxDir("b"))
	if len(inbox) != 0 {
		t.Fatalf("inbox should be empty, got %v", inbox)
	}
}

func TestRemoveInbox(t *testing.T) {
	sp, _ := Open(t.TempDir())
	if err := sp.Send(msg("a", "r-chan1", "reply", 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := sp.RemoveInbox("r-chan1"); err != nil {
		t.Fatalf("RemoveInbox: %v", err)
	}
	if _, err := os.Stat(sp.InboxDir("r-chan1")); !os.IsNotExist(err) {
		t.Fatalf("inbox dir still exists (err=%v)", err)
	}
}
```

Also add `"fmt"` and `"sync"` to the test file's import block.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/arda/projects/tincan && go test ./internal/spool/`
Expected: FAIL (compile error: undefined `RemoveInbox`; the other two new tests compile and should pass — only the compile failure blocks)

- [ ] **Step 3: Write the implementation**

Append to `internal/spool/spool.go`:

```go
// RemoveInbox deletes a participant's inbox directory. Used to clean up
// ephemeral r-<id> reply channels after a successful ask.
func (s *Spool) RemoveInbox(name string) error {
	return os.RemoveAll(s.InboxDir(name))
}
```

- [ ] **Step 4: Run tests to verify they pass (with the race detector)**

Run: `cd /Users/arda/projects/tincan && go test ./internal/spool/ -race -v`
Expected: PASS (12 tests), no race reports

- [ ] **Step 5: Commit**

```bash
cd /Users/arda/projects/tincan && gofmt -l . && go vet ./... && \
git add internal/spool/ && \
git commit -m "feat: exactly-once concurrent claims, log mode, RemoveInbox"
```

---

### Task 5: CLI send + recv

**Files:**
- Create: `internal/cli/cli.go`
- Test: `internal/cli/cli_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/cli_test.go`:

```go
package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/c0ze/tincan/internal/envelope"
)

// run invokes the CLI and returns (exit code, stdout, stderr).
func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestSendThenRecvJSON(t *testing.T) {
	room := t.TempDir()
	code, _, stderr := run("send", "--room", room, "--to", "codex", "--from", "orch",
		"--body", "review PR 56", "--artifact", "a.txt", "--artifact", "b.txt")
	if code != 0 {
		t.Fatalf("send exit %d, stderr=%q", code, stderr)
	}
	code, stdout, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "5")
	if code != 0 {
		t.Fatalf("recv exit %d, stderr=%q", code, stderr)
	}
	e, err := envelope.Unmarshal([]byte(stdout))
	if err != nil {
		t.Fatalf("recv output is not an envelope: %v\n%s", err, stdout)
	}
	if e.Body != "review PR 56" || e.From != "orch" || e.To != "codex" {
		t.Fatalf("wrong envelope: %+v", e)
	}
	if len(e.Artifacts) != 2 || e.Artifacts[0] != "a.txt" || e.Artifacts[1] != "b.txt" {
		t.Fatalf("artifacts not preserved: %+v", e.Artifacts)
	}
}

func TestRecvFormatBody(t *testing.T) {
	room := t.TempDir()
	if code, _, stderr := run("send", "--room", room, "--to", "x", "--from", "y", "--body", "just the text"); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	code, stdout, _ := run("recv", "--room", room, "--as", "x", "--timeout", "5", "--format", "body")
	if code != 0 {
		t.Fatalf("recv exit %d", code)
	}
	if strings.TrimSpace(stdout) != "just the text" {
		t.Fatalf("body format output = %q", stdout)
	}
}

func TestRecvTimeoutExitsThreeSilently(t *testing.T) {
	code, stdout, _ := run("recv", "--room", t.TempDir(), "--as", "nobody", "--timeout", "1")
	if code != 3 {
		t.Fatalf("want exit 3 on timeout, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("want no stdout on timeout, got %q", stdout)
	}
}

func TestSendBodyFile(t *testing.T) {
	room := t.TempDir()
	f := room + "/task.md"
	if err := os.WriteFile(f, []byte("long instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("send", "--room", room, "--to", "x", "--from", "y", "--body-file", f); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	_, stdout, _ := run("recv", "--room", room, "--as", "x", "--timeout", "5", "--format", "body")
	if strings.TrimSpace(stdout) != "long instructions" {
		t.Fatalf("got %q", stdout)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"send", "--to", "x"},                        // missing --from and body
		{"send", "--to", "x", "--from", "y"},         // missing body
		{"recv"},                                     // missing --as
		{"recv", "--as", "x", "--format", "yaml"},    // bad format
		{"send", "--to", "x", "--from", "y", "--body", "a", "--body-file", "b"}, // both bodies
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != 2 {
			t.Fatalf("args %v: want exit 2, got %d", args, code)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, stdout, _ := run("help")
	if code != 0 || !strings.Contains(stdout, "tincan") {
		t.Fatalf("help: code=%d out=%q", code, stdout)
	}
}

var _ = io.Discard // keep io imported for later tasks
```

Also add `"os"` to the test file's import block.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/arda/projects/tincan && go test ./internal/cli/`
Expected: FAIL (compile error: undefined `Run`)

- [ ] **Step 3: Write the implementation**

Create `internal/cli/cli.go`:

```go
// Package cli implements the tincan command-line interface.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/spool"
)

// Exit codes (CLI contract; skills branch on these).
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitTimeout = 3
)

const usageText = `tincan — local message passing between AI coding agents

Usage:
  tincan send  --to <name> --from <name> (--body <s> | --body-file <f>) [flags]
  tincan recv  --as <name> [--timeout <sec>] [--format json|body] [--log] [flags]
  tincan ask   --to <name> --from <name> (--body <s> | --body-file <f>) [--timeout <sec>] [flags]
  tincan reply --channel <id> (--body <s> | --body-file <f>) [--from <name>] [flags]

Common flags:
  --room <path>       room directory (default: current directory)
  --artifact <path>   artifact pointer, repeatable (send/ask/reply)

Exit codes: 0 ok, 1 error, 2 usage, 3 timeout.
`

// Run executes a tincan command and returns its exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}
	switch args[0] {
	case "send":
		return cmdSend(args[1:], stdout, stderr)
	case "recv":
		return cmdRecv(args[1:], stdout, stderr)
	case "ask":
		return cmdAsk(args[1:], stdout, stderr)
	case "reply":
		return cmdReply(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "tincan: unknown command %q\n%s", args[0], usageText)
		return ExitUsage
	}
}

// stringList is a repeatable string flag (--artifact a --artifact b).
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// bodyFrom resolves --body / --body-file (exactly one required).
func bodyFrom(body, bodyFile string) (string, error) {
	switch {
	case body != "" && bodyFile != "":
		return "", errors.New("use --body or --body-file, not both")
	case body != "":
		return body, nil
	case bodyFile != "":
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", err
		}
		return string(data), nil
	default:
		return "", errors.New("--body or --body-file is required")
	}
}

func printEnvelope(e *envelope.Envelope, format string, stdout, stderr io.Writer) int {
	if format == "body" {
		fmt.Fprintln(stdout, e.Body)
		return ExitOK
	}
	data, err := envelope.Marshal(e)
	if err != nil {
		fmt.Fprintf(stderr, "tincan: %v\n", err)
		return ExitError
	}
	fmt.Fprintln(stdout, string(data))
	return ExitOK
}

func cmdSend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		to       = fs.String("to", "", "recipient name")
		from     = fs.String("from", "", "sender name")
		room     = fs.String("room", ".", "room directory")
		corr     = fs.String("corr", "", "correlation id")
		replyTo  = fs.String("reply-to", "", "reply channel")
		body     = fs.String("body", "", "message body")
		bodyFile = fs.String("body-file", "", "read body from file")
	)
	var artifacts stringList
	fs.Var(&artifacts, "artifact", "artifact path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *to == "" || *from == "" {
		fmt.Fprintln(stderr, "tincan send: --to and --from are required")
		return ExitUsage
	}
	b, err := bodyFrom(*body, *bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tincan send: %v\n", err)
		return ExitUsage
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan send: %v\n", err)
		return ExitError
	}
	e := &envelope.Envelope{
		ID: envelope.NewID(), CorrID: *corr, From: *from, To: *to,
		ReplyTo: *replyTo, TS: time.Now().UTC(), Body: b, Artifacts: artifacts,
	}
	if err := sp.Send(e); err != nil {
		fmt.Fprintf(stderr, "tincan send: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "sent id=%s to=%s\n", e.ID, e.To)
	return ExitOK
}

func cmdRecv(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		as      = fs.String("as", "", "my inbox name")
		room    = fs.String("room", ".", "room directory")
		timeout = fs.Int("timeout", 570, "seconds to wait")
		format  = fs.String("format", "json", "output format: json|body")
		logMsgs = fs.Bool("log", false, "keep consumed messages in .tincan/log/")
	)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *as == "" {
		fmt.Fprintln(stderr, "tincan recv: --as is required")
		return ExitUsage
	}
	if *format != "json" && *format != "body" {
		fmt.Fprintln(stderr, "tincan recv: --format must be json or body")
		return ExitUsage
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan recv: %v\n", err)
		return ExitError
	}
	e, err := sp.Recv(*as, time.Duration(*timeout)*time.Second, *logMsgs)
	if errors.Is(err, spool.ErrTimeout) {
		return ExitTimeout
	}
	if err != nil {
		fmt.Fprintf(stderr, "tincan recv: %v\n", err)
		return ExitError
	}
	return printEnvelope(e, *format, stdout, stderr)
}

func cmdAsk(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "tincan ask: not implemented yet")
	return ExitError
}

func cmdReply(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "tincan reply: not implemented yet")
	return ExitError
}
```

(`cmdAsk`/`cmdReply` are stubs so `Run` compiles; Task 6 replaces them.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/arda/projects/tincan && go test ./internal/cli/ -v`
Expected: PASS (7 tests)

- [ ] **Step 5: Commit**

```bash
cd /Users/arda/projects/tincan && gofmt -l . && go vet ./... && \
git add internal/cli/ && \
git commit -m "feat: CLI send and recv with exit-code contract"
```

---

### Task 6: CLI ask + reply

**Files:**
- Modify: `internal/cli/cli.go` (replace the cmdAsk/cmdReply stubs)
- Modify: `internal/cli/cli_test.go` (append tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/cli_test.go`:

```go
func TestAskReplyRoundTrip(t *testing.T) {
	room := t.TempDir()
	askDone := make(chan int, 1)
	var askOut bytes.Buffer
	go func() {
		askDone <- Run([]string{"ask", "--room", room, "--to", "codex", "--from", "orch",
			"--body", "2+2?", "--timeout", "30", "--format", "body"}, &askOut, io.Discard)
	}()
	// Listener side: receive the request.
	code, reqJSON, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "30")
	if code != 0 {
		t.Fatalf("listener recv exit %d: %s", code, stderr)
	}
	req, err := envelope.Unmarshal([]byte(reqJSON))
	if err != nil {
		t.Fatalf("bad request json: %v", err)
	}
	if req.ReplyTo == "" || !strings.HasPrefix(req.ReplyTo, "r-") {
		t.Fatalf("request has no r-* reply channel: %+v", req)
	}
	if req.CorrID != req.ReplyTo {
		t.Fatalf("corr_id %q != reply_to %q", req.CorrID, req.ReplyTo)
	}
	// Listener replies.
	if code, _, stderr := run("reply", "--room", room, "--channel", req.ReplyTo,
		"--from", "codex", "--body", "4"); code != 0 {
		t.Fatalf("reply exit %d: %s", code, stderr)
	}
	if code := <-askDone; code != 0 {
		t.Fatalf("ask exit %d", code)
	}
	if strings.TrimSpace(askOut.String()) != "4" {
		t.Fatalf("ask output = %q, want 4", askOut.String())
	}
	// Successful ask must clean up its reply channel.
	if _, err := os.Stat(filepath.Join(room, ".tincan", "inbox", req.ReplyTo)); !os.IsNotExist(err) {
		t.Fatalf("reply channel not cleaned up (err=%v)", err)
	}
}

func TestAskTimeoutPrintsPendingAndLateReplyIsCollectable(t *testing.T) {
	room := t.TempDir()
	code, stdout, _ := run("ask", "--room", room, "--to", "codex", "--from", "orch",
		"--body", "anyone there?", "--timeout", "1")
	if code != 3 {
		t.Fatalf("want exit 3, got %d", code)
	}
	line := strings.TrimSpace(stdout)
	if !strings.HasPrefix(line, "pending channel=r-") {
		t.Fatalf("want 'pending channel=r-...', got %q", line)
	}
	channel := strings.TrimPrefix(line, "pending channel=")
	// The request is still queued for codex.
	code, reqJSON, _ := run("recv", "--room", room, "--as", "codex", "--timeout", "5")
	if code != 0 {
		t.Fatalf("request lost after ask timeout (exit %d)", code)
	}
	req, _ := envelope.Unmarshal([]byte(reqJSON))
	if req.ReplyTo != channel {
		t.Fatalf("queued request reply_to %q != pending channel %q", req.ReplyTo, channel)
	}
	// Late reply lands on the leftover channel and is collectable with plain recv.
	if code, _, stderr := run("reply", "--room", room, "--channel", channel,
		"--from", "codex", "--body", "late answer"); code != 0 {
		t.Fatalf("late reply failed: %s", stderr)
	}
	code, stdout, _ = run("recv", "--room", room, "--as", channel, "--timeout", "5", "--format", "body")
	if code != 0 || strings.TrimSpace(stdout) != "late answer" {
		t.Fatalf("late collect: code=%d out=%q", code, stdout)
	}
}

func TestReplyUsageErrors(t *testing.T) {
	cases := [][]string{
		{"reply"},                              // missing channel
		{"reply", "--channel", "r-x"},          // missing body
		{"ask", "--to", "x", "--from", "y"},    // missing body
		{"ask", "--from", "y", "--body", "b"},  // missing to
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != 2 {
			t.Fatalf("args %v: want exit 2, got %d", args, code)
		}
	}
}
```

Also add `"path/filepath"` to the test file's import block.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/arda/projects/tincan && go test ./internal/cli/`
Expected: FAIL — `TestAskReplyRoundTrip` and the others hit the "not implemented yet" stubs (exit 1, or exit 2 mismatches)

- [ ] **Step 3: Write the implementation**

In `internal/cli/cli.go`, replace the two stub functions entirely with:

```go
func cmdAsk(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		to       = fs.String("to", "", "recipient name")
		from     = fs.String("from", "", "sender name")
		room     = fs.String("room", ".", "room directory")
		timeout  = fs.Int("timeout", 570, "seconds to wait for the reply")
		format   = fs.String("format", "json", "output format: json|body")
		body     = fs.String("body", "", "message body")
		bodyFile = fs.String("body-file", "", "read body from file")
	)
	var artifacts stringList
	fs.Var(&artifacts, "artifact", "artifact path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *to == "" || *from == "" {
		fmt.Fprintln(stderr, "tincan ask: --to and --from are required")
		return ExitUsage
	}
	b, err := bodyFrom(*body, *bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ExitUsage
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ExitError
	}
	channel := "r-" + envelope.NewID()
	e := &envelope.Envelope{
		ID: envelope.NewID(), CorrID: channel, From: *from, To: *to,
		ReplyTo: channel, TS: time.Now().UTC(), Body: b, Artifacts: artifacts,
	}
	if err := sp.Send(e); err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ExitError
	}
	reply, err := sp.Recv(channel, time.Duration(*timeout)*time.Second, false)
	if errors.Is(err, spool.ErrTimeout) {
		// Leave the channel in place: a late reply still lands there and is
		// collected with `tincan recv --as <channel>`.
		fmt.Fprintf(stdout, "pending channel=%s\n", channel)
		return ExitTimeout
	}
	if err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ExitError
	}
	code := printEnvelope(reply, *format, stdout, stderr)
	if err := sp.RemoveInbox(channel); err != nil {
		fmt.Fprintf(stderr, "tincan ask: cleanup: %v\n", err)
		return ExitError
	}
	return code
}

func cmdReply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		channel  = fs.String("channel", "", "reply channel (the request's reply_to)")
		from     = fs.String("from", "", "sender name (optional)")
		room     = fs.String("room", ".", "room directory")
		body     = fs.String("body", "", "message body")
		bodyFile = fs.String("body-file", "", "read body from file")
	)
	var artifacts stringList
	fs.Var(&artifacts, "artifact", "artifact path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *channel == "" {
		fmt.Fprintln(stderr, "tincan reply: --channel is required")
		return ExitUsage
	}
	b, err := bodyFrom(*body, *bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tincan reply: %v\n", err)
		return ExitUsage
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan reply: %v\n", err)
		return ExitError
	}
	e := &envelope.Envelope{
		ID: envelope.NewID(), CorrID: *channel, From: *from, To: *channel,
		TS: time.Now().UTC(), Body: b, Artifacts: artifacts,
	}
	if err := sp.Send(e); err != nil {
		fmt.Fprintf(stderr, "tincan reply: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "replied channel=%s\n", *channel)
	return ExitOK
}
```

- [ ] **Step 4: Run tests to verify they pass (race detector on)**

Run: `cd /Users/arda/projects/tincan && go test ./... -race`
Expected: PASS — all three packages (`envelope`, `spool`, `cli`)

- [ ] **Step 5: Commit**

```bash
cd /Users/arda/projects/tincan && gofmt -l . && go vet ./... && \
git add internal/cli/ && \
git commit -m "feat: CLI ask and reply with ephemeral r-<id> reply channels"
```

---

### Task 7: main entrypoint, end-to-end smoke test, README status

**Files:**
- Create: `cmd/tincan/main.go`
- Modify: `README.md` (drop the "not implemented" status banner)

- [ ] **Step 1: Write main.go**

Create `cmd/tincan/main.go`:

```go
// Command tincan passes messages between AI coding agents over a local
// filesystem spool. See PROTOCOL.md in the repository root.
package main

import (
	"os"

	"github.com/c0ze/tincan/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
```

- [ ] **Step 2: Build and run the full test suite**

Run: `cd /Users/arda/projects/tincan && go build ./... && go test ./... -race && go vet ./... && gofmt -l .`
Expected: builds clean, all tests PASS, no vet/gofmt output

- [ ] **Step 3: End-to-end smoke test with the real binary**

```bash
cd /Users/arda/projects/tincan && go build -o /tmp/tincan-smoke/tincan ./cmd/tincan && \
ROOM=$(mktemp -d) && \
( sleep 1 && REQ=$(/tmp/tincan-smoke/tincan recv --room "$ROOM" --as codex --timeout 20) && \
  CH=$(printf '%s' "$REQ" | sed -n 's/.*"reply_to": "\(r-[0-9a-f]*\)".*/\1/p') && \
  /tmp/tincan-smoke/tincan reply --room "$ROOM" --channel "$CH" --from codex --body "LGTM" ) & \
/tmp/tincan-smoke/tincan ask --room "$ROOM" --to codex --from orch --body "review this" --timeout 20 --format body
```

Expected output: `LGTM` (the ask blocks until the backgrounded listener replies). Exit code 0.

- [ ] **Step 4: Update the README status banner**

In `README.md`, replace:

```markdown
> **Status:** the `tincan` engine (`cmd/tincan`) is not implemented yet — this repo
> currently holds the design, the skills, and the installer. The `go install` path
> below goes live once the engine (Phase 1) is built and pushed.
```

with:

```markdown
> **Status:** Phase 1 (core engine) implemented — `send`/`recv`/`ask`/`reply` over the
> filesystem spool, with tests. The `go install` path below goes live once this repo
> is pushed to GitHub. Fan-out courier docs and other-agent shims: see spec phases 2–3.
```

- [ ] **Step 5: Commit**

```bash
cd /Users/arda/projects/tincan && gofmt -l . && go vet ./... && \
git add cmd/ README.md && \
git commit -m "feat: tincan main entrypoint; Phase 1 complete"
```

---

## Self-review notes (already applied)

- Spec §5 flags all covered: `send` (`--to --from --room --corr --reply-to --body --body-file --artifact`), `recv` (`--as --room --timeout --format --log`), `ask` (adds `--timeout --format`), `reply` (`--channel --from --room --body --body-file --artifact`). `--log` was listed under Phase 2 in the spec but costs ~6 lines here, so it ships in Task 3/4; `gc` stays deferred to Phase 2.
- Timeout exit code 3 (spec §5), silent on stdout: Task 5 test. Pending-channel contract (spec §5 ask): Task 6 test, including late collection via plain `recv --as r-<id>` (the unified-namespace fix from spec review).
- Watch-then-scan (spec §3): Recv adds the fsnotify watch before the first claim scan.
- Exactly-once claim under concurrency (spec §10/§12): Task 4 race-detector test.
- Two rapid `send`s can share a UnixNano tick; order between them is then decided by ID, not send order. Delivery and exactly-once still hold — acceptable for Phase 1 (agents don't depend on sub-microsecond ordering).
