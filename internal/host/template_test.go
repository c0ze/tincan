package host

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitTemplate(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "codex exec -o {out} -", want: []string{"codex", "exec", "-o", "{out}", "-"}},
		{in: `grok -p "{body}" --always-approve`, want: []string{"grok", "-p", "{body}", "--always-approve"}},
		{in: `my-llm --system 'be brief'  x`, want: []string{"my-llm", "--system", "be brief", "x"}},
		{in: `a\ b c`, want: []string{"a b", "c"}},
		{in: `x '' y`, want: []string{"x", "", "y"}},
		{in: "tabs\tand\nnewlines", want: []string{"tabs", "and", "newlines"}},
		{in: `a "unterminated`, wantErr: true},
		{in: "   ", wantErr: true},
		{in: `x \`, wantErr: true},
	}
	for _, c := range cases {
		got, err := SplitTemplate(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("SplitTemplate(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitTemplate(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitTemplate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderSubstitutesEveryPlaceholderPerToken(t *testing.T) {
	v := Vars{Body: "review  PR 56\n\"quoted\" 'single' $HOME", Out: "/tmp/o.md", Room: "/r", Name: "codex", ID: "abc"}
	in := []string{"agent", "-p", "{body}", "--out={out}", "{room}/{name}/{id}", "plain"}
	got := Render(in, v)
	want := []string{"agent", "-p", v.Body, "--out=/tmp/o.md", "/r/codex/abc", "plain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Render = %q, want %q", got, want)
	}
	if in[2] != "{body}" {
		t.Fatal("Render mutated its input")
	}
}

func TestRenderDoesNotRescanSubstitutedText(t *testing.T) {
	got := Render([]string{"{body}"}, Vars{Body: "{out}", Out: "X"})
	if got[0] != "{out}" {
		t.Fatalf("body containing a placeholder was re-substituted: %q", got[0])
	}
}

func TestPaths(t *testing.T) {
	if got, want := StatePath("/r", "codex"), filepath.Join("/r", ".tincan", "hosts", "codex.json"); got != want {
		t.Fatalf("StatePath = %q, want %q", got, want)
	}
	if got, want := LogPath("/r", "codex"), filepath.Join("/r", ".tincan", "hosts", "codex.log"); got != want {
		t.Fatalf("LogPath = %q, want %q", got, want)
	}
}
