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
