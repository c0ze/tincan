package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/internal/filelock"
	"github.com/c0ze/tincan/internal/fsutil"
)

func TestSessionPoliciesAndCustomExecutables(t *testing.T) {
	for _, label := range Names(Builtin()) {
		p, err := WithSession(Builtin()[label], label, "")
		if err != nil {
			t.Fatal(err)
		}
		if (p.Session == "persistent") != SupportsSessions(label) {
			t.Errorf("unexpected default for %s: %s", label, p.Session)
		}
	}
	p := Builtin()["claude"]
	p.Exec = []string{"custom-wrapper", "--argument", "{body}"}
	auto, err := WithSession(p, "claude", "")
	if err != nil || auto.Session != "stateless" || !reflect.DeepEqual(auto.Exec, p.Exec) {
		t.Fatalf("custom executable was implicitly adapted: %+v, %v", auto, err)
	}
	explicit, err := WithSession(p, "claude", "persistent")
	if err != nil || explicit.Session != "persistent" || !reflect.DeepEqual(explicit.Exec, p.Exec) {
		t.Fatalf("explicit adapter changed command: %+v, %v", explicit, err)
	}
	for _, mode := range []string{"invalid", "persistent"} {
		if _, err := WithSession(p, "unknown", mode); err == nil {
			t.Errorf("accepted unsupported mode/provider: %s", mode)
		}
	}
	config := writeConfig(t, `{"claude":{"exec":["claude","-p","{body}"],"session":"stateless"}}`)
	loaded, err := LoadConfig(config)
	if err != nil || loaded["claude"].Session != "stateless" {
		t.Fatalf("session config lost: %+v, %v", loaded, err)
	}
	data, err := json.Marshal(explicit)
	var detached Preset
	if err != nil || json.Unmarshal(data, &detached) != nil || detached.Session != "persistent" {
		t.Fatal("detached preset lost session mode")
	}
}

func sessionFixture(provider, id string) string {
	switch provider {
	case "claude":
		return fmt.Sprintf("{\"type\":\"system\",\"session_id\":%q,\"plugins\":[\"PRIVATE_PLUGIN_INVENTORY\"]}\n"+
			"{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"thinking\",\"thinking\":\"PRIVATE_THOUGHT\"},{\"type\":\"text\",\"text\":\"ANSWER\"}]}}\n"+
			"{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":%q,\"is_error\":false,\"result\":\"ANSWER\"}\n", id, id)
	case "grok":
		return fmt.Sprintf("{\"type\":\"available_commands\",\"data\":[\"PRIVATE_PLUGIN_INVENTORY\"]}\n"+
			"{\"type\":\"thought\",\"data\":\"PRIVATE_THOUGHT\"}\n"+
			"{\"type\":\"text\",\"data\":\"ANS\"}\n{\"type\":\"text\",\"data\":\"WER\"}\n"+
			"{\"type\":\"end\",\"stopReason\":\"end_turn\",\"sessionId\":%q}\n", id)
	case "agy":
		return fmt.Sprintf("{\"event\":\"init\",\"conversation_id\":%q,\"init\":{\"tools\":[\"PRIVATE_PLUGIN_INVENTORY\"]}}\n"+
			"{\"event\":\"step_update\",\"step_update\":{\"step_type\":\"agent_response\",\"text_delta\":\"ANSWER\"}}\n"+
			"{\"event\":\"result\",\"result\":{\"conversation_id\":%q,\"status\":\"SUCCESS\",\"response\":\"ANSWER\\n\"}}\n", id, id)
	case "kimi":
		return fmt.Sprintf("{\"role\":\"meta\",\"type\":\"system.version\",\"version\":\"PRIVATE_PLUGIN_INVENTORY\"}\n"+
			"{\"role\":\"assistant\",\"content\":\"ANSWER\"}\n"+
			"{\"role\":\"meta\",\"type\":\"session.resume_hint\",\"session_id\":%q,\"content\":\"To resume this session: PRIVATE_HINT\"}\n", id)
	}
	panic(provider)
}

func fixtureSessionID(provider string) string {
	id := "11111111-1111-4111-8111-111111111111"
	if provider == "kimi" {
		id = "session_" + id
	}
	return id
}

