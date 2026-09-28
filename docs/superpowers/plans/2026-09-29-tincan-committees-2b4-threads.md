# tincan committees 2b-4: committees in threads Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Writing `@reviewers` in a web thread asks that committee to review the change. Each member's review appears in the thread as it arrives. When an agent asked, that agent gets a follow-up turn to synthesize the results. Chain budget, Stop and archive all behave as they do for agent turns.

**Architecture:**
- **Thread journal.** It gains a `committee` message role, which stands for one review. Its intent carries the review ID and the committee snapshot frozen at mention time. It also gains a `review` role for member results, and keyed chain reservations so a replay is never charged twice.
- **Dispatcher planning and build.** The thread dispatcher plans committee messages in the same transaction as the post or the handoffs marker. On a later pass it builds and publishes the review, following phase 1's submit pattern: a transaction check, then the work, then a second transaction.
- **Coordinator.** The review coordinator posts results and completes committee messages through a `review.ThreadSink`, which the room's dispatcher implements and passes per room pass. `review` never imports `thread`.
- **Stop and archive.** `finishStop` gains a committee branch.
- **Synthesis.** It is an ordinary `planTurn` handoff to the agent that mentioned the committee.

**Tech Stack:** Go 1.25, existing packages, plain JS UI.

**Spec:** `docs/superpowers/specs/2026-09-28-tincan-committees-design.md`: §6.4 (lock nesting, settle with `thread_done`), §6.10 "Thread reviews" and "Synthesis", §10, §11 (thread tests).

## Global Constraints

- **Mention resolution order:** available presets, then committees, then thread listeners, then hosted listeners. A committee name can never equal a preset name (2b-1 validation).
- **Committee message:**
  - `Role: "committee"`, `Author: <committee name>`, `ReplyTo`: the mentioning message, `Chain`: its chain.
  - Its intent carries `RequestID: <tid>-<mid>`, `Review: <review_id>` (from `review.NewID(machine, canonical room, request ID)`) and `Committee`: the frozen snapshot.
  - It starts `pending`, becomes `running` once the review is published, and ends `done`, `cancelled` or `error`.
- **Budget:** an agent handoff reserves one execution per member with key `review:<review_id>:<n>`. A replayed keyed reservation is a no-op. If the chain cannot afford every member, one `suggested` message is posted for the whole committee (`@<name> please review …`). Owner mentions reserve nothing.
- **Build (submit pattern):**
  - A transaction checks the message is `pending`, the thread open and the chain not stopped.
  - A review that already exists with origin `thread:<tid>:<mid>` is adopted; otherwise the review is published from the snapshot, with scope `""` (uncommitted in Git, else none).
  - The question is the mentioning message plus the transcript before it, oldest cut first, to 64 KiB.
  - A second transaction marks the message `running` if it is still `pending`. Otherwise it records that the review must be cancelled, and `review.RequestCancel` runs after the transaction.
  - A build error marks the message `error`.
- **Results:** a role `review` message, author `<committee>/<member>`, `Review` = `<review_id>/<n>`, state `done`. It is appended once, and only while the thread is open; otherwise it is skipped but still counted as posted. Review messages never trigger mentions.
- **Completion** runs once, when the review is closed or cancelled, and only while the thread is open. The committee message becomes:
  - `done`, with a summary and the bundle path, when any member finished;
  - `cancelled` for a cancelled review;
  - `error` when no member finished.
  - When the mentioning message was an agent turn, the same transaction plans a synthesis turn for that agent through `planTurn(..., auto=true)`, triggered by the committee message.
- **Stop and archive:** `finishStop` requests cancellation of every unsettled review referenced by the thread's committee messages (between transactions), then marks `pending` and `running` committee messages `cancelled` in its settle transaction. It does not wait for peers.
- **Settle** (thread origins): every recorded result is posted and `thread_done` is set.
- **Lock nesting** (spec §6.4): review → thread only. Thread code never takes a review lock inside `Thread.Update`.
- No new dependencies. gofmt, `go vet ./...` and `GOOS=windows go vet ./...` must stay clean. Never use `git stash`. Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **The committee is edited between the mention and the build.** The review uses the frozen snapshot; the reservation and the member count match. Pinned in Task 3 and Task 4.
2. **Stop while the committee message is `pending`, or between publish and `running`.** No review keeps running: never built, or built then cancelled. The thread returns to open, and the review settles. Pinned in Task 4 and Task 6.
3. **A member result arrives after the thread was archived.** Nothing is appended to the archived thread. The review still settles and is never stuck waiting to post. Pinned in Task 5.
4. **A late result after the committee message completed.** It is posted, with no second synthesis turn. Pinned in Task 5.
5. **A replayed handoff after a crash** is not double-charged: the keyed reservations are no-ops. Pinned in Task 1 and Task 3.

---

### Task 1: Journal model — roles, review references, frozen snapshots, keyed reservations

**Files:**
- Modify: `internal/thread/model.go`, `internal/thread/prompt.go` (`transcriptOf`)
- Test: `internal/thread/model_committee_test.go` (package `thread`)

**Interfaces:**
- Produces the constants `RoleCommittee = "committee"` and `RoleReview = "review"`.
- Produces the event fields `Review string` (`json:"review,omitempty"`), `Committee *committee.Committee` (`json:"committee,omitempty"`) and `Key string` (`json:"key,omitempty"`).
- Produces the message fields `Review string` (`json:"review,omitempty"`) and `Committee *committee.Committee` (`json:"committee,omitempty"`).
- `Chain` gains an unexported `keys map[string]bool`.

- [ ] **Step 1: Write the failing test.** Create `internal/thread/model_committee_test.go`:

