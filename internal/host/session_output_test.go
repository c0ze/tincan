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

// One stdout event over the in-memory limit (a huge tool result, say) is
// skipped up to its newline; the run fails only without a final result.
func TestSessionOversizedEventIsSkipped(t *testing.T) {
	huge := "{\"type\":\"user\",\"tool_result\":\"" + strings.Repeat("x", 3*MaxCapturedOutput) + "\"}\n"
	for _, final := range []bool{true, false} {
		s, err := PrepareSession(t.TempDir(), "worker", "claude", Builtin()["claude"])
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitAfter(sessionFixture("claude", s.record.ID), "\n")
		fixture := lines[0] + lines[1] + huge
		if final {
			fixture += lines[2]
		}
		for len(fixture) > 0 {
			n := min(64<<10, len(fixture))
			if _, err := s.Output("stdout", []byte(fixture[:n])); err != nil {
				t.Fatalf("oversized event failed the stream: %v", err)
			}
			if cap(s.parser.pending) > 2*MaxCapturedOutput {
				t.Fatalf("parser buffered %d bytes", cap(s.parser.pending))
			}
			fixture = fixture[n:]
		}
		body, err := s.Reply(RunSpec{}, Result{ExitCode: 0})
		if final && (err != nil || body != "ANSWER") {
			t.Fatalf("final reply = %q, %v", body, err)
		}
		if !final && (err == nil || !strings.Contains(err.Error(), "output limit")) {
			t.Fatalf("missing final result after a skipped event: %q, %v", body, err)
		}
	}
}

// A skipped event may have carried part or all of the answer. Only a later
// event that replaces the answer whole makes the run successful again.
func TestSessionSkippedEventNeverYieldsPartialAnswer(t *testing.T) {
	big := strings.Repeat("x", MaxCapturedOutput+1)
	cases := []struct {
		provider, fixture string
		ok                bool
	}{
		{"kimi", "{\"role\":\"assistant\",\"content\":\"COMMENTARY\"}\n{\"role\":\"assistant\",\"content\":\"" + big + "\"}\n" +
			"{\"role\":\"meta\",\"type\":\"session.resume_hint\",\"session_id\":\"%ID%\"}\n", false},
		{"kimi", "{\"role\":\"assistant\",\"content\":\"" + big + "\"}\n{\"role\":\"assistant\",\"content\":\"ANSWER\"}\n" +
			"{\"role\":\"meta\",\"type\":\"session.resume_hint\",\"session_id\":\"%ID%\"}\n", true},
		{"grok", "{\"type\":\"text\",\"data\":\"ANS\"}\n{\"type\":\"text\",\"data\":\"" + big + "\"}\n" +
			"{\"type\":\"end\",\"stopReason\":\"end_turn\",\"sessionId\":\"%ID%\"}\n", false},
		{"grok", "{\"type\":\"text\",\"data\":\"" + big + "\"}\n{\"type\":\"tool_call\"}\n{\"type\":\"text\",\"data\":\"ANSWER\"}\n" +
			"{\"type\":\"end\",\"stopReason\":\"end_turn\",\"sessionId\":\"%ID%\"}\n", true},
		{"agy", "{\"event\":\"init\",\"conversation_id\":\"%ID%\"}\n{\"event\":\"result\",\"result\":{\"conversation_id\":\"%ID%" +
			"\",\"status\":\"SUCCESS\",\"response\":\"ANSWER\"}}\n{\"event\":\"step_update\",\"pad\":\"" + big + "\"}\n", false},
	}
	for i, c := range cases {
		s, err := PrepareSession(t.TempDir(), "worker", c.provider, Builtin()[c.provider])
		if err != nil {
			t.Fatal(err)
		}
		id := s.record.ID
		if id == "" {
			id = fixtureSessionID(c.provider)
		}
		if _, err := s.Output("stdout", []byte(strings.ReplaceAll(c.fixture, "%ID%", id))); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		body, err := s.Reply(RunSpec{}, Result{ExitCode: 0})
		if c.ok && (err != nil || body != "ANSWER") {
			t.Errorf("case %d (%s): reply = %q, %v", i, c.provider, body, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "output limit")) {
			t.Errorf("case %d (%s): partial answer accepted: %q, %v", i, c.provider, body, err)
		}
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
	for _, provider := range []string{"claude", "grok", "agy", "kimi", "cursor"} {
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
