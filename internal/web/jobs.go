package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

type jobView struct {
	JobID     string    `json:"job_id"`
	State     string    `json:"state"`
	Mode      string    `json:"mode,omitempty"`
	Note      string    `json:"note,omitempty"`
	Result    string    `json:"result,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Acked     bool      `json:"acked,omitempty"`
}

func viewJob(j reviewjob.Job) jobView {
	return jobView{JobID: j.ID, State: j.State, Mode: j.Mode, Note: j.Note, Result: j.Result, ExpiresAt: j.ExpiresAt, Created: j.Created, Updated: j.Updated, Acked: j.Acked}
}

// initJobs builds the reviewer job service when a state directory is set.
func (s *Server) initJobs() {
	if s.cfg.StateDir == "" {
		return
	}
	s.jobs = &reviewjob.Service{StateDir: s.cfg.StateDir, Registry: s.cfg.Registry, Presets: s.cfg.Dispatch.PresetMap, Executable: s.cfg.Dispatch.Executable}
	s.coord = &review.Coordinator{Transport: transport{s}}
}

func (s *Server) jobsReady(w http.ResponseWriter) bool {
	if s.jobs == nil {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return false
	}
	return true
}

func (s *Server) failJob(w http.ResponseWriter, err error) {
	var je *reviewjob.Error
	if errors.As(err, &je) {
		s.fail(w, je.Status, err)
		return
	}
	s.fail(w, http.StatusInternalServerError, err)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	var req reviewjob.CreateRequest
	if err := readJSON(w, r, packet.MaxEncoded, &req); err != nil {
		s.failBody(w, err)
		return
	}
	if !s.admitJob() {
		s.fail(w, http.StatusServiceUnavailable, errors.New("tincan web is shutting down"))
		return
	}
	defer s.creating.Done()
	j, err := s.jobs.Create(r.Context(), req)
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, viewJob(j))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	j, err := s.jobs.Status(r.PathValue("job"))
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewJob(j))
}

func (s *Server) ackJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	var in struct{}
	if r.ContentLength != 0 {
		if err := readJSON(w, r, 1<<10, &in); err != nil {
			s.failBody(w, err)
			return
		}
	}
	j, err := s.jobs.Ack(r.Context(), r.PathValue("job"))
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewJob(j))
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	var in struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if r.ContentLength != 0 {
		if err := readJSON(w, r, 1<<10, &in); err != nil {
			s.failBody(w, err)
			return
		}
	}
	j, acked, err := s.jobs.Cancel(r.Context(), r.PathValue("job"), in.ExpiresAt)
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"job": viewJob(j), "acked": acked})
}

// admitJob lets a job creation in unless Run has begun shutting down; an
// admitted creation calls s.creating.Done once Create has returned.
func (s *Server) admitJob() bool {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.jobsClosed {
		return false
	}
	s.creating.Add(1)
	return true
}

// closeJobs refuses job creations from now on.
func (s *Server) closeJobs() {
	s.jobsMu.Lock()
	s.jobsClosed = true
	s.jobsMu.Unlock()
}

// joinJobs waits, after closeJobs and once the janitor has stopped, for the
// admitted creations to return from Create and then for the background
// creations they and the janitor started.
func (s *Server) joinJobs() {
	if s.jobs == nil {
		return
	}
	s.creating.Wait()
	s.jobs.Wait()
}

// runJobJanitor runs the reviewer job janitor every minute until ctx ends;
// Run joins the background creations (joinJobs).
func (s *Server) runJobJanitor(ctx context.Context) {
	if s.jobs == nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := s.jobs.Janitor(ctx); err != nil {
			s.logOnce("jobs.janitor", "tincan web: review jobs: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
