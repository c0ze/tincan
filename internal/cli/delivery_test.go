package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// `send --to Codex` then `recv --as codex` on a case-folding filesystem used to
// time out and quarantine the message. The second spelling is now refused by
// name and the message stays deliverable under the first.
func TestCaseAliasedRecvIsRefusedAndMessageKept(t *testing.T) {
	room := t.TempDir()
	if code, _, err := run("send", "--room", room, "--to", "Codex", "--from", "orch", "--body", "kept"); code != 0 {
		t.Fatal(err)
	}
	upper, err1 := os.Lstat(filepath.Join(room, ".tincan", "inbox", "Codex"))
	lower, err2 := os.Lstat(filepath.Join(room, ".tincan", "inbox", "codex"))
	if err1 != nil || err2 != nil || !os.SameFile(upper, lower) {
		t.Skip("case-sensitive filesystem")
	}
	code, _, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "1")
	if code != ExitError || !strings.Contains(stderr, `use "Codex"`) {
		t.Fatalf("aliased recv: exit %d, stderr=%q", code, stderr)
	}
	code, out, stderr := run("recv", "--room", room, "--as", "Codex", "--timeout", "1", "--format", "body")
	if code != 0 || out != "kept\n" {
		t.Fatalf("message lost: %d %q %q", code, out, stderr)
	}
}

func TestFailedOutputLeavesMessageAvailable(t *testing.T) {
	for _, format := range []string{"body", "json"} {
		t.Run(format, func(t *testing.T) {
			room := t.TempDir()
			if code, _, err := run("send", "--room", room, "--to", "worker", "--from", "test", "--body", "recover me"); code != 0 {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			if code := Run([]string{"recv", "--room", room, "--as", "worker", "--timeout", "1", "--format", format}, brokenOutput{}, &stderr); code != ExitError {
				t.Fatalf("got %d", code)
			}
			code, out, err := run("recv", "--room", room, "--as", "worker", "--timeout", "1", "--format", "body")
			if code != 0 || out != "recover me\n" {
				t.Fatalf("message lost: %d %q %q", code, out, err)
			}
		})
	}
}
