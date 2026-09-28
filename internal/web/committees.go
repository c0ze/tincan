package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
)

const committeeBodyLimit = 16 << 10

type committeesView struct {
	Role       string                `json:"role"` // hub | peer
	From       string                `json:"from,omitempty"`
	FetchedAt  *time.Time            `json:"fetched_at,omitempty"`
	Error      string                `json:"error,omitempty"`
	Committees []committee.Committee `json:"committees"`
}

var errNoState = errors.New("committees need tincan web's state directory")

func (s *Server) listCommittees(w http.ResponseWriter, r *http.Request) {
	if s.cfg.StateDir == "" {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return
	}
	if s.cfg.CommitteesFrom != "" {
		s.writeJSON(w, http.StatusOK, s.peerCommittees(r.Context())) // Task 6
		return
	}
	list, err := committee.NewStore(s.cfg.StateDir).List()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, committeesView{Role: "hub", Committees: list})
}

func (s *Server) hubOnly(w http.ResponseWriter) bool {
	if s.cfg.StateDir == "" {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return false
	}
	if s.cfg.CommitteesFrom != "" {
		s.fail(w, http.StatusConflict, fmt.Errorf("committees are edited on %s", s.cfg.CommitteesFrom))
		return false
	}
	return true
}

func (s *Server) putCommittee(w http.ResponseWriter, r *http.Request) {
	if !s.hubOnly(w) {
		return
	}
	var in struct {
		Members         []string `json:"members"`
		DeadlineMinutes int      `json:"deadline_minutes"`
		Instructions    string   `json:"instructions"`
		SkipExhausted   bool     `json:"skip_exhausted"`
	}
	if err := readJSON(w, r, committeeBodyLimit, &in); err != nil {
		s.failBody(w, err)
		return
	}
	c := committee.Committee{Name: r.PathValue("name"), Members: in.Members, DeadlineMinutes: in.DeadlineMinutes, Instructions: in.Instructions, SkipExhausted: in.SkipExhausted}
	if err := c.Normalize(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	warnings, err := s.validateCommittee(r.Context(), c)
	if err != nil {
		status := http.StatusBadRequest
		var unreachable *unreachableError
		if errors.As(err, &unreachable) {
			status = http.StatusBadGateway
		}
		s.fail(w, status, err)
		return
	}
	saved, err := committee.NewStore(s.cfg.StateDir).Put(r.Context(), c)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"committee": saved, "warnings": warnings})
}

func (s *Server) deleteCommittee(w http.ResponseWriter, r *http.Request) {
	if !s.hubOnly(w) {
		return
	}
	found, err := committee.NewStore(s.cfg.StateDir).Delete(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		s.fail(w, http.StatusNotFound, errNotFound)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

type unreachableError struct {
	machine string
	err     error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("cannot validate members on %s: %v", e.machine, e.err)
}

// catalogueOf returns machine's preset catalogue: this machine's directly,
// a peer's through the outbound client.
func (s *Server) catalogueOf(ctx context.Context, machine string) ([]PresetInfo, error) {
	if machine == s.cfg.Machine {
		return s.catalogue()
	}
	p, ok := s.peers[machine]
	if !ok {
		return nil, fmt.Errorf("unknown machine %q (this machine is %q; peers are configured with --peer)", machine, s.cfg.Machine)
	}
	var list []PresetInfo
	if err := p.call(ctx, "GET", "presets", nil, 1<<20, &list); err != nil {
		return nil, &unreachableError{machine, err}
	}
	return list, nil
}

// validateCommittee applies the save-time checks of committees §6.1: every
// member resolves, is available and has a bare or absolute executable on its
// machine; the name is no preset on any reachable machine; bypass presets
// produce warnings.
func (s *Server) validateCommittee(ctx context.Context, c committee.Committee) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	catalogues := map[string][]PresetInfo{}
	var warnings []string
	for _, raw := range c.Members {
		m, _ := committee.ParseMember(raw)
		list, ok := catalogues[m.Machine]
		if !ok {
			var err error
			if list, err = s.catalogueOf(ctx, m.Machine); err != nil {
				return nil, err
			}
			catalogues[m.Machine] = list
		}
		var info *PresetInfo
		for i := range list {
			if list[i].Name == m.Preset {
				info = &list[i]
			}
		}
		switch {
		case info == nil:
			return nil, fmt.Errorf("member %s: no preset %q on %s", raw, m.Preset, m.Machine)
		case info.ExecKind == "relative":
			return nil, fmt.Errorf("member %s: relative executables cannot review (use a bare name or an absolute path)", raw)
		case !info.Available:
			return nil, fmt.Errorf("member %s: preset is not available on %s (executable not found)", raw, m.Machine)
		}
		for _, w := range info.Warnings {
			warnings = append(warnings, raw+": "+w)
		}
	}
	machines := append([]string{s.cfg.Machine}, sortedPeerNames(s.peers)...)
	for _, machine := range machines {
		list, ok := catalogues[machine]
		if !ok {
			var err error
			if list, err = s.catalogueOf(ctx, machine); err != nil {
				warnings = append(warnings, fmt.Sprintf("could not check preset names on %s: %v", machine, err))
				continue
			}
		}
		for _, p := range list {
			if p.Name == c.Name {
				return nil, fmt.Errorf("committee name %q is a preset name on %s; mentions would be ambiguous", c.Name, machine)
			}
		}
	}
	return warnings, nil
}

func (s *Server) peerCommittees(ctx context.Context) committeesView {
	return committeesView{Role: "peer", From: s.cfg.CommitteesFrom, Error: "not implemented", Committees: []committee.Committee{}}
}
