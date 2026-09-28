package thread

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/host"
)

func TestParseMentions(t *testing.T) {
	cases := map[string][]string{
		"@codex review this":                    {"codex"},
		"hey @claude, then @codex.":             {"claude", "codex"},
		"(@grok) and [@agy]":                    {"grok", "agy"},
		"mail me at a@b.com":                    nil,
		"`@codex` in code":                      nil,
		"```\n@codex\n```\n@claude":             {"claude"},
		"@you note to self":                     {"you"},
		"@codex @codex twice":                   {"codex"},
		"@codex-audit: status?":                 {"codex-audit"},
		"@" + strings.Repeat("a", 70) + " long": {strings.Repeat("a", 64)},
	}
	for in, want := range cases {
		if got := ParseMentions(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseMentions(%q) = %q, want %q", in, got, want)
		}
	}
}

func testResolver(t *testing.T, alive map[string]host.State) Resolver {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, fakeAgentName())
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	return Resolver{
		Room: dir,
		Presets: map[string]host.Preset{
			"claude":  {Exec: []string{bin}},
			"codex":   {Exec: []string{bin}},
			"missing": {Exec: []string{filepath.Join(dir, "not-installed")}},
		},
		Alive: func(name string) (host.State, bool) { st, ok := alive[name]; return st, ok },
	}
}

func TestResolveOrderAndCanonicalNames(t *testing.T) {
	r := testResolver(t, map[string]host.State{"codex-audit": {Preset: "codex", Owner: "o"}})
	meta := Meta{ID: "t7f2a9c1", Listeners: map[string]string{"claude.t7f2a9c1": "claude"}}
	got, unresolved := r.Resolve(meta, []string{"claude", "claude.t7f2a9c1", "codex-audit", "missing", "nobody", "you"})
	want := []Target{
		{Mention: "claude", Listener: "claude.t7f2a9c1", Preset: "claude"},
		{Mention: "codex-audit", Listener: "codex-audit", Preset: "codex", Existing: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(unresolved, []string{"missing", "nobody"}) {
		t.Fatalf("unresolved %q", unresolved)
	}
}

func TestResolveIgnoresInteractiveListeners(t *testing.T) {
	r := testResolver(t, map[string]host.State{"alice": {Owner: ""}})
	got, unresolved := r.Resolve(Meta{ID: "t7f2a9c1"}, []string{"alice"})
	if len(got) != 0 || !reflect.DeepEqual(unresolved, []string{"alice"}) {
		t.Fatalf("got %+v unresolved %q", got, unresolved)
	}
}

// fakeAgentName is a file name the platform treats as a program: Windows
// resolves executables by extension (PATHEXT), not by mode bits.
func fakeAgentName() string {
	if runtime.GOOS == "windows" {
		return "agent.exe"
	}
	return "agent"
}
