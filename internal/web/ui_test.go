// internal/web/ui_test.go
package web

import (
	"strings"
	"testing"
)

// The UI must build every URL from the <meta> base, send the CSRF header on
// mutations, and never inject server text with innerHTML except the
// server-rendered "html" field.
func TestUIContract(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{`meta[name="tincan-base"]`, `"X-Tincan-Request"`, "new EventSource(", "client_id", "api/peers/", `"quotas"`, "renderLimits"} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if n := strings.Count(src, "innerHTML"); n != 1 {
		t.Errorf("innerHTML used %d times; only the server-rendered message html may use it", n)
	}
	html, _ := uiFS.ReadFile("ui/index.html")
	if strings.Contains(string(html), "<script>") || strings.Contains(string(html), "style=") {
		t.Error("index.html has inline script or style (blocked by CSP)")
	}
}
