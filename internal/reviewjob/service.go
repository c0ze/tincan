package reviewjob

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
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/spool"
)

type CreateRequest struct {
	JobID     string         `json:"job_id"`
	ReviewID  string         `json:"review_id"`
	Requester string         `json:"requester"`
	Preset    string         `json:"preset"`
	ExpiresAt time.Time      `json:"expires_at"`
	Packet    *packet.Packet `json:"packet"`
	Prompt    string         `json:"prompt"`
	PromptSHA string         `json:"prompt_sha256"`
}

type Service struct {
	StateDir   string
	Registry   *rooms.Registry
	Presets    func() (map[string]host.Preset, error)
	Now        func() time.Time
	Executable string

	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
}

func (s *Service) store() Store { return Store{Dir: filepath.Join(s.StateDir, "reviews")} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) workspace(id string) string {
	return filepath.Join(s.StateDir, "reviews", "ws", id)
}

func (s *Service) packetPath(id string) string {
	return filepath.Join(s.StateDir, "reviews", "jobs", id+".input.json")
}

// Wait blocks until background creations started by this process finish.
func (s *Service) Wait() { s.wg.Wait() }

// Create validates and records a job, then materializes and launches it in
// the background (committees §6.6). An identical replay returns the job;
// a different identity is a conflict; expired or tombstoned jobs are gone.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Job, error) {
	if err := validID(req.JobID); err != nil {
		return Job{}, invalid(err.Error())
	}
	if req.Packet == nil {
		return Job{}, invalid("packet is required")
	}
	if err := req.Packet.Validate(); err != nil {
		return Job{}, invalid(err.Error())
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	if hex.EncodeToString(sum[:]) != req.PromptSHA {
		return Job{}, invalid("prompt_sha256 does not match the prompt")
	}
	if len(req.Prompt) > 128<<10 {
		return Job{}, invalid("prompt exceeds 128 KiB")
	}
	now := s.now()
	if !req.ExpiresAt.After(now) {
		return Job{}, gone("the job has expired")
	}
	if req.ExpiresAt.After(now.Add(MaxLifetime)) {
		return Job{}, invalid(fmt.Sprintf("expires_at is more than %v away", MaxLifetime))
	}
	presets, err := s.Presets()
	if err != nil {
		return Job{}, err
	}
	if _, ok := presets[req.Preset]; !ok {
		return Job{}, notFound(fmt.Sprintf("no preset %q on this machine", req.Preset))
	}
	if err := spool.ValidName(req.Preset); err != nil {
		return Job{}, invalid(err.Error())
	}
	want := Job{ID: req.JobID, ReviewID: req.ReviewID, Requester: req.Requester, Preset: req.Preset,
		PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA, ExpiresAt: req.ExpiresAt.UTC(), State: "creating", Created: now.UTC()}
	st := s.store()
	lock, err := st.Lock(ctx, req.JobID)
	if err != nil {
		return Job{}, err
	}
	defer lock.Close()
	cur, ok, err := st.Get(req.JobID)
	if err != nil {
		return Job{}, err
	}
	if ok {
		if cur.Tombstone && cur.PacketSHA == "" {
			return Job{}, gone("the job was cancelled before it was created")
		}
		if !cur.SameIdentity(want) {
			return Job{}, conflict("a different job already uses this job_id")
		}
		return cur, nil
	}
	if err := writeInput(s.packetPath(req.JobID), req); err != nil {
		return Job{}, err
	}
	if err := st.Save(want); err != nil {
		return Job{}, err
	}
	s.start(req.JobID)
	return want, nil
}

// start runs creation in the background unless it already runs here.
func (s *Service) start(id string) {
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]bool{}
	}
	if s.inflight[id] {
		s.mu.Unlock()
		return
	}
	s.inflight[id] = true
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, id)
			s.mu.Unlock()
		}()
		s.create(context.Background(), id)
	}()
}

