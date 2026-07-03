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
