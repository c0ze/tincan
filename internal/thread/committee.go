package thread

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

// planCommittee records one committee review in the post or handoffs
// transaction (committees §6.10): the committee message, its intent with
// the frozen snapshot and derived review ID, keyed reservations for an
// agent handoff (or one suggestion when the chain cannot afford them), and
// pending.
func (d *Dispatcher) planCommittee(t *Thread, tx *Tx, tg Target, trigger Message, chain string, auto bool) (string, error) {
	c := *tg.Committee
	if auto {
		ch := tx.Snap.Chains[chain]
		if ch != nil && ch.Stopped {
			return "", nil
		}
		used := 0
		if ch != nil {
			used = ch.Used
		}
		if used+len(c.Members) > tx.Meta.Budget {
			if !chainHasSuggestion(tx.Snap, chain) {
				d.system(tx, fmt.Sprintf("Chain budget of %d cannot cover committee %s (%d members); sending it is up to you.", tx.Meta.Budget, c.Name, len(c.Members)), chain)
			}
			ev := tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, ReplyTo: trigger.ID, Chain: chain,
				Text: fmt.Sprintf("@%s please review the handoff from %s above.", c.Name, trigger.Author)})
			tx.Append(Event{Kind: KindState, Message: ev.ID, State: StateSuggested})
			return ev.ID, nil
		}
	}
	room, err := rooms.Canonical(d.Opts.Room)
	if err != nil {
		return "", err
	}
	msg := tx.Append(Event{Kind: KindMessage, Author: c.Name, Role: RoleCommittee, ReplyTo: trigger.ID, Chain: chain})
	rid := tx.Meta.ID + "-" + msg.ID
	reviewID := review.NewID(d.Machine, room, rid)
	tx.Append(Event{Kind: KindIntent, Message: msg.ID, RequestID: rid, Review: reviewID, Committee: &c})
	if auto {
		for i := range c.Members {
			tx.Append(Event{Kind: KindChain, Chain: chain, Op: OpReserve, Key: fmt.Sprintf("review:%s:%d", reviewID, i)})
		}
	}
	tx.Append(Event{Kind: KindState, Message: msg.ID, State: StatePending})
	return msg.ID, nil
}

const maxReviewQuestion = 64 << 10

// reviewQuestion is the mentioning message plus the transcript before it,
// oldest lines dropped first, within 64 KiB (committees §6.10).
func reviewQuestion(s *Snapshot, trigger Message) string {
	ask := "Request from " + trigger.Author + ":\n" + trigger.Text + "\n"
	if len(ask) > maxReviewQuestion {
		return cutUTF8(ask, maxReviewQuestion)
	}
	var lines []string
	for _, m := range transcriptOf(s, trigger.ID) {
		if m.N < trigger.N {
			lines = append(lines, line(m))
		}
	}
	budget := maxReviewQuestion - len(ask) - 64
	start, size := len(lines), 0
	for start > 0 && size+len(lines[start-1]) <= budget {
		start--
		size += len(lines[start])
	}
	if start == len(lines) {
		return ask
	}
	return "Thread so far:\n" + strings.Join(lines[start:], "") + "\n" + ask
}

// buildReview publishes a pending committee message's review (phase 1's
// submit pattern): check under the thread lock, publish outside it, then
// mark it running — or, if a Stop arrived meanwhile, cancel what was just
// published. An existing review for this message is adopted.
func (d *Dispatcher) buildReview(ctx context.Context, t *Thread, m Message) error {
	var snap *Snapshot
	ok := false
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		c := tx.Snap.Chains[cur.Chain]
		ok = tx.Meta.Status == StatusOpen && cur.State == StatePending && (c == nil || !c.Stopped)
		snap = tx.Snap
		return nil
	}); err != nil || !ok {
		return err
	}
	room, err := rooms.Canonical(d.Opts.Room)
	if err != nil {
		return err
	}
	origin := "thread:" + t.ID + ":" + m.ID
	var buildErr error
	if in, err := review.ReadInput(room, m.Review); err == nil {
		if in.Origin != origin {
			buildErr = errors.New("review " + m.Review + " belongs to " + in.Origin)
		}
	} else {
		trigger, _ := snap.Message(m.ReplyTo)
		from := d.CommitteesFrom
		in, _, perr := review.Publish(ctx, review.PublishRequest{StateDir: d.StateDir, Room: room, Committee: m.Committee.Name,
			Snapshot: m.Committee, Question: reviewQuestion(snap, trigger), RequestID: m.RequestID, Origin: origin,
			Machine: d.Machine, CommitteesFrom: &from, InCoordinator: true, Registry: d.Registry})
		switch {
		case perr != nil:
			buildErr = perr
		case in.ReviewID != m.Review:
			buildErr = errors.New("review id mismatch: machine or room changed since the mention")
		}
	}
	cancelIt := false
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		if cur.State != StatePending {
			cancelIt = buildErr == nil
			return nil
		}
		if buildErr != nil {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StateError, Text: "ERROR review: " + buildErr.Error()})
			return nil
		}
		tx.Append(Event{Kind: KindState, Message: m.ID, State: StateRunning})
		return nil
	}); err != nil {
		return err
	}
	if cancelIt {
		_, err := review.RequestCancel(ctx, room, m.Review)
		return err
	}
	return nil
}
