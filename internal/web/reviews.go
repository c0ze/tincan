package web

import (
	"context"
	"fmt"
	"net/url"
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
