package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultExecTimeoutSec bounds one agent run when neither the preset nor the
// command line says otherwise. 0 disables the bound.
const DefaultExecTimeoutSec = 3600

// Preset describes how to run one headless agent CLI for a message.
type Preset struct {
	// Exec is the argv template; see Render for the placeholders.
	Exec []string `json:"exec"`
	// Stdin is "body" (pipe the body to the agent's stdin) or "none".
	Stdin string `json:"stdin"`
	// Reply is "stdout" (the reply is the agent's stdout) or "file" (the
	// reply is the contents of the {out} file the agent wrote).
	Reply string `json:"reply"`
	// ExecTimeoutSec kills the run after this many seconds; 0 = no bound.
	ExecTimeoutSec int `json:"exec_timeout_sec"`
}

// configPreset is the on-disk shape of one ~/.config/tincan/agents.json
// entry: the same fields, but an absent exec_timeout_sec must mean "default"
// rather than 0 ("no bound"), hence the pointer.
type configPreset struct {
	Exec           []string `json:"exec"`
	Stdin          string   `json:"stdin"`
	Reply          string   `json:"reply"`
	ExecTimeoutSec *int     `json:"exec_timeout_sec"`
}

// Builtin returns the presets that ship with tincan: the headless
// invocations verified on 2026-09-07 (spec §5), each using the narrowest
// unattended mode that works. Returns a fresh map every call.
func Builtin() map[string]Preset {
	stdout := func(argv ...string) Preset {
		return Preset{Exec: argv, Stdin: "none", Reply: "stdout", ExecTimeoutSec: DefaultExecTimeoutSec}
	}
	return map[string]Preset{
		"codex": {
			Exec:           []string{"codex", "exec", "-s", "workspace-write", "--skip-git-repo-check", "-o", "{out}", "-"},
			Stdin:          "body",
			Reply:          "file",
			ExecTimeoutSec: DefaultExecTimeoutSec,
		},
		"grok":   stdout("grok", "-p", "{body}", "--always-approve"),
		"kimi":   stdout("kimi", "-p", "{body}"),
		"agy":    stdout("agy", "-p", "{body}", "--dangerously-skip-permissions", "--model", "gemini-3.8-flash-high", "--print-timeout", "150m"),
		"gemini": stdout("gemini", "-p", "{body}"),
		"claude": stdout("claude", "-p", "{body}", "--dangerously-skip-permissions"),
	}
}

// ConfigPath is ~/.config/tincan/agents.json, or "" when the home directory
// cannot be determined (then no user presets apply).
func ConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "tincan", "agents.json")
}

// LoadConfig reads user presets from path. A missing file (or an empty
// path) yields an empty map; a malformed file or an invalid preset is an
// error naming the file.
func LoadConfig(path string) (map[string]Preset, error) {
	if path == "" {
		return map[string]Preset{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Preset{}, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]configPreset
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]Preset, len(raw))
	for name, c := range raw {
		p := Preset{Exec: c.Exec, Stdin: c.Stdin, Reply: c.Reply, ExecTimeoutSec: DefaultExecTimeoutSec}
		if c.ExecTimeoutSec != nil {
			p.ExecTimeoutSec = *c.ExecTimeoutSec
		}
		if err := p.normalize(); err != nil {
			return nil, fmt.Errorf("%s: preset %q: %w", path, name, err)
		}
		out[name] = p
	}
	return out, nil
}

// normalize fills the enum defaults and validates the preset's shape.
func (p *Preset) normalize() error {
	if len(p.Exec) == 0 {
		return errors.New("exec is required")
	}
	switch p.Stdin {
	case "":
		p.Stdin = "none"
	case "none", "body":
	default:
		return fmt.Errorf("stdin must be body or none, got %q", p.Stdin)
	}
	switch p.Reply {
	case "":
		p.Reply = "stdout"
	case "stdout", "file":
	default:
		return fmt.Errorf("reply must be stdout or file, got %q", p.Reply)
	}
	if p.ExecTimeoutSec < 0 {
		return errors.New("exec_timeout_sec must be >= 0")
	}
	if p.Reply == "file" && !hasPlaceholder(p.Exec, "{out}") {
		return errors.New(`reply "file" requires an {out} placeholder in exec`)
	}
	return nil
}

func hasPlaceholder(exec []string, ph string) bool {
	for _, tok := range exec {
		if strings.Contains(tok, ph) {
			return true
		}
	}
	return false
}

// Effective returns the built-in presets overlaid with the user config at
// path: an entry in the file replaces the built-in of the same name whole.
func Effective(path string) (map[string]Preset, error) {
	all := Builtin()
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	for name, p := range cfg {
		all[name] = p
	}
	return all, nil
}

// Names returns the preset names in sorted order.
func Names(m map[string]Preset) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Overrides carries command-line flags layered on top of a preset. A nil
// Exec, an empty Stdin/Reply and a negative ExecTimeoutSec mean "not given".
type Overrides struct {
	Exec           []string
	Stdin          string
	Reply          string
	ExecTimeoutSec int
}

// Resolve picks the preset for a hosted listener: presetFlag if given, else
// the listener's own name if that is a known preset, else --exec is required
// (and the result is labelled "custom"). Overrides are applied last, then the
// result is validated. The label is what the state file and `status` show.
func Resolve(presets map[string]Preset, name, presetFlag string, ov Overrides) (label string, p Preset, err error) {
	switch {
	case presetFlag != "":
		base, ok := presets[presetFlag]
		if !ok && ov.Exec == nil {
			return "", Preset{}, fmt.Errorf("unknown preset %q (see `tincan presets`, or pass --exec)", presetFlag)
		}
		if !ok {
			base = Preset{ExecTimeoutSec: DefaultExecTimeoutSec}
		}
		label, p = presetFlag, base
	default:
		if base, ok := presets[name]; ok {
			label, p = name, base
		} else if ov.Exec != nil {
			label, p = "custom", Preset{ExecTimeoutSec: DefaultExecTimeoutSec}
		} else {
			return "", Preset{}, fmt.Errorf("%q is not a known preset; pass --preset <p> or --exec '<template>' (see `tincan presets`)", name)
		}
	}
	// Copy the argv so overrides and callers never alias the shared map.
	p.Exec = append([]string(nil), p.Exec...)
	if ov.Exec != nil {
		p.Exec = append([]string(nil), ov.Exec...)
	}
	if ov.Stdin != "" {
		p.Stdin = ov.Stdin
	}
	if ov.Reply != "" {
		p.Reply = ov.Reply
	}
	if ov.ExecTimeoutSec >= 0 {
		p.ExecTimeoutSec = ov.ExecTimeoutSec
	}
	if err := p.normalize(); err != nil {
		return "", Preset{}, err
	}
	return label, p, nil
}

// PostProcess applies the preset-level reply cleanup (spec §5): trailing
// whitespace is trimmed for every preset; "kimi" additionally drops the
// trailing "To resume this session: …" line its CLI appends. Nothing else is
// rewritten.
func PostProcess(label, body string) string {
	body = strings.TrimRight(body, " \t\r\n")
	if label == "kimi" {
		lines := strings.Split(body, "\n")
		if strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "To resume this session:") {
			body = strings.TrimRight(strings.Join(lines[:len(lines)-1], "\n"), " \t\r\n")
		}
	}
	return body
}