// create materializes the workspace and launches the member, holding the job
// lock throughout. It resumes a job left in "creating" by a dead process:
// a request already saved in the recorded workspace is adopted, so a member
// that ran is never run again (committees §6.6, round 5 #1).
func (s *Service) create(ctx context.Context, id string) {
	st := s.store()
	lock, err := st.Lock(ctx, id)
	if err != nil {
		return
	}
	defer lock.Close()
	j, ok, err := st.Get(id)
	if err != nil || !ok || j.State != "creating" {
		return
	}
	fail := func(msg string) {
		j.State, j.Result = "error", "ERROR "+msg
		st.Save(j)
	}
	if j.Workspace != "" {
		if r, err := request.Get(j.Workspace, j.ID); err == nil {
			j.State = "running"
			importRequest(&j, r)
			st.Save(j)
			return
		}
		os.RemoveAll(j.Workspace)
	}
	req, err := readInput(s.packetPath(id))
	if err != nil {
		fail("job input lost: " + err.Error())
		return
	}
	presets, err := s.Presets()
	if err != nil {
		fail(err.Error())
		return
	}
	preset, ok := presets[j.Preset]
	if !ok {
		fail("preset " + j.Preset + " no longer exists")
		return
	}
	pinned, err := pin(preset)
	if err != nil {
		fail(err.Error())
		return
	}
	left := time.Until(j.ExpiresAt)
	if left < time.Minute {
		j.State, j.Result = "error", "ERROR expired before launch"
		st.Save(j)
		return
	}
	pinned.ExecTimeoutSec = int(left / time.Second)
	j.Workspace = s.workspace(id)
	if err := st.Save(j); err != nil { // the workspace is recorded before anything runs
		return
	}
	note, reason, err := materializeCheckout(ctx, j.Workspace, req.Packet, s.Registry)
	j.Mode = "checkout"
	if err == nil && reason != "" {
		j.Mode = "packet"
		note, err = materializePacket(j.Workspace, req.Packet, reason)
	}
	if err != nil {
		os.RemoveAll(j.Workspace)
		fail("workspace: " + err.Error())
		return
	}
	j.Note = note
	body := note + "\n\n" + req.Prompt
	o := dispatch.Options{Room: j.Workspace, Executable: s.Executable, Presets: map[string]host.Preset{j.Preset: pinned}}
	r, err := dispatch.Send(ctx, o, dispatch.SendSpec{Agent: j.Preset, Preset: j.Preset, From: "review", Body: body, RequestID: j.ID})
	if err != nil {
		// A launch failure after the request was saved must never run later.
		if saved, gerr := request.Get(j.Workspace, j.ID); gerr == nil && !saved.Terminal() {
			request.Cancel(ctx, j.Workspace, j.ID)
		}
		fail("launch: " + err.Error())
		return
	}
	j.State = "running"
	importRequest(&j, r)
	st.Save(j)
}

