// Package request stores durable request state and results. A transport timeout
// never submits work again; callers retain the request ID and collect its result.
package request

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/filelock"
	"github.com/c0ze/tincan/internal/fsutil"
	"github.com/c0ze/tincan/internal/spool"
)

const MaxBodyBytes = 1024 * 1024
const MaxResultBytes = 2 * 1024 * 1024

// Envelopes are capped in encoded bytes; result strings may expand sixfold
// under JSON escaping. Every record we accept for writing must remain readable.
const MaxRecordBytes = spool.MaxMessageBytes + 6*MaxResultBytes + 64*1024

var errReplyJournalBusy = errors.New("interactive reply journal is busy")

type Record struct {
	ID              string    `json:"id"`
	Agent           string    `json:"agent"`
	Status          string    `json:"status"`
	Result          string    `json:"result,omitempty"`
	Created         time.Time `json:"created"`
	Updated         time.Time `json:"updated"`
	CancelRequested bool      `json:"cancel_requested,omitempty"`
	Interactive     bool      `json:"interactive,omitempty"`
	// An interactive record is saved before dispatch. Until this confirmation
	// is saved, only the creating Submit call may attempt publication.
	InteractivePublished bool `json:"interactive_published,omitempty"`
	// Only publication uncertainty can be resolved by a later validated reply;
	// execution failures, cancellations, and other terminal results stay final.
	InteractivePublicationUncertain bool               `json:"interactive_publication_uncertain,omitempty"`
	Envelope                        *envelope.Envelope `json:"request"`
}

func (r Record) Terminal() bool {
	switch r.Status {
	case "completed", "failed", "interrupted", "canceled":
		return true
	}
	return false
}

func canonicalRoom(room string) string {
	if abs, err := filepath.Abs(room); err == nil {
		room = abs
	}
	if real, err := filepath.EvalSymlinks(room); err == nil {
		room = real
	}
	return room
}

func Dir(room string) string { return filepath.Join(canonicalRoom(room), ".tincan", "requests") }

func validID(id string) error {
	if err := spool.ValidName(id); err != nil {
		return err
	}
	if len(id) > 128 {
		return fmt.Errorf("request ID must be a portable name of 1–128 bytes")
	}
	return nil
}

// ValidateID checks a client-supplied idempotency key before any work is started.
func ValidateID(id string) error { return validID(id) }

func recordPath(room, id string) string { return filepath.Join(Dir(room), id+".json") }

func lock(ctx context.Context, room, id string) (*filelock.Lock, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if err := fsutil.MkdirPrivate(Dir(room)); err != nil {
		return nil, err
	}
	return filelock.Acquire(ctx, filepath.Join(Dir(room), "locks", id+".lock"))
}

func Get(room, id string) (Record, error) {
	if err := validID(id); err != nil {
		return Record{}, err
	}
	data, err := fsutil.ReadFile(recordPath(room, id), MaxRecordBytes)
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, err
	}
	if r.ID != id || r.Envelope == nil || r.Envelope.ID != id || r.Agent != r.Envelope.To {
		return Record{}, fmt.Errorf("corrupt request record %q", id)
	}
	return r, nil
}

func save(room string, r Record) error {
	r.Updated = time.Now().UTC()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if int64(len(data)) > MaxRecordBytes {
		return fmt.Errorf("encoded request record exceeds %d bytes", MaxRecordBytes)
	}
	return fsutil.WriteFileAtomic(recordPath(room, r.ID), data)
}

func newRecord(e *envelope.Envelope) Record {
	copy := *e
	return Record{ID: e.ID, Agent: e.To, Status: "queued", Created: time.Now().UTC(), Updated: time.Now().UTC(), Envelope: &copy}
}

func sameRequest(r Record, e *envelope.Envelope) bool {
	return r.Envelope != nil && r.Envelope.To == e.To && r.Envelope.From == e.From && r.Envelope.Body == e.Body && r.Envelope.Kind == e.Kind && strings.Join(r.Envelope.Artifacts, "\x00") == strings.Join(e.Artifacts, "\x00")
}

// Submit is idempotent for an explicitly supplied ID and identical request.
// Persist before enqueueing. Retrying the same ID repairs an interrupted enqueue;
// a running or completed request is never submitted a second time.
func Submit(ctx context.Context, room, agent, from, body, id string) (Record, error) {
	return submit(ctx, room, agent, from, body, id, false)
}

// SubmitInteractive bridges an existing interactive /listen receiver into the
// request journal. Hosted MCP work uses Submit and needs no ephemeral reply.
// A saved dispatch intent is never republished by a retry: legacy receivers can
// consume work before replying, so missing transport evidence is uncertain.
func SubmitInteractive(ctx context.Context, room, agent, from, body, id string) (Record, error) {
	return submit(ctx, room, agent, from, body, id, true)
}

