package web

import (
	"strings"
	"testing"
)

func TestRenderMarkdownEscapesHTML(t *testing.T) {
	out := RenderMarkdown("<script>alert(1)</script> <img src=x onerror=alert(1)>")
	if strings.Contains(out, "<script") || strings.Contains(out, "<img") {
		t.Fatalf("raw HTML rendered: %s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("not escaped: %s", out)
	}
}

func TestRenderMarkdownSubset(t *testing.T) {
	out := RenderMarkdown("Hi @codex, see `x < y`.\n\n- one\n- two\n\n```go\nfmt.Println(\"<b>\")\n```\n[docs](https://example.com/a?b=1&c=2) [bad](javascript:alert(1))")
	for _, want := range []string{
		`<span class="mention">@codex</span>`,
		`<code>x &lt; y</code>`,
		`<ul><li>one</li><li>two</li></ul>`,
		`<pre><code>fmt.Println(&#34;&lt;b&gt;&#34;)</code></pre>`,
		`<a href="https://example.com/a?b=1&amp;c=2" rel="noopener noreferrer" target="_blank">docs</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "javascript:") && strings.Contains(out, "href=\"javascript") {
		t.Fatalf("javascript link rendered: %s", out)
	}
}
