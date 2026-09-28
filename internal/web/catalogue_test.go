package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func TestPresetsCatalogueIsRedactedAndWarns(t *testing.T) {
	bin := filepath.Join(t.TempDir(), fakeAgentName())
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	qdir := t.TempDir()
	os.WriteFile(filepath.Join(qdir, "claude-quota.json"), []byte(`{"percent": 40, "reset_at": "2099-01-01T00:00:00Z", "fetched_at": "2099-01-01T00:00:00Z"}`), 0o600)
	s, err := New(Config{Owner: owner, AllowedHosts: testHosts, Machine: "box", Registry: rooms.Open(filepath.Join(t.TempDir(), "rooms.json")),
		QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json"),
		Dispatch: dispatch.Options{Presets: map[string]host.Preset{
			"claude":   {Exec: []string{bin, "-p", "{body}", "--dangerously-skip-permissions"}, Stdin: "none", Reply: "stdout", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/secret/dir"}},
			"relative": {Exec: []string{"./reviewer"}, Stdin: "none", Reply: "stdout"},
			"codexy":   {Exec: []string{bin, "exec", "-s", "danger-full-access"}, Stdin: "none", Reply: "stdout"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, s.Handler(), "GET", "/api/presets", "", ownerHdr())
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "/secret/dir") {
		t.Fatalf("catalogue: %d %s", rec.Code, rec.Body)
	}
	var got []PresetInfo
	decode(t, rec.Body.String(), &got)
	by := map[string]PresetInfo{}
	for _, p := range got {
		by[p.Name] = p
	}
	c := by["claude"]
	if !c.Available || c.ExecKind != "absolute" || c.Bypass != "--dangerously-skip-permissions" || len(c.Warnings) == 0 ||
		len(c.EnvKeys) != 1 || c.EnvKeys[0] != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("claude entry: %+v", c)
	}
	if by["relative"].ExecKind != "relative" || by["relative"].Available {
		t.Fatalf("relative entry: %+v", by["relative"])
	}
	if by["codexy"].Bypass != "-s danger-full-access" {
		t.Fatalf("sandbox bypass not detected: %+v", by["codexy"])
	}
}

func TestBypassFlag(t *testing.T) {
	for argv, want := range map[string]string{
		"claude -p {body}":                        "",
		"grok -p {body} --always-approve":         "--always-approve",
		"codex exec --sandbox danger-full-access": "--sandbox danger-full-access",
		"codex exec --sandbox=danger-full-access": "--sandbox=danger-full-access",
		"gemini --yolo":                           "--yolo",
		"codex exec -s read-only":                 "",
	} {
		if got := bypassFlag(strings.Fields(argv)); got != want {
			t.Errorf("%s: %q, want %q", argv, got, want)
		}
	}
}
