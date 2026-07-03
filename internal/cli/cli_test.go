package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/c0ze/tincan/internal/envelope"
)

// run invokes the CLI and returns (exit code, stdout, stderr).
func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestSendThenRecvJSON(t *testing.T) {
	room := t.TempDir()
	code, _, stderr := run("send", "--room", room, "--to", "codex", "--from", "orch",
		"--body", "review PR 56", "--artifact", "a.txt", "--artifact", "b.txt")
	if code != 0 {
		t.Fatalf("send exit %d, stderr=%q", code, stderr)
	}
	code, stdout, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "5")
	if code != 0 {
		t.Fatalf("recv exit %d, stderr=%q", code, stderr)
	}
	e, err := envelope.Unmarshal([]byte(stdout))
	if err != nil {
		t.Fatalf("recv output is not an envelope: %v\n%s", err, stdout)
	}
	if e.Body != "review PR 56" || e.From != "orch" || e.To != "codex" {
		t.Fatalf("wrong envelope: %+v", e)
	}
	if len(e.Artifacts) != 2 || e.Artifacts[0] != "a.txt" || e.Artifacts[1] != "b.txt" {
		t.Fatalf("artifacts not preserved: %+v", e.Artifacts)
	}
}

func TestRecvFormatBody(t *testing.T) {
	room := t.TempDir()
	if code, _, stderr := run("send", "--room", room, "--to", "x", "--from", "y", "--body", "just the text"); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	code, stdout, _ := run("recv", "--room", room, "--as", "x", "--timeout", "5", "--format", "body")
	if code != 0 {
		t.Fatalf("recv exit %d", code)
	}
	if strings.TrimSpace(stdout) != "just the text" {
		t.Fatalf("body format output = %q", stdout)
	}
}

func TestRecvTimeoutExitsThreeSilently(t *testing.T) {
	code, stdout, stderr := run("recv", "--room", t.TempDir(), "--as", "nobody", "--timeout", "1")
	if code != 3 {
		t.Fatalf("want exit 3 on timeout, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("want no stdout on timeout, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("want no stderr on timeout, got %q", stderr)
	}
}

func TestSendBodyFile(t *testing.T) {
	room := t.TempDir()
	f := room + "/task.md"
	if err := os.WriteFile(f, []byte("long instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("send", "--room", room, "--to", "x", "--from", "y", "--body-file", f); code != 0 {
		t.Fatalf("send failed: %s", stderr)
	}
	_, stdout, _ := run("recv", "--room", room, "--as", "x", "--timeout", "5", "--format", "body")
	if strings.TrimSpace(stdout) != "long instructions" {
		t.Fatalf("got %q", stdout)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"send", "--to", "x"},                // missing --from and body
		{"send", "--to", "x", "--from", "y"}, // missing body
		{"recv"},                             // missing --as
		{"recv", "--as", "x", "--format", "yaml"},                               // bad format
		{"send", "--to", "x", "--from", "y", "--body", "a", "--body-file", "b"}, // both bodies
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != 2 {
			t.Fatalf("args %v: want exit 2, got %d", args, code)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, stdout, _ := run("help")
	if code != 0 || !strings.Contains(stdout, "tincan") {
		t.Fatalf("help: code=%d out=%q", code, stdout)
	}
}

var _ = io.Discard // keep io imported for later tasks
