// internal/thread/prompt.go
package thread

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxPromptBytes  = 256 << 10
	MaxTriggerBytes = 128 << 10
	MaxPostBytes    = 128 << 10
	maxHeaderBytes  = 2 << 10
)

type PromptInput struct {
	Listener     string
	Title        string
	Room         string
	Participants []string
	Transcript   []Message
	Trigger      Message
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func line(m Message) string {
	text := m.Text
	if m.State == StateError {
		text = "[error] " + text
	}
	return m.Author + ": " + text + "\n"
}

func BuildPrompt(in PromptInput) string {
	header := fmt.Sprintf("You are %s in tincan thread '%s' (room %s). Participants: you (the owner, @you), %s. "+
		"All participants work in this same directory; use `git status` and `git diff` to see changes others made. "+
		"Mention another participant with @name to hand them work. Your reply is posted to the thread.\n\n",
		in.Listener, in.Title, in.Room, strings.Join(in.Participants, ", "))
	header = cutUTF8(header, maxHeaderBytes)

	trigger := in.Trigger.Text
	if len(trigger) > MaxTriggerBytes {
		trigger = cutUTF8(trigger, MaxTriggerBytes) + fmt.Sprintf("\n[… truncated, full text in thread message %s]", in.Trigger.ID)
	}
	tail := "Message to answer:\n" + in.Trigger.Author + ": " + trigger + "\n"

	budget := MaxPromptBytes - len(header) - len(tail) - 128
	lines := make([]string, len(in.Transcript))
	for i, m := range in.Transcript {
		lines[i] = line(m)
	}
	start, size := len(lines), 0
	for start > 0 && size+len(lines[start-1]) <= budget {
		start--
		size += len(lines[start])
	}
	var b strings.Builder
	b.WriteString(header)
	if len(lines) > 0 {
		b.WriteString("Thread so far:\n")
		if start > 0 {
			fmt.Fprintf(&b, "[%d earlier messages omitted]\n", start)
		}
		for _, l := range lines[start:] {
			b.WriteString(l)
		}
		b.WriteString("\n")
	}
	b.WriteString(tail)
	return b.String()
}

// transcriptOf lists finished messages (user and system text, completed or
// failed agent turns) except excludeID, in display order.
func transcriptOf(s *Snapshot, excludeID string) []Message {
	var out []Message
	for _, m := range s.Messages {
		if m.ID == excludeID || m.State == StateSuggested {
			continue
		}
		if m.Role == RoleAgent && m.State != StateDone && m.State != StateError {
			continue
		}
		if m.Role == RoleCommittee && (m.State == StatePending || m.State == StateRunning) {
			continue
		}
		out = append(out, m)
	}
	return out
}
