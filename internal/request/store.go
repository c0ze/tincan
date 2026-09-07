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

type Record struct {
	ID              string             `json:"id"`
	Agent           string             `json:"agent"`
	Status          string             `json:"status"`
	Result          string             `json:"result,omitempty"`
	Created         time.Time          `json:"created"`
	Updated         time.Time          `json:"updated"`
	CancelRequested bool               `json:"cancel_requested,omitempty"`
	Interactive     bool               `json:"interactive,omitempty"`
	Envelope        *envelope.Envelope `json:"request"`
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
		return r, fmt.Errorf("request %s was saved but enqueue failed; retry the same request_id: %w", id, err)
	}
	return r, nil
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
		if err != nil || r.Terminal() || timeout == 0 {
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
