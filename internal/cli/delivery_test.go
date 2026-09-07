package cli

import (
	"bytes"
	"errors"
	"testing"
)

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("disk full") }

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
