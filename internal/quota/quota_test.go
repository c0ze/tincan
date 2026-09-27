package quota

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func unix(t time.Time) float64 { return float64(t.Unix()) }

func TestParseSchema2(t *testing.T) {
	data := []byte(`{"percent": 83.5, "reset_at": ` + ftoa(unix(now.Add(50*time.Hour))) + `, "short_percent": 12, "short_reset_at": ` + ftoa(unix(now.Add(2*time.Hour))) +
		`, "fetched_at": ` + ftoa(unix(now.Add(-time.Minute))) + `, "attempted_at": ` + ftoa(unix(now.Add(-time.Minute))) + `, "schema": 2}`)
	e, err := Parse("codex-gmail", data, now)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != StateOK || *e.Percent != 83.5 || *e.ShortPercent != 12 || !e.ResetAt.Equal(now.Add(50*time.Hour)) || e.ShortResetAt == nil {
		t.Fatalf("entry %+v", e)
	}
}

func TestParseLegacyUsesResetSecs(t *testing.T) {
	data := []byte(`{"percent": 40, "reset_secs": 3600, "fetched_at": ` + ftoa(unix(now.Add(-2*time.Minute))) + `}`)
	e, err := Parse("claude", data, now)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != StateOK || !e.ResetAt.Equal(now.Add(58*time.Minute)) || e.AttemptedAt == nil || !e.AttemptedAt.Equal(*e.FetchedAt) || e.ShortResetAt != nil {
		t.Fatalf("entry %+v", e)
	}
}

func TestParseFailureOnlyRecordIsError(t *testing.T) {
	e, err := Parse("mimo", []byte(`{"attempted_at": `+ftoa(unix(now.Add(-time.Minute)))+`, "error": "auth expired", "schema": 2}`), now)
	if err != nil || e.State != StateError || e.Percent != nil || e.Error != "auth expired" {
		t.Fatalf("entry %+v %v", e, err)
	}
}

func TestErrorNewerThanSuccessWins(t *testing.T) {
	data := []byte(`{"percent": 10, "reset_at": ` + ftoa(unix(now.Add(time.Hour))) + `, "fetched_at": ` + ftoa(unix(now.Add(-10*time.Minute))) +
		`, "attempted_at": ` + ftoa(unix(now.Add(-time.Minute))) + `, "error": "429"}`)
	e, _ := Parse("claude", data, now)
	if e.State != StateError || e.Percent == nil {
		t.Fatalf("entry %+v", e)
	}
}

func TestStaleness(t *testing.T) {
	old := []byte(`{"percent": 10, "reset_at": ` + ftoa(unix(now.Add(time.Hour))) + `, "fetched_at": ` + ftoa(unix(now.Add(-16*time.Minute))) + `}`)
	if e, _ := Parse("grok", old, now); e.State != StateStale {
		t.Fatalf("16-minute-old reading: %s", e.State)
	}
	passed := []byte(`{"percent": 100, "reset_at": ` + ftoa(unix(now.Add(-time.Minute))) + `, "fetched_at": ` + ftoa(unix(now.Add(-5*time.Minute))) + `}`)
	if e, _ := Parse("grok", passed, now); e.State != StateStale {
		t.Fatalf("reset passed without a newer success: %s", e.State)
	}
}

func TestTruthTableEdges(t *testing.T) {
	// Equal attempt and success timestamps with an error count as a failure.
	at := ftoa(unix(now.Add(-time.Minute)))
	if e, _ := Parse("c", []byte(`{"percent": 5, "reset_at": `+ftoa(unix(now.Add(time.Hour)))+`, "fetched_at": `+at+`, "attempted_at": `+at+`, "error": "boom"}`), now); e.State != StateError {
		t.Fatalf("equal timestamps with error: %s", e.State)
	}
	// An observation from the future beyond the skew bound is invalid.
	future := ftoa(unix(now.Add(time.Hour)))
	if e, _ := Parse("c", []byte(`{"percent": 5, "fetched_at": `+future+`}`), now); e.FetchedAt != nil || e.State != StateUnknown {
		t.Fatalf("future fetched_at accepted: %+v", e)
	}
	// An ended short window has no current reading.
	data := `{"percent": 5, "reset_at": ` + ftoa(unix(now.Add(time.Hour))) + `, "short_percent": 90, "short_reset_at": ` + ftoa(unix(now.Add(-time.Minute))) + `, "fetched_at": ` + at + `}`
	if e, _ := Parse("c", []byte(data), now); e.ShortPercent != nil || e.State != StateOK {
		t.Fatalf("ended short window kept: %+v", e)
	}
}

func TestInvalidValuesBecomeMissing(t *testing.T) {
	data := []byte(`{"percent": 5000, "reset_at": 12, "fetched_at": ` + ftoa(unix(now.Add(-time.Minute))) + `}`)
	e, err := Parse("x", data, now)
	if err != nil || e.Percent != nil || e.ResetAt != nil || e.State != StateUnknown {
		t.Fatalf("entry %+v %v", e, err)
	}
	if _, err := Parse("x", []byte(`not json`), now); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestLoadDiscoversFilesAndAppliesMapping(t *testing.T) {
	dir := t.TempDir()
	good := []byte(`{"percent": 1, "reset_at": ` + ftoa(unix(now.Add(time.Hour))) + `, "fetched_at": ` + ftoa(unix(now.Add(-time.Minute))) + `}`)
	for _, name := range []string{"claude-quota.json", "codex-quota-default.json", "codex-quota-gmail.json", "weather.json", "notes-quota.txt"} {
		os.WriteFile(filepath.Join(dir, name), good, 0o600)
	}
	os.WriteFile(filepath.Join(dir, "huge-quota.json"), make([]byte, 70<<10), 0o600)
	cfg := filepath.Join(t.TempDir(), "quotas.json")
	os.WriteFile(cfg, []byte(`{"codex-gmail": {"label": "Codex gmail", "presets": ["codex-gmail"]}}`), 0o600)
	entries, err := Load(dir, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range entries {
		got[e.ID] = e
	}
	if len(got) != 3 {
		t.Fatalf("entries %v", got)
	}
	if p := got["codex-default"].Presets; len(p) != 1 || p[0] != "codex" {
		t.Fatalf("codex-default presets %v", p)
	}
	if g := got["codex-gmail"]; g.Label != "Codex gmail" || g.Presets[0] != "codex-gmail" || !g.Explicit || got["claude"].Explicit {
		t.Fatalf("mapped entry %+v", g)
	}
	if p := got["claude"].Presets; p[0] != "claude" {
		t.Fatalf("claude presets %v", p)
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
