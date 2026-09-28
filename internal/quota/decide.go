package quota

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// Decision is a machine-local answer to "is this preset's quota
// exhausted?" (committees spec §8). Anything uncertain is not exhausted.
type Decision struct {
	Exhausted bool       `json:"exhausted"`
	Window    string     `json:"window,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	Reason    string     `json:"reason"`
}

// Decide counts mappings over the whole quotas.json (including entries whose
// cache file is absent) before reading any cache: exactly one explicit
// mapping with a blocking window, a fresh successful reading, and that
// window at 100% with a future reset are all required.
func Decide(cacheDir, configPath, preset string, now time.Time) Decision {
	no := func(reason string) Decision { return Decision{Reason: reason} }
	maps := map[string]mapping{}
	data, err := os.ReadFile(configPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return no("no quota entry maps this preset (no quotas.json)")
	case err != nil:
		return no("quotas.json unreadable: " + err.Error())
	}
	if err := json.Unmarshal(data, &maps); err != nil {
		return no("quotas.json is malformed")
	}
	var id string
	var m mapping
	n := 0
	for eid, em := range maps {
		for _, p := range em.Presets {
			if p == preset {
				id, m = eid, em
				n++
				break
			}
		}
	}
	switch {
	case n == 0:
		return no("no quota entry maps this preset")
	case n > 1:
		return no("several quota entries map this preset")
	case m.Blocking == "":
		return no("the quota entry does not declare blocking")
	}
	files, err := Files(cacheDir)
	if err != nil {
		return no("quota caches unreadable: " + err.Error())
	}
	path, ok := files[id]
	if !ok {
		return no("no cached quota reading for " + id)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return no("quota cache unreadable: " + err.Error())
	}
	e, err := Parse(id, raw, now)
	if err != nil {
		return no("quota cache malformed")
	}
	if e.State != StateOK {
		return no("quota reading is " + e.State)
	}
	check := func(window string, pct *float64, reset *time.Time) (Decision, bool) {
		if pct != nil && *pct >= 100 && reset != nil && reset.After(now) {
			return Decision{Exhausted: true, Window: window, ResetAt: reset, Reason: window + " quota exhausted"}, true
		}
		return Decision{}, false
	}
	if m.Blocking == "weekly" || m.Blocking == "either" {
		if d, ok := check("weekly", e.Percent, e.ResetAt); ok {
			return d
		}
	}
	if m.Blocking == "short" || m.Blocking == "either" {
		if d, ok := check("short", e.ShortPercent, e.ShortResetAt); ok {
			return d
		}
	}
	return no("quota not exhausted")
}
