package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/quota"
)

// PresetInfo is one entry of this machine's redacted preset catalogue
// (committees §6.7): enough to pick committee members and warn about them,
// never an env value.
type PresetInfo struct {
	Name             string       `json:"name"`
	Available        bool         `json:"available"`
	Executable       string       `json:"executable"`
	ExecKind         string       `json:"exec_kind"`
	Provider         string       `json:"provider,omitempty"`
	SessionSupported bool         `json:"session_supported"`
	EnvKeys          []string     `json:"env_keys,omitempty"`
	EnvUnset         []string     `json:"env_unset,omitempty"`
	Bypass           string       `json:"bypass,omitempty"`
	Warnings         []string     `json:"warnings,omitempty"`
	Quota            *quota.Entry `json:"quota,omitempty"`
}

// bypassFlag returns the first known permission-bypass argument in argv, as
// written ("-s danger-full-access" for a split sandbox flag), or "".
func bypassFlag(argv []string) string {
	for i, a := range argv {
		switch a {
		case "--dangerously-skip-permissions", "--always-approve", "--yolo", "--sandbox=danger-full-access":
			return a
		case "-s", "--sandbox":
			if i+1 < len(argv) && argv[i+1] == "danger-full-access" {
				return a + " danger-full-access"
			}
		}
	}
	return ""
}

func execKind(name string) string {
	switch {
	case filepath.IsAbs(name):
		return "absolute"
	case strings.ContainsAny(name, `/\`):
		return "relative"
	default:
		return "bare"
	}
}

func (s *Server) catalogue() ([]PresetInfo, error) {
	presets, err := s.cfg.Dispatch.PresetMap()
	if err != nil {
		return nil, err
	}
	entries, _ := quota.Load(s.cfg.QuotaDir, s.cfg.QuotaConfig, time.Now())
	out := make([]PresetInfo, 0, len(presets))
	for _, name := range host.Names(presets) {
		p := presets[name]
		info := PresetInfo{Name: name, ExecKind: execKind(p.Exec[0])}
		if info.ExecKind != "relative" {
			if path, err := host.ResolveExecutable(p.Exec[0], ""); err == nil {
				info.Available, info.Executable = true, path
			}
		}
		info.Provider, _ = host.SessionProvider(p, name)
		info.SessionSupported = info.Provider != ""
		pub := p.Public()
		info.EnvKeys, info.EnvUnset = pub.EnvKeys, pub.EnvUnset
		if info.Bypass = bypassFlag(p.Exec); info.Bypass != "" {
			info.Warnings = append(info.Warnings, fmt.Sprintf("permission bypass (%s): a reviewer could modify files", info.Bypass))
		}
		if info.ExecKind == "relative" {
			info.Warnings = append(info.Warnings, "relative executable: cannot be a committee member")
		}
		info.Quota = quotaFor(entries, name)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// quotaFor mirrors the UI's choice: an explicit mapping wins, else the first
// entry naming the preset.
func quotaFor(entries []quota.Entry, preset string) *quota.Entry {
	var first *quota.Entry
	for i := range entries {
		for _, p := range entries[i].Presets {
			if p != preset {
				continue
			}
			if entries[i].Explicit {
				return &entries[i]
			}
			if first == nil {
				first = &entries[i]
			}
		}
	}
	return first
}

func (s *Server) apiPresets(w http.ResponseWriter, r *http.Request) {
	list, err := s.catalogue()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}
