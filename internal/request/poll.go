package request

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/internal/filelock"
	"github.com/c0ze/tincan/internal/spool"
)

// pollLockPath gives every collector of one private reply channel the same
// persistent lock. It is separate from the journal lock acquired by Finish.
func pollLockPath(room, channel string) string {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(channel)))
	return filepath.Join(Dir(room), "poll-locks", key+".lock")
}

// Poll ingests an interactive reply and saves its result before acknowledgment.
// Collector ownership spans claim/recovery, Finish, and Ack, so a concurrent
// waiter cannot recover a reply that another live Poll is still processing.
// Hosted requests publish terminal results directly and never use this bridge.
func Poll(ctx context.Context, room, id string) (Record, error) {
	r, err := Get(room, id)
	if err != nil || !r.Interactive || r.Envelope.ReplyTo == "" {
		return r, err
	}
	channel := r.Envelope.ReplyTo
	if err := spool.ValidName(channel); err != nil {
		return r, err
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	owner, err := filelock.Try(pollLockPath(room, channel))
	if errors.Is(err, filelock.ErrLocked) {
		// Another collector owns the reply. Return its latest journal state
		// promptly so Wait can honor its own timeout and cancellation.
		return Get(room, id)
	}
	if err != nil {
		return r, err
	}
	defer owner.Close()
	// A previous collector or hosted handoff may have changed the journal
	// while this waiter was acquiring ownership. Terminal records still need
	// recovery: a process can exit after Finish but before acknowledging.
	r, err = Get(room, id)
	if err != nil || !r.Interactive || r.Envelope.ReplyTo == "" {
		return r, err
	}
	if r.Envelope.ReplyTo != channel {
		return r, errors.New("request reply channel changed during collection")
	}
	sp, err := spool.Open(room)
	if err != nil {
		return r, err
	}
	claims, err := sp.RecoverInFlight(channel)
	if err != nil {
		return r, err
	}
	if len(claims) == 0 {
		d, err := sp.ClaimContext(ctx, channel, time.Millisecond)
		if errors.Is(err, spool.ErrTimeout) {
			return reconcileIdleInteractivePublication(ctx, room, r, sp)
		}
		if err != nil {
			return r, err
		}
		claims = []*spool.Delivery{d}
	}
	for _, d := range claims {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if d.Envelope.From != "" && d.Envelope.From != r.Agent {
			if err := d.Ack(true); err != nil {
				return r, err
			}
			return r, fmt.Errorf("reply sender %q does not match request agent %q", d.Envelope.From, r.Agent)
		}
		finished, err := Finish(room, r.Envelope, d.Envelope.Body)
		if err != nil {
			return r, err
		}
		r = finished
		if err := d.Ack(false); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Reconcile an abandoned publication intent without asking a caller to submit
// again. A live Submit holds the journal lock until its publication confirmation
// is saved; Try keeps Poll/Wait bounded while that owner is still working.
func reconcileIdleInteractivePublication(ctx context.Context, room string, r Record, sp *spool.Spool) (Record, error) {
	if r.Terminal() || r.InteractivePublished {
		return r, nil
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	// A very short Claim timeout may expire while preparing directories, even
	// though a reply is queued. Give that real response priority over declaring
	// the original publication uncertain; the next Poll can collect it.
	presence, _, err := sp.Present(r.Envelope.ReplyTo)
	if err != nil {
		return r, err
	}
	if presence.Queued > 0 {
		return r, nil
	}
	owner, err := filelock.Try(filepath.Join(Dir(room), "locks", r.ID+".lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return Get(room, r.ID)
	}
	if err != nil {
		return r, err
	}
	defer owner.Close()
	current, err := Get(room, r.ID)
	if err != nil {
		return r, err
	}
	if current.Status != "queued" || !current.Interactive || current.InteractivePublished {
		return current, nil
	}
	return resolveInteractivePublication(room, current)
}
