package thread

import (
	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/host"
)

type Target struct {
	Mention  string // as written
	Listener string // canonical listener name
	Preset   string
	Existing bool // an existing room listener, not owned by the thread
	// Committee is set when the mention names a committee (phase 2b-4).
	Committee *committee.Committee
}

type Resolver struct {
	Room    string
	Presets map[string]host.Preset
	// Alive reports a live listener; hosted listeners have a non-empty Owner.
	Alive func(name string) (host.State, bool)
	// Committees this machine knows, by name; nil disables @committee.
	Committees map[string]committee.Committee
}

func (r Resolver) available(name string) bool {
	p, ok := r.Presets[name]
	if !ok || len(p.Exec) == 0 {
		return false
	}
	_, err := host.ResolveExecutable(p.Exec[0], r.Room)
	return err == nil
}

// Resolve maps mention names to canonical targets (spec §6.1). "you" is
// skipped silently; duplicates by canonical listener are dropped.
func (r Resolver) Resolve(meta Meta, names []string) ([]Target, []string) {
	var out []Target
	var unresolved []string
	seen := map[string]bool{}
	add := func(t Target) {
		if !seen[t.Listener] {
			seen[t.Listener] = true
			out = append(out, t)
		}
	}
	for _, name := range names {
		switch {
		case name == "you":
		case r.available(name):
			listener := name + "." + meta.ID
			if envelope.ValidComponent(listener) != nil {
				unresolved = append(unresolved, name)
				continue
			}
			add(Target{Mention: name, Listener: listener, Preset: name})
		case r.Committees[name].Name != "":
			c := r.Committees[name]
			add(Target{Mention: name, Listener: "committee:" + name, Committee: &c})
		case meta.Listeners[name] != "":
			add(Target{Mention: name, Listener: name, Preset: meta.Listeners[name]})
		default:
			if _, isPreset := r.Presets[name]; !isPreset && r.Alive != nil {
				if st, ok := r.Alive(name); ok && st.Owner != "" {
					add(Target{Mention: name, Listener: name, Preset: st.Preset, Existing: true})
					continue
				}
			}
			unresolved = append(unresolved, name)
		}
	}
	return out, unresolved
}