func submit(ctx context.Context, room, agent, from, body, id string, needsReply bool) (Record, error) {
	if err := spool.ValidName(agent); err != nil {
		return Record{}, err
	}
	if err := spool.ValidName(from); err != nil {
		return Record{}, err
	}
	if len(body) > MaxBodyBytes {
		return Record{}, fmt.Errorf("prompt exceeds %d bytes", MaxBodyBytes)
	}
	if id == "" {
		id = envelope.NewID()
	}
	l, err := lock(ctx, room, id)
	if err != nil {
		return Record{}, err
	}
	defer l.Close()
	channelHash := sha256.Sum256([]byte(id))
	channel := "r-" + hex.EncodeToString(channelHash[:])
	if !needsReply {
		channel = ""
	}
	e := &envelope.Envelope{ID: id, To: agent, From: from, Body: body, ReplyTo: channel, CorrID: channel, TS: time.Now().UTC()}
	r, err := Get(room, id)
	if err == nil {
		if !sameRequest(r, e) {
			return Record{}, fmt.Errorf("request ID %q already belongs to different work; use a new ID", id)
		}
		if r.Status != "queued" {
			return r, nil
		}
		if r.Interactive {
			if r.InteractivePublished {
				return r, nil
			}
			return resolveInteractivePublication(room, r)
		}
		e = r.Envelope
	} else if errors.Is(err, os.ErrNotExist) {
		r = newRecord(e)
		r.Interactive = needsReply
		if err := save(room, r); err != nil {
			return Record{}, err
		}
	} else {
		return Record{}, err
	}
	sp, err := spool.Open(room)
	if err != nil {
		return Record{}, err
	}
	if err := sp.Send(e); err != nil {
		if r.Interactive {
			resolved, resolveErr := resolveInteractivePublication(room, r)
			if resolveErr != nil {
				return r, fmt.Errorf("interactive request %s dispatch was not confirmed: %w (publication inspection: %v)", id, err, resolveErr)
			}
			return resolved, fmt.Errorf("interactive request %s dispatch returned an error and will not be resent automatically: %w", id, err)
		}
		return r, fmt.Errorf("request %s was saved but enqueue failed; retry the same request_id: %w", id, err)
	}
	if r.Interactive {
		r.InteractivePublished = true
		if err := save(room, r); err != nil {
			return r, fmt.Errorf("interactive request %s was published but confirmation failed; retry the same request_id to inspect without resending: %w", id, err)
		}
	}
	return r, nil
}

// resolveInteractivePublication handles a durable dispatch intent left by a
// process exit or older interactive records without confirmation. Absence is
// not permission to resend: a legacy receiver may already have consumed and
// acted on the envelope without writing anything to the request journal.
// Native queued requests deliberately retain their separate enqueue repair.
func resolveInteractivePublication(room string, r Record) (Record, error) {
	if err := envelope.Validate(r.Envelope); err != nil {
		return r, err
	}
	root := filepath.Join(canonicalRoom(room), ".tincan")
	filename := envelope.Filename(r.Envelope)
	paths := []string{
		filepath.Join(root, "inbox", r.Agent, filename),
		filepath.Join(root, "inflight", r.Agent, filename, "message.json"),
	}
	for _, path := range paths {
		data, err := fsutil.ReadFile(path, spool.MaxMessageBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return r, fmt.Errorf("cannot inspect interactive publication: %w", err)
		}
		existing, err := envelope.Unmarshal(data)
		if err != nil {
			return r, fmt.Errorf("cannot inspect interactive publication: %w", err)
		}
		if existing.ID != r.ID || !sameRequest(r, existing) || !existing.TS.Equal(r.Envelope.TS) ||
			existing.ReplyTo != r.Envelope.ReplyTo || existing.CorrID != r.Envelope.CorrID {
			return r, fmt.Errorf("%w: interactive request %s", spool.ErrEnvelopeConflict, r.ID)
		}
		r.InteractivePublished = true
		r.InteractivePublicationUncertain = false
		return r, save(room, r)
	}
	r.Status = "interrupted"
	r.InteractivePublicationUncertain = true
	r.Result = "ERROR interrupted: interactive request publication was not confirmed; delivery and execution may have occurred, and the request was not resent"
	return r, save(room, r)
}

