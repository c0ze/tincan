// Package quota reads the usage caches written by the owner's existing quota
// refreshers. It never runs those refreshers, calls provider APIs or reads
// credentials.
package quota

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	StateOK      = "ok"
	StateStale   = "stale"
	StateError   = "error"
	StateUnknown = "unknown"

	StaleAfter = 15 * time.Minute
	maxFile    = 64 << 10
)

type Entry struct {
	ID           string     `json:"id"`
	Label        string     `json:"label"`
	Presets      []string   `json:"presets"`
	Percent      *float64   `json:"percent"`
	ResetAt      *time.Time `json:"reset_at"`
	ShortPercent *float64   `json:"short_percent"`
	ShortResetAt *time.Time `json:"short_reset_at"`
	FetchedAt    *time.Time `json:"fetched_at"`
	AttemptedAt  *time.Time `json:"attempted_at"`
	Error        string     `json:"error,omitempty"`
	State        string     `json:"state"`
	Explicit     bool       `json:"explicit"`           // mapping came from quotas.json
	Blocking     string     `json:"blocking,omitempty"` // weekly | short | either (used by phase 2)
}

type mapping struct {
	Label    string   `json:"label"`
	Presets  []string `json:"presets"`
	Blocking string   `json:"blocking"`
}

var fileRE = regexp.MustCompile(`^([a-z0-9]+)-quota(?:-([A-Za-z0-9_-]+))?\.json$`)

func DefaultCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache")
}

func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tincan", "quotas.json")
}

var minTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func number(raw map[string]any, key string) (float64, bool) {
	v, ok := raw[key].(float64)
	return v, ok
}

// clockSkew bounds how far in the future an observation (fetched/attempted)
// may be; reset times may lie up to 400 days ahead.
const clockSkew = 5 * time.Minute

func stamp(raw map[string]any, key string, now time.Time, maxAhead time.Duration) *time.Time {
	v, ok := number(raw, key)
	if !ok {
		return nil
	}
	t := time.Unix(0, int64(v*float64(time.Second))).UTC()
	if t.Before(minTime) || t.After(now.Add(maxAhead)) {
		return nil
	}
	return &t
}

func pct(raw map[string]any, key string) *float64 {
	v, ok := number(raw, key)
	if !ok || v < 0 || v > 1000 {
		return nil
	}
	return &v
}

// Parse normalizes one cache file of either known schema.
func Parse(id string, data []byte, now time.Time) (Entry, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return Entry{}, err
	}
	e := Entry{ID: id, Label: id}
	e.FetchedAt = stamp(raw, "fetched_at", now, clockSkew)
	e.AttemptedAt = stamp(raw, "attempted_at", now, clockSkew)
	if e.AttemptedAt == nil {
		e.AttemptedAt = e.FetchedAt
	}
	e.Percent = pct(raw, "percent")
	e.ShortPercent = pct(raw, "short_percent")
	e.ResetAt = stamp(raw, "reset_at", now, 400*24*time.Hour)
	if e.ResetAt == nil && e.FetchedAt != nil {
		if secs, ok := number(raw, "reset_secs"); ok && secs >= 0 && secs < 400*24*3600 {
			t := e.FetchedAt.Add(time.Duration(secs * float64(time.Second)))
			e.ResetAt = &t
		}
	}
	e.ShortResetAt = stamp(raw, "short_reset_at", now, 400*24*time.Hour)
	if e.ShortResetAt != nil && !e.ShortResetAt.After(now) {
		e.ShortPercent, e.ShortResetAt = nil, nil // that window has ended; no current reading
	}
	if s, ok := raw["error"].(string); ok {
		e.Error = s
	}
	e.State = state(e, now)
	return e, nil
}

func state(e Entry, now time.Time) string {
	// A failure at or after the last success wins (equal timestamps = failed).
	if e.Error != "" && e.AttemptedAt != nil && (e.FetchedAt == nil || !e.AttemptedAt.Before(*e.FetchedAt)) {
		return StateError
	}
	if e.FetchedAt == nil || e.Percent == nil {
		return StateUnknown
	}
	if now.Sub(*e.FetchedAt) > StaleAfter {
		return StateStale
	}
	if e.ResetAt != nil && !e.ResetAt.After(now) && e.FetchedAt.Before(*e.ResetAt) {
		return StateStale
	}
	return StateOK
}

// Files maps entry IDs to cache files in dir (regular files ≤ 64 KiB only).
func Files(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, de := range entries {
		m := fileRE.FindStringSubmatch(de.Name())
		if m == nil {
			continue
		}
		info, err := de.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFile {
			continue
		}
		id := m[1]
		if m[2] != "" {
			id += "-" + m[2]
		}
		out[id] = filepath.Join(dir, de.Name())
	}
	return out, nil
}

// Load reads every cache file in cacheDir and applies the optional mapping.
func Load(cacheDir, configPath string, now time.Time) ([]Entry, error) {
	files, err := Files(cacheDir)
	if err != nil {
		return nil, err
	}
	maps := map[string]mapping{}
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &maps); err != nil {
			return nil, err
		}
	}
	out := []Entry{}
	for id, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		e, err := Parse(id, data, now)
		if err != nil {
			e = Entry{ID: id, Label: id, State: StateError, Error: "unreadable cache: " + err.Error()}
		}
		e.Presets = []string{id}
		if id == "codex-default" {
			e.Presets = []string{"codex"}
		}
		if m, ok := maps[id]; ok {
			if m.Label != "" {
				e.Label = m.Label
			}
			if len(m.Presets) > 0 {
				e.Presets = m.Presets
				e.Explicit = true
			}
			e.Blocking = m.Blocking
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}
