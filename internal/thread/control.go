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
	type result struct{ id, state, text string }
	var settled []result
	pending := false
	for _, m := range snap.Messages {
		if !m.Active() {
			continue
		}
		r, err := request.Cancel(ctx, room, m.RequestID)
		if errors.Is(err, os.ErrNotExist) {
			settled = append(settled, result{m.ID, StateCancelled, "cancelled before it started"})
			continue
		}
		if err != nil {
			return err
		}
		if !r.Terminal() {
			host.Cancel(ctx, room, m.Listener, m.RequestID) // best effort; polling decides
			pending = true
			continue
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

// Retry re-runs a failed turn: the same request when it was never saved or
// never ran, otherwise a fresh turn for the same listener and trigger.
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
		if !ok || m.Role != RoleAgent || (m.State != StateError && m.State != StateCancelled) {
			return errors.New("only failed or cancelled agent turns can be retried")
		}
		if r, err := request.Get(d.Opts.Room, m.RequestID); errors.Is(err, os.ErrNotExist) || (err == nil && !r.Terminal()) {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StatePending})
			return nil
		}
		trigger, ok := tx.Snap.Message(m.ReplyTo)
		if !ok {
			return errors.New("the message this turn answered is gone")
		}
		_, existing := tx.Meta.Listeners[m.Listener]
		_, err := d.planTurn(t, tx, Target{Mention: m.Listener, Listener: m.Listener, Preset: m.Preset, Existing: !existing}, trigger, m.Chain, false)
		return err
	})
}

// Janitor stops thread-owned listeners with no active turn whose last
// journal activity is older than idle. Sessions are kept.
func (d *Dispatcher) Janitor(ctx context.Context, idle time.Duration) error {
	metas, err := List(d.Opts.Room)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-idle)
	for _, meta := range metas {
		t, err := Open(d.Opts.Room, meta.ID)
		if err != nil {
			continue
		}
		snap, err := t.Snapshot()
		if err != nil {
			continue
		}
		last := map[string]time.Time{}
		busy := map[string]bool{}
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
			if busy[name] || last[name].After(cutoff) {
				continue
			}
			if _, alive := host.Existing(ctx, d.Opts.Room, name); alive {
				host.Down(ctx, d.Opts.Room, name, 10*time.Second)
			}
		}
	}
	return nil
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