func feedSession(t *testing.T, s *SessionRun, fixture string) string {
	t.Helper()
	var progress strings.Builder
	// Splitting across JSON tokens and strings exercises real pipe chunking.
	for len(fixture) > 0 {
		n := min(7, len(fixture))
		out, err := s.Output("stdout", []byte(fixture[:n]))
		if err != nil {
			t.Fatal(err)
		}
		progress.Write(out)
		fixture = fixture[n:]
	}
	return progress.String()
}

func TestSessionProviderFormatsResumeAndIsolation(t *testing.T) {
	for _, label := range []string{"claude", "grok", "agy", "kimi"} {
		t.Run(label, func(t *testing.T) {
			room := t.TempDir()
			p := Builtin()[label]
			first, err := PrepareSession(room, "one", label, p)
			if err != nil {
				t.Fatal(err)
			}
			id := first.record.ID
			if id == "" {
				id = fixtureSessionID(label)
			}
			progress := feedSession(t, first, sessionFixture(label, id))
			if progress != "ANSWER" {
				t.Fatalf("progress leaked metadata or lost assistant text: %q", progress)
			}
			body, err := first.Reply(RunSpec{}, Result{ExitCode: 0})
			if err != nil || body != "ANSWER" {
				t.Fatalf("final reply = %q, %v", body, err)
			}
			second, err := PrepareSession(room, "one", label, p)
			if err != nil {
				t.Fatal(err)
			}
			if !hasExactArg(second.Preset.Exec, id) || hasExactArg(second.Preset.Exec, "--continue") || hasExactArg(second.Preset.Exec, "--session-id") {
				t.Fatalf("not an explicit resume: %q", second.Preset.Exec)
			}
			feedSession(t, second, sessionFixture(label, id))
			if _, err := second.Reply(RunSpec{}, Result{ExitCode: 0}); err != nil {
				t.Fatal(err)
			}
			for _, target := range []struct{ room, name string }{{room, "two"}, {t.TempDir(), "one"}} {
				other, err := PrepareSession(target.room, target.name, label, p)
				if err != nil {
					t.Fatal(err)
				}
				if hasExactArg(other.Preset.Exec, id) || other.record.Ready {
					t.Fatalf("session crossed room/listener boundary: %q", other.Preset.Exec)
				}
			}
			changed := p
			changed.Exec = append(append([]string(nil), p.Exec...), "--model", "different")
			if _, err := PrepareSession(room, "one", label, changed); err == nil {
				t.Fatal("changed preset silently reused existing session")
			}
			if _, err := PrepareSession(room, "one", "agy", Builtin()["agy"]); label != "agy" && err == nil {
				t.Fatal("changed provider silently reused existing session")
			}
		})
	}
}

func TestSessionPendingAndFailureNeverReset(t *testing.T) {
	room := t.TempDir()
	p := Builtin()["agy"]
	first, err := PrepareSession(room, "worker", "agy", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSession(room, "worker", "agy", p); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("pending initialization silently restarted: %v", err)
	}
	feedSession(t, first, sessionFixture("agy", fixtureSessionID("agy")))
	before, err := os.ReadFile(first.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []Result{
		{ExitCode: 1, Stderr: []byte("resume missing"), Stdout: []byte("PRIVATE_PLUGIN_INVENTORY")},
		{ExitCode: -1, Err: errors.New("start failed")},
		{Killed: true}, {TimedOut: true},
	} {
		resumed, err := PrepareSession(room, "worker", "agy", p)
		if err != nil {
			t.Fatal(err)
		}
		body, err := resumed.Reply(RunSpec{}, failure)
		if err != nil || !strings.HasPrefix(body, "ERROR ") || strings.Contains(body, "PRIVATE_PLUGIN") {
			t.Fatalf("failure reply = %q, %v", body, err)
		}
		after, _ := os.ReadFile(first.path)
		if string(after) != string(before) {
			t.Fatal("failure rewrote saved session")
		}
	}
	resumed, err := PrepareSession(room, "worker", "agy", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Output("stdout", []byte("{\"event\":\"init\",\"conversation_id\":\"replacement\"}\n")); err == nil {
		t.Fatal("unexpected provider session change accepted")
	}
	after, _ := os.ReadFile(first.path)
	if string(after) != string(before) {
		t.Fatal("session mismatch replaced saved pointer")
	}
}

func TestSessionMalformedIncompleteAndErrorReplies(t *testing.T) {
	cases := map[string]string{
		"invalid JSON":    "not-json\n",
		"truncated JSON":  "{\"event\":",
		"missing final":   "{\"event\":\"init\",\"conversation_id\":\"one\"}\n",
		"empty answer":    "{\"event\":\"result\",\"result\":{\"conversation_id\":\"one\",\"status\":\"SUCCESS\",\"response\":\" \"}}\n",
		"provider error":  "{\"event\":\"result\",\"result\":{\"conversation_id\":\"one\",\"status\":\"ERROR\",\"response\":\"failed resume\"}}\n",
		"missing ID":      "{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"answer\"}}\n",
		"oversized event": strings.Repeat("x", MaxCapturedOutput+1),
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := PrepareSession(t.TempDir(), "worker", "agy", Builtin()["agy"])
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Output("stdout", []byte(fixture))
			if err == nil {
				_, err = s.Reply(RunSpec{}, Result{ExitCode: 0})
			}
			if err == nil {
				t.Fatal("bad provider reply was accepted")
			}
		})
	}
}

