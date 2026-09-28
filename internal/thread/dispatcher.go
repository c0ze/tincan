// internal/thread/dispatcher.go
package thread

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
)

var ErrNotOwner = errors.New("another tincan web process dispatches this room")

type Dispatcher struct {
	Opts dispatch.Options
	From string // request sender name, default "web"
	// Hook, when set, is called at named points and aborts the pass when it
	// returns an error; tests use it to simulate crashes.
	Hook func(point string) error

	mu   sync.Mutex
	lock *filelock.Lock
}

func New(o dispatch.Options) *Dispatcher { return &Dispatcher{Opts: o, From: "web"} }

func (d *Dispatcher) Acquire() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lock != nil {
		return nil
	}
	if err := fsutil.MkdirPrivate(filepath.Join(d.Opts.Room, ".tincan")); err != nil {
		return err
	}
	l, err := filelock.Try(filepath.Join(d.Opts.Room, ".tincan", "dispatcher.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return ErrNotOwner
	}
	if err != nil {
		return err
	}
	d.lock = l
	return nil
}

func (d *Dispatcher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lock != nil {
		d.lock.Close()
		d.lock = nil
	}
}

func (d *Dispatcher) hook(point string) error {
	if d.Hook != nil {
		return d.Hook(point)
	}
	return nil
}

func (d *Dispatcher) Resolver() (Resolver, error) {
	presets, err := d.Opts.PresetMap()
	if err != nil {
		return Resolver{}, err
	}
	return Resolver{Room: d.Opts.Room, Presets: presets, Alive: func(name string) (host.State, bool) {
		return host.Existing(context.Background(), d.Opts.Room, name)
	}}, nil
}

func (d *Dispatcher) system(tx *Tx, text, chain string) {
	tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, Text: text, Chain: chain})
}

// chainHasSuggestion reports whether chain already has a suggested handoff,
// so the "budget reached" notice is posted only once per chain.
func chainHasSuggestion(snap *Snapshot, chain string) bool {
	for _, m := range snap.Messages {
		if m.Chain == chain && m.State == StateSuggested {
			return true
		}
	}
	return false
}

func unresolvedNote(names []string) string {
	at := make([]string, len(names))
	for i, n := range names {
		at[i] = "@" + n
	}
	return "Not dispatched: " + strings.Join(at, ", ") + " (unknown in this room or not available on this machine)."
}

