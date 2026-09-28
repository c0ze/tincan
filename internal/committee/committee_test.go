package committee

import (
	"strings"
	"testing"
)

func TestNormalizeDefaultsAndValidates(t *testing.T) {
	c := Committee{Name: "reviewers", Members: []string{"codex-gmail@cachyos", "claude-personal@macmini"}}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.DeadlineMinutes != DefaultDeadlineMinutes || c.SkipExhausted {
		t.Fatalf("defaults: %+v", c)
	}
	for name, bad := range map[string]Committee{
		"empty name":        {Name: "", Members: []string{"a@b"}},
		"reserved you":      {Name: "you", Members: []string{"a@b"}},
		"dot in name":       {Name: "a.b", Members: []string{"a@b"}},
		"leading dash":      {Name: "-a", Members: []string{"a@b"}},
		"long name":         {Name: strings.Repeat("a", 65), Members: []string{"a@b"}},
		"no members":        {Name: "a"},
		"too many":          {Name: "a", Members: []string{"a@m", "b@m", "c@m", "d@m", "e@m", "f@m", "g@m", "h@m", "i@m"}},
		"duplicate":         {Name: "a", Members: []string{"x@m", "x@m"}},
		"no machine":        {Name: "a", Members: []string{"codex"}},
		"bad machine":       {Name: "a", Members: []string{"codex@mac.mini"}},
		"deadline zero":     {Name: "a", Members: []string{"a@b"}, DeadlineMinutes: -1},
		"deadline too long": {Name: "a", Members: []string{"a@b"}, DeadlineMinutes: 241},
		"instructions":      {Name: "a", Members: []string{"a@b"}, Instructions: strings.Repeat("x", MaxInstructions+1)},
	} {
		c := bad
		if err := c.Normalize(); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}
}

func TestParseMember(t *testing.T) {
	m, err := ParseMember("claude-personal@macmini")
	if err != nil || m.Preset != "claude-personal" || m.Machine != "macmini" {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{"", "@m", "p@", "p@m@n", "../x@m", "p@M-1."} {
		if _, err := ParseMember(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
