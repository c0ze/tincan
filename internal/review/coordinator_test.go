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

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestHappyPathSubmitsCollectsClosesAndSettles(t *testing.T) {
	room, in := published(t, []string{"codex@macmini", "grok@cachyos"}, 30*time.Minute, false)
	f := newFake()
	clk := &clock{time.Now()}
	c := &Coordinator{Transport: f, Now: clk.now}
	ctx := context.Background()
	c.Reconcile(ctx, room, nil)
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[0].State != "running" || st.Members[1].State != "running" || f.creates[st.Members[0].JobID] != 1 {
		t.Fatalf("after submit: %+v", st.Members)
	}
	f.finish(st.Members[0].JobID, "codex says ok")
	f.finish(st.Members[1].JobID, "grok says ok")
	clk.advance(6 * time.Second)
	c.Reconcile(ctx, room, nil)
	st, _ = ReadState(room, in.ReviewID)
	if st.Status != "closed" || st.Closure == nil {
		t.Fatalf("not closed: %+v", st)
	}
	b, ok, _ := ReadBundle(room, in.ReviewID)
	if !ok || !strings.Contains(b, "codex says ok") || !strings.Contains(b, "grok says ok") {
		t.Fatalf("bundle: %q", b)
	}
	clk.advance(16 * time.Second)
	c.Reconcile(ctx, room, nil)
	st, _ = ReadState(room, in.ReviewID)
	if !st.Settled || f.acks[st.Members[0].JobID] == 0 || f.acks[st.Members[1].JobID] == 0 {
		t.Fatalf("not settled/acked: %+v acks=%v", st, f.acks)
	}
	c.Reconcile(ctx, room, nil) // settled reviews are left alone
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
		c.Reconcile(ctx, room, nil)
		clk.advance(20 * time.Second)
	}
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[1].State != "submitting" {
		t.Fatalf("offline member: %+v", st.Members[1])
	}
	f.finish(st.Members[0].JobID, "ok")
	clk.t = in.Deadline.Add(time.Second)
	c.Reconcile(ctx, room, nil)
	st, _ = ReadState(room, in.ReviewID)
	if st.Members[1].State != "unreachable" || st.Status != "closed" {
		t.Fatalf("at deadline: %+v", st)
	}
	clk.t = st.Members[1].ExpiresAt.Add(6 * time.Minute)
	c.Reconcile(ctx, room, nil)
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
	c.Reconcile(ctx, room, nil)
	// The peer accepted a job whose response was lost, then went quiet.
	f.jobs[in.ReviewID+"-0"] = &JobStatus{State: "running"}
	clk.t = in.Deadline.Add(time.Second)
	c.Reconcile(ctx, room, nil)
	f.down["cachyos"] = false
	f.finish(in.ReviewID+"-0", "late but real")
	clk.advance(6 * time.Second)
	c.Reconcile(ctx, room, nil)
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
	c.Reconcile(context.Background(), room, nil)
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
	c.Reconcile(ctx, room, nil)
	f.finish(in.ReviewID+"-0", "result")
	clk.advance(6 * time.Second)
	c.Reconcile(ctx, room, nil)
	first, _, _ := ReadBundle(room, in.ReviewID)
	st, _ := ReadState(room, in.ReviewID)
	st.Status = "closing" // as if the process died before persisting "closed"
	WriteState(room, in.ReviewID, st)
	os.Remove(filepath.Join(Dir(room, in.ReviewID), "bundle.md"))
	clk.advance(time.Hour)
	c.Reconcile(ctx, room, nil)
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
	c.Reconcile(ctx, room, nil)
	f.finish(in.ReviewID+"-0", "on time")
	clk.t = in.Deadline.Add(time.Second)
	c.Reconcile(ctx, room, nil)
	st, _ := ReadState(room, in.ReviewID)
	if st.Status != "closed" || !st.Members[1].Late {
		t.Fatalf("late member: %+v", st)
	}
	RequestCancel(ctx, room, in.ReviewID)
	clk.advance(16 * time.Second)
	c.Reconcile(ctx, room, nil)
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
	(&Coordinator{Transport: f, Now: time.Now}).Reconcile(context.Background(), room, nil)
	st, _ := ReadState(room, in.ReviewID)
	if st.Members[0].State != "cancelled" || f.creates[in.ReviewID+"-0"] != 0 || !st.Settled {
		t.Fatalf("%+v creates=%v", st, f.creates)
	}
}

func TestSkipExhaustedMembers(t *testing.T) {
	room, in := published(t, []string{"codex@macmini", "grok@cachyos"}, 10*time.Minute, true)
	f := newFake()
	f.exhausted["codex"] = true
	(&Coordinator{Transport: f, Now: time.Now}).Reconcile(context.Background(), room, nil)
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
	(&Coordinator{Transport: newFake(), Now: time.Now}).Reconcile(context.Background(), room, nil)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale staging kept")
	}
}

// slowTransport fails every call to "slow" after a delay, counting calls.
type slowTransport struct {
	*fakeTransport
	mu    sync.Mutex
	calls int
}

func (s *slowTransport) hit(machine string) error {
	if machine != "slow" {
		return nil
	}
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	return errors.New("i/o timeout")
}

func (s *slowTransport) Create(ctx context.Context, machine string, req reviewjob.CreateRequest) (JobStatus, error) {
	if err := s.hit(machine); err != nil {
		return JobStatus{}, err
	}
	return s.fakeTransport.Create(ctx, machine, req)
}

func (s *slowTransport) Status(ctx context.Context, machine, id string) (JobStatus, error) {
	if err := s.hit(machine); err != nil {
		return JobStatus{}, err
	}
	return s.fakeTransport.Status(ctx, machine, id)
}

// One unresponsive machine costs one call per pass, however many members
// it has: a pass (which also runs the room's thread dispatch and holds the
// review lock that cancellation needs) is never stalled by N timeouts.
func TestUnresponsiveMachineCostsOneCallPerPass(t *testing.T) {
	room, _ := published(t, []string{"a@slow", "b@slow", "c@slow", "d@macmini"}, 10*time.Minute, false)
	st := &slowTransport{fakeTransport: newFake()}
	c := &Coordinator{Transport: st, Now: time.Now}
	c.Reconcile(context.Background(), room, nil)
	if st.calls != 1 {
		t.Fatalf("calls to the unresponsive machine in one pass: %d", st.calls)
	}
	if st.creates[mustState(t, room).Members[3].JobID] != 1 {
		t.Fatal("a member on a healthy machine was not submitted")
	}
}

func mustState(t *testing.T, room string) State {
	t.Helper()
	ids, _ := List(room)
	st, err := ReadState(room, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	return st
}

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
