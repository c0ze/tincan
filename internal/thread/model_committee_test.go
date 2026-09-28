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
