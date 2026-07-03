// Package envelope defines the tincan message format and its on-disk naming.
package envelope

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Envelope is one tincan message. One envelope = one JSON file in a spool inbox.
type Envelope struct {
	ID        string    `json:"id"`
	CorrID    string    `json:"corr_id,omitempty"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	TS        time.Time `json:"ts"`
	Body      string    `json:"body"`
	Artifacts []string  `json:"artifacts,omitempty"`
}

// NewID returns a 32-char random hex string.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

// Filename returns the spool filename for e: zero-padded UnixNano, then ID,
// so lexical order == chronological order. It assumes e.TS is set and
// post-1970; unset or pre-epoch timestamps do not sort correctly.
func Filename(e *Envelope) string {
	return fmt.Sprintf("%020d-%s.json", e.TS.UnixNano(), e.ID)
}

// Marshal renders e as indented JSON (readable when inspecting a spool by hand).
func Marshal(e *Envelope) ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
}

// Unmarshal parses one envelope.
func Unmarshal(data []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}
