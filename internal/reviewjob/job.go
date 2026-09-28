// Package reviewjob runs committee member jobs on the member's machine
// (committees spec §6.6): it materializes a private workspace from a
// packet, runs the member preset once with a pinned executable, and reports
// status, acknowledgement, cancellation and cleanup.
package reviewjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

const (
	MaxLifetime   = 270 * time.Minute
	TombstoneKeep = 7 * 24 * time.Hour
	MaxResult     = 1 << 20
)

type Job struct {
	ID        string    `json:"job_id"`
	ReviewID  string    `json:"review_id"`
	Requester string    `json:"requester"`
	Preset    string    `json:"preset"`
	PacketSHA string    `json:"packet_sha256"`
	PromptSHA string    `json:"prompt_sha256"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	Workspace string    `json:"workspace,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Note      string    `json:"note,omitempty"`
	Result    string    `json:"result,omitempty"`
	Tombstone bool      `json:"tombstone,omitempty"`
	Acked     bool      `json:"acked,omitempty"`
	Cleaned   bool      `json:"cleaned,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

func (j Job) Terminal() bool {
	return j.State == "done" || j.State == "error" || j.State == "cancelled"
}

func (j Job) SameIdentity(o Job) bool {
	return j.ID == o.ID && j.PacketSHA == o.PacketSHA && j.PromptSHA == o.PromptSHA && j.Preset == o.Preset && j.ExpiresAt.Equal(o.ExpiresAt)
}

// Error is a job API failure with its HTTP status (committees §6.4 obligation 1).
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func conflict(msg string) error { return &Error{409, msg} }
func gone(msg string) error     { return &Error{410, msg} }
func notFound(msg string) error { return &Error{404, msg} }
func invalid(msg string) error  { return &Error{422, msg} }

// Store keeps job records under <state>/reviews/jobs.
type Store struct{ Dir string }

func (s Store) jobsDir() string { return filepath.Join(s.Dir, "jobs") }

func (s Store) Path(id string) string { return filepath.Join(s.jobsDir(), id+".json") }

func validID(id string) error {
	if err := envelope.ValidComponent(id); err != nil || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid job id %q", id)
	}
	return nil
}

func (s Store) Lock(ctx context.Context, id string) (*filelock.Lock, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if err := fsutil.MkdirPrivate(s.jobsDir()); err != nil {
		return nil, err
	}
	return filelock.Acquire(ctx, filepath.Join(s.jobsDir(), id+".lock"))
}

func (s Store) TryLock(id string) (*filelock.Lock, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if err := fsutil.MkdirPrivate(s.jobsDir()); err != nil {
		return nil, err
	}
	return filelock.Try(filepath.Join(s.jobsDir(), id+".lock"))
}

func (s Store) Get(id string) (Job, bool, error) {
	var j Job
	if err := validID(id); err != nil {
		return j, false, err
	}
	data, err := fsutil.ReadFile(s.Path(id), 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, false, fmt.Errorf("%s: %w", s.Path(id), err)
	}
	return j, true, nil
}

func (s Store) Save(j Job) error {
	if err := validID(j.ID); err != nil {
		return err
	}
	if err := fsutil.MkdirPrivate(s.jobsDir()); err != nil {
		return err
	}
	j.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.Path(j.ID), data)
}

func (s Store) List() ([]Job, error) {
	entries, err := os.ReadDir(s.jobsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(id, ".") {
			continue
		}
		if j, ok, err := s.Get(id); err == nil && ok {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (s Store) Remove(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	for _, p := range []string{s.Path(id), filepath.Join(s.jobsDir(), id+".lock")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
