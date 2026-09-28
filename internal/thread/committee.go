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

var _ review.ThreadSink = (*Dispatcher)(nil)

// PostResult appends a member's review to the thread once, while the thread
// is open; otherwise it is skipped (and counts as posted).
func (d *Dispatcher) PostResult(ctx context.Context, in review.Input, m review.Member, text string) error {
	tid, mid, ok := review.ThreadOrigin(in.Origin)
	if !ok {
		return nil
	}
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return nil // the thread is gone: nothing to post to
	}
	ref := fmt.Sprintf("%s/%d", in.ReviewID, m.Index)
	return t.Update(ctx, func(tx *Tx) error {
		if tx.Meta.Status != StatusOpen {
			return nil
		}
		for _, msg := range tx.Snap.Messages {
			if msg.Role == RoleReview && msg.Review == ref {
				return nil
			}
		}
		cm, ok := tx.Snap.Message(mid)
		if !ok {
			return nil
		}
		ev := tx.Append(Event{Kind: KindMessage, Author: in.Committee.Name + "/" + m.Member, Role: RoleReview, Text: text, ReplyTo: mid, Chain: cm.Chain, Review: ref})
		tx.Append(Event{Kind: KindState, Message: ev.ID, State: StateDone, Text: text})
		return nil
	})
}

// Complete moves the committee message to its final state once the review
// closed or was cancelled, and — when an agent asked — plans that agent's
// synthesis turn in the same transaction. It reports done once the message
// is final (including when Stop or archive finalized it).
func (d *Dispatcher) Complete(ctx context.Context, in review.Input, st review.State) (bool, error) {
	tid, mid, ok := review.ThreadOrigin(in.Origin)
	if !ok {
		return true, nil
	}
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return true, nil
	}
	done := false
	err = t.Update(ctx, func(tx *Tx) error {
		cm, ok := tx.Snap.Message(mid)
		if !ok || (cm.State != StatePending && cm.State != StateRunning) {
			done = true
			return nil
		}
		if tx.Meta.Status != StatusOpen {
			return nil // finishStop finalizes it
		}
		state, text := committeeOutcome(in, st)
		tx.Append(Event{Kind: KindState, Message: mid, State: state, Text: text})
		done = true
		if state != StateDone {
			return nil
		}
		trigger, ok := tx.Snap.Message(cm.ReplyTo)
		if !ok || trigger.Role != RoleAgent || trigger.Listener == "" {
			return nil
		}
		final, _ := tx.Snap.Message(mid)
		_, owned := tx.Meta.Listeners[trigger.Listener]
		_, err := d.planTurn(t, tx, Target{Mention: trigger.Listener, Listener: trigger.Listener, Preset: trigger.Preset, Existing: !owned}, final, cm.Chain, true)
		return err
	})
	return done, err
}

// committeeOutcome summarizes a finished review for its committee message.
func committeeOutcome(in review.Input, st review.State) (string, string) {
	var b strings.Builder
	finished := 0
	fmt.Fprintf(&b, "Committee %s (v%d) ", in.Committee.Name, in.Committee.Version)
	if st.Status == "cancelled" {
		b.WriteString("review was cancelled.\n")
	} else {
		b.WriteString("review closed.\n")
	}
	for _, m := range st.Members {
		fmt.Fprintf(&b, "- %s: %s", m.Member, m.State)
		if m.Late {
			b.WriteString(" (late)")
		}
		if m.Note != "" {
			b.WriteString(" — " + m.Note)
		}
		b.WriteString("\n")
		if m.State == "done" {
			finished++
		}
	}
	fmt.Fprintf(&b, "\nBundle: .tincan/reviews/%s/bundle.md\n", in.ReviewID)
	switch {
	case st.Status == "cancelled":
		return StateCancelled, b.String()
	case finished == 0:
		return StateError, b.String()
	}
	return StateDone, b.String()
}
