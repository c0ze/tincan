package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	// Session is persistent or stateless. Empty selects supported provider defaults.
	Session string `json:"session,omitempty"`
	// Env sets variables for the agent process, after EnvUnset (spec §5.1).
	// Values are private: public views show keys only (see Public).
	Env map[string]string `json:"env,omitempty"`
	// EnvUnset removes inherited variables, e.g. ANTHROPIC_API_KEY so a
	// profile's own login is used.
	EnvUnset []string `json:"env_unset,omitempty"`
	// Provider names the persistent-session adapter explicitly, for wrappers
	// and executables whose base name is not the adapter's (spec §5.3).
	Provider string `json:"provider,omitempty"`
}

// configPreset is the on-disk shape of one ~/.config/tincan/agents.json
// entry: the same fields, but an absent exec_timeout_sec must mean "default"
// rather than 0 ("no bound"), hence the pointer.
type configPreset struct {
	Exec           []string          `json:"exec"`
	Stdin          string            `json:"stdin"`
	Reply          string            `json:"reply"`
	ExecTimeoutSec *int              `json:"exec_timeout_sec"`
	Session        string            `json:"session,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvUnset       []string          `json:"env_unset,omitempty"`
	Provider       string            `json:"provider,omitempty"`
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
		p := Preset{Exec: c.Exec, Stdin: c.Stdin, Reply: c.Reply, ExecTimeoutSec: DefaultExecTimeoutSec, Session: c.Session,
			Env: c.Env, EnvUnset: c.EnvUnset, Provider: c.Provider}
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
	if p.Session != "" && p.Session != "persistent" && p.Session != "stateless" {
		return fmt.Errorf("session must be persistent or stateless, got %q", p.Session)
	}
	if p.Reply == "file" && !hasPlaceholder(p.Exec, "{out}") {
		return errors.New(`reply "file" requires an {out} placeholder in exec`)
	}
	if len(p.Env) == 0 {
		p.Env = nil
	}
	if len(p.EnvUnset) == 0 {
		p.EnvUnset = nil
	}
	for k, v := range p.Env {
		if err := validEnvKey(k); err != nil {
			return err
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("env %s: value contains a NUL byte", k)
		}
	}
	seen := map[string]bool{}
	for _, k := range p.EnvUnset {
		if err := validEnvKey(k); err != nil {
			return err
		}
		if seen[k] {
			return fmt.Errorf("env_unset lists %s twice", k)
		}
		if _, ok := p.Env[k]; ok {
			return fmt.Errorf("%s is both set in env and removed by env_unset", k)
		}
		seen[k] = true
	}
	if p.Provider != "" && !SupportsSessions(p.Provider) {
		return fmt.Errorf("provider must be claude, grok, agy or kimi, got %q", p.Provider)
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

var envKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// validEnvKey applies the spec §5.1 key rules to env and env_unset names.
func validEnvKey(k string) error {
	switch {
	case !envKeyPattern.MatchString(k):
		return fmt.Errorf("env key %q must match %s", k, envKeyPattern)
	case k == "PATH":
		return errors.New("env cannot change PATH; tincan resolves agent executables itself")
	case strings.HasPrefix(k, "TINCAN_"):
		return fmt.Errorf("env key %s uses the reserved TINCAN_ prefix", k)
	case parentSessionEnv[k]:
		return fmt.Errorf("env key %s is a parent-session variable tincan always removes", k)
	}
	return nil
}

// PublicPreset is a preset as shown by `tincan presets` and other public
// views: env values are withheld, only their keys are listed (spec §5.2).
type PublicPreset struct {
	Exec           []string `json:"exec"`
	Stdin          string   `json:"stdin"`
	Reply          string   `json:"reply"`
	ExecTimeoutSec int      `json:"exec_timeout_sec"`
	Session        string   `json:"session,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	EnvKeys        []string `json:"env_keys,omitempty"`
	EnvUnset       []string `json:"env_unset,omitempty"`
}

// Public returns the redacted view of p.
func (p Preset) Public() PublicPreset {
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		keys = nil
	}
	return PublicPreset{Exec: p.Exec, Stdin: p.Stdin, Reply: p.Reply, ExecTimeoutSec: p.ExecTimeoutSec,
		Session: p.Session, Provider: p.Provider, EnvKeys: keys, EnvUnset: p.EnvUnset}
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
// whitespace is trimmed for every preset; a kimi provider (see
// SessionProvider) additionally drops the trailing "To resume this session: …"
// line its CLI appends. Nothing else is rewritten.
func PostProcess(provider, body string) string {
	body = strings.TrimRight(body, " \t\r\n")
	if provider == "kimi" {
		lines := strings.Split(body, "\n")
		if strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "To resume this session:") {
			body = strings.TrimRight(strings.Join(lines[:len(lines)-1], "\n"), " \t\r\n")
		}
	}
	return body
}