func TestSessionResetRequiresStoppedHostAndExcludesLaunch(t *testing.T) {
	room, name := t.TempDir(), "worker"
	s, err := PrepareSession(room, name, "agy", Builtin()["agy"])
	if err != nil {
		t.Fatal(err)
	}
	feedSession(t, s, sessionFixture("agy", fixtureSessionID("agy")))
	lifetime, err := filelock.Try(lifetimePath(room, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := ClearSession(context.Background(), room, name); err == nil {
		t.Fatal("reset accepted a running host")
	}
	lifetime.Close()
	launch, err := filelock.Try(launchPath(room, name))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := ClearSession(ctx, room, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reset raced launch: %v", err)
	}
	launch.Close()
	if _, err := os.Stat(s.path); err != nil {
		t.Fatalf("failed reset lost state: %v", err)
	}
	if err := ClearSession(context.Background(), room, name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session reset did not remove pointer: %v", err)
	}
	fresh, err := PrepareSession(room, name, "agy", Builtin()["agy"])
	if err != nil || fresh.record.Ready || hasExactArg(fresh.Preset.Exec, fixtureSessionID("agy")) {
		t.Fatalf("reset did not start fresh: %+v, %v", fresh, err)
	}
}

func TestSessionOptionsAndPrivateStorage(t *testing.T) {
	for _, flag := range []string{"--continue", "--resume=other", "--session-id", "--conversation", "--no-session-persistence"} {
		p := Builtin()["claude"]
		p.Exec = append(p.Exec, flag)
		if _, err := PrepareSession(t.TempDir(), "worker", "claude", p); err == nil {
			t.Errorf("accepted conflicting option %s", flag)
		}
	}
	args, err := sessionArgs([]string{"wrapper", "--output-format=text", "--", "{body}"}, "claude")
	if err != nil || !reflect.DeepEqual(args, []string{"wrapper", "--output-format", "stream-json", "--verbose", "--", "{body}"}) {
		t.Fatalf("adapter options misplaced: %q, %v", args, err)
	}
	room := t.TempDir()
	s, err := PrepareSession(room, "worker", "agy", Builtin()["agy"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsutil.ReadFile(s.path, 16<<10); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.path), "corrupt.json"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSession(room, "corrupt", "agy", Builtin()["agy"]); err == nil {
		t.Fatal("corrupt saved session was ignored")
	}
}

func TestSessionLongStreamDoesNotDependOnCapturedTail(t *testing.T) {
	s, err := PrepareSession(t.TempDir(), "worker", "agy", Builtin()["agy"])
	if err != nil {
		t.Fatal(err)
	}
	init := fmt.Sprintf("{\"event\":\"init\",\"conversation_id\":%q}\n", fixtureSessionID("agy"))
	if _, err := s.Output("stdout", []byte(init)); err != nil {
		t.Fatal(err)
	}
	metadata := []byte("{\"event\":\"metadata\",\"private\":\"" + strings.Repeat("x", 32<<10) + "\"}\n")
	for i := 0; i < 40; i++ { // total stream exceeds Run's 1 MiB capture tail
		if progress, err := s.Output("stdout", metadata); err != nil || len(progress) != 0 {
			t.Fatalf("metadata escaped or parser rejected bounded events: %d, %v", len(progress), err)
		}
	}
	final := []byte("{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"ANSWER\"}}")
	if _, err := s.Output("stdout", final); err != nil {
		t.Fatal(err)
	}
	// The parser saw every byte, including an early ID that is no longer in the
	// process capture, and flush handles a final line without a trailing newline.
	body, err := s.Reply(RunSpec{}, Result{ExitCode: 0, Stdout: []byte("truncated capture")})
	if err != nil || body != "ANSWER" {
		t.Fatalf("lost early state or final answer: %q, %v", body, err)
	}
}

func TestSessionStructuredProviderFailures(t *testing.T) {
	failures := map[string]string{
		"claude": `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"denied"}`,
		"grok":   `{"type":"end","stopReason":"max_tokens"}`,
		"agy":    `{"event":"result","result":{"status":"ERROR","response":"denied"}}`,
		"kimi":   `{"type":"error","error":"denied"}`,
	}
	for provider, event := range failures {
		t.Run(provider, func(t *testing.T) {
			s, err := PrepareSession(t.TempDir(), "worker", provider, Builtin()[provider])
			if err != nil {
				t.Fatal(err)
			}
			id := s.record.ID
			if id == "" {
				id = fixtureSessionID(provider)
			}
			feedSession(t, s, sessionFixture(provider, id))
			if _, err := s.Output("stdout", []byte(event+"\n")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reply(RunSpec{}, Result{ExitCode: 0}); err == nil || !strings.Contains(err.Error(), "provider failed") {
				t.Fatalf("provider failure was not explicit: %v", err)
			}
			if body, err := s.Reply(RunSpec{}, Result{ExitCode: 1, Stdout: []byte("PRIVATE_PLUGIN_INVENTORY")}); err == nil || !strings.Contains(err.Error(), "provider failed (exit=1)") || strings.Contains(err.Error()+body, "PRIVATE_PLUGIN") {
				t.Fatalf("nonzero provider failure was masked or leaked metadata: %q, %v", body, err)
			}
		})
	}
}

func TestSessionNonzeroErrorFinalLineWithoutNewline(t *testing.T) {
	s, err := PrepareSession(t.TempDir(), "worker", "claude", Builtin()["claude"])
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"result","subtype":"success","session_id":%q,"is_error":true,"result":"API Error: request declined"}`, s.record.ID)
	if _, err := s.Output("stdout", []byte(line)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(RunSpec{}, Result{ExitCode: 1}); err == nil || !strings.Contains(err.Error(), "API Error: request declined") {
		t.Fatalf("structured nonzero error lost: %v", err)
	}
}

func TestSessionIDsCannotSelectTitlesOrMostRecent(t *testing.T) {
	for _, provider := range []string{"claude", "grok", "agy", "kimi"} {
		if !validSessionID(provider, fixtureSessionID(provider)) {
			t.Fatalf("rejected valid %s ID", provider)
		}
		for _, id := range []string{"", "last", "latest", "session-title", "../escape", "--continue"} {
			if validSessionID(provider, id) {
				t.Fatalf("accepted %s resume selector %q as an ID", provider, id)
			}
		}
	}
}

func TestSessionStderrStaysPrivateButFailuresKeepDetails(t *testing.T) {
	s, err := PrepareSession(t.TempDir(), "worker", "claude", Builtin()["claude"])
	if err != nil {
		t.Fatal(err)
	}
	if progress, err := s.Output("stderr", []byte("PRIVATE_PLUGIN_INVENTORY")); err != nil || len(progress) != 0 {
		t.Fatalf("persistent stderr escaped as progress: %q, %v", progress, err)
	}
	body, err := s.Reply(RunSpec{}, Result{ExitCode: 2, Stderr: []byte("invalid provider option")})
	if err != nil || !strings.Contains(body, "invalid provider option") {
		t.Fatalf("terminal stderr detail was lost: %q, %v", body, err)
	}
	legacy := &SessionRun{Preset: Preset{Session: "stateless"}}
	if progress, err := legacy.Output("stderr", []byte("legacy diagnostic")); err != nil || string(progress) != "legacy diagnostic" {
		t.Fatalf("stateless stream behavior changed: %q, %v", progress, err)
	}
}
