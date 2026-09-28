// Package review owns committee reviews in a room (committees spec §6.2–§6.4,
// §6.8–§6.10): publication by entry points, and a coordinator that advances
// each member through submit, collect, acknowledge and cancel until the
// review closes and settles.
package review

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
)

type Input struct {
	ReviewID     string              `json:"review_id"`
	Committee    committee.Committee `json:"committee"`
	Question     string              `json:"question"`
	QuestionSHA  string              `json:"question_sha256"`
	Scope        string              `json:"scope"`
	IncludedTree string              `json:"included_tree,omitempty"`
	Origin       string              `json:"origin"`
	Requester    string              `json:"requester"`
	Created      time.Time           `json:"created"`
	Deadline     time.Time           `json:"deadline"`
	PacketSHA    string              `json:"packet_sha256"`
}

// SameIdentity compares what a replay must match (committees §6.2).
func (in Input) SameIdentity(o Input) bool {
	return in.Committee.Name == o.Committee.Name && in.Committee.Version == o.Committee.Version &&
		slices.Equal(in.Committee.Members, o.Committee.Members) && in.QuestionSHA == o.QuestionSHA &&
		in.Scope == o.Scope && in.IncludedTree == o.IncludedTree && in.Origin == o.Origin
}

type Member struct {
	Member      string    `json:"member"`
	Index       int       `json:"index"`
	JobID       string    `json:"job_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	State       string    `json:"state"`
	Late        bool      `json:"late,omitempty"`
	Posted      bool      `json:"posted,omitempty"`
	Acked       bool      `json:"acked,omitempty"`
	CancelAcked bool      `json:"cancel_acked,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Started     time.Time `json:"started,omitempty"`
	Finished    time.Time `json:"finished,omitempty"`
	Note        string    `json:"note,omitempty"`
}

type ClosedMember struct {
	Index    int    `json:"index"`
	State    string `json:"state"`
	Late     bool   `json:"late,omitempty"`
	Included bool   `json:"included"`
}

type Closure struct {
	At      time.Time      `json:"at"`
	Members []ClosedMember `json:"members"`
}

type State struct {
	ReviewID        string     `json:"review_id"`
	Status          string     `json:"status"`
	Settled         bool       `json:"settled"`
	CancelRequested bool       `json:"cancel_requested,omitempty"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	Closure         *Closure   `json:"closure,omitempty"`
	ThreadDone      bool       `json:"thread_done,omitempty"`
	Members         []Member   `json:"members"`
}

// Final reports member states that need no further collection.
func Final(state string) bool {
	switch state {
	case "done", "error", "skipped", "cancelled", "expired":
		return true
	}
	return false
}

// NewID derives a review ID from the requesting machine, room and key; an
// empty key yields a random ID (committees §6.2).
func NewID(machine, room, key string) string {
	if key == "" {
		var b [8]byte
		rand.Read(b[:])
		return "rv-" + hex.EncodeToString(b[:])
	}
	sum := sha256.Sum256([]byte(machine + "\x00" + room + "\x00" + key))
	return "rv-" + hex.EncodeToString(sum[:])[:16]
}
