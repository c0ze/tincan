package thread

import "strings"

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isNameByte(c byte) bool { return isNameStart(c) || c == '.' || c == '_' || c == '-' }

func mentionBoundary(prev byte) bool {
	return prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' || prev == '(' || prev == '[' || prev == ','
}

// stripCode blanks fenced blocks and inline code spans so mentions inside
// them are ignored; newlines are kept so boundaries stay intact.
func stripCode(text string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			b.WriteString("\n")
			continue
		}
		if inFence {
			b.WriteString("\n")
			continue
		}
		inCode := false
		for i := 0; i < len(line); i++ {
			if line[i] == '`' {
				inCode = !inCode
				b.WriteByte(' ')
				continue
			}
			if inCode && line[i] != '\n' {
				b.WriteByte(' ')
				continue
			}
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

func ParseMentions(text string) []string {
	s := stripCode(text)
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(s); i++ {
		if s[i] != '@' || (i > 0 && !mentionBoundary(s[i-1])) || i+1 >= len(s) || !isNameStart(s[i+1]) {
			continue
		}
		j := i + 1
		for j < len(s) && isNameByte(s[j]) {
			j++
		}
		name := strings.TrimRight(s[i+1:j], "._-")
		if len(name) > 64 {
			name = name[:64]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}
