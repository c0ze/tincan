package thread_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/review"
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
	t.Skip("Task 6")
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

// A running committee message is the coordinator's to finish: the agent
// paths (collect, handoffs) must never touch it.
func TestRunningCommitteeMessageIsLeftToTheCoordinator(t *testing.T) {
	e, d, _ := committeeEnv(t, "x@box")
	th, _ := thread.Create(e.room, "t", "a", "", 6)
	d.Post(context.Background(), th.ID, "@reviewers check", "")
	e.settle(d, th, func(s thread.Snapshot) bool {
		cms := committeeMessages(s)
		return len(cms) == 1 && cms[0].State == thread.StateRunning
	})
	for i := 0; i < 3; i++ {
		d.Reconcile(context.Background())
	}
	snap, _ := th.Snapshot()
	if cm := committeeMessages(snap)[0]; cm.State != thread.StateRunning {
		t.Fatalf("agent paths touched the committee message: %+v", cm)
	}
}

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
	t.Skip("Task 6")
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
