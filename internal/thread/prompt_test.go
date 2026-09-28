// internal/thread/prompt_test.go
package thread

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func msg(author, text string) Message {
	return Message{Author: author, Role: RoleAgent, Text: text, State: StateDone}
}

func TestPromptContainsHeaderTranscriptAndTrigger(t *testing.T) {
	p := BuildPrompt(PromptInput{
		Listener: "codex.t7f2a9c1", Title: "audit", Room: "/w/app",
		Participants: []string{"claude.t7f2a9c1", "codex.t7f2a9c1"},
		Transcript:   []Message{{Author: "you", Role: RoleUser, Text: "fix it"}, msg("claude.t7f2a9c1", "fixed")},
		Trigger:      msg("claude.t7f2a9c1", "@codex review"),
	})
	for _, want := range []string{"You are codex.t7f2a9c1", "'audit'", "/w/app", "git diff", "@you", "you: fix it", "claude.t7f2a9c1: fixed", "claude.t7f2a9c1: @codex review"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestPromptTruncatesOldestAndCountsOmitted(t *testing.T) {
	var tr []Message
	for i := 0; i < 400; i++ {
		tr = append(tr, msg("claude.t", strings.Repeat("x", 1024)))
	}
	p := BuildPrompt(PromptInput{Listener: "a", Title: "t", Room: "/r", Transcript: tr, Trigger: msg("you", "go")})
	if len(p) > MaxPromptBytes {
		t.Fatalf("prompt %d bytes", len(p))
	}
	if !strings.Contains(p, "earlier messages omitted]") {
		t.Fatal("no omitted marker")
	}
	if !strings.HasSuffix(strings.TrimSpace(p), "you: go") {
		t.Fatal("trigger not last")
	}
}

func TestPromptCutsOversizedTriggerAtRuneBoundary(t *testing.T) {
	big := strings.Repeat("ü", MaxTriggerBytes) // 2 bytes each
	p := BuildPrompt(PromptInput{Listener: "a", Title: "t", Room: "/r", Trigger: Message{ID: "m1", Author: "you", Text: big}})
	if len(p) > MaxPromptBytes || !utf8.ValidString(p) || !strings.Contains(p, "[… truncated") {
		t.Fatalf("len %d valid %v", len(p), utf8.ValidString(p))
	}
}

func TestTranscriptOfSkipsUnfinishedAndTrigger(t *testing.T) {
	s := newSnapshot(Meta{})
	s.Messages = []Message{
		{ID: "1", Author: "you", Role: RoleUser, Text: "a"},
		{ID: "2", Author: "claude.t", Role: RoleAgent, State: StateRunning},
		{ID: "3", Author: "codex.t", Role: RoleAgent, State: StateError, Text: "ERROR boom"},
		{ID: "4", Author: "you", Role: RoleUser, Text: "trigger"},
		{ID: "5", Author: "system", Role: RoleSystem, State: StateSuggested, Text: "@x please"},
	}
	got := transcriptOf(s, "4")
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("transcript %+v", got)
	}
}
