package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

// transport carries coordinator calls to members: this machine's jobs
// in-process, a peer's over the machine link (committees §6.7).
type transport struct{ s *Server }

func (t transport) peerFor(machine string) (*peer, error) {
	if p, ok := t.s.peers[machine]; ok {
		return p, nil
	}
	return nil, &reviewjob.Error{Status: 404, Msg: fmt.Sprintf("unknown machine %q", machine)}
}

func jobStatus(j reviewjob.Job) review.JobStatus {
	return review.JobStatus{State: j.State, Result: j.Result, Mode: j.Mode}
}

func (t transport) Create(ctx context.Context, machine string, req reviewjob.CreateRequest) (review.JobStatus, error) {
	if machine == t.s.cfg.Machine {
		j, err := t.s.jobs.Create(ctx, req)
		return jobStatus(j), err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return review.JobStatus{}, err
	}
	var v jobView
	err = p.call(ctx, "POST", "review-jobs", req, 1<<20, &v)
	return review.JobStatus{State: v.State, Result: v.Result, Mode: v.Mode}, err
}

func (t transport) Status(ctx context.Context, machine, id string) (review.JobStatus, error) {
	if machine == t.s.cfg.Machine {
		j, err := t.s.jobs.Status(id)
		return jobStatus(j), err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return review.JobStatus{}, err
	}
	var v jobView
	err = p.call(ctx, "GET", "review-jobs/"+url.PathEscape(id), nil, 8<<20, &v)
	return review.JobStatus{State: v.State, Result: v.Result, Mode: v.Mode}, err
}

func (t transport) Ack(ctx context.Context, machine, id string) error {
	if machine == t.s.cfg.Machine {
		_, err := t.s.jobs.Ack(ctx, id)
		return err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return err
	}
	return p.call(ctx, "POST", "review-jobs/"+url.PathEscape(id)+"/ack", map[string]string{}, 8<<20, nil)
}

func (t transport) Cancel(ctx context.Context, machine, id string, exp time.Time) (review.JobStatus, bool, error) {
	if machine == t.s.cfg.Machine {
		j, acked, err := t.s.jobs.Cancel(ctx, id, exp)
		return jobStatus(j), acked, err
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return review.JobStatus{}, false, err
	}
	var out struct {
		Job   jobView `json:"job"`
		Acked bool    `json:"acked"`
	}
	err = p.call(ctx, "POST", "review-jobs/"+url.PathEscape(id)+"/cancel", map[string]any{"expires_at": exp}, 8<<20, &out)
	return review.JobStatus{State: out.Job.State, Result: out.Job.Result, Mode: out.Job.Mode}, out.Acked, err
}

func (t transport) QuotaStatus(ctx context.Context, machine, preset string) (quota.Decision, error) {
	if machine == t.s.cfg.Machine {
		return quota.Decide(t.s.cfg.QuotaDir, t.s.cfg.QuotaConfig, preset, time.Now()), nil
	}
	p, err := t.peerFor(machine)
	if err != nil {
		return quota.Decision{}, err
	}
	var d quota.Decision
	err = p.call(ctx, "GET", "presets/"+url.PathEscape(preset)+"/quota-status", nil, 64<<10, &d)
	return d, err
}

type memberView struct {
	Member string `json:"member"`
	State  string `json:"state"`
	Late   bool   `json:"late,omitempty"`
	Note   string `json:"note,omitempty"`
}

type reviewSummary struct {
	ReviewID  string       `json:"review_id"`
	Committee string       `json:"committee"`
	Status    string       `json:"status"`
	Settled   bool         `json:"settled"`
	Created   time.Time    `json:"created"`
	Deadline  time.Time    `json:"deadline"`
	Members   []memberView `json:"members"`
}

type reviewDetail struct {
	reviewSummary
	Question string         `json:"question"`
	Scope    string         `json:"scope"`
	Results  map[int]string `json:"results"`
	Bundle   string         `json:"bundle,omitempty"`
}

func summarize(in review.Input, st review.State) reviewSummary {
	s := reviewSummary{ReviewID: in.ReviewID, Committee: in.Committee.Name, Status: st.Status, Settled: st.Settled, Created: in.Created, Deadline: in.Deadline, Members: []memberView{}}
	for _, m := range st.Members {
		s.Members = append(s.Members, memberView{Member: m.Member, State: m.State, Late: m.Late, Note: m.Note})
	}
	return s
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	ids, err := review.List(room.Path)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []reviewSummary{}
	for i := len(ids) - 1; i >= 0; i-- {
		in, err1 := review.ReadInput(room.Path, ids[i])
		st, err2 := review.ReadState(room.Path, ids[i])
		if err1 == nil && err2 == nil {
			out = append(out, summarize(in, st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) getReview(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	in, err := review.ReadInput(room.Path, id)
	if err != nil {
		s.fail(w, http.StatusNotFound, fmt.Errorf("review %s not found", id))
		return
	}
	st, err := review.ReadState(room.Path, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	d := reviewDetail{reviewSummary: summarize(in, st), Question: in.Question, Scope: in.Scope, Results: map[int]string{}}
	for _, m := range st.Members {
		if res, ok, _ := review.ReadResult(room.Path, id, m.Index); ok {
			d.Results[m.Index] = res
		}
	}
	d.Bundle, _, _ = review.ReadBundle(room.Path, id)
	s.writeJSON(w, http.StatusOK, d)
}

func (s *Server) startReview(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	if s.cfg.StateDir == "" {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return
	}
	var in struct {
		Committee string `json:"committee"`
		Question  string `json:"question"`
		Scope     string `json:"scope"`
		RequestID string `json:"request_id"`
	}
	if err := readJSON(w, r, 80<<10, &in); err != nil {
		s.failBody(w, err)
		return
	}
	from := s.cfg.CommitteesFrom
	rin, st, err := review.Publish(r.Context(), review.PublishRequest{StateDir: s.cfg.StateDir, Room: room.Path, Committee: in.Committee,
		Question: in.Question, Scope: in.Scope, RequestID: in.RequestID, Origin: "web", Machine: s.cfg.Machine,
		CommitteesFrom: &from, InCoordinator: true, Registry: s.cfg.Registry})
	switch {
	case errors.Is(err, review.ErrConflict):
		s.fail(w, http.StatusConflict, err)
	case err != nil && strings.Contains(err.Error(), "not found"):
		s.fail(w, http.StatusNotFound, err)
	case err != nil:
		s.fail(w, http.StatusBadRequest, err)
	default:
		s.writeJSON(w, http.StatusCreated, summarize(rin, st))
	}
}

func (s *Server) cancelReview(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	in, err := review.ReadInput(room.Path, id)
	if err != nil {
		s.fail(w, http.StatusNotFound, fmt.Errorf("review %s not found", id))
		return
	}
	st, err := review.RequestCancel(r.Context(), room.Path, id)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, summarize(in, st))
}