// pin resolves the preset's executable before any workspace exists and
// returns a stateless preset that runs that exact path (committees §6.6).
func pin(p host.Preset) (host.Preset, error) {
	name := p.Exec[0]
	if strings.ContainsAny(name, `/\`) && !filepath.IsAbs(name) {
		return host.Preset{}, fmt.Errorf("preset executable %q is relative; reviewers need a bare name or an absolute path", name)
	}
	path, err := host.ResolveExecutable(name, "")
	if err != nil {
		return host.Preset{}, err
	}
	if err := host.PinnedExecutable(path); err != nil {
		return host.Preset{}, err
	}
	p.Exec = append([]string{path}, p.Exec[1:]...)
	p.Pinned = true
	return host.WithSession(p, "reviewer", "stateless")
}

// importRequest copies a terminal request's outcome into the job.
func importRequest(j *Job, r request.Record) {
	if !r.Terminal() {
		return
	}
	res := r.Result
	if len(res) > MaxResult {
		res = res[:MaxResult] + "\n[truncated]"
	}
	switch {
	case r.Status == "completed" && !strings.HasPrefix(r.Result, "ERROR"):
		j.State = "done"
	case r.Status == "canceled":
		j.State = "cancelled"
	default:
		j.State = "error"
	}
	j.Result = res
}

// refresh imports the request outcome of a running job. The caller holds
// the job lock.
func (s *Service) refresh(j *Job) bool {
	if j.State != "running" || j.Workspace == "" {
		return false
	}
	r, err := request.Get(j.Workspace, j.ID)
	if err != nil || !r.Terminal() {
		return false
	}
	importRequest(j, r)
	return true
}

// Status returns the job, importing a finished result when the lock is free;
// while creation holds the lock it returns the stored record.
func (s *Service) Status(id string) (Job, error) {
	st := s.store()
	j, ok, err := st.Get(id)
	if err != nil {
		return j, invalid(err.Error())
	}
	if !ok {
		return j, notFound("no such job")
	}
	lock, err := st.TryLock(id)
	if err != nil {
		return j, nil
	}
	defer lock.Close()
	if j, _, err = st.Get(id); err == nil && s.refresh(&j) {
		st.Save(j)
	}
	return j, err
}

// Ack records that the requester has the result; cleanup may follow.
func (s *Service) Ack(ctx context.Context, id string) (Job, error) {
	st := s.store()
	lock, err := st.Lock(ctx, id)
	if err != nil {
		return Job{}, invalid(err.Error())
	}
	defer lock.Close()
	j, ok, err := st.Get(id)
	if err != nil {
		return j, err
	}
	if !ok {
		return j, notFound("no such job")
	}
	s.refresh(&j)
	if !j.Terminal() {
		return j, conflict("the job has not finished")
	}
	j.Acked = true
	return j, st.Save(j)
}

// Cancel stops a job. A job that already finished keeps its result and the
// cancel is acknowledged; an unknown job gets a tombstone that refuses a
// later create. acked reports whether the member can no longer run.
func (s *Service) Cancel(ctx context.Context, id string, expiresAt time.Time) (Job, bool, error) {
	st := s.store()
	lock, err := st.Lock(ctx, id)
	if err != nil {
		return Job{}, false, invalid(err.Error())
	}
	defer lock.Close()
	j, ok, err := st.Get(id)
	if err != nil {
		return j, false, err
	}
	if !ok {
		if expiresAt.IsZero() {
			expiresAt = s.now().Add(TombstoneKeep)
		}
		j = Job{ID: id, State: "cancelled", Tombstone: true, ExpiresAt: expiresAt.UTC(), Created: s.now().UTC()}
		return j, true, st.Save(j)
	}
	s.refresh(&j)
	if j.Terminal() {
		j.Tombstone = true
		return j, true, st.Save(j)
	}
	j.Tombstone = true
	if j.State == "creating" && j.Workspace == "" {
		j.State, j.Result = "cancelled", "ERROR canceled before launch"
		return j, true, st.Save(j)
	}
	if j.Workspace != "" {
		if r, err := request.Cancel(ctx, j.Workspace, j.ID); err == nil {
			importRequest(&j, r)
		}
		if !j.Terminal() {
			host.Cancel(ctx, j.Workspace, j.Preset, j.ID)
			if _, alive := host.Existing(ctx, j.Workspace, j.Preset); !alive {
				j.State, j.Result = "cancelled", "ERROR canceled: the reviewer's host is not running"
			}
		}
	}
	if err := st.Save(j); err != nil {
		return j, false, err
	}
	return j, j.Terminal(), nil
}

var errNoInput = errors.New("no job input")

// writeInput / readInput keep the create request (packet and prompt) beside
// the record, so a resumed creation uses unchanged inputs.
func writeInput(path string, req CreateRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data)
}

func readInput(path string) (CreateRequest, error) {
	var req CreateRequest
	data, err := fsutil.ReadFile(path, packet.MaxEncoded+(1<<20))
	if errors.Is(err, os.ErrNotExist) {
		return req, errNoInput
	}
	if err != nil {
		return req, err
	}
	return req, json.Unmarshal(data, &req)
}
