// Package envelope defines the tincan message format and its on-disk naming.
package envelope

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
	// Kind classifies the message: "" (default) is a normal message; "stop"
	// is a wind-down control message (see `tincan stop`). omitempty keeps
	// existing/normal-message JSON byte-identical to before this field.
	Kind string `json:"kind,omitempty"`
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
	if err := Validate(e); err != nil {
		return nil, err
	}
	return json.MarshalIndent(e, "", "  ")
}

// Unmarshal parses one envelope.
func Unmarshal(data []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	if err := Validate(&e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ValidComponent accepts one portable filename component, including legacy
// human-readable IDs, while rejecting traversal, alternate streams, Windows
// device names and control characters.
func ValidComponent(value string) error {
	if value == "" || value == "." || value == ".." || len(value) > 128 || !utf8.ValidString(value) ||
		strings.ContainsAny(value, `/\\<>:"|?*`) || strings.HasSuffix(value, ".") || strings.HasSuffix(value, " ") {
		return fmt.Errorf("tincan: invalid name or ID %q", value)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("tincan: invalid name or ID %q", value)
		}
	}
	base := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		return fmt.Errorf("tincan: reserved name or ID %q", value)
	}
	return nil
}

// Validate checks the parts of an envelope used for routing or filesystem paths.
// Artifact paths are descriptive metadata and are never opened by the spool.
func Validate(e *Envelope) error {
	if e == nil {
		return fmt.Errorf("tincan: nil envelope")
	}
	for _, value := range []string{e.ID, e.To} {
		if err := ValidComponent(value); err != nil {
			return err
		}
	}
	for _, value := range []string{e.From, e.ReplyTo, e.CorrID} {
		if value != "" {
			if err := ValidComponent(value); err != nil {
				return err
			}
		}
	}
	// UnixNano is undefined outside this range and would break queue ordering.
	if e.TS.IsZero() || e.TS.Before(time.Unix(0, 0)) || e.TS.After(time.Unix(0, int64(1<<63-1))) {
		return fmt.Errorf("tincan: timestamp outside supported range")
	}
	return nil
}
