package review

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/packet"
)

func TestPromptIsModeNeutralAndBounded(t *testing.T) {
	c := committee.Committee{Name: "reviewers", Instructions: "Cite file:line."}
	p := &packet.Packet{Question: "Is it safe?", Manifest: packet.Manifest{Complete: false}}
	for i := 0; i < 500; i++ {
		p.Manifest.Changes = append(p.Manifest.Changes, packet.Change{Path: fmt.Sprintf("secret%d.env", i), Reason: "secret-looking path"})
	}
	got := BuildPrompt(c, "codex@cachyos", p)
	for _, want := range []string{"member `codex@cachyos` of committee `reviewers`", "Review only", "Cite file:line.", "Is it safe?", "partial", "and 300 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	for _, mode := range []string{"git diff --cached", "packet/"} {
		if strings.Contains(got, mode) {
			t.Errorf("prompt mentions workspace mode %q", mode)
		}
	}
	huge := &packet.Packet{Question: strings.Repeat("q", packet.MaxQuestion)}
	c.Instructions = strings.Repeat("i", committee.MaxInstructions)
	if n := len(BuildPrompt(c, "m@x", huge)); n > MaxPrompt {
		t.Fatalf("prompt %d bytes", n)
	}
}

func TestBundleRendersOnlyTheClosureAndIsBounded(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	in := Input{Committee: committee.Committee{Name: "reviewers", Version: 3}, Scope: "uncommitted", IncludedTree: "abc"}
	st := State{Members: []Member{{Member: "a@m", Index: 0, Started: at.Add(-4 * time.Minute), Finished: at}, {Member: "b@m", Index: 1}, {Member: "c@m", Index: 2, Note: "quota exhausted"}},
		Closure: &Closure{At: at, Members: []ClosedMember{{Index: 0, State: "done", Included: true}, {Index: 1, State: "running", Late: true}, {Index: 2, State: "skipped"}}}}
	results := map[int]string{0: "LGTM", 1: "arrived after close"}
	b := string(RenderBundle(in, st, results))
	for _, want := range []string{"reviewers", "v3", "## a@m — done in 4m0s", "LGTM", "## b@m — late (still running)", "## c@m — skipped: quota exhausted"} {
		if !strings.Contains(b, want) {
			t.Errorf("bundle lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "arrived after close") {
		t.Fatal("bundle includes a result not in the closure")
	}
	if string(RenderBundle(in, st, results)) != b {
		t.Fatal("bundle not deterministic")
	}
	big := map[int]string{0: strings.Repeat("x", 3<<20)}
	if n := len(RenderBundle(in, st, big)); n > MaxBundle {
		t.Fatalf("bundle %d bytes", n)
	}
}
