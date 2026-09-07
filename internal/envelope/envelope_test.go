package envelope

import (
	"reflect"
	"strings"
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
		Kind:      "stop",
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

func TestMarshalOmitsKindKeyWhenEmpty(t *testing.T) {
	e := &Envelope{
		ID:   NewID(),
		From: "orch",
		To:   "codex",
		TS:   time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC),
		Body: "review PR 56",
	}
	data, err := Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"kind"`) {
		t.Fatalf("expected no \"kind\" key when Kind is empty, got:\n%s", data)
	}
}

func TestUnmarshalLegacyJSONWithoutKindDefaultsEmpty(t *testing.T) {
	legacy := []byte(`{
		"id": "abc123",
		"from": "orch",
		"to": "codex",
		"ts": "2026-07-03T12:00:00Z",
		"body": "review PR 56"
	}`)
	e, err := Unmarshal(legacy)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if e.Kind != "" {
		t.Fatalf("want Kind == \"\" for legacy JSON, got %q", e.Kind)
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

func TestRejectsUnsafePortableComponents(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../x", `a\b`, "x:y", "x\x00y", "x\ny", "x?", "x*", "x|", "x<", "x>", "x\"", "x.", "x ", "CON", "nul.txt", "com1", "LPT9.txt", strings.Repeat("a", 129), string([]byte{0xff})} {
		if err := ValidComponent(bad); err == nil {
			t.Errorf("accepted unsafe component %q", bad)
		}
	}
	for _, good := range []string{"worker", "r-abc", "legacy_123", "agent.2", "日本語", strings.Repeat("a", 128)} {
		if err := ValidComponent(good); err != nil {
			t.Errorf("rejected safe component %q: %v", good, err)
		}
	}
}

func TestUnmarshalRejectsMissingEnvelopeFields(t *testing.T) {
	for _, data := range []string{"null", "{}", `{"id":"safe","from":"a","ts":"2026-07-03T12:00:00Z"}`, `{"id":"../escape","from":"a","to":"b","ts":"2026-07-03T12:00:00Z"}`} {
		if _, err := Unmarshal([]byte(data)); err == nil {
			t.Errorf("accepted invalid envelope %s", data)
		}
	}
}

func TestValidateTimestampRange(t *testing.T) {
	e := &Envelope{ID: "legacy-id", From: "a", To: "b"}
	for _, ts := range []time.Time{{}, time.Unix(-1, 0), time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		e.TS = ts
		if err := Validate(e); err == nil {
			t.Errorf("accepted timestamp %v", ts)
		}
	}
	for _, ts := range []time.Time{time.Unix(0, 0), time.Unix(0, 1<<63-1)} {
		e.TS = ts
		if err := Validate(e); err != nil {
			t.Errorf("rejected timestamp %v: %v", ts, err)
		}
	}
}