```go
package thread

import (
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
)

func TestKeyedReservationsCountOnce(t *testing.T) {
	s := newSnapshot(Meta{ID: "t1", Budget: 6})
	s.apply(Event{Seq: 1, Kind: KindChain, Chain: "c1", Op: OpReserve})
	s.apply(Event{Seq: 2, Kind: KindChain, Chain: "c1", Op: OpReserve, Key: "review:rv-x:0"})
	s.apply(Event{Seq: 3, Kind: KindChain, Chain: "c1", Op: OpReserve, Key: "review:rv-x:0"})
	s.apply(Event{Seq: 4, Kind: KindChain, Chain: "c1", Op: OpReserve, Key: "review:rv-x:1"})
	if got := s.Chains["c1"].Used; got != 3 {
		t.Fatalf("used = %d, want 3 (unkeyed + two distinct keys)", got)
	}
}

func TestCommitteeIntentAndReviewReference(t *testing.T) {
	s := newSnapshot(Meta{ID: "t1"})
	c := committee.Committee{Name: "reviewers", Version: 2, Members: []string{"a@m"}}
	s.apply(Event{Seq: 1, Kind: KindMessage, ID: "m1", N: 1, Author: "reviewers", Role: RoleCommittee, Time: time.Now()})
	s.apply(Event{Seq: 2, Kind: KindIntent, Message: "m1", RequestID: "t1-m1", Review: "rv-abc", Committee: &c})
	s.apply(Event{Seq: 3, Kind: KindMessage, ID: "m2", N: 2, Author: "reviewers/a@m", Role: RoleReview, Review: "rv-abc/0", Text: "ok", Time: time.Now()})
	m1, _ := s.Message("m1")
	if m1.Review != "rv-abc" || m1.Committee == nil || m1.Committee.Version != 2 || m1.RequestID != "t1-m1" {
		t.Fatalf("committee message: %+v", m1)
	}
	if m2, _ := s.Message("m2"); m2.Review != "rv-abc/0" {
		t.Fatalf("review message: %+v", m2)
	}
}

func TestTranscriptSkipsUnfinishedCommitteeMessages(t *testing.T) {
	s := newSnapshot(Meta{ID: "t1"})
	s.apply(Event{Seq: 1, Kind: KindMessage, ID: "m1", N: 1, Author: "you", Role: RoleUser, Text: "@reviewers look"})
	s.apply(Event{Seq: 2, Kind: KindMessage, ID: "m2", N: 2, Author: "reviewers", Role: RoleCommittee})
	s.apply(Event{Seq: 3, Kind: KindState, Message: "m2", State: StateRunning})
	s.apply(Event{Seq: 4, Kind: KindMessage, ID: "m3", N: 3, Author: "reviewers/a@m", Role: RoleReview, Text: "LGTM"})
	got := transcriptOf(s, "")
	if len(got) != 2 || got[0].ID != "m1" || got[1].ID != "m3" {
		t.Fatalf("transcript: %+v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/thread -run 'TestKeyedReservations|TestCommitteeIntent|TestTranscriptSkips'`
Expected: build failure (`unknown field Key`).

- [ ] **Step 3: Implement.** In `model.go`:
- Add `RoleCommittee = "committee"` and `RoleReview = "review"` to the role constants.
- Add the three `Event` fields and the two `Message` fields (import `internal/committee`).
- Add `keys map[string]bool` to `Chain`.
- In `apply`:
  - For `KindMessage`, set `Review: e.Review` in the `Message` literal.
  - In the `KindIntent` case, also set `m.Review, m.Committee = e.Review, e.Committee`.
  - In the `KindChain`/`OpReserve` case, replace `c.Used++` with:

    ```go
    		case OpReserve:
    			if e.Key != "" {
    				if c.keys == nil {
    					c.keys = map[string]bool{}
    				}
    				if c.keys[e.Key] {
    					break // a replayed keyed reservation is charged once
    				}
    				c.keys[e.Key] = true
    			}
    			c.Used++
    ```

- Update the `Event` doc comment: the committee intent carries `Review` and `Committee`, a review message carries `Review`, and a chain reserve may carry `Key`.

In `prompt.go` `transcriptOf`, add after the `RoleAgent` check:

```go
		if m.Role == RoleCommittee && (m.State == StatePending || m.State == StateRunning) {
			continue
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/thread`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/thread
git commit -m "Add committee and review messages and keyed reservations to the thread journal"
```

---

### Task 2: Review side — snapshot publication, the thread sink, settle with `thread_done`

**Files:**
- Modify: `internal/review/publish.go` (`PublishRequest.Snapshot`), `internal/review/coordinator.go` (`ThreadSink`, `ThreadOrigin`, `Reconcile(ctx, room, sink)`, posting and completion, settle), `internal/web/events.go` and `internal/web/reviews_test.go` (callers)
- Modify: `internal/committee/store.go` (`List(stateDir, fromPeer)`)
- Test: `internal/review/coordinator_test.go`, `internal/review/publish_test.go`

**Interfaces:**
- Produces the field `PublishRequest.Snapshot *committee.Committee`. When it is set, it is used instead of `committee.Lookup`.
- Produces:

  ```go
  type ThreadSink interface {
      PostResult(ctx context.Context, in Input, m Member, text string) error
      Complete(ctx context.Context, in Input, st State) (done bool, err error)
  }
  func ThreadOrigin(origin string) (tid, mid string, ok bool) // "thread:<tid>:<mid>"
  func (c *Coordinator) Reconcile(ctx context.Context, room string, sink ThreadSink) error
  ```

- Produces `func committee.List(stateDir string, fromPeer bool) ([]Committee, error)`, which `Lookup` now uses.

- [ ] **Step 1: Write the failing tests.** Add to `internal/review/publish_test.go`:

```go
func TestPublishUsesTheFrozenSnapshot(t *testing.T) {
	req, _ := publishFixture(t)
	snap := committee.Committee{Name: "reviewers", Version: 1, Members: []string{"only@macmini"}, DeadlineMinutes: 5}
	req.Snapshot = &snap
	req.Origin = "thread:t1:m1"
	in, st, err := Publish(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Members) != 1 || in.Committee.Members[0] != "only@macmini" || in.Origin != "thread:t1:m1" {
		t.Fatalf("snapshot ignored: %+v %+v", in, st.Members)
	}
}
```

Add to `internal/review/coordinator_test.go`:

```go
type fakeSink struct {
	posted   map[string]string
	complete int
	done     bool
}

func (f *fakeSink) PostResult(ctx context.Context, in Input, m Member, text string) error {
	f.posted[m.JobID] = text
	return nil
}

func (f *fakeSink) Complete(ctx context.Context, in Input, st State) (bool, error) {
	f.complete++
	return f.done, nil
}

func TestThreadOriginPostsResultsAndCompletesBeforeSettling(t *testing.T) {
	room, in := published(t, []string{"a@macmini"}, 10*time.Minute, false)
	in.Origin = "thread:t1:m1"
	writeJSON(filepath.Join(Dir(room, in.ReviewID), "input.json"), in)
	f := newFake()
	sink := &fakeSink{posted: map[string]string{}}
	clk := &clock{time.Now()}
	c := &Coordinator{Transport: f, Now: clk.now}
	ctx := context.Background()
	c.Reconcile(ctx, room, sink)
	f.finish(in.ReviewID+"-0", "posted result")
	clk.advance(20 * time.Second)
	c.Reconcile(ctx, room, sink)
	st, _ := ReadState(room, in.ReviewID)
	if sink.posted[in.ReviewID+"-0"] != "posted result" || !st.Members[0].Posted {
		t.Fatalf("result not posted: %+v %+v", sink.posted, st.Members[0])
	}
	if st.Settled || st.ThreadDone {
		t.Fatal("settled before the thread completed the committee message")
	}
	sink.done = true
	clk.advance(20 * time.Second)
	c.Reconcile(ctx, room, sink)
	st, _ = ReadState(room, in.ReviewID)
	if !st.ThreadDone || !st.Settled {
		t.Fatalf("not settled after completion: %+v", st)
	}
	n := sink.complete
	c.Reconcile(ctx, room, sink)
	if sink.complete != n {
		t.Fatal("completion called again after thread_done")
	}
}

