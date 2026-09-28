package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestBuiltinPresetsMatchSpec(t *testing.T) {
	b := Builtin()
	got := Names(b)
	want := []string{"agy", "claude", "codex", "gemini", "grok", "kimi"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	codex := b["codex"]
	if !reflect.DeepEqual(codex.Exec, []string{"codex", "exec", "-s", "workspace-write", "--skip-git-repo-check", "-o", "{out}", "-"}) {
		t.Fatalf("codex exec = %q", codex.Exec)
	}
	if codex.Stdin != "body" || codex.Reply != "file" || codex.ExecTimeoutSec != DefaultExecTimeoutSec {
		t.Fatalf("codex preset = %+v", codex)
	}
	agy := b["agy"]
	if !reflect.DeepEqual(agy.Exec, []string{"agy", "-p", "{body}", "--dangerously-skip-permissions", "--model", "gemini-3.8-flash-high", "--print-timeout", "150m"}) {
		t.Fatalf("agy exec = %q", agy.Exec)
	}
	for _, n := range []string{"agy", "claude", "gemini", "grok", "kimi"} {
		p := b[n]
		if p.Stdin != "none" || p.Reply != "stdout" || p.ExecTimeoutSec != DefaultExecTimeoutSec {
			t.Fatalf("%s preset = %+v, want stdin none / reply stdout / default timeout", n, p)
		}
		if p.Exec[0] != n || p.Exec[1] != "-p" || p.Exec[2] != "{body}" {
			t.Fatalf("%s exec = %q, want `%s -p {body} …`", n, p.Exec, n)
		}
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigMissingFileIsEmpty(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "nope.json"), ""} {
		got, err := LoadConfig(path)
		if err != nil || len(got) != 0 {
			t.Fatalf("LoadConfig(%q) = %v, %v; want empty, nil", path, got, err)
		}
	}
}

func TestLoadConfigFillsDefaults(t *testing.T) {
	path := writeConfig(t, `{"myllm": {"exec": ["my-llm", "-"]}, "slow": {"exec": ["s", "{body}"], "exec_timeout_sec": 1800}, "forever": {"exec": ["f", "{body}"], "exec_timeout_sec": 0}}`)
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := got["myllm"]; p.Stdin != "none" || p.Reply != "stdout" || p.ExecTimeoutSec != DefaultExecTimeoutSec {
		t.Fatalf("defaults not filled: %+v", p)
	}
	if got["slow"].ExecTimeoutSec != 1800 || got["forever"].ExecTimeoutSec != 0 {
		t.Fatalf("explicit timeouts lost: slow=%d forever=%d", got["slow"].ExecTimeoutSec, got["forever"].ExecTimeoutSec)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]string{
		"parse":           `{not json`,
		"bad stdin":       `{"x": {"exec": ["a"], "stdin": "pipe"}}`,
		"bad reply":       `{"x": {"exec": ["a"], "reply": "mail"}}`,
		"file needs out":  `{"x": {"exec": ["a", "{body}"], "reply": "file"}}`,
		"empty exec":      `{"x": {"exec": []}}`,
		"missing exec":    `{"x": {"stdin": "body"}}`,
		"negative timeou": `{"x": {"exec": ["a"], "exec_timeout_sec": -5}}`,
	}
	for name, content := range cases {
		path := writeConfig(t, content)
		_, err := LoadConfig(path)
		if err == nil {
			t.Errorf("%s: want error, got nil", name)
			continue
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("%s: error %q does not name the file", name, err)
		}
	}
}

func TestEffectiveConfigReplacesBuiltinWholesale(t *testing.T) {
	path := writeConfig(t, `{"kimi": {"exec": ["kimi", "--print", "{body}"]}, "fake": {"exec": ["fake", "{body}"], "stdin": "body"}}`)
	got, err := Effective(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("want 6 built-ins + fake = 7 presets, got %d: %v", len(got), Names(got))
	}
	if !reflect.DeepEqual(got["kimi"].Exec, []string{"kimi", "--print", "{body}"}) {
		t.Fatalf("kimi not overridden: %+v", got["kimi"])
	}
	if got["fake"].Stdin != "body" || got["codex"].Reply != "file" {
		t.Fatalf("merge wrong: fake=%+v codex=%+v", got["fake"], got["codex"])
	}
}

func TestResolve(t *testing.T) {
	none := Overrides{ExecTimeoutSec: -1}
	presets := Builtin()
	cases := []struct {
		desc, name, flag string
		ov               Overrides
		wantLabel        string
		wantExec0        string
		wantTimeout      int
		wantErr          bool
	}{
		{desc: "name is a preset", name: "codex", ov: none, wantLabel: "codex", wantExec0: "codex", wantTimeout: 3600},
		{desc: "--preset wins over name", name: "x", flag: "kimi", ov: none, wantLabel: "kimi", wantExec0: "kimi", wantTimeout: 3600},
		{desc: "unknown --preset without exec", name: "x", flag: "nope", ov: none, wantErr: true},
		{desc: "unknown --preset with exec", name: "x", flag: "nope", ov: Overrides{Exec: []string{"z", "{body}"}, ExecTimeoutSec: -1}, wantLabel: "nope", wantExec0: "z", wantTimeout: 3600},
		{desc: "unknown name without exec", name: "x", ov: none, wantErr: true},
		{desc: "unknown name with exec is custom", name: "x", ov: Overrides{Exec: []string{"my", "{body}"}, ExecTimeoutSec: -1}, wantLabel: "custom", wantExec0: "my", wantTimeout: 3600},
		{desc: "overrides apply", name: "codex", ov: Overrides{Exec: []string{"cx", "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 5}, wantLabel: "codex", wantExec0: "cx", wantTimeout: 5},
		{desc: "timeout 0 means none", name: "codex", ov: Overrides{ExecTimeoutSec: 0}, wantLabel: "codex", wantExec0: "codex", wantTimeout: 0},
		{desc: "reply file needs {out}", name: "kimi", ov: Overrides{Reply: "file", ExecTimeoutSec: -1}, wantErr: true},
		{desc: "bad stdin override", name: "kimi", ov: Overrides{Stdin: "pipe", ExecTimeoutSec: -1}, wantErr: true},
	}
	for _, c := range cases {
		label, p, err := Resolve(presets, c.name, c.flag, c.ov)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got label=%q %+v", c.desc, label, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.desc, err)
			continue
		}
		if label != c.wantLabel || p.Exec[0] != c.wantExec0 || p.ExecTimeoutSec != c.wantTimeout {
			t.Errorf("%s: got label=%q exec0=%q timeout=%d, want %q %q %d", c.desc, label, p.Exec[0], p.ExecTimeoutSec, c.wantLabel, c.wantExec0, c.wantTimeout)
		}
	}
	// Resolve must not mutate the shared preset map.
	if presets["codex"].Exec[0] != "codex" {
		t.Fatal("Resolve mutated the presets map")
	}
}

func TestPostProcess(t *testing.T) {
	cases := []struct{ label, in, want string }{
		{"codex", "answer\n\n  \n", "answer"},
		{"grok", "  keep leading\n", "  keep leading"},
		{"kimi", "the answer\n\nTo resume this session: kimi --resume abc\n", "the answer"},
		{"kimi", "no resume line\n", "no resume line"},
		{"kimi", "To resume this session: kimi --resume abc", ""},
		{"gemini", "text\nTo resume this session: x\n", "text\nTo resume this session: x"},
	}
	for _, c := range cases {
		if got := PostProcess(c.label, c.in); got != c.want {
			t.Errorf("PostProcess(%q, %q) = %q, want %q", c.label, c.in, got, c.want)
		}
	}
}

func TestNamesSorted(t *testing.T) {
	got := Names(map[string]Preset{"b": {}, "a": {}, "c": {}})
	if !sort.StringsAreSorted(got) || len(got) != 3 {
		t.Fatalf("Names = %v", got)
	}
}

func TestLoadConfigAccountProfileFields(t *testing.T) {
	path := writeConfig(t, `{"claude-personal":{"exec":["claude","-p","{body}"],
		"env":{"CLAUDE_CONFIG_DIR":"~/.claude-personal"},"env_unset":["ANTHROPIC_API_KEY"],"provider":"claude"}}`)
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := got["claude-personal"]
	if p.Env["CLAUDE_CONFIG_DIR"] != "~/.claude-personal" || !reflect.DeepEqual(p.EnvUnset, []string{"ANTHROPIC_API_KEY"}) || p.Provider != "claude" {
		t.Fatalf("profile fields lost: %+v", p)
	}
}

func TestLoadConfigRejectsBadProfiles(t *testing.T) {
	for name, entry := range map[string]string{
		"lowercase key":    `{"exec":["x"],"env":{"home":"y"}}`,
		"leading digit":    `{"exec":["x"],"env":{"1A":"y"}}`,
		"PATH":             `{"exec":["x"],"env":{"PATH":"/bin"}}`,
		"reserved prefix":  `{"exec":["x"],"env":{"TINCAN_ROOM":"/"}}`,
		"scrubbed key":     `{"exec":["x"],"env":{"CLAUDECODE":"1"}}`,
		"too long":         fmt.Sprintf(`{"exec":["x"],"env":{"A%s":"y"}}`, strings.Repeat("B", 64)),
		"NUL value":        `{"exec":["x"],"env":{"A":"b\u0000c"}}`,
		"unset PATH":       `{"exec":["x"],"env_unset":["PATH"]}`,
		"unset reserved":   `{"exec":["x"],"env_unset":["TINCAN_ROOM"]}`,
		"set and unset":    `{"exec":["x"],"env":{"A":"1"},"env_unset":["A"]}`,
		"duplicate unset":  `{"exec":["x"],"env_unset":["A","A"]}`,
		"unknown provider": `{"exec":["x"],"provider":"codex"}`,
	} {
		if _, err := LoadConfig(writeConfig(t, `{"p":`+entry+`}`)); err == nil {
			t.Errorf("%s: accepted %s", name, entry)
		}
	}
}

func TestNormalizeDropsEmptyProfileCollections(t *testing.T) {
	got, err := LoadConfig(writeConfig(t, `{"p":{"exec":["x"],"env":{},"env_unset":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got["p"].Env != nil || got["p"].EnvUnset != nil {
		t.Fatalf("empty collections kept: %#v", got["p"])
	}
	data, _ := json.Marshal(got["p"])
	var back Preset
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back, got["p"]) {
		t.Fatalf("round trip changed preset: %#v vs %#v (%v)", back, got["p"], err)
	}
}

func TestPublicPresetWithholdsEnvValues(t *testing.T) {
	p := Preset{Exec: []string{"codex", "exec"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60,
		Env:      map[string]string{"CODEX_HOME": "~/.codex-gmail", "B_TOKEN": "s3cret"},
		EnvUnset: []string{"OPENAI_API_KEY"}}
	v := p.Public()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret") || strings.Contains(string(data), ".codex-gmail") {
		t.Fatalf("public view leaked a value: %s", data)
	}
	if !reflect.DeepEqual(v.EnvKeys, []string{"B_TOKEN", "CODEX_HOME"}) || !reflect.DeepEqual(v.EnvUnset, []string{"OPENAI_API_KEY"}) {
		t.Fatalf("public view keys: %+v", v)
	}
	if !reflect.DeepEqual(v.Exec, p.Exec) || v.ExecTimeoutSec != 60 {
		t.Fatalf("public view lost fields: %+v", v)
	}
}
