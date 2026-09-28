// internal/thread/control.go
package thread

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/spool"
)

// barrier stops every chain and moves the thread to status. From this point
// no turn is submitted and no handoff is planned (both check under the lock).
func barrier(tx *Tx, status, note string) {
	for id, c := range tx.Snap.Chains {
		if !c.Stopped {
			tx.Append(Event{Kind: KindChain, Chain: id, Op: OpStop})
		}
	}
	for _, m := range tx.Snap.Messages {
		if c := tx.Snap.Chains[m.Chain]; m.Chain != "" && c == nil {
			tx.Append(Event{Kind: KindChain, Chain: m.Chain, Op: OpStop})
		}
	}
	tx.Meta.Status = status
	tx.SaveMeta()
	tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, Text: note})
}

func (d *Dispatcher) RequestStop(ctx context.Context, tid string) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		if tx.Meta.Status != StatusOpen {
			return nil
		}
		barrier(tx, StatusStopping, "Stopped by you.")
		return nil
	})
}

func (d *Dispatcher) SetArchived(ctx context.Context, tid string, archived bool) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		switch {
		case archived && (tx.Meta.Status == StatusOpen || tx.Meta.Status == StatusStopping):
			barrier(tx, StatusArchiving, "Archived.")
		case !archived && tx.Meta.Status == StatusArchived:
			tx.Meta.Status = StatusOpen
			tx.SaveMeta()
			tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, Text: "Unarchived; agents start fresh conversations."})
		case !archived:
			return fmt.Errorf("thread is %s", tx.Meta.Status)
		}
		return nil
	})
}

// finishStop runs on the dispatcher: cancel unsubmitted turns, cancel and
// await submitted ones, then reopen or finish archiving.
func (d *Dispatcher) finishStop(ctx context.Context, t *Thread, snap Snapshot) error {
	room := d.Opts.Room
	// Committee reviews started from this thread are cancelled first,
	// outside any thread transaction (review → thread is the only lock
	// nesting); cancelling does not wait for peers (committees §6.10).
	if canonical, err := rooms.Canonical(room); err == nil {
		for _, m := range snap.Messages {
			if m.Role != RoleCommittee || m.Review == "" {
				continue
			}
			if st, err := review.ReadState(canonical, m.Review); err == nil && !st.Settled && !st.CancelRequested {
				if _, err := review.RequestCancel(ctx, canonical, m.Review); err != nil {
					return err
				}
			}
		}
	}
	type result struct{ id, state, text string }
	var settled []result
	pending := false
	for _, m := range snap.Messages {
		// A failed or cancelled turn may still own a saved, runnable request
		// (e.g. its launch failed); cancel it so it never runs later. This is
		// best effort and does not hold the thread in stopping/archiving.
		if m.Role == RoleAgent && (m.State == StateError || m.State == StateCancelled) {
			d.abandon(ctx, m.Listener, m.RequestID)
		}
		if !m.Active() {
			continue
		}
		r, err := request.Cancel(ctx, room, m.RequestID)
		if errors.Is(err, os.ErrNotExist) {
			settled = append(settled, result{m.ID, StateCancelled, "no request record; treated as cancelled"})
			continue
		}
		if err != nil {
			return err
		}
		if !r.Terminal() {
			if _, alive := host.Existing(ctx, room, m.Listener); !alive {
				// The hosted listener is gone (crashed or its process was
				// killed): host.Cancel can never reach it and the request
				// would never become terminal on its own, stranding the
				// thread in stopping/archiving forever. Settle it directly,
				// after a best-effort host.Cancel in case the probe is wrong.
				host.Cancel(ctx, room, m.Listener, m.RequestID)
				r, err = request.Finish(room, r.Envelope, "ERROR interrupted: hosted listener is not running; stopped by the owner")
				if err != nil {
					return err
				}
			} else {
				host.Cancel(ctx, room, m.Listener, m.RequestID) // best effort; polling decides
				pending = true
				continue
			}
		}
		state, text := outcome(r)
		settled = append(settled, result{m.ID, state, text})
	}
	if err := t.Update(ctx, func(tx *Tx) error {
		for _, s := range settled {
			if cur, _ := tx.Snap.Message(s.id); cur.Active() {
				tx.Append(Event{Kind: KindState, Message: s.id, State: s.state, Text: s.text})
			}
		}
		for _, m := range tx.Snap.Messages {
			if m.Role == RoleCommittee && (m.State == StatePending || m.State == StateRunning) {
				tx.Append(Event{Kind: KindState, Message: m.ID, State: StateCancelled, Text: "Cancelled: the thread was stopped."})
			}
		}
		// Completed turns never hand off after a Stop.
		for _, m := range tx.Snap.Messages {
			if m.Role == RoleAgent && m.State == StateDone && !m.Handoffs {
				tx.Append(Event{Kind: KindHandoffs, Message: m.ID})
			}
		}
		return nil
	}); err != nil || pending {
		return err
	}
	if snap.Meta.Status == StatusArchiving {
		for name := range snap.Meta.Listeners {
			if err := host.Down(ctx, room, name, 10*time.Second); err != nil {
				return err
			}
			if err := host.ClearSession(ctx, room, name); err != nil {
				return err
			}
		}
	}
	return t.Update(ctx, func(tx *Tx) error {
		switch tx.Meta.Status {
		case StatusStopping:
			tx.Meta.Status = StatusOpen
		case StatusArchiving:
			tx.Meta.Status = StatusArchived
		default:
			return nil
		}
		tx.SaveMeta()
		return nil
	})
}

