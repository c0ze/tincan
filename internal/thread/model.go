// internal/thread/model.go

// Package thread stores tincan web chat threads as append-only journals in
// the room and dispatches their agent turns.
package thread

import (
	"sort"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
)

const (
	StatusOpen      = "open"
	StatusStopping  = "stopping"
	StatusArchiving = "archiving"
	StatusArchived  = "archived"

	KindMessage  = "message"
	KindIntent   = "intent"
	KindState    = "state"
	KindHandoffs = "handoffs"
	KindChain    = "chain"
	KindThread   = "thread"
	KindRetry    = "retry"

	RoleUser      = "user"
	RoleAgent     = "agent"
	RoleSystem    = "system"
	RoleCommittee = "committee" // one committee review (committees spec §6.10)
	RoleReview    = "review"    // one member's review result

	StatePending       = "pending"
	StateRunning       = "running"
	StateDone          = "done"
	StateError         = "error"
	StateCancelled     = "cancelled"
	StateSuggested     = "suggested"
	StateUncollectable = "uncollectable"

	OpReserve = "reserve"
	OpStop    = "stop"
)

type Meta struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Primary   string            `json:"primary"`
	Created   time.Time         `json:"created"`
	Status    string            `json:"status"`
	Budget    int               `json:"budget"`
	ClientID  string            `json:"client_id,omitempty"`
	Listeners map[string]string `json:"listeners,omitempty"` // thread listener → preset
}

// Event is one journal line. Fields are used by kind:
// message: ID N Author Role Text ReplyTo Chain ClientID; intent: Message
// Listener Preset RequestID PromptSHA; state: Message State Text; handoffs:
// Message Produced; chain: Chain Op (reserve may carry Key: a replayed keyed
// reservation is charged once); thread: (none, marks a meta change). A
// committee intent also carries Review (the review ID) and Committee (the
// snapshot frozen at mention time); a review message carries Review
// ("<review_id>/<n>").
type Event struct {
	Seq       int64                `json:"seq"`
	Kind      string               `json:"kind"`
	Time      time.Time            `json:"time"`
	ID        string               `json:"id,omitempty"`
	N         int64                `json:"n,omitempty"`
	Author    string               `json:"author,omitempty"`
	Role      string               `json:"role,omitempty"`
	Text      string               `json:"text,omitempty"`
	ReplyTo   string               `json:"reply_to,omitempty"`
	Chain     string               `json:"chain,omitempty"`
	ClientID  string               `json:"client_id,omitempty"`
	Message   string               `json:"message,omitempty"`
	Listener  string               `json:"listener,omitempty"`
	Preset    string               `json:"preset,omitempty"`
	RequestID string               `json:"request_id,omitempty"`
	PromptSHA string               `json:"prompt_sha256,omitempty"`
	State     string               `json:"state,omitempty"`
	Produced  []string             `json:"produced,omitempty"`
	Op        string               `json:"op,omitempty"`
	Meta      *Meta                `json:"meta,omitempty"`
	Review    string               `json:"review,omitempty"`
	Committee *committee.Committee `json:"committee,omitempty"`
	Key       string               `json:"key,omitempty"`
}

type Message struct {
	ID        string               `json:"id"`
	N         int64                `json:"n"`
	Seq       int64                `json:"seq"`
	Time      time.Time            `json:"time"`
	Author    string               `json:"author"`
	Role      string               `json:"role"`
	Text      string               `json:"text"`
	ReplyTo   string               `json:"reply_to,omitempty"`
	Chain     string               `json:"chain,omitempty"`
	ClientID  string               `json:"client_id,omitempty"`
	State     string               `json:"state,omitempty"`
	Listener  string               `json:"listener,omitempty"`
	Preset    string               `json:"preset,omitempty"`
	RequestID string               `json:"request_id,omitempty"`
	Handoffs  bool                 `json:"handoffs,omitempty"`
	Retried   bool                 `json:"retried,omitempty"`
	Updated   time.Time            `json:"updated"`
	Review    string               `json:"review,omitempty"`
	Committee *committee.Committee `json:"committee,omitempty"`
}

type Chain struct {
	Used    int             `json:"used"`
	Stopped bool            `json:"stopped"`
	keys    map[string]bool // keyed reservations already charged
}

type Snapshot struct {
	Meta     Meta
	Messages []Message
	Chains   map[string]*Chain
	MaxSeq   int64
	NextN    int64
	index    map[string]int
}

func newSnapshot(m Meta) *Snapshot {
	return &Snapshot{Meta: m, Chains: map[string]*Chain{}, NextN: 1, index: map[string]int{}}
}

func (s *Snapshot) apply(e Event) {
	if e.Seq > s.MaxSeq {
		s.MaxSeq = e.Seq
	}
	switch e.Kind {
	case KindMessage:
		s.index[e.ID] = len(s.Messages)
		s.Messages = append(s.Messages, Message{ID: e.ID, N: e.N, Seq: e.Seq, Time: e.Time, Author: e.Author, Role: e.Role, Text: e.Text, ReplyTo: e.ReplyTo, Chain: e.Chain, ClientID: e.ClientID, Updated: e.Time, Review: e.Review})
		if e.N >= s.NextN {
			s.NextN = e.N + 1
		}
	case KindIntent, KindState, KindHandoffs, KindRetry:
		i, ok := s.index[e.Message]
		if !ok {
			return
		}
		m := &s.Messages[i]
		m.Seq, m.Updated = e.Seq, e.Time
		switch e.Kind {
		case KindIntent:
			m.Listener, m.Preset, m.RequestID = e.Listener, e.Preset, e.RequestID
			m.Review, m.Committee = e.Review, e.Committee
		case KindState:
			m.State = e.State
			if e.Text != "" || e.State == StateDone {
				m.Text = e.Text
			}
		case KindHandoffs:
			m.Handoffs = true
		case KindRetry:
			m.Retried = true
		}
	case KindThread:
		// The journal is authoritative for meta: a thread event carrying a
		// non-nil Meta (written at commit time by SaveMeta) replaces the
		// snapshot's meta, overriding the thread.json cache it started from.
		if e.Meta != nil {
			s.Meta = *e.Meta
		}
	case KindChain:
		c := s.Chains[e.Chain]
		if c == nil {
			c = &Chain{}
			s.Chains[e.Chain] = c
		}
		switch e.Op {
		case OpReserve:
			if e.Key != "" {
				if c.keys == nil {
					c.keys = map[string]bool{}
				}
				if c.keys[e.Key] {
					break // a replayed keyed reservation is charged once
				}
				c.keys[e.Key] = true
			}
			c.Used++
		case OpStop:
			c.Stopped = true
		}
	}
}

func (s *Snapshot) sortMessages() {
	sort.SliceStable(s.Messages, func(i, j int) bool { return s.Messages[i].N < s.Messages[j].N })
	for i, m := range s.Messages {
		s.index[m.ID] = i
	}
}

func (s *Snapshot) Message(id string) (Message, bool) {
	i, ok := s.index[id]
	if !ok {
		return Message{}, false
	}
	return s.Messages[i], true
}

func (s *Snapshot) ByClientID(id string) (Message, bool) {
	for _, m := range s.Messages {
		if m.ClientID == id && m.Role == RoleUser {
			return m, true
		}
	}
	return Message{}, false
}

// Active reports agent messages that are not yet terminal.
func (m Message) Active() bool {
	return m.Role == RoleAgent && (m.State == StatePending || m.State == StateRunning)
}