// Start marks an accepted request running, unless it has a terminal result.
func Start(room string, e *envelope.Envelope) (Record, error) {
	l, err := lock(context.Background(), room, e.ID)
	if err != nil {
		return Record{}, err
	}
	defer l.Close()
	r, err := Get(room, e.ID)
	if errors.Is(err, os.ErrNotExist) {
		r = newRecord(e)
	} else if err != nil {
		return Record{}, err
	}
	if !sameRequest(r, e) {
		return Record{}, fmt.Errorf("request ID collision: %s", e.ID)
	}
	if r.Terminal() {
		return r, nil
	}
	if r.CancelRequested {
		r.Status, r.Result = "canceled", "ERROR canceled: execution was canceled before starting"
		return r, save(room, r)
	}
	r.Status = "running"
	r.Interactive = false // Start is called only by an owned hosted executor.
	return r, save(room, r)
}

// Finish persists a terminal result before the host replies or acknowledges its
// delivery. Existing terminal results win, including cancellations and duplicates.
func Finish(room string, e *envelope.Envelope, body string) (Record, error) {
	return finish(room, e, body, nil)
}

// finishInteractiveReply is used only for an actual collected reply. Its
// validation and terminal-state decision share the journal lock, so a Submit
// retry cannot race a collector and make a real answer permanently disappear.
func finishInteractiveReply(room string, e, reply *envelope.Envelope) (Record, error) {
	if err := envelope.Validate(reply); err != nil {
		return Record{}, err
	}
	return finish(room, e, reply.Body, reply)
}

func finish(room string, e *envelope.Envelope, body string, reply *envelope.Envelope) (Record, error) {
	var l *filelock.Lock
	var err error
	if reply == nil {
		l, err = lock(context.Background(), room, e.ID)
	} else {
		if err := validID(e.ID); err != nil {
			return Record{}, err
		}
		l, err = filelock.Try(filepath.Join(Dir(room), "locks", e.ID+".lock"))
		if errors.Is(err, filelock.ErrLocked) {
			return Record{}, errReplyJournalBusy
		}
	}
	if err != nil {
		return Record{}, err
	}
	defer l.Close()
	r, err := Get(room, e.ID)
	if errors.Is(err, os.ErrNotExist) {
		r = newRecord(e)
		r.Interactive = reply != nil
	} else if err != nil {
		return Record{}, err
	}
	if !sameRequest(r, e) {
		return Record{}, fmt.Errorf("request ID collision: %s", e.ID)
	}
	if reply != nil {
		// A hosted handoff may have changed mode after Poll read the record.
		// Interactive collection cannot finish native execution.
		if !r.Interactive {
			return r, nil
		}
		if reply.To != r.Envelope.ReplyTo || (reply.From != "" && reply.From != r.Agent) {
			return r, errors.New("interactive reply does not match the request channel and agent")
		}
	}
	resolvesPublication := reply != nil && r.Interactive && r.Status == "interrupted" && r.InteractivePublicationUncertain
	if r.Terminal() && !resolvesPublication {
		return r, nil
	}
	if reply != nil {
		r.InteractivePublished = true
		r.InteractivePublicationUncertain = false
	}
	if len(body) > MaxResultBytes {
		body = body[:MaxResultBytes] + "\n[output truncated]"
	}
	r.Status, r.Result = "completed", body
	switch {
	case r.CancelRequested || strings.HasPrefix(body, "ERROR canceled"):
		r.Status = "canceled"
		if !strings.HasPrefix(body, "ERROR") {
			r.Result = "ERROR canceled: the request may have produced changes before cancellation\n" + body
		}
	case strings.HasPrefix(body, "ERROR interrupted"):
		r.Status = "interrupted"
	case strings.HasPrefix(body, "ERROR"):
		r.Status = "failed"
	}
	return r, save(room, r)
}

// Cancel prevents queued work from starting. Running work remains running until
// the host acknowledges cancellation; callers should also call host.Cancel.
func Cancel(ctx context.Context, room, id string) (Record, error) {
	l, err := lock(ctx, room, id)
	if err != nil {
		return Record{}, err
	}
	defer l.Close()
	r, err := Get(room, id)
	if err != nil {
		return Record{}, err
	}
	if r.Terminal() {
		return r, nil
	}
	if r.Interactive {
		return r, errors.New("interactive listeners cannot acknowledge request cancellation; no cancellation was recorded")
	}
	r.CancelRequested = true
	if r.Status == "queued" {
		r.Status = "canceled"
		r.Result = "ERROR canceled: request was canceled before execution"
	}
	return r, save(room, r)
}

func Wait(ctx context.Context, room, id string, timeout time.Duration) (Record, error) {
	if timeout < 0 {
		return Record{}, errors.New("wait timeout must be nonnegative")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := Poll(ctx, room, id)
		if err != nil || (r.Terminal() && !r.InteractivePublicationUncertain) || timeout == 0 {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-deadline.C:
			return Get(room, id)
		case <-tick.C:
		}
	}
}