// Retry re-runs a failed turn: the same request, in place, when it was never
// saved or never ran and its chain can still submit; otherwise (including
// when the original chain was stopped by a Stop or Archive barrier, which
// bars it from ever submitting again) a fresh turn in a brand-new chain, as
// Post itself uses, for the same listener and trigger. A record that already
// carries a cancel request never counts as re-runnable in place. A turn can
// be retried fresh only once; the KindRetry marker on the original message
// rejects a second call. An in-place retry sets no marker (its pending and
// running states already refuse a concurrent Retry), so if it fails again it
// can be retried again.
func (d *Dispatcher) Retry(ctx context.Context, tid, mid string) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		if tx.Meta.Status != StatusOpen {
			return fmt.Errorf("thread is %s", tx.Meta.Status)
		}
		m, ok := tx.Snap.Message(mid)
		if !ok || m.Role != RoleAgent {
			return errors.New("only failed or cancelled agent turns can be retried")
		}
		if m.Retried {
			return errors.New("already retried")
		}
		if m.State != StateError && m.State != StateCancelled {
			return errors.New("only failed or cancelled agent turns can be retried")
		}
		r, err := request.Get(d.Opts.Room, m.RequestID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		stale := errors.Is(err, os.ErrNotExist) || (err == nil && !r.Terminal() && !r.CancelRequested)
		stopped := false
		if c := tx.Snap.Chains[m.Chain]; c != nil {
			stopped = c.Stopped
		}
		if stale && !stopped {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StatePending})
			return nil
		}
		if stale {
			// The old request is stranded in a chain that will never submit
			// again; cancel it defensively (it may already be gone) and
			// retry fresh instead of resetting to a pending state submit()
			// would refuse forever.
			cr, cerr := request.Cancel(ctx, d.Opts.Room, m.RequestID)
			if cerr != nil && !errors.Is(cerr, os.ErrNotExist) {
				return cerr
			}
			if cerr == nil && !cr.Terminal() {
				host.Cancel(ctx, d.Opts.Room, m.Listener, m.RequestID) // best effort
			}
		}
		trigger, ok := tx.Snap.Message(m.ReplyTo)
		if !ok {
			return errors.New("the message this turn answered is gone")
		}
		_, owned := tx.Meta.Listeners[m.Listener]
		newID, err := d.planTurn(t, tx, Target{Mention: m.Listener, Listener: m.Listener, Preset: m.Preset, Existing: !owned}, trigger, "c"+randHex(6), false)
		if err != nil {
			return err
		}
		tx.Append(Event{Kind: KindRetry, Message: m.ID, Produced: []string{newID}})
		return nil
	})
}

// Janitor stops thread-owned listeners with no active turn anywhere in the
// room whose last journal activity anywhere in the room is older than idle,
// and that are not otherwise busy or holding queued work. Sessions are kept.
// A listener name is room-wide (any thread may address another thread's
// listener by its full name), so busy/last-activity are computed across
// every thread before any stop decision is made.
func (d *Dispatcher) Janitor(ctx context.Context, idle time.Duration) error {
	metas, err := List(d.Opts.Room)
	if err != nil {
		return err
	}
	var errs []error
	cutoff := time.Now().Add(-idle)
	last := map[string]time.Time{}
	busy := map[string]bool{}
	listeners := map[string]bool{}
	for _, meta := range metas {
		t, err := Open(d.Opts.Room, meta.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		snap, err := t.Snapshot()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, m := range snap.Messages {
			if m.Listener == "" {
				continue
			}
			if m.Updated.After(last[m.Listener]) {
				last[m.Listener] = m.Updated
			}
			if m.Active() {
				busy[m.Listener] = true
			}
		}
		for name := range snap.Meta.Listeners {
			listeners[name] = true
		}
	}
	queued := map[string]int{}
	if sp, err := spool.Open(d.Opts.Room); err != nil {
		errs = append(errs, err)
	} else if pres, err := sp.ListPresence(); err != nil {
		errs = append(errs, err)
	} else {
		for _, p := range pres {
			queued[p.Name] = p.Queued
		}
	}
	for name := range listeners {
		if busy[name] || last[name].After(cutoff) || queued[name] > 0 {
			continue
		}
		if st, ok, _ := host.ReadState(d.Opts.Room, name); ok && st.State == "busy" {
			continue
		}
		if _, alive := host.Existing(ctx, d.Opts.Room, name); alive {
			if err := host.Down(ctx, d.Opts.Room, name, 10*time.Second); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func WaitStatus(ctx context.Context, t *Thread, notIn ...string) (Meta, error) {
	for {
		snap, err := t.Snapshot()
		if err != nil {
			return Meta{}, err
		}
		blocked := false
		for _, s := range notIn {
			if snap.Meta.Status == s {
				blocked = true
			}
		}
		if !blocked {
			return snap.Meta, nil
		}
		select {
		case <-ctx.Done():
			return snap.Meta, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
