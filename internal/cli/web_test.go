package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTailscale puts a `tailscale` script first on PATH that runs body.
func fakeTailscale(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix shebang script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

const tailscaleStatusJSON = `{"Self":{"UserID":7,"DNSName":"box.tailnet.ts.net."},"User":{"7":{"LoginName":"me@example.com"}}}`

func TestWebIdentityDetectsOwnerAndDNSName(t *testing.T) {
	fakeTailscale(t, "echo '"+tailscaleStatusJSON+"'")
	var stderr bytes.Buffer
	owner, hosts, err := webIdentity(context.Background(), "", "", &stderr)
	if err != nil || owner != "me@example.com" || len(hosts) != 1 || hosts[0] != "box.tailnet.ts.net" {
		t.Fatalf("%q %v %v", owner, hosts, err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected note: %s", stderr.String())
	}
}

func TestWebIdentityExplicitOwnerStillAllowsDNSName(t *testing.T) {
	fakeTailscale(t, "echo '"+tailscaleStatusJSON+"'")
	owner, hosts, err := webIdentity(context.Background(), "other@example.com", "", &bytes.Buffer{})
	if err != nil || owner != "other@example.com" || len(hosts) != 1 || hosts[0] != "box.tailnet.ts.net" {
		t.Fatalf("%q %v %v", owner, hosts, err)
	}
}

func TestWebIdentityExplicitOwnerSurvivesDetectionFailure(t *testing.T) {
	fakeTailscale(t, "echo not running >&2\nexit 1")
	var stderr bytes.Buffer
	owner, hosts, err := webIdentity(context.Background(), "other@example.com", "", &stderr)
	if err != nil || owner != "other@example.com" || len(hosts) != 0 {
		t.Fatalf("%q %v %v", owner, hosts, err)
	}
	if note := stderr.String(); strings.Count(note, "\n") != 1 || !strings.Contains(note, "--origin https://<host>") {
		t.Fatalf("want a one-line --origin note, got %q", note)
	}
	// With --origin set the note is unnecessary.
	stderr.Reset()
	if _, _, err := webIdentity(context.Background(), "other@example.com", "https://box.example", &stderr); err != nil || stderr.Len() != 0 {
		t.Fatalf("with --origin: %v %q", err, stderr.String())
	}
}

func TestWebIdentityWithoutOwnerFailsWhenDetectionFails(t *testing.T) {
	fakeTailscale(t, "echo not running >&2\nexit 1")
	if _, _, err := webIdentity(context.Background(), "", "", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--owner") {
		t.Fatalf("got %v, want an error mentioning --owner", err)
	}
}

func TestMachineNameDefaults(t *testing.T) {
	for _, c := range []struct{ flag, dns, host, want string }{
		{"", "macmini.brill-decibel.ts.net", "Mac-mini.local", "macmini"},
		{"", "cachyos.brill-decibel.ts.net", "cachyos-desktop", "cachyos"},
		{"", "", "cachyos-desktop", "cachyos-desktop"},
		{"", "", "Mac-mini.local", "Mac-mini"},
		{"box", "macmini.x.ts.net", "h", "box"},
	} {
		if got := machineName(c.flag, c.dns, c.host); got != c.want {
			t.Errorf("machineName(%q,%q,%q) = %q, want %q", c.flag, c.dns, c.host, got, c.want)
		}
	}
}
