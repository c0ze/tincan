package reviewjob

import (
	"path/filepath"
	"testing"
	"time"
)

func stateDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStoreRoundTripAndIdentity(t *testing.T) {
	s := Store{Dir: filepath.Join(stateDir(t), "reviews")}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	j := Job{ID: "rv-0123456789abcdef-0", ReviewID: "rv-0123456789abcdef", Preset: "codex", PacketSHA: "p", PromptSHA: "q", ExpiresAt: exp, State: "creating"}
	if err := s.Save(j); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(j.ID)
	if err != nil || !ok || !got.SameIdentity(j) || got.State != "creating" {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
	other := j
	other.PromptSHA = "different"
	if got.SameIdentity(other) {
		t.Fatal("different prompt counted as the same identity")
	}
	if _, ok, _ := s.Get("rv-missing-0"); ok {
		t.Fatal("found a missing job")
	}
	if _, _, err := s.Get("../escape"); err == nil {
		t.Fatal("invalid job id accepted")
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	l, err := s.TryLock(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TryLock(j.ID); err == nil {
		t.Fatal("second TryLock succeeded")
	}
	l.Close()
}
