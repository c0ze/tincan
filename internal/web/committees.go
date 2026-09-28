package web

import (
	"context"
	"encoding/json"
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
		s.writeJSON(w, http.StatusOK, s.peerCommittees(r.Header.Get(peerCallHeader) == ""))
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
		if p := s.peers[machine]; !ok && p != nil && !p.online.Load() {
			// Never wait on a peer the health check reports down.
			warnings = append(warnings, fmt.Sprintf("preset names on %s not checked: %s is offline", machine, machine))
			continue
		}
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

// peerCommittees serves the cached copy (committees §6.7). With refresh set
// it first refreshes a copy whose last fetch *attempt* is more than 10 s old,
// so a failing hub is asked at most every 10 s. Calls arriving over the
// machine link never refresh: two machines misconfigured as each other's
// peer would otherwise call back into each other until the peer timeout.
func (s *Server) peerCommittees(refresh bool) committeesView {
	if refresh {
		s.refreshCommittees(false)
	}
	view := committeesView{Role: "peer", From: s.cfg.CommitteesFrom, Committees: []committee.Committee{}}
	c, ok, err := committee.LoadCache(s.cfg.StateDir)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	if ok {
		view.Committees, view.Error = c.Committees, c.Error
		if !c.FetchedAt.IsZero() {
			t := c.FetchedAt
			view.FetchedAt = &t
		}
	}
	return view
}

// refreshCommittees fetches the hub's committees into the cache; unless force
// is set it does nothing when the last attempt (successful or not) is under
// 10 s old. Callers share one fetch at a time and re-check freshness after
// waiting, and the fetch uses its own deadline rather than a browser
// request's context. The cache file is rewritten, and a committees note
// published, only when its content changes.
func (s *Server) refreshCommittees(force bool) error {
	s.committeesMu.Lock()
	defer s.committeesMu.Unlock()
	if !force && time.Since(s.committeesAttempt) < 10*time.Second {
		return nil
	}
	s.committeesAttempt = time.Now()
	s.committeesAttempts++
	old, _, _ := committee.LoadCache(s.cfg.StateDir)
	next := old
	next.From = s.cfg.CommitteesFrom
	var view committeesView
	var err error
	if p := s.peers[s.cfg.CommitteesFrom]; p == nil {
		err = fmt.Errorf("%s is not a configured peer", s.cfg.CommitteesFrom)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = p.call(ctx, "GET", "committees", nil, 1<<20, &view)
		cancel()
	}
	if err == nil && view.Role != "hub" {
		err = fmt.Errorf("%s is not a committees hub (it reads committees from %q)", s.cfg.CommitteesFrom, view.From)
	}
	if err == nil {
		for i := range view.Committees {
			if nerr := view.Committees[i].Normalize(); nerr != nil {
				err = fmt.Errorf("hub sent an invalid committee: %w", nerr)
				break
			}
		}
	}
	if err != nil {
		next.Error = err.Error()
	} else {
		next.Error, next.Committees = "", view.Committees
		next.FetchedAt, next.AttemptedAt = s.committeesAttempt.UTC(), s.committeesAttempt.UTC()
	}
	if next.Committees == nil {
		next.Committees = []committee.Committee{}
	}
	changed := next.Error != old.Error || next.From != old.From || !sameCommittees(next.Committees, old.Committees)
	if changed || !next.FetchedAt.Equal(old.FetchedAt) {
		if serr := committee.SaveCache(s.cfg.StateDir, next); serr != nil {
			return serr
		}
	}
	if changed {
		s.hub.publish(note{Kind: "committees"})
	}
	return err
}

func sameCommittees(a, b []committee.Committee) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// syncCommittees refreshes the peer cache every 60 s.
func (s *Server) syncCommittees(ctx context.Context) {
	if s.cfg.CommitteesFrom == "" || s.cfg.StateDir == "" {
		return
	}
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		if err := s.refreshCommittees(true); err != nil {
			s.logOnce("committees.refresh", fmt.Sprintf("tincan web: committees from %s: %v", s.cfg.CommitteesFrom, err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
