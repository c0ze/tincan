package quota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecideSkipsOnlyWhenEveryConditionHolds(t *testing.T) {
	now := time.Now()
	future := ftoa(unix(now.Add(2 * time.Hour)))
	fetched := ftoa(unix(now.Add(-time.Minute)))
	full := `{"percent": 100, "reset_at": ` + future + `, "short_percent": 20, "short_reset_at": ` + future + `, "fetched_at": ` + fetched + `}`
	shortFull := `{"percent": 40, "reset_at": ` + future + `, "short_percent": 100, "short_reset_at": ` + future + `, "fetched_at": ` + fetched + `}`
	cases := []struct {
		name, config, cache string
		exhausted           bool
		reason              string
	}{
		{"weekly exhausted", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, full, true, ""},
		{"either takes short", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "either"}}`, shortFull, true, ""},
		{"short not blocking weekly", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, shortFull, false, "not exhausted"},
		{"no blocking", `{"codex-gmail": {"presets": ["codex-gmail"]}}`, full, false, "blocking"},
		{"unmapped", `{}`, full, false, "no quota entry"},
		{"ambiguous with missing cache", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}, "other": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, full, false, "several"},
		{"malformed config", `{not json`, full, false, "malformed"},
		{"no cache", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, "", false, "no cached"},
		{"stale", `{"codex-gmail": {"presets": ["codex-gmail"], "blocking": "weekly"}}`, `{"percent": 100, "reset_at": ` + future + `, "fetched_at": ` + ftoa(unix(now.Add(-2*time.Hour))) + `}`, false, "stale"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "quotas.json")
		os.WriteFile(cfg, []byte(c.config), 0o600)
		if c.cache != "" {
			os.WriteFile(filepath.Join(dir, "codex-quota-gmail.json"), []byte(c.cache), 0o600)
		}
		d := Decide(dir, cfg, "codex-gmail", now)
		if d.Exhausted != c.exhausted || (c.reason != "" && !strings.Contains(d.Reason, c.reason)) {
			t.Errorf("%s: %+v", c.name, d)
		}
		if d.Exhausted && (d.ResetAt == nil || d.Window == "") {
			t.Errorf("%s: exhausted without window/reset: %+v", c.name, d)
		}
	}
}
