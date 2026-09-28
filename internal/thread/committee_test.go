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
