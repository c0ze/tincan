package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

type JobStatus struct {
	State  string `json:"state"`
	Result string `json:"result,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

type Transport interface {
	Create(ctx context.Context, machine string, req reviewjob.CreateRequest) (JobStatus, error)
	Status(ctx context.Context, machine, jobID string) (JobStatus, error)
	Ack(ctx context.Context, machine, jobID string) error
	Cancel(ctx context.Context, machine, jobID string, expiresAt time.Time) (JobStatus, bool, error)
	QuotaStatus(ctx context.Context, machine, preset string) (quota.Decision, error)
}

const (
	submitEvery = 15 * time.Second
	pollEvery   = 5 * time.Second
	cancelEvery = 15 * time.Second
	ackEvery    = 15 * time.Second
	collectTail = 5 * time.Minute
)

// Coordinator advances reviews (committees §6.4). It runs in tincan web's
// room pass on the dispatcher that owns the room; rate limits live in
// memory and reset harmlessly on restart.
type Coordinator struct {
	Transport Transport
	Now       func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

// Classify maps a transport error to permanent, gone or transient.
func Classify(err error) string {
	var hs interface{ HTTPStatus() int }
	if errors.As(err, &hs) {
		switch hs.HTTPStatus() {
		case 400, 404, 409, 422:
			return "permanent"
		case 410:
			return "gone"
		}
	}
	return "transient"
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// due reports whether op on key may run again, and records the attempt.
func (c *Coordinator) due(key string, every time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]time.Time{}
	}
	now := c.now()
	if t, ok := c.last[key]; ok && now.Sub(t) < every {
		return false
	}
	c.last[key] = now
	return true
}

// Reconcile advances every unsettled review in room one step and removes
// stale staging directories.
func (c *Coordinator) Reconcile(ctx context.Context, room string) error {
	c.cleanStaging(room)
	ids, err := List(room)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := c.reconcile(ctx, room, id); err != nil {
			errs = append(errs, fmt.Errorf("review %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Coordinator) cleanStaging(room string) {
	dir := filepath.Join(Root(room), "staging")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && c.now().Sub(info.ModTime()) > time.Hour {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func (c *Coordinator) reconcile(ctx context.Context, room, id string) error {
	st, err := ReadState(room, id)
	if err != nil || st.Settled {
		return err
	}
	lock, err := Lock(ctx, room, id)
	if err != nil {
		return err
	}
	defer lock.Close()
	if st, err = ReadState(room, id); err != nil || st.Settled {
		return err
	}
	in, err := ReadInput(room, id)
	if err != nil {
		return err
	}
	save := func() error { return WriteState(room, id, st) }
	for i := range st.Members {
		if err := c.member(ctx, room, in, &st, i, save); err != nil {
			return err
		}
	}
	if err := c.close(room, in, &st, save); err != nil {
		return err
	}
	if c.settled(room, in, st) {
		st.Settled = true
		return save()
	}
	return nil
}

func split(member string) (preset, machine string) {
	m, _ := committee.ParseMember(member)
	return m.Preset, m.Machine
}

func (c *Coordinator) adopt(room, id string, m *Member, js JobStatus) error {
	now := c.now()
	if js.Mode != "" {
		m.Mode = js.Mode
	}
	switch js.State {
	case "creating":
		if m.State == "submitting" || m.State == "unreachable" {
			m.State = "submitted"
		}
	case "running":
		m.State = "running"
	case "done", "error", "cancelled":
		if js.Result != "" {
			if err := WriteResult(room, id, m.Index, js.Result); err != nil {
				return err
			}
		}
		m.State, m.Finished = js.State, now
	}
	return nil
}

func (c *Coordinator) member(ctx context.Context, room string, in Input, st *State, i int, save func() error) error {
	m := &st.Members[i]
	preset, machine := split(m.Member)
	now := c.now()
	key := m.JobID + "/"
	switch {
	case st.CancelRequested && !m.CancelAcked:
		switch {
		case m.State == "planned":
			m.State, m.CancelAcked, m.Finished = "cancelled", true, now
			return save()
		case Final(m.State):
			m.CancelAcked = true
			return save()
		case now.After(m.ExpiresAt):
			m.CancelAcked = true // the job cannot be running past its expiry
			return save()
		case c.due(key+"cancel", cancelEvery):
			js, acked, err := c.Transport.Cancel(ctx, machine, m.JobID, m.ExpiresAt)
			if err != nil {
				return nil
			}
			if err := c.adopt(room, in.ReviewID, m, js); err != nil {
				return err
			}
			if acked {
				m.CancelAcked = true
				if !Final(m.State) {
					m.State, m.Finished = "cancelled", now
				}
			}
			return save()
		}
		return nil
	case m.State == "planned" && !st.CancelRequested:
		if in.Committee.SkipExhausted {
			if d, err := c.Transport.QuotaStatus(ctx, machine, preset); err == nil && d.Exhausted {
				m.State, m.Finished = "skipped", now
				m.Note = "quota exhausted (" + d.Window + ")"
				if d.ResetAt != nil {
					m.Note += " until " + d.ResetAt.UTC().Format(time.RFC3339)
				}
				return save()
			}
		}
		m.State, m.Started = "submitting", now
		if err := save(); err != nil { // persisted before the call (committees §6.4)
			return err
		}
		return c.submit(ctx, room, in, m, machine, preset, save)
	case m.State == "submitting":
		if now.After(in.Deadline) {
			m.State, m.Note = "unreachable", "peer not reachable"
			return save()
		}
		return c.submit(ctx, room, in, m, machine, preset, save)
	case m.State == "submitted" || m.State == "running" || m.State == "unreachable":
		if now.After(m.ExpiresAt.Add(collectTail)) {
			if m.State != "unreachable" {
				m.State, m.Note = "expired", "result not retrievable"
			}
			m.Finished = now
			return save()
		}
		if !c.due(key+"poll", pollEvery) {
			return nil
		}
		js, err := c.Transport.Status(ctx, machine, m.JobID)
		if err != nil {
			return nil
		}
		before := *m
		if err := c.adopt(room, in.ReviewID, m, js); err != nil {
			return err
		}
		if *m != before {
			return save()
		}
		return nil
	}
	return c.ack(ctx, room, in, m, machine, save)
}

func (c *Coordinator) submit(ctx context.Context, room string, in Input, m *Member, machine, preset string, save func() error) error {
	if !c.due(m.JobID+"/submit", submitEvery) {
		return nil
	}
	p, err := ReadPacket(room, in.ReviewID)
	if err != nil {
		return err
	}
	prompt, err := ReadPrompt(room, in.ReviewID, m.Index)
	if err != nil {
		return err
	}
	sum := sha256Hex(prompt)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	js, err := c.Transport.Create(cctx, machine, reviewjob.CreateRequest{JobID: m.JobID, ReviewID: in.ReviewID, Requester: in.Requester,
		Preset: preset, ExpiresAt: m.ExpiresAt, Packet: p, Prompt: prompt, PromptSHA: sum})
	switch {
	case err == nil:
		if m.State == "submitting" {
			m.State = "submitted"
		}
		if err := c.adopt(room, in.ReviewID, m, js); err != nil {
			return err
		}
	case Classify(err) == "permanent":
		m.State, m.Note, m.Finished = "error", err.Error(), c.now()
	case Classify(err) == "gone":
		m.State, m.Note, m.Finished = "expired", err.Error(), c.now()
	default:
		return nil // transient: stay submitting, retry later
	}
	return save()
}

func (c *Coordinator) ack(ctx context.Context, room string, in Input, m *Member, machine string, save func() error) error {
	if m.Acked || (m.State != "done" && m.State != "error" && m.State != "cancelled") {
		return nil
	}
	if _, ok, _ := ReadResult(room, in.ReviewID, m.Index); !ok {
		return nil
	}
	if c.now().After(m.ExpiresAt.Add(collectTail)) {
		m.Acked = true
		return save()
	}
	if !c.due(m.JobID+"/ack", ackEvery) {
		return nil
	}
	err := c.Transport.Ack(ctx, machine, m.JobID)
	if err == nil || Classify(err) != "transient" {
		m.Acked = true
		return save()
	}
	return nil
}

// close finishes a running review once (committees §6.4): closing with an
// immutable closure, the bundle rendered from it, then closed.
func (c *Coordinator) close(room string, in Input, st *State, save func() error) error {
	now := c.now()
	if st.Status == "running" {
		all := true
		for _, m := range st.Members {
			if !Final(m.State) {
				all = false
			}
		}
		if !all && !now.After(in.Deadline) {
			return nil
		}
		cl := &Closure{At: now.UTC()}
		for i := range st.Members {
			m := &st.Members[i]
			_, has, _ := ReadResult(room, in.ReviewID, m.Index)
			late := !Final(m.State) && m.State != "unreachable"
			m.Late = late
			cl.Members = append(cl.Members, ClosedMember{Index: m.Index, State: m.State, Late: late, Included: has})
		}
		st.Status, st.Closure = "closing", cl
		at := cl.At
		st.ClosedAt = &at
		if err := save(); err != nil {
			return err
		}
	}
	if st.Status != "closing" {
		return nil
	}
	results := map[int]string{}
	for _, cm := range st.Closure.Members {
		if cm.Included {
			r, _, _ := ReadResult(room, in.ReviewID, cm.Index)
			results[cm.Index] = r
		}
	}
	if err := writeFile(filepath.Join(Dir(room, in.ReviewID), "bundle.md"), RenderBundle(in, *st, results)); err != nil {
		return err
	}
	st.Status = "closed"
	return save()
}

// settled reports whether every obligation of every member has ended
// (committees §6.4): collected or past its tail, acknowledged or nothing to
// acknowledge, cancelled or no cancel requested.
func (c *Coordinator) settled(room string, in Input, st State) bool {
	if st.Status != "closed" && st.Status != "cancelled" {
		return false
	}
	now := c.now()
	for _, m := range st.Members {
		pastTail := now.After(m.ExpiresAt.Add(collectTail))
		_, hasResult, _ := ReadResult(room, in.ReviewID, m.Index)
		collected := Final(m.State) || pastTail
		acked := m.Acked || pastTail || !hasResult
		cancelled := !st.CancelRequested || m.CancelAcked || Final(m.State) || now.After(m.ExpiresAt)
		if !collected || !acked || !cancelled {
			return false
		}
	}
	return true
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeFile(path string, data []byte) error { return fsutil.WriteFileAtomic(path, data) }