func TestThreadOrigin(t *testing.T) {
	if tid, mid, ok := ThreadOrigin("thread:t1:m2"); !ok || tid != "t1" || mid != "m2" {
		t.Fatalf("%q %q %v", tid, mid, ok)
	}
	for _, bad := range []string{"mcp", "thread:", "thread:t1", "thread::m"} {
		if _, _, ok := ThreadOrigin(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
```

Every existing `c.Reconcile(ctx, room)` and `(&Coordinator{…}).Reconcile(context.Background(), room)` call in `coordinator_test.go` becomes `Reconcile(ctx, room, nil)`. Do the same in `internal/web/reviews_test.go` (`hub.coord.Reconcile(context.Background(), room, nil)`) and in `internal/web/events.go` `runRoomPass`, which becomes `s.coord.Reconcile(ctx, room.Path, d)` in Task 7. Until then pass `nil`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/review -run 'TestPublishUsesTheFrozenSnapshot|TestThreadOrigin'`
Expected: build failure.

- [ ] **Step 3: Implement.** In `internal/committee/store.go`, factor the list out of `Lookup`:

```go
// List returns the committees this machine sees: the peer cache when it
// reads committees from a hub, else the hub store.
func List(stateDir string, fromPeer bool) ([]Committee, error) {
	if !fromPeer {
		return NewStore(stateDir).List()
	}
	c, ok, err := LoadCache(stateDir)
	if err != nil || !ok {
		return nil, err
	}
	return c.Committees, nil
}
```

Keep `Lookup`'s error messages, including the peer cache age.

In `publish.go`, add `Snapshot *committee.Committee` to `PublishRequest`, and replace the `committee.Lookup` call with:

```go
	var c committee.Committee
	if req.Snapshot != nil {
		c = *req.Snapshot
		if err := c.Normalize(); err != nil {
			return Input{}, State{}, err
		}
	} else if c, err = committee.Lookup(req.StateDir, req.Committee, fromPeer); err != nil {
		return Input{}, State{}, err
	}
```

Declare `err` before this block if the surrounding code does not already.

In `coordinator.go`:

```go
// ThreadSink is how the coordinator reaches the thread a review came from
// (committees §6.10); the room's dispatcher implements it. It is called
// with the review lock held (review → thread is the only lock nesting).
type ThreadSink interface {
	PostResult(ctx context.Context, in Input, m Member, text string) error
	Complete(ctx context.Context, in Input, st State) (done bool, err error)
}

// ThreadOrigin parses "thread:<tid>:<mid>".
func ThreadOrigin(origin string) (tid, mid string, ok bool) {
	rest, found := strings.CutPrefix(origin, "thread:")
	if !found {
		return "", "", false
	}
	tid, mid, found = strings.Cut(rest, ":")
	return tid, mid, found && tid != "" && mid != ""
}
```

Change `Reconcile(ctx, room string)` to `Reconcile(ctx context.Context, room string, sink ThreadSink)`. Pass `sink` into `reconcile(ctx, room, id, sink)`. In `reconcile`, after `c.close(...)` and before the settle check, add:

```go
	if _, _, ok := ThreadOrigin(in.Origin); ok && sink != nil {
		for i := range st.Members {
			m := &st.Members[i]
			if m.Posted {
				continue
			}
			text, has, _ := ReadResult(room, id, m.Index)
			if !has {
				continue
			}
			if err := sink.PostResult(ctx, in, *m, text); err != nil {
				return err
			}
			m.Posted = true
			if err := save(); err != nil {
				return err
			}
		}
		if (st.Status == "closed" || st.Status == "cancelled") && !st.ThreadDone {
			done, err := sink.Complete(ctx, in, st)
			if err != nil {
				return err
			}
			if done {
				st.ThreadDone = true
				if err := save(); err != nil {
					return err
				}
			}
		}
	}
```

In `settled(room, in, st)`, return false for a thread origin unless `st.ThreadDone` is set and every member with a result is `Posted`:

```go
	if _, _, ok := ThreadOrigin(in.Origin); ok {
		if !st.ThreadDone {
			return false
		}
		for _, m := range st.Members {
			if _, has, _ := ReadResult(room, in.ReviewID, m.Index); has && !m.Posted {
				return false
			}
		}
	}
```

Add `strings` to the imports.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/review ./internal/committee ./internal/web`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/review internal/committee internal/web
git commit -m "Publish thread reviews from a frozen snapshot and reach threads through a sink"
```

---

### Task 3: Resolve `@committee` and plan committee messages

**Files:**
- Modify: `internal/thread/resolve.go` (`Target.Committee`, `Resolver.Committees`), `internal/thread/dispatcher.go` (the `Machine`, `StateDir`, `CommitteesFrom` and `Registry` fields; `Resolver()` loads committees; `planCommittee`; Post and handoffs call it)
- Create: `internal/thread/committee.go` (`planCommittee`, and later the build and sink)
- Test: `internal/thread/committee_test.go` (package `thread_test`)

**Interfaces:**
- Produces the fields `Target.Committee *committee.Committee` and `Resolver.Committees map[string]committee.Committee`.
- Produces the fields `Dispatcher.Machine`, `StateDir` and `CommitteesFrom string`, and `Registry *rooms.Registry`.
- Produces `func (d *Dispatcher) planCommittee(t *Thread, tx *Tx, tg Target, trigger Message, chain string, auto bool) (string, error)`.
- Test helper `committeeEnv(t, members ...string) (*env, *thread.Dispatcher, string)`: a dispatcher with a state directory holding committee `reviewers` (the given members), machine `box`, registry with the room. It returns the state directory.

- [ ] **Step 1: Write the failing tests.** Create `internal/thread/committee_test.go`:

```go
package thread_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
)

func committeeEnv(t *testing.T, members ...string) (*env, *thread.Dispatcher, string) {
	t.Helper()
	e := newEnv(t, "a", "b")
	state, _ := filepath.EvalSymlinks(t.TempDir())
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "reviewers", Members: members})
	reg := rooms.Open(filepath.Join(state, "rooms.json"))
	if _, err := reg.Add(e.room); err != nil {
		t.Fatal(err)
	}
	d := e.dispatcher()
	d.Machine, d.StateDir, d.Registry = "box", state, reg
	return e, d, state
}

func committeeMessages(s thread.Snapshot) []thread.Message {
	var out []thread.Message
	for _, m := range s.Messages {
		if m.Role == thread.RoleCommittee {
			out = append(out, m)
		}
	}
	return out
}

func TestOwnerMentionPlansACommitteeMessageWithoutReserving(t *testing.T) {
	e, d, state := committeeEnv(t, "x@box", "y@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	msg, err := d.Post(context.Background(), th.ID, "@reviewers please check", "")
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := th.Snapshot()
	cms := committeeMessages(snap)
	if len(cms) != 1 {
		t.Fatalf("committee messages: %+v", snap.Messages)
	}
	cm := cms[0]
	if cm.Author != "reviewers" || cm.ReplyTo != msg.ID || cm.State != thread.StatePending || cm.Review == "" || cm.Committee == nil || len(cm.Committee.Members) != 2 {
		t.Fatalf("committee message: %+v", cm)
	}
	if c := snap.Chains[msg.Chain]; c != nil && c.Used != 0 {
		t.Fatalf("owner mention reserved budget: %d", c.Used)
	}
	// The snapshot is frozen: editing the committee later does not change it.
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "reviewers", Members: []string{"z@box"}})
	snap, _ = th.Snapshot()
	if got := committeeMessages(snap)[0].Committee.Members; len(got) != 2 {
		t.Fatalf("snapshot changed: %v", got)
	}
}

func TestAgentHandoffReservesPerMemberOrSuggests(t *testing.T) {
	e, d, _ := committeeEnv(t, "x@box", "y@box")
	e.script("a", "reply", "done. @reviewers please review")
	th, _ := thread.Create(e.room, "t", "a", "", 3)
	d.Post(context.Background(), th.ID, "go", "")
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return len(committeeMessages(s)) == 1 })
	chain := committeeMessages(snap)[0].Chain
	if used := snap.Chains[chain].Used; used != 2 {
		t.Fatalf("used = %d, want one per member", used)
	}

	// A chain that cannot afford the committee gets one suggestion instead.
	th2, _ := thread.Create(e.room, "t2", "a", "", 1)
	d.Post(context.Background(), th2.ID, "go", "")
	snap2 := e.settle(d, th2, func(s thread.Snapshot) bool {
		for _, m := range s.Messages {
			if m.State == thread.StateSuggested {
				return true
			}
		}
		return false
	})
	if len(committeeMessages(snap2)) != 0 {
		t.Fatal("planned a committee the chain could not afford")
	}
	found := false
	for _, m := range snap2.Messages {
		if m.State == thread.StateSuggested && strings.Contains(m.Text, "@reviewers") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no committee suggestion: %+v", snap2.Messages)
	}
}
```

`e.settle` reconciles until the condition holds. The committee message is then `pending` or later: building is Task 4, and without it the message just stays `pending`, which is all these assertions need. The agent `a` runs through the fixture binary.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/thread -run 'TestOwnerMention|TestAgentHandoffReserves'`
Expected: build failure (`d.Machine undefined`).

- [ ] **Step 3: Implement.** In `resolve.go`: add `Committee *committee.Committee` to `Target` and `Committees map[string]committee.Committee` to `Resolver`. In `Resolve`, add after the `case r.available(name):` branch:

```go
		case r.Committees[name].Name != "":
			c := r.Committees[name]
			add(Target{Mention: name, Listener: "committee:" + name, Committee: &c})
```

In `dispatcher.go`, add the fields to `Dispatcher`:

```go
	// Committees (phase 2b-4): set by tincan web; empty disables @committee.
	Machine        string
	StateDir       string
	CommitteesFrom string
	Registry       *rooms.Registry
```

In `Resolver()`, after the presets load:

```go
	var committees map[string]committee.Committee
	if d.StateDir != "" && d.Machine != "" {
		list, err := committee.List(d.StateDir, d.CommitteesFrom != "")
		if err != nil {
			return Resolver{}, err
		}
		committees = map[string]committee.Committee{}
		for _, c := range list {
			committees[c.Name] = c
		}
	}
```

and set `Committees: committees` in the returned `Resolver`.

In `Post`'s target loop and in `handoffs`' target loop, replace the `planTurn` call with a dispatch on the target kind. In `Post`:

```go
		for _, tg := range targets {
			var err error
			if tg.Committee != nil {
				_, err = d.planCommittee(t, tx, tg, out, chain, false)
			} else {
				_, err = d.planTurn(t, tx, tg, out, chain, false)
			}
			if err != nil {
				return err
			}
		}
```

In `handoffs`, use the same, with `auto = true`: `id, err := d.planCommittee(t, tx, tg, cur, cur.Chain, true)` or `planTurn`, keeping the `produced` append.

Create `internal/thread/committee.go`:

```go
package thread

import (
	"fmt"

	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

// planCommittee records one committee review in the post or handoffs
// transaction (committees §6.10): the committee message, its intent with
// the frozen snapshot and derived review ID, keyed reservations for an
// agent handoff (or one suggestion when the chain cannot afford them), and
// pending.
func (d *Dispatcher) planCommittee(t *Thread, tx *Tx, tg Target, trigger Message, chain string, auto bool) (string, error) {
	c := *tg.Committee
	if auto {
		ch := tx.Snap.Chains[chain]
		if ch != nil && ch.Stopped {
			return "", nil
		}
		used := 0
		if ch != nil {
			used = ch.Used
		}
		if used+len(c.Members) > tx.Meta.Budget {
			if !chainHasSuggestion(tx.Snap, chain) {
				d.system(tx, fmt.Sprintf("Chain budget of %d cannot cover committee %s (%d members); sending it is up to you.", tx.Meta.Budget, c.Name, len(c.Members)), chain)
			}
			ev := tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, ReplyTo: trigger.ID, Chain: chain,
				Text: fmt.Sprintf("@%s please review the handoff from %s above.", c.Name, trigger.Author)})
			tx.Append(Event{Kind: KindState, Message: ev.ID, State: StateSuggested})
			return ev.ID, nil
		}
	}
	room, err := rooms.Canonical(d.Opts.Room)
	if err != nil {
		return "", err
	}
	msg := tx.Append(Event{Kind: KindMessage, Author: c.Name, Role: RoleCommittee, ReplyTo: trigger.ID, Chain: chain})
	rid := tx.Meta.ID + "-" + msg.ID
	reviewID := review.NewID(d.Machine, room, rid)
	tx.Append(Event{Kind: KindIntent, Message: msg.ID, RequestID: rid, Review: reviewID, Committee: &c})
	if auto {
		for i := range c.Members {
			tx.Append(Event{Kind: KindChain, Chain: chain, Op: OpReserve, Key: fmt.Sprintf("review:%s:%d", reviewID, i)})
		}
	}
	tx.Append(Event{Kind: KindState, Message: msg.ID, State: StatePending})
	return msg.ID, nil
}
```

`review` imports `committee`, `packet`, `rooms` and `reviewjob`; none of them import `thread`. Check this with `go list -deps ./internal/review | grep internal/thread`, which must print nothing.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/thread`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/thread
git commit -m "Resolve @committee in threads and plan committee messages with keyed budget"
```

---

### Task 4: Build the review (the submit pattern)

**Files:**
- Modify: `internal/thread/committee.go` (`buildReview`, `reviewQuestion`), `internal/thread/dispatcher.go` (`reconcileThread` calls it)
- Test: `internal/thread/committee_test.go`

**Interfaces:**
- Produces `func (d *Dispatcher) buildReview(ctx context.Context, t *Thread, m Message) error` and `func reviewQuestion(s *Snapshot, trigger Message) string`.
- Constant: `maxReviewQuestion = 64 << 10`.

- [ ] **Step 1: Write the failing tests.** Add to `internal/thread/committee_test.go`:

```go
func TestBuildPublishesFromTheSnapshotAndRuns(t *testing.T) {
	e, d, state := committeeEnv(t, "x@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d.Post(context.Background(), th.ID, "earlier context", "")
	e.settle(d, th, quiescent)
	d.Post(context.Background(), th.ID, "@reviewers is it safe?", "")
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "reviewers", Members: []string{"x@box", "y@box", "z@box"}})
	snap := e.settle(d, th, func(s thread.Snapshot) bool {
		cms := committeeMessages(s)
		return len(cms) == 1 && cms[0].State == thread.StateRunning
	})
	cm := committeeMessages(snap)[0]
	in, err := review.ReadInput(e.room, cm.Review)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Committee.Members) != 1 || in.Origin != "thread:"+th.ID+":"+cm.ID || !strings.Contains(in.Question, "is it safe?") || !strings.Contains(in.Question, "earlier context") {
		t.Fatalf("published input: %+v", in)
	}
	// A second pass adopts the existing review instead of rebuilding.
	d.Reconcile(context.Background())
	if ids, _ := review.List(e.room); len(ids) != 1 {
		t.Fatalf("reviews: %v", ids)
	}
}

