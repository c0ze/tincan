package web

import (
	"html"
	"regexp"
	"strings"
)

var (
	linkRE    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	mentionRE = regexp.MustCompile(`(^|[\s(\[,])@([A-Za-z0-9][A-Za-z0-9._-]{0,63})`)
)

func isItem(l string) bool {
	t := strings.TrimLeft(l, " ")
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
}

func itemText(l string) string { return strings.TrimLeft(l, " ")[2:] }

// text escapes s and decorates links and mentions; nothing from s is ever
// emitted unescaped.
func text(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range linkRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(mentions(html.EscapeString(s[last:m[0]])))
		b.WriteString(`<a href="` + html.EscapeString(s[m[4]:m[5]]) + `" rel="noopener noreferrer" target="_blank">` + html.EscapeString(s[m[2]:m[3]]) + `</a>`)
		last = m[1]
	}
	b.WriteString(mentions(html.EscapeString(s[last:])))
	return b.String()
}

func mentions(escaped string) string {
	return mentionRE.ReplaceAllString(escaped, `$1<span class="mention">@$2</span>`)
}

func inline(s string) string {
	parts := strings.Split(s, "`")
	if len(parts)%2 == 0 { // unbalanced: treat the last backtick literally
		parts[len(parts)-2] += "`" + parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString("<code>" + html.EscapeString(p) + "</code>")
		} else {
			b.WriteString(text(p))
		}
	}
	return b.String()
}

// RenderMarkdown renders paragraphs, fenced and inline code, bullet lists,
// http(s) links and @mentions. Raw HTML is always escaped.
func RenderMarkdown(src string) string {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), "```"):
			j := i + 1
			var code []string
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				code = append(code, lines[j])
				j++
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>")
			i = j + 1
		case strings.TrimSpace(l) == "":
			i++
		case isItem(l):
			b.WriteString("<ul>")
			for i < len(lines) && isItem(lines[i]) {
				b.WriteString("<li>" + inline(itemText(lines[i])) + "</li>")
				i++
			}
			b.WriteString("</ul>")
		default:
			var para []string
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" && !isItem(lines[i]) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				para = append(para, inline(lines[i]))
				i++
			}
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
		}
	}
	return b.String()
}
