package thread

import (
	"fmt"

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