func TestStopBeforeBuildNeverPublishes(t *testing.T) {
	e, d, _ := committeeEnv(t, "x@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d.Post(context.Background(), th.ID, "@reviewers check", "")
	d.RequestStop(context.Background(), th.ID)
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusOpen })
	if cm := committeeMessages(snap)[0]; cm.State != thread.StateCancelled {
		t.Fatalf("committee message after stop: %+v", cm)
	}
	if ids, _ := review.List(e.room); len(ids) != 0 {
		t.Fatalf("a stopped committee was published: %v", ids)
	}
}

func TestBuildErrorMarksTheMessage(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs an unwritable directory")
	}
	e, d, _ := committeeEnv(t, "x@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d.Post(context.Background(), th.ID, "@reviewers check", "")
	// A registry that cannot be written makes publication fail.
	locked := t.TempDir()
	os.Chmod(locked, 0o500)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	d.Registry = rooms.Open(filepath.Join(locked, "sub", "rooms.json"))
	snap := e.settle(d, th, func(s thread.Snapshot) bool {
		cms := committeeMessages(s)
		return len(cms) == 1 && cms[0].State == thread.StateError
	})
	if cm := committeeMessages(snap)[0]; !strings.Contains(cm.Text, "review") {
		t.Fatalf("error text: %q", cm.Text)
	}
}
```

Add imports `os`, `runtime` and `internal/review`.
- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/thread -run 'TestBuild|TestStopBeforeBuild'`
Expected: FAIL. Committee messages stay `pending`, and Stop leaves them `pending` too; Task 6 makes Stop cancel them. `TestStopBeforeBuildNeverPublishes` therefore passes only after Task 6. Mark it `t.Skip("Task 6")` for this task, and remove the skip in Task 6.

