// Package committee defines review committees — named groups of
// preset@machine reviewers — and stores them on the hub machine, with a
// cached copy on its peer (committees spec §6.1).
package committee

import (
	"errors"
	"fmt"
	"regexp"
)

const (
	MaxMembers             = 8
	MaxInstructions        = 8 << 10
	DefaultDeadlineMinutes = 30
	MaxDeadlineMinutes     = 240
	MaxCommittees          = 64
)

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	presetRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	machineRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// Committee is one owner-defined group of reviewers.
type Committee struct {
	Name            string   `json:"name"`
	Version         int      `json:"version"`
	Members         []string `json:"members"`
	DeadlineMinutes int      `json:"deadline_minutes"`
	Instructions    string   `json:"instructions,omitempty"`
	SkipExhausted   bool     `json:"skip_exhausted"`
}

// Member is one parsed preset@machine reference.
type Member struct{ Preset, Machine string }

// ParseMember splits and validates a preset@machine reference. Presets may
// contain dots (thread listeners never become members); machine names are
// single DNS-style labels.
func ParseMember(s string) (Member, error) {
	var m Member
	at := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '@' {
			if at >= 0 {
				return m, fmt.Errorf("member %q has more than one @", s)
			}
			at = i
		}
	}
	if at < 0 {
		return m, fmt.Errorf("member %q must be preset@machine", s)
	}
	m.Preset, m.Machine = s[:at], s[at+1:]
	if !presetRe.MatchString(m.Preset) || m.Preset[len(m.Preset)-1] == '.' {
		return m, fmt.Errorf("member %q: invalid preset name", s)
	}
	if !machineRe.MatchString(m.Machine) {
		return m, fmt.Errorf("member %q: invalid machine name", s)
	}
	return m, nil
}

// ValidName checks a committee name: no dots (reserved for thread
// listeners) and not "you" (the owner's author name in threads).
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("committee name %q must match %s", name, nameRe)
	}
	if name == "you" {
		return errors.New(`committee name "you" is reserved`)
	}
	return nil
}

// Normalize fills defaults and validates the committee's own shape. Whether
// members exist on their machines is checked by the web layer at save time.
func (c *Committee) Normalize() error {
	if err := ValidName(c.Name); err != nil {
		return err
	}
	if len(c.Members) == 0 || len(c.Members) > MaxMembers {
		return fmt.Errorf("a committee needs 1 to %d members", MaxMembers)
	}
	seen := map[string]bool{}
	for _, s := range c.Members {
		if _, err := ParseMember(s); err != nil {
			return err
		}
		if seen[s] {
			return fmt.Errorf("member %s is listed twice", s)
		}
		seen[s] = true
	}
	if c.DeadlineMinutes == 0 {
		c.DeadlineMinutes = DefaultDeadlineMinutes
	}
	if c.DeadlineMinutes < 1 || c.DeadlineMinutes > MaxDeadlineMinutes {
		return fmt.Errorf("deadline_minutes must be 1 to %d", MaxDeadlineMinutes)
	}
	if len(c.Instructions) > MaxInstructions {
		return fmt.Errorf("instructions exceed %d bytes", MaxInstructions)
	}
	return nil
}