// Post appends an owner message and plans its agent turns. Any process may
// call it; only the lock holder submits the planned turns.
func (d *Dispatcher) Post(ctx context.Context, tid, text, clientID string) (Message, error) {
	if strings.TrimSpace(text) == "" {
		return Message{}, errors.New("message is empty")
	}
	if len(text) > MaxPostBytes {
		return Message{}, fmt.Errorf("message exceeds %d bytes", MaxPostBytes)
	}
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return Message{}, err
	}
	res, err := d.Resolver()
	if err != nil {
		return Message{}, err
	}
	var out Message
	err = t.Update(ctx, func(tx *Tx) error {
		if clientID != "" {
			if m, ok := tx.Snap.ByClientID(clientID); ok {
				out = m
				return nil
			}
		}
		if tx.Meta.Status != StatusOpen {
			return fmt.Errorf("thread is %s", tx.Meta.Status)
		}
		chain := "c" + randHex(6)
		ev := tx.Append(Event{Kind: KindMessage, Author: "you", Role: RoleUser, Text: text, Chain: chain, ClientID: clientID})
		out, _ = tx.Snap.Message(ev.ID)
		names := ParseMentions(text)
		if len(names) == 0 {
			names = []string{tx.Meta.Primary}
		}
		targets, unresolved := res.Resolve(tx.Meta, names)
		if len(unresolved) > 0 {
			d.system(tx, unresolvedNote(unresolved), chain)
		}
		for _, tg := range targets {
			if _, err := d.planTurn(t, tx, tg, out, chain, false); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func participants(tx *Tx, extra string) []string {
	set := map[string]bool{extra: true}
	for l := range tx.Meta.Listeners {
		set[l] = true
	}
	for _, m := range tx.Snap.Messages {
		if m.Role == RoleAgent && m.Listener != "" {
			set[m.Listener] = true
		}
	}
	var out []string
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// planTurn records one agent turn durably: message, prompt file, intent and
// pending state. Automatic turns reserve chain budget or become suggestions.
// It returns the new message ID ("" when nothing was planned).
func (d *Dispatcher) planTurn(t *Thread, tx *Tx, tg Target, trigger Message, chain string, auto bool) (string, error) {
	if auto {
		c := tx.Snap.Chains[chain]
		if c != nil && c.Stopped {
			return "", nil
		}
		used := 0
		if c != nil {
			used = c.Used
		}
		if used >= tx.Meta.Budget {
			if !chainHasSuggestion(tx.Snap, chain) {
				d.system(tx, fmt.Sprintf("Chain budget of %d reached; further handoffs are suggestions.", tx.Meta.Budget), chain)
			}
			ev := tx.Append(Event{Kind: KindMessage, Author: "tincan", Role: RoleSystem, ReplyTo: trigger.ID, Chain: chain,
				Text: fmt.Sprintf("@%s please pick up the handoff from %s above.", tg.Mention, trigger.Author)})
			tx.Append(Event{Kind: KindState, Message: ev.ID, State: StateSuggested})
			return ev.ID, nil
		}
		tx.Append(Event{Kind: KindChain, Chain: chain, Op: OpReserve})
	}
	msg := tx.Append(Event{Kind: KindMessage, Author: tg.Listener, Role: RoleAgent, ReplyTo: trigger.ID, Chain: chain})
	rid := tx.Meta.ID + "-" + msg.ID
	if !tg.Existing {
		if tx.Meta.Listeners == nil {
			tx.Meta.Listeners = map[string]string{}
		}
		if tx.Meta.Listeners[tg.Listener] == "" {
			tx.Meta.Listeners[tg.Listener] = tg.Preset
			tx.SaveMeta()
		}
	}
	prompt := BuildPrompt(PromptInput{
		Listener: tg.Listener, Title: tx.Meta.Title, Room: t.Room,
		Participants: participants(tx, tg.Listener),
		Transcript:   transcriptOf(tx.Snap, trigger.ID),
		Trigger:      trigger,
	})
	if err := t.WritePrompt(rid, prompt); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(prompt))
	tx.Append(Event{Kind: KindIntent, Message: msg.ID, Listener: tg.Listener, Preset: tg.Preset, RequestID: rid, PromptSHA: hex.EncodeToString(sum[:])})
	tx.Append(Event{Kind: KindState, Message: msg.ID, State: StatePending})
	return msg.ID, nil
}

// Reconcile advances every open thread one step: finishes Stop/archive,
// submits planned turns, collects results and processes handoffs.
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	d.mu.Lock()
	owned := d.lock != nil
	d.mu.Unlock()
	if !owned {
		return ErrNotOwner
	}
	metas, err := List(d.Opts.Room)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range metas {
		if m.Status == StatusArchived {
			continue
		}
		if err := d.reconcileThread(ctx, m.ID); err != nil {
			errs = append(errs, fmt.Errorf("thread %s: %w", m.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (d *Dispatcher) reconcileThread(ctx context.Context, tid string) error {
	t, err := Open(d.Opts.Room, tid)
	if err != nil {
		return err
	}
	snap, err := t.Snapshot()
	if err != nil {
		return err
	}
	if snap.Meta.Status == StatusArchived {
		return nil
	}
	if snap.Meta.Status == StatusStopping || snap.Meta.Status == StatusArchiving {
		return d.finishStop(ctx, t, snap)
	}
	for _, m := range snap.Messages {
		if m.Role != RoleAgent {
			continue
		}
		var err error
		switch {
		case m.State == StatePending && m.RequestID != "":
			err = d.submit(ctx, t, m)
		case m.State == StateRunning:
			err = d.collect(ctx, t, m)
		case m.State == StateDone && !m.Handoffs:
			err = d.handoffs(ctx, t, m)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Dispatcher) submit(ctx context.Context, t *Thread, m Message) error {
	ok := false
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		c := tx.Snap.Chains[cur.Chain]
		ok = tx.Meta.Status == StatusOpen && cur.State == StatePending && (c == nil || !c.Stopped)
		return nil
	}); err != nil || !ok {
		return err
	}
	if err := d.hook("before-submit"); err != nil {
		return err
	}
	prompt, err := t.ReadPrompt(m.RequestID)
	var sendErr error
	if err != nil {
		sendErr = fmt.Errorf("prompt: %w", err)
	} else {
		_, sendErr = dispatch.Send(ctx, d.Opts, dispatch.SendSpec{Agent: m.Listener, Preset: m.Preset, From: d.From, Body: prompt, RequestID: m.RequestID})
	}
	if sendErr != nil {
		// Send may have saved and enqueued the request before its launch
		// failed; the turn is about to become an error, so the saved record
		// must not run on the listener's next launch.
		d.abandon(ctx, m.Listener, m.RequestID)
	}
	if err := d.hook("after-submit"); err != nil {
		return err
	}
	return t.Update(ctx, func(tx *Tx) error {
		if cur, _ := tx.Snap.Message(m.ID); cur.State != StatePending {
			return nil
		}
		if sendErr != nil {
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StateError, Text: "ERROR submit: " + sendErr.Error()})
			return nil
		}
		tx.Append(Event{Kind: KindState, Message: m.ID, State: StateRunning})
		return nil
	})
}

// abandon cancels the saved, non-terminal request of a turn that is being
// given up, so a later launch of its listener cannot run the stale prompt:
// a queued record becomes canceled atomically, and a running one gets
// CancelRequested plus a best-effort host.Cancel. Missing records and errors
// are ignored (best effort).
func (d *Dispatcher) abandon(ctx context.Context, listener, rid string) {
	if rid == "" {
		return
	}
	if r, err := request.Get(d.Opts.Room, rid); err != nil || r.Terminal() {
		return
	}
	r, err := request.Cancel(ctx, d.Opts.Room, rid)
	if err == nil && !r.Terminal() {
		host.Cancel(ctx, d.Opts.Room, listener, rid)
	}
}

// outcome maps a terminal request record to a message state and text.
func outcome(r request.Record) (string, string) {
	switch r.Status {
	case "completed":
		if strings.HasPrefix(r.Result, "ERROR") {
			return StateError, r.Result
		}
		return StateDone, r.Result
	case "canceled":
		return StateCancelled, r.Result
	default:
		return StateError, r.Result
	}
}

func (d *Dispatcher) collect(ctx context.Context, t *Thread, m Message) error {
	r, err := request.Poll(ctx, d.Opts.Room, m.RequestID)
	if errors.Is(err, os.ErrNotExist) {
		return t.Update(ctx, func(tx *Tx) error {
			if cur, _ := tx.Snap.Message(m.ID); cur.State != StateRunning {
				return nil
			}
			tx.Append(Event{Kind: KindState, Message: m.ID, State: StateUncollectable, Text: "result expired: the request record no longer exists"})
			return nil
		})
	}
	if err != nil {
		return err
	}
	if !r.Terminal() {
		return nil
	}
	state, text := outcome(r)
	if err := t.Update(ctx, func(tx *Tx) error {
		if cur, _ := tx.Snap.Message(m.ID); cur.State != StateRunning {
			return nil
		}
		tx.Append(Event{Kind: KindState, Message: m.ID, State: state, Text: text})
		return nil
	}); err != nil {
		return err
	}
	return d.hook("after-terminal")
}

func (d *Dispatcher) handoffs(ctx context.Context, t *Thread, m Message) error {
	names := ParseMentions(m.Text)
	res, err := d.Resolver()
	if err != nil {
		return err
	}
	if err := t.Update(ctx, func(tx *Tx) error {
		cur, _ := tx.Snap.Message(m.ID)
		if cur.Handoffs {
			return nil
		}
		var produced []string
		c := tx.Snap.Chains[cur.Chain]
		if tx.Meta.Status == StatusOpen && (c == nil || !c.Stopped) && len(names) > 0 {
			targets, unresolved := res.Resolve(tx.Meta, names)
			if len(unresolved) > 0 {
				d.system(tx, unresolvedNote(unresolved), cur.Chain)
			}
			for _, tg := range targets {
				if tg.Listener == cur.Listener {
					continue
				}
				id, err := d.planTurn(t, tx, tg, cur, cur.Chain, true)
				if err != nil {
					return err
				}
				if id != "" {
					produced = append(produced, id)
				}
			}
		}
		tx.Append(Event{Kind: KindHandoffs, Message: cur.ID, Produced: produced})
		return nil
	}); err != nil {
		return err
	}
	return d.hook("after-handoffs")
}
