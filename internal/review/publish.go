package review

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

var (
	ErrNoCoordinator = errors.New("tincan web is not running on this machine; start it (see docs/web.md)")
	ErrConflict      = errors.New("a different review already uses this request_id")
)

type PublishRequest struct {
	StateDir       string
	Room           string
	Committee      string
	Question       string
	Scope          string
	RequestID      string
	Origin         string
	Machine        string
	CommitteesFrom *string
	// Snapshot, when set, is the committee frozen at mention time (thread
	// reviews); it is used instead of looking the committee up again.
	Snapshot      *committee.Committee
	InCoordinator bool
	Registry      *rooms.Registry
	Now           func() time.Time
}

// Publish creates a review in the room (committees §6.3): coordinator check,
// registry membership, replay, per-attempt staging, then one rename.
func Publish(ctx context.Context, req PublishRequest) (Input, State, error) {
	now := time.Now
	if req.Now != nil {
		now = req.Now
	}
	if strings.TrimSpace(req.Question) == "" {
		return Input{}, State{}, errors.New("the question is empty")
	}
	if req.StateDir == "" {
		return Input{}, State{}, errors.New("no tincan state directory")
	}
	machine, fromPeer := req.Machine, false
	if req.CommitteesFrom != nil {
		fromPeer = *req.CommitteesFrom != ""
	}
	if !req.InCoordinator {
		if !rooms.CoordinatorAlive(req.StateDir, now()) {
			return Input{}, State{}, ErrNoCoordinator
		}
		hb, err := rooms.ReadHeartbeat(req.StateDir)
		if err != nil {
			return Input{}, State{}, ErrNoCoordinator
		}
		if machine == "" {
			machine = hb.Machine
		}
		if req.CommitteesFrom == nil {
			fromPeer = hb.CommitteesFrom != ""
		}
	}
	if machine == "" {
		return Input{}, State{}, errors.New("this machine's name is unknown")
	}
	var c committee.Committee
	if req.Snapshot != nil {
		c = *req.Snapshot
		if err := c.Normalize(); err != nil {
			return Input{}, State{}, err
		}
	} else {
		var err error
		if c, err = committee.Lookup(req.StateDir, req.Committee, fromPeer); err != nil {
			return Input{}, State{}, err
		}
	}
	room, err := rooms.Canonical(req.Room)
	if err != nil {
		return Input{}, State{}, err
	}
	reg := req.Registry
	if reg == nil {
		reg = rooms.Default()
	}
	if err := reg.Touch(room); err != nil {
		return Input{}, State{}, fmt.Errorf("room registry: %w", err)
	}
	if r, ok, err := reg.Get(rooms.ID(room)); err != nil || !ok || r.Missing {
		return Input{}, State{}, fmt.Errorf("%s cannot be coordinated (excluded, too broad, or the room registry is unavailable)", room)
	}
	id := NewID(machine, room, req.RequestID)
	p, err := packet.Build(ctx, room, req.Scope, req.Question)
	if err != nil {
		return Input{}, State{}, err
	}
	sum := sha256.Sum256([]byte(req.Question))
	created := now().UTC()
	in := Input{ReviewID: id, Committee: c, Question: req.Question, QuestionSHA: hex.EncodeToString(sum[:]), Scope: p.Manifest.Scope,
		IncludedTree: p.Manifest.IncludedTree, Origin: req.Origin, Requester: machine, Created: created,
		Deadline: created.Add(time.Duration(c.DeadlineMinutes) * time.Minute), PacketSHA: packet.Hash(p)}
	if existing, err := ReadInput(room, id); err == nil {
		return replay(room, id, existing, in)
	}
	st := State{ReviewID: id, Status: "running", Members: make([]Member, len(c.Members))}
	for i, m := range c.Members {
		st.Members[i] = Member{Member: m, Index: i, JobID: id + "-" + strconv.Itoa(i), ExpiresAt: in.Deadline.Add(30 * time.Minute), State: "planned"}
	}
	var nonce [6]byte
	rand.Read(nonce[:])
	staging := filepath.Join(Root(room), "staging", id+"."+hex.EncodeToString(nonce[:]))
	if err := fsutil.MkdirPrivate(filepath.Join(staging, "prompts")); err != nil {
		return Input{}, State{}, err
	}
	defer os.RemoveAll(staging)
	if err := fsutil.MkdirPrivate(filepath.Join(staging, "results")); err != nil {
		return Input{}, State{}, err
	}
	for i, m := range c.Members {
		if err := fsutil.WriteFileAtomic(filepath.Join(staging, "prompts", strconv.Itoa(i)+".txt"), []byte(BuildPrompt(c, m, p))); err != nil {
			return Input{}, State{}, err
		}
	}
	for name, v := range map[string]any{"input.json": in, "packet.json": p, "review.json": st} {
		if err := writeJSON(filepath.Join(staging, name), v); err != nil {
			return Input{}, State{}, err
		}
	}
	if err := os.Rename(staging, Dir(room, id)); err != nil {
		if existing, rerr := ReadInput(room, id); rerr == nil {
			return replay(room, id, existing, in)
		}
		return Input{}, State{}, err
	}
	fsutil.SyncDir(Root(room))
	return in, st, nil
}

func replay(room, id string, existing, want Input) (Input, State, error) {
	if !existing.SameIdentity(want) {
		return Input{}, State{}, ErrConflict
	}
	st, err := ReadState(room, id)
	return existing, st, err
}

// RequestCancel records cancellation (committees §6.4); the coordinator
// then cancels members. A running review becomes cancelled at once.
func RequestCancel(ctx context.Context, room, id string) (State, error) {
	// Taking the lock would create the review directory; an unknown ID must
	// leave nothing behind.
	if _, err := ReadState(room, id); err != nil {
		return State{}, fmt.Errorf("review %s not found: %w", id, err)
	}
	l, err := Lock(ctx, room, id)
	if err != nil {
		return State{}, err
	}
	defer l.Close()
	st, err := ReadState(room, id)
	if err != nil {
		return st, err
	}
	if st.Settled || st.CancelRequested {
		return st, nil
	}
	st.CancelRequested = true
	if st.Status == "running" {
		st.Status = "cancelled"
	}
	return st, WriteState(room, id, st)
}
