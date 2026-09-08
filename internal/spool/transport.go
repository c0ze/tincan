package spool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

// ErrEnvelopeConflict means a queued/in-flight filename already identifies
// different content. Identical retries are successful and never replace it.
var ErrEnvelopeConflict = errors.New("tincan: envelope filename already belongs to different content")

// One persistent lock coordinates short queue transitions, including duplicate
// comparisons. It is never held while running a command or waiting for work.
// The stable inode is deliberately not removed, even by GC or RemoveInbox.
func (s *Spool) lockTransport(ctx context.Context) (*filelock.Lock, error) {
	return filelock.Acquire(ctx, filepath.Join(s.root, "transport.lock"))
}

// published checks both possible locations under the transport lock. That
// excludes rename/Ack races while comparing content, so a conflicting envelope
// cannot disappear halfway through comparison and be mistaken for a retry.
func (s *Spool) published(e *envelope.Envelope) (bool, error) {
	filename := envelope.Filename(e)
	gate := filepath.Join(s.inFlightDir(e.To), filename)
	found := false
	for _, path := range []string{filepath.Join(s.InboxDir(e.To), filename), filepath.Join(gate, "message.json")} {
		data, err := fsutil.ReadFile(path, MaxMessageBytes)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		existing, err := envelope.Unmarshal(data)
		if err != nil {
			return false, fmt.Errorf("%w: %s", ErrEnvelopeConflict, e.ID)
		}
		want, got := *e, *existing
		// Timestamps with different zone representations denote the same
		// instant; nil/empty artifact lists are also identical JSON content.
		want.TS, got.TS = want.TS.UTC(), got.TS.UTC()
		wantData, _ := envelope.Marshal(&want)
		gotData, _ := envelope.Marshal(&got)
		if !bytes.Equal(wantData, gotData) {
			return false, fmt.Errorf("%w: %s", ErrEnvelopeConflict, e.ID)
		}
		found = true
	}
	if found {
		return true, nil
	}
	// A crash may leave an empty gate before claim rename or after Ack. No
	// transition can still own that gate while this process holds the lock.
	if err := os.Remove(gate); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return false, nil
}
