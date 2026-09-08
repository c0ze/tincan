package host

import (
	"context"
	"strings"
	"testing"
)

const claudeMCPDiagnosticFixture = "Client.listTools() called but server does not advertise tools capability - returning empty list"

func TestSessionClaudeMCPDiagnosticAfterFinal(t *testing.T) {
	s, err := PrepareSession(t.TempDir(), "worker", "claude", Builtin()["claude"])
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code 2.1.252 emitted this SDK diagnostic after a successful result
	// on the Mac mini, causing a completed answer to become an output-sink error.
	fixture := sessionFixture("claude", s.record.ID) + claudeMCPDiagnosticFixture + "\n"
	if progress := feedSession(t, s, fixture); progress != "ANSWER" {
		t.Fatalf("progress = %q", progress)
	}
	body, err := s.Reply(RunSpec{}, Result{ExitCode: 0})
	if err != nil || body != "ANSWER" {
		t.Fatalf("final reply = %q, %v", body, err)
	}
}

func TestSessionClaudeMCPDiagnosticThroughRunner(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	s, err := PrepareSession(t.TempDir(), "worker", "claude", Builtin()["claude"])
	if err != nil {
		t.Fatal(err)
	}
	fixture := sessionFixture("claude", s.record.ID) + claudeMCPDiagnosticFixture + "\n"
	var progress strings.Builder
	spec := RunSpec{Argv: fakeExec("stdout", fixture), Output: func(stream string, data []byte) error {
		filtered, err := s.Output(stream, data)
		progress.Write(filtered)
		return err
	}}
	res := Run(context.Background(), spec)
	body, err := s.Reply(spec, res)
	if res.Err != nil || res.ExitCode != 0 || err != nil || body != "ANSWER" || progress.String() != "ANSWER" {
		t.Fatalf("result=%+v reply=%q err=%v progress=%q", res, body, err, progress.String())
	}
}

func TestSessionClaudeMCPDiagnosticFraming(t *testing.T) {
	for _, position := range []string{"before", "between", "after without newline"} {
		t.Run(position, func(t *testing.T) {
			s, err := PrepareSession(t.TempDir(), "worker", "claude", Builtin()["claude"])
			if err != nil {
				t.Fatal(err)
			}
			fixture := sessionFixture("claude", s.record.ID)
			switch position {
			case "before":
				fixture = claudeMCPDiagnosticFixture + "\r\n" + fixture
			case "between":
				fixture = strings.Replace(fixture, "\n", "\n"+claudeMCPDiagnosticFixture+"\n", 1)
			case "after without newline":
				fixture += claudeMCPDiagnosticFixture
			}
			if progress := feedSession(t, s, fixture); progress != "ANSWER" {
				t.Fatalf("progress = %q", progress)
			}
			body, err := s.Reply(RunSpec{}, Result{ExitCode: 0})
			if err != nil || body != "ANSWER" {
				t.Fatalf("final reply = %q, %v", body, err)
			}
		})
	}
}

func TestSessionClaudeDiagnosticDoesNotHideFailures(t *testing.T) {
	for _, trailing := range []string{"not-json\n", "{\"type\":", "null\n", claudeMCPDiagnosticFixture + " unexpected suffix\n"} {
		s, err := PrepareSession(t.TempDir(), "worker", "claude", Builtin()["claude"])
		if err != nil {
			t.Fatal(err)
		}
		feedSession(t, s, sessionFixture("claude", s.record.ID)+claudeMCPDiagnosticFixture+"\n")
		_, err = s.Output("stdout", []byte(trailing))
		if err == nil {
			_, err = s.Reply(RunSpec{}, Result{ExitCode: 0})
		}
		if err == nil {
			t.Fatalf("invalid output after a successful result was accepted: %q", trailing)
		}
	}
	for _, provider := range []string{"claude", "grok", "agy", "kimi"} {
		s, err := PrepareSession(t.TempDir(), "worker", provider, Builtin()[provider])
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Output("stdout", []byte(claudeMCPDiagnosticFixture+"\n"))
		if provider != "claude" && err == nil {
			t.Fatalf("Claude diagnostic was accepted for %s", provider)
		}
		if _, err := s.Reply(RunSpec{}, Result{ExitCode: 0}); err == nil {
			t.Fatalf("diagnostic without a final answer was accepted for %s", provider)
		}
	}
}
