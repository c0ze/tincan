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
	for _, want := range []string{`meta[name="tincan-base"]`, `"X-Tincan-Request"`, "new EventSource(", "client_id", "api/peers/", `"quotas"`, "renderLimits", "msgGen", `key === "local"`} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	// Room and thread management (spec §4.1, §11): add a room, hide/unhide
	// it, show missing rooms instead of skipping them, reach archived threads.
	for _, want := range []string{
		`api(m.key, "rooms", { method: "POST", body: { path } })`,
		`method: "PATCH", body: { hidden: !r.hidden }`,
		"+ add room",
		`r.missing ? " (missing)" : ""`,
		"showArchived",
		"show archived",
		// Composer and polling robustness.
		"draft.id",
		"pollBusy",
		// Committees page (committees spec §6.1, §6.7).
		`"#/committees"`,
		`api("local", "committees")`,
		`method: "PUT"`,
		`method: "DELETE"`,
		`"presets"`,
		`n.kind === "committees"`,
		// Save warnings survive the note-driven refresh; a note never
		// re-renders over an open editor.
		"state.committeeNotice",
		"!state.editingCommittee",
		// A list refresh that finishes after the editor opened must not
		// replace it; members that no longer resolve stay removable.
		`state.current.view !== "committees" || state.editingCommittee`,
		"box.disabled = !box.checked &&",
		"no longer available",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if strings.Contains(src, "r.missing) continue") {
		t.Error("app.js still skips missing rooms")
	}
	if n := strings.Count(src, "innerHTML"); n != 1 {
		t.Errorf("innerHTML used %d times; only the server-rendered message html may use it", n)
	}
	html, _ := uiFS.ReadFile("ui/index.html")
	if strings.Contains(string(html), "<script>") || strings.Contains(string(html), "style=") {
		t.Error("index.html has inline script or style (blocked by CSP)")
	}
}