- [ ] **Step 3: Implement.** Append to `internal/thread/committee.go` (imports `context`, `errors`, `strings`, `internal/review`):

```go
const maxReviewQuestion = 64 << 10

// reviewQuestion is the mentioning message plus the transcript before it,
// oldest lines dropped first, within 64 KiB (committees §6.10).
func reviewQuestion(s *Snapshot, trigger Message) string {
	ask := "Request from " + trigger.Author + ":\n" + trigger.Text + "\n"
	if len(ask) > maxReviewQuestion {
		return cutUTF8(ask, maxReviewQuestion)
	}
	var lines []string
	for _, m := range transcriptOf(s, trigger.ID) {
		if m.N < trigger.N {
			lines = append(lines, line(m))
		}
	}
	budget := maxReviewQuestion - len(ask) - 64
	start, size := len(lines), 0
	for start > 0 && size+len(lines[start-1]) <= budget {
		start--
		size += len(lines[start])
	}
	if start == len(lines) {
		return ask
	}
	return "Thread so far:\n" + strings.Join(lines[start:], "") + "\n" + ask
}

// buildReview publishes a pending committee message's review (phase 1's
// submit pattern): check under the thread lock, publish outside it, then
// mark it running — or, if a Stop arrived meanwhile, cancel what was just
// published. An existing review for this message is adopted.
func (d *Dispatcher) buildReview(ctx context.Context, t *Thread, m Message) error {
	var snap *Snapshot
	ok := false
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		c := tx.Snap.Chains[cur.Chain]
		ok = tx.Meta.Status == StatusOpen && cur.State == StatePending && (c == nil || !c.Stopped)
		snap = tx.Snap
		return nil
	}); err != nil || !ok {
		return err
	}
	room, err := rooms.Canonical(d.Opts.Room)
	if err != nil {
		return err
	}
	origin := "thread:" + t.ID + ":" + m.ID
	var buildErr error
	if in, err := review.ReadInput(room, m.Review); err == nil {
		if in.Origin != origin {
			buildErr = errors.New("review " + m.Review + " belongs to " + in.Origin)
		}
	} else {
		trigger, _ := snap.Message(m.ReplyTo)
		from := d.CommitteesFrom
		in, _, perr := review.Publish(ctx, review.PublishRequest{StateDir: d.StateDir, Room: room, Committee: m.Committee.Name,
			Snapshot: m.Committee, Question: reviewQuestion(snap, trigger), RequestID: m.RequestID, Origin: origin,
			Machine: d.Machine, CommitteesFrom: &from, InCoordinator: true, Registry: d.Registry})
		switch {
		case perr != nil:
			buildErr = perr
		case in.ReviewID != m.Review:
			buildErr = errors.New("review id mismatch: machine or room changed since the mention")
		}
	}
	cancelIt := false
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		if cur.State != StatePending {
			cancelIt = buildErr == nil
			return nil
		}
		if buildErr != nil {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StateError, Text: "ERROR review: " + buildErr.Error()})
			return nil
		}
		tx.Append(Event{Kind: KindState, Message: m.ID, State: StateRunning})
		return nil
	}); err != nil {
		return err
	}
	if cancelIt {
		_, err := review.RequestCancel(ctx, room, m.Review)
		return err
	}
	return nil
}
```

