package thread

import "strings"

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isNameByte(c byte) bool { return isNameStart(c) || c == '.' || c == '_' || c == '-' }

// mentionBoundary reports bytes a mention may follow: whitespace, opening
// brackets, a comma, emphasis markers (**@codex**, _@codex_) and a colon
// (cc:@codex). A letter or digit never does (mail addresses).
func mentionBoundary(prev byte) bool {
	switch prev {
	case ' ', '\t', '\n', '\r', '(', '[', ',', '*', '_', ':':
		return true
	}
	return false
}

// fence reports a line that opens or closes a fenced code block: optional
// indentation, then three or more backticks or tildes (ch, n), then the
// info string. As in CommonMark, a backtick fence's info string may not
// contain a backtick, so "```go test```" is inline code, not a fence.
func fence(line string) (ch byte, n int, info string) {
	s := strings.TrimLeft(line, " \t")
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0, ""
	}
	for n < len(s) && s[n] == s[0] {
		n++
	}
	info = strings.TrimSpace(s[n:])
	if n < 3 || (s[0] == '`' && strings.IndexByte(info, '`') >= 0) {
		return 0, 0, ""
	}
	return s[0], n, info
}

// stripCode blanks fenced blocks, inline code spans and quoted lines so
// mentions inside them are ignored: quoting someone's "@codex" is not a
// request to it. Newlines are kept so boundaries stay intact.
func stripCode(text string) string {
	var b strings.Builder
	var fenceCh byte
	fenceLen := 0 // > 0 inside a fenced block
	for _, line := range strings.SplitAfter(text, "\n") {
		if fenceLen > 0 {
			// Only a bare run of the same character, at least as long as
			// the opening one, closes the block.
			if ch, n, info := fence(line); ch == fenceCh && n >= fenceLen && info == "" {
				fenceLen = 0
			}
			b.WriteString("\n")
			continue
		}
		if ch, n, _ := fence(line); n > 0 {
			fenceCh, fenceLen = ch, n
			b.WriteString("\n")
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
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
