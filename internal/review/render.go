package review

import (
	"fmt"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/packet"
)

const (
	MaxPrompt = 128 << 10
	MaxBundle = 1 << 20
)

// BuildPrompt is the coordinator's mode-neutral prompt for one member
// (committees §6.8); the receiver prepends its own workspace note.
func BuildPrompt(c committee.Committee, member string, p *packet.Packet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are reviewing a change for the owner as member `%s` of committee `%s`. Review only: do not modify, create or delete files, and do not run commands that change state. Cite file:line.\n", member, c.Name)
	var omitted []packet.Change
	for _, ch := range p.Manifest.Changes {
		if !ch.Included {
			omitted = append(omitted, ch)
		}
	}
	if len(omitted) > 0 {
		b.WriteString("\nThese changed paths were not sent to you:\n")
		for i, ch := range omitted {
			if i == 200 {
				fmt.Fprintf(&b, "- … and %d more\n", len(omitted)-200)
				break
			}
			fmt.Fprintf(&b, "- %s (%s)\n", ch.Path, ch.Reason)
		}
	}
	if !p.Manifest.Complete {
		b.WriteString("\nThis is a partial review: the full patch was too large, so only some post-change file contents are available.\n")
	}
	if c.Instructions != "" {
		b.WriteString("\nCommittee instructions:\n" + c.Instructions + "\n")
	}
	b.WriteString("\nQuestion:\n" + p.Question + "\n")
	s := b.String()
	if len(s) > MaxPrompt {
		s = s[:MaxPrompt-32] + "\n[prompt truncated]\n"
	}
	return s
}

// RenderBundle renders bundle.md from the closure snapshot alone, so a
// re-render after a crash is byte-identical (committees §6.4, §6.9).
func RenderBundle(in Input, st State, results map[int]string) []byte {
	if st.Closure == nil {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Committee `%s` (v%d) review\n\n", in.Committee.Name, in.Committee.Version)
	fmt.Fprintf(&b, "Scope: %s", in.Scope)
	if in.IncludedTree != "" {
		fmt.Fprintf(&b, " · included tree %s", in.IncludedTree)
	}
	fmt.Fprintf(&b, "\nClosed: %s\n", st.Closure.At.UTC().Format(time.RFC3339))
	var late []string
	for _, cm := range st.Closure.Members {
		if cm.Late {
			late = append(late, st.Members[cm.Index].Member)
		}
	}
	if len(late) > 0 {
		fmt.Fprintf(&b, "Late (still running at close): %s\n", strings.Join(late, ", "))
	}
	n := len(st.Closure.Members)
	per := 200 << 10
	if n > 0 && (896<<10)/n < per {
		per = (896 << 10) / n
	}
	for _, cm := range st.Closure.Members {
		m := st.Members[cm.Index]
		switch {
		case cm.Late:
			fmt.Fprintf(&b, "\n## %s — late (still running)\n", m.Member)
		case cm.State == "done":
			fmt.Fprintf(&b, "\n## %s — done in %s\n", m.Member, m.Finished.Sub(m.Started).Round(time.Second))
		case cm.State == "skipped" || cm.State == "unreachable" || cm.State == "error" || cm.State == "expired":
			fmt.Fprintf(&b, "\n## %s — %s", m.Member, cm.State)
			if m.Note != "" {
				fmt.Fprintf(&b, ": %s", m.Note)
			}
			b.WriteString("\n")
		default:
			fmt.Fprintf(&b, "\n## %s — %s\n", m.Member, cm.State)
		}
		if cm.Included {
			r := results[cm.Index]
			if len(r) > per {
				r = r[:per] + fmt.Sprintf("\n\n[truncated; full result in results/%d.md]", cm.Index)
			}
			b.WriteString("\n" + r + "\n")
		}
	}
	out := b.String()
	if len(out) > MaxBundle {
		out = out[:MaxBundle-40] + "\n[bundle truncated]\n"
	}
	return []byte(out)
}