In `reconcileThread`'s switch, add before the agent cases:

```go
		case m.Role == RoleCommittee && m.State == StatePending:
			err = d.buildReview(ctx, t, m)
```

The existing loop skips non-agent roles with `if m.Role != RoleAgent { continue }`. Change it to also let committee messages through: `if m.Role != RoleAgent && m.Role != RoleCommittee { continue }`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/thread`
Expected: PASS (with the Task 6 skip in place).

- [ ] **Step 5: Commit**

```bash
git add internal/thread
git commit -m "Build thread committee reviews with the submit pattern"
```

---

### Task 5: The thread sink — results, completion and synthesis

**Files:**
- Modify: `internal/thread/committee.go` (`PostResult`, `Complete`)
- Test: `internal/thread/committee_test.go`

**Interfaces:**
- `*Dispatcher` implements `review.ThreadSink`:
  - `PostResult(ctx context.Context, in review.Input, m review.Member, text string) error`
  - `Complete(ctx context.Context, in review.Input, st review.State) (bool, error)`
- Compile-time check: `var _ review.ThreadSink = (*Dispatcher)(nil)`.

- [ ] **Step 1: Write the failing tests.** Add to `internal/thread/committee_test.go`:

```go
// running builds a committee message to running and returns it with its input.
func running(t *testing.T, e *env, d *thread.Dispatcher, th *thread.Thread, text string) (thread.Message, review.Input) {
	t.Helper()
	d.Post(context.Background(), th.ID, text, "")
	snap := e.settle(d, th, func(s thread.Snapshot) bool {
		cms := committeeMessages(s)
		return len(cms) == 1 && cms[0].State == thread.StateRunning
	})
	cm := committeeMessages(snap)[0]
	in, err := review.ReadInput(e.room, cm.Review)
	if err != nil {
		t.Fatal(err)
	}
	return cm, in
}

func TestResultsPostOnceAndCompletionIsOnce(t *testing.T) {
	e, d, _ := committeeEnv(t, "x@box", "y@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	cm, in := running(t, e, d, th, "@reviewers check")
	ctx := context.Background()
	st, _ := review.ReadState(e.room, in.ReviewID)
	for i := 0; i < 2; i++ {
		if err := d.PostResult(ctx, in, st.Members[0], "x says ok"); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := th.Snapshot()
	n := 0
	for _, m := range snap.Messages {
		if m.Role == thread.RoleReview {
			n++
			if m.Author != "reviewers/x@box" || m.ReplyTo != cm.ID || m.Text != "x says ok" || m.State != thread.StateDone {
				t.Fatalf("review message: %+v", m)
			}
		}
	}
	if n != 1 {
		t.Fatalf("posted %d times", n)
	}
	st.Status = "closed"
	st.Members[0].State = "done"
	st.Members[1].State = "error"
	done, err := d.Complete(ctx, in, st)
	if err != nil || !done {
		t.Fatalf("complete: %v %v", done, err)
	}
	snap, _ = th.Snapshot()
	got, _ := snap.Message(cm.ID)
	if got.State != thread.StateDone || !strings.Contains(got.Text, "bundle.md") {
		t.Fatalf("committee message: %+v", got)
	}
	if done, _ := d.Complete(ctx, in, st); !done {
		t.Fatal("second completion not reported done")
	}
	if len(agentMessages(snap, "")) != 0 {
		t.Fatal("an owner mention planned a synthesis turn")
	}
}

func TestAgentMentionGetsOneSynthesisTurn(t *testing.T) {
	e, d, _ := committeeEnv(t, "x@box")
	e.script("a", "reply", "done. @reviewers please review")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d.Post(context.Background(), th.ID, "go", "")
	snap := e.settle(d, th, func(s thread.Snapshot) bool {
		cms := committeeMessages(s)
		return len(cms) == 1 && cms[0].State == thread.StateRunning
	})
	cm := committeeMessages(snap)[0]
	in, _ := review.ReadInput(e.room, cm.Review)
	st, _ := review.ReadState(e.room, in.ReviewID)
	st.Status, st.Members[0].State = "closed", "done"
	ctx := context.Background()
	d.PostResult(ctx, in, st.Members[0], "LGTM from x")
	d.Complete(ctx, in, st)
	d.Complete(ctx, in, st)
	e.script("a", "reply", "synthesized")
	snap = e.settle(d, th, quiescent)
	var synth []thread.Message
	for _, m := range agentMessages(snap, "") {
		if m.ReplyTo == cm.ID {
			synth = append(synth, m)
		}
	}
	if len(synth) != 1 || synth[0].Listener != agentMessages(snap, "")[0].Listener {
		t.Fatalf("synthesis turns: %+v", synth)
	}
	if last := readLast(e, "a"); !strings.Contains(last, "LGTM from x") {
		t.Fatalf("synthesis prompt lacks the member review:\n%s", last)
	}
	// A late result after completion is posted, with no second synthesis.
	st.Members = append(st.Members, review.Member{Member: "late@box", Index: 1})
	d.PostResult(ctx, in, st.Members[1], "late words")
	snap = e.settle(d, th, quiescent)
	count := 0
	for _, m := range agentMessages(snap, "") {
		if m.ReplyTo == cm.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("late result caused another synthesis: %d", count)
	}
}

func TestResultsAreNotPostedToAnArchivedThread(t *testing.T) {
	e, d, _ := committeeEnv(t, "x@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	_, in := running(t, e, d, th, "@reviewers check")
	d.SetArchived(context.Background(), th.ID, true)
	e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusArchived })
	st, _ := review.ReadState(e.room, in.ReviewID)
	if err := d.PostResult(context.Background(), in, st.Members[0], "too late"); err != nil {
		t.Fatal(err)
	}
	snap, _ := th.Snapshot()
	for _, m := range snap.Messages {
		if m.Role == thread.RoleReview {
			t.Fatal("posted into an archived thread")
		}
	}
	st.Status = "cancelled"
	if done, err := d.Complete(context.Background(), in, st); err != nil || !done {
		t.Fatalf("archived thread completion: %v %v", done, err)
	}
}

func readLast(e *env, name string) string {
	b, _ := os.ReadFile(filepath.Join(e.fixture, name+".last"))
	return string(b)
}
```

`TestResultsAreNotPostedToAnArchivedThread` depends on Task 6: archiving must cancel the committee message, so that `Complete` sees it terminal and reports done. Mark it `t.Skip("Task 6")` in this task, and remove the skip in Task 6.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/thread -run 'TestResultsPost|TestAgentMentionGets'`
Expected: build failure (`d.PostResult undefined`).

- [ ] **Step 3: Implement.** Append to `internal/thread/committee.go`:

```go
var _ review.ThreadSink = (*Dispatcher)(nil)

// PostResult appends a member's review to the thread once, while the thread
// is open; otherwise it is skipped (and counts as posted).
func (d *Dispatcher) PostResult(ctx context.Context, in review.Input, m review.Member, text string) error {
	tid, mid, ok := review.ThreadOrigin(in.Origin)
	if !ok {
		return nil
	}
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return nil // the thread is gone: nothing to post to
	}
	ref := fmt.Sprintf("%s/%d", in.ReviewID, m.Index)
	return t.Update(ctx, func(tx *Tx) error {
		if tx.Meta.Status != StatusOpen {
			return nil
		}
		for _, msg := range tx.Snap.Messages {
			if msg.Role == RoleReview && msg.Review == ref {
				return nil
			}
		}
		cm, ok := tx.Snap.Message(mid)
		if !ok {
			return nil
		}
		ev := tx.Append(Event{Kind: KindMessage, Author: in.Committee.Name + "/" + m.Member, Role: RoleReview, Text: text, ReplyTo: mid, Chain: cm.Chain, Review: ref})
		tx.Append(Event{Kind: KindState, Message: ev.ID, State: StateDone, Text: text})
		return nil
	})
}

// Complete moves the committee message to its final state once the review
// closed or was cancelled, and — when an agent asked — plans that agent's
// synthesis turn in the same transaction. It reports done once the message
// is final (including when Stop or archive finalized it).
func (d *Dispatcher) Complete(ctx context.Context, in review.Input, st review.State) (bool, error) {
	tid, mid, ok := review.ThreadOrigin(in.Origin)
	if !ok {
		return true, nil
	}
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return true, nil
	}
	done := false
	err = t.Update(ctx, func(tx *Tx) error {
		cm, ok := tx.Snap.Message(mid)
		if !ok || (cm.State != StatePending && cm.State != StateRunning) {
			done = true
			return nil
		}
		if tx.Meta.Status != StatusOpen {
			return nil // finishStop finalizes it
		}
		state, text := committeeOutcome(in, st)
		tx.Append(Event{Kind: KindState, Message: mid, State: state, Text: text})
		done = true
		if state != StateDone {
			return nil
		}
		trigger, ok := tx.Snap.Message(cm.ReplyTo)
		if !ok || trigger.Role != RoleAgent || trigger.Listener == "" {
			return nil
		}
		final, _ := tx.Snap.Message(mid)
		_, owned := tx.Meta.Listeners[trigger.Listener]
		_, err := d.planTurn(t, tx, Target{Mention: trigger.Listener, Listener: trigger.Listener, Preset: trigger.Preset, Existing: !owned}, final, cm.Chain, true)
		return err
	})
	return done, err
}

// committeeOutcome summarizes a finished review for its committee message.
func committeeOutcome(in review.Input, st review.State) (string, string) {
	var b strings.Builder
	finished := 0
	fmt.Fprintf(&b, "Committee %s (v%d) ", in.Committee.Name, in.Committee.Version)
	if st.Status == "cancelled" {
		b.WriteString("review was cancelled.\n")
	} else {
		b.WriteString("review closed.\n")
	}
	for _, m := range st.Members {
		fmt.Fprintf(&b, "- %s: %s", m.Member, m.State)
		if m.Late {
			b.WriteString(" (late)")
		}
		if m.Note != "" {
			b.WriteString(" — " + m.Note)
		}
		b.WriteString("\n")
		if m.State == "done" {
			finished++
		}
	}
	fmt.Fprintf(&b, "\nBundle: .tincan/reviews/%s/bundle.md\n", in.ReviewID)
	switch {
	case st.Status == "cancelled":
		return StateCancelled, b.String()
	case finished == 0:
		return StateError, b.String()
	}
	return StateDone, b.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/thread`
Expected: PASS (with the Task 6 skips in place).

- [ ] **Step 5: Commit**

```bash
git add internal/thread
git commit -m "Post member reviews into threads and plan one synthesis turn"
```

---

### Task 6: Stop and archive cancel committee reviews

**Files:**
- Modify: `internal/thread/control.go` (`finishStop` committee branch)
- Test: `internal/thread/committee_test.go` (remove the Task 6 skips; add a stop-while-running test)

**Interfaces:**
- No new API. `finishStop` requests cancellation of every unsettled review referenced by the thread's committee messages before its settle transaction, and marks `pending` and `running` committee messages `cancelled` inside it.

- [ ] **Step 1: Write the failing test.** Remove the `t.Skip("Task 6")` lines from `TestStopBeforeBuildNeverPublishes` and `TestResultsAreNotPostedToAnArchivedThread`, then add:

```go
func TestStopWhileRunningCancelsTheReview(t *testing.T) {
	e, d, _ := committeeEnv(t, "x@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	cm, in := running(t, e, d, th, "@reviewers check")
	d.RequestStop(context.Background(), th.ID)
	snap := e.settle(d, th, func(s thread.Snapshot) bool { return s.Meta.Status == thread.StatusOpen })
	if got, _ := snap.Message(cm.ID); got.State != thread.StateCancelled {
		t.Fatalf("committee message after stop: %+v", got)
	}
	st, _ := review.ReadState(e.room, in.ReviewID)
	if !st.CancelRequested {
		t.Fatalf("review not cancelled: %+v", st)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/thread -run 'TestStop|TestResultsAreNotPosted'`
Expected: FAIL. Committee messages stay `pending`/`running` after Stop, and archive never completes the barrier for them.

- [ ] **Step 3: Implement.** In `finishStop`, at the start (after `room := d.Opts.Room`):

```go
	// Committee reviews started from this thread are cancelled first,
	// outside any thread transaction (review → thread is the only lock
	// nesting); cancelling does not wait for peers (committees §6.10).
	if canonical, err := rooms.Canonical(room); err == nil {
		for _, m := range snap.Messages {
			if m.Role != RoleCommittee || m.Review == "" {
				continue
			}
			if st, err := review.ReadState(canonical, m.Review); err == nil && !st.Settled && !st.CancelRequested {
				if _, err := review.RequestCancel(ctx, canonical, m.Review); err != nil {
					return err
				}
			}
		}
	}
```

In its settle transaction (the `t.Update` that appends the settled results), add:

```go
		for _, m := range tx.Snap.Messages {
			if m.Role == RoleCommittee && (m.State == StatePending || m.State == StateRunning) {
				tx.Append(Event{Kind: KindState, Message: m.ID, State: StateCancelled, Text: "Cancelled: the thread was stopped."})
			}
		}
```

Add imports `internal/review` and `internal/rooms` to `control.go`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/thread`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/thread
git commit -m "Cancel committee reviews when a thread is stopped or archived"
```

---

### Task 7: Wire threads to the coordinator in `tincan web`

**Files:**
- Modify: `internal/web/events.go` (`dispatcher()` sets the committee fields; `runRoomPass` passes `d` as the sink)
- Test: `internal/web/reviews_test.go`

**Interfaces:**
- Consumes: the `thread.Dispatcher` fields and the `review.ThreadSink` implementation.
- Produces: `s.dispatcher(room)` sets `d.Machine`, `d.StateDir`, `d.CommitteesFrom` and `d.Registry` from `s.cfg`, and `runRoomPass` calls `s.coord.Reconcile(ctx, room.Path, d)`.

- [ ] **Step 1: Write the failing test.** Add to `internal/web/reviews_test.go`:

```go
func TestCommitteeMentionEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts")
	}
	s, rid := apiServer(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TINCAN_STATE_DIR", state)
	s.cfg.StateDir = state
	script := filepath.Join(t.TempDir(), "reviewer")
	os.WriteFile(script, []byte("#!/bin/sh\necho LGTM from the committee\n"), 0o755)
	s.cfg.Dispatch.Presets["reviewer"] = host.Preset{Exec: []string{script, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}
	s.cfg.Dispatch.Executable = buildTincan(t)
	s.initJobs()
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"reviewer@box"}})
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"t","primary":"claude"}`, mut())
	var th struct {
		ID string `json:"id"`
	}
	decode(t, rec.Body.String(), &th)
	do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+th.ID+"/messages", `{"text":"@solo please check"}`, mut())
	room, _, _ := s.cfg.Registry.Get(rid)
	t.Cleanup(func() {
		s.jobs.Wait()
		ws, _ := os.ReadDir(filepath.Join(state, "reviews", "ws"))
		for _, j := range ws {
			host.Down(context.Background(), filepath.Join(state, "reviews", "ws", j.Name()), "reviewer", 5*time.Second)
		}
	})
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		s.runRoomPass(context.Background(), room, false)
		tt, _ := thread.Open(room.Path, th.ID)
		snap, _ := tt.Snapshot()
		var cm, rv *thread.Message
		for i := range snap.Messages {
			switch snap.Messages[i].Role {
			case thread.RoleCommittee:
				cm = &snap.Messages[i]
			case thread.RoleReview:
				rv = &snap.Messages[i]
			}
		}
		if cm != nil && cm.State == thread.StateDone && rv != nil {
			if !strings.Contains(rv.Text, "LGTM from the committee") {
				t.Fatalf("review message: %+v", rv)
			}
			st, _ := review.ReadState(room.Path, cm.Review)
			if !st.ThreadDone {
				t.Fatalf("thread_done not set: %+v", st)
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("committee mention did not complete")
}
```

Add imports `internal/thread` as needed. `apiServer`'s machine is `box`, and `s.cfg.Dispatch.Presets` is the map shared with the dispatcher.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/web -run TestCommitteeMentionEndToEnd`
Expected: FAIL (the committee is unresolved and never built: the dispatcher has no machine or state directory).

- [ ] **Step 3: Implement.** In `events.go` `dispatcher(room)`, after `d := thread.New(s.opts(room))`:

```go
	d.Machine, d.StateDir, d.CommitteesFrom, d.Registry = s.cfg.Machine, s.cfg.StateDir, s.cfg.CommitteesFrom, s.cfg.Registry
```

In `runRoomPass`, change `s.coord.Reconcile(ctx, room.Path, nil)` to `s.coord.Reconcile(ctx, room.Path, d)`.

`postMessage` resolves through the room's dispatcher (`s.dispatcherFor(room)`). Check that `dispatcherFor` goes through `dispatcher()`, or sets the same fields. If it constructs a fresh `thread.New` for non-owned rooms, set the fields there too, so a post resolves committees on any web process.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web ./internal/thread ./internal/review`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web
git commit -m "Connect thread committee reviews to the coordinator in tincan web"
```

---

### Task 8: Committee and review messages in the UI; docs

**Files:**
- Modify: `internal/web/ui/app.js` (`renderMessage`), `internal/web/ui/app.css`, `internal/web/ui_test.go`, `docs/web.md`

**Interfaces:**
- Consumes: the message fields `role`, `review`, `committee`, `state`, `text`, `html`.
- Produces: rendering of `role === "committee"` (an in-progress card listing members, or the summary, plus an "Open review" button calling `showReview(key, rid, m.review)`) and `role === "review"` (a normal body with a "review" badge). Retry is not offered for committee messages.

- [ ] **Step 1: Write the failing test.** Add to `TestUIContract`'s second list:

```go
		// Committee messages in threads (2b-4).
		`m.role === "committee"`,
		"Open review",
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/web -run TestUIContract`
Expected: FAIL.

- [ ] **Step 3: Implement.** In `renderMessage`, right after the `who` line is appended:

```js
  if (m.role === "committee") {
    const cur = state.current;
    const members = (m.committee && m.committee.members) || [];
    if (m.state === "pending" || m.state === "running") {
      box.append(el("div", "working", `committee ${m.author} reviewing… (${members.join(", ")})`));
    } else {
      box.append(m.text ? renderBody(m) : el("div", "body muted", m.state));
    }
    if (m.review && cur) {
      const open = el("button", "secondary", "Open review");
      open.onclick = () => showReview(cur.key, cur.rid, m.review);
      box.append(el("div", "tools")).lastChild.append(open);
    }
    return box;
  }
```

For review messages, add a `review` class so CSS can badge them. The existing path renders the body, and `box` already carries `msg review` from `` `msg ${m.role}` ``. Make sure the error/cancelled tools block (Retry) runs only for `m.role === "agent"`: wrap it in `if (m.role === "agent" && (m.state === "error" || m.state === "cancelled"))`.

`showReview`'s Back button calls `showActivity`. From a thread, Back should return to the thread. Record where the review was opened from: `state.reviewOpen.from = "thread"` when opened from a message. The Back button then calls `openCurrent()` in that case, and `showActivity()` otherwise.

In `app.css`, add `.msg.committee` (accent left border), `.msg.review .who::after { content: " · review"; color: var(--muted); }`.

In `docs/web.md` "Reviews", add a subsection "In threads":
- write `@reviewers` in a thread to ask that committee;
- each member's review appears in the thread as it arrives;
- when an agent mentioned the committee, the agent gets a follow-up turn with the reviews to synthesize them;
- an agent's `@committee` costs one chain execution per member, plus the synthesis turn; a chain that cannot afford it gets a "Send" suggestion instead;
- Stop and Archive cancel the thread's reviews;
- the committee used is the definition at the moment of the mention.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web && node --check internal/web/ui/app.js`
Expected: PASS.

- [ ] **Step 5: Run the whole suite, vet and format**

Run: `gofmt -l . ; go vet ./... && GOOS=windows go vet ./... && go test ./...`
Expected: no gofmt output; every package passes.

- [ ] **Step 6: Commit**

```bash
git add internal/web/ui internal/web/ui_test.go docs/web.md
git commit -m "Show committee reviews in threads"
```
