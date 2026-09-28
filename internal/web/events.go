// internal/web/events.go
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
	"github.com/fsnotify/fsnotify"
)

type note struct {
	Kind   string `json:"kind"` // thread | messages | activity | peer | quota | committees
	Room   string `json:"room,omitempty"`
	Thread string `json:"thread,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
}

type hub struct {
	mu   sync.Mutex
	subs map[chan note]struct{}
	fp   map[string]string // fingerprints from the last scan
}

func newHub() *hub { return &hub{subs: map[chan note]struct{}{}, fp: map[string]string{}} }

func (h *hub) subscribe() (chan note, func()) {
	ch := make(chan note, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// publish never blocks: a slow client misses notes and refetches on reconnect.
func (h *hub) publish(n note) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- n:
		default:
		}
	}
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case n := <-ch:
			data, _ := json.Marshal(n)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func statFP(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// scan compares per-room fingerprints with the last scan and publishes notes.
func (s *Server) scan(list []rooms.Room) {
	next := map[string]string{}
	var notes []note
	for _, room := range list {
		if room.Missing {
			continue
		}
		act := statFP(filepath.Join(room.Path, ".tincan", "requests")) + statFP(filepath.Join(room.Path, ".tincan", "hosts")) + statFP(filepath.Join(room.Path, ".tincan", "present"))
		next["a:"+room.ID] = act
		next["r:"+room.ID] = statFP(thread.Root(room.Path))
		metas, _ := thread.List(room.Path)
		for _, m := range metas {
			dir := filepath.Join(thread.Root(room.Path), m.ID)
			next["t:"+room.ID+":"+m.ID] = statFP(filepath.Join(dir, "events.jsonl")) + statFP(filepath.Join(dir, "thread.json"))
		}
	}
	if files, err := quota.Files(s.cfg.QuotaDir); err == nil {
		fp := ""
		for _, id := range sortedKeys(files) {
			fp += id + "=" + statFP(files[id]) + ";"
		}
		next["q:"] = fp
	}
	if s.cfg.StateDir != "" {
		// Only the hub's store: a peer publishes its own note when a refresh
		// changes its cache (refreshCommittees).
		next["c:"] = statFP(committee.NewStore(s.cfg.StateDir).Path())
	}
	s.hub.mu.Lock()
	prev := s.hub.fp
	s.hub.fp = next
	s.hub.mu.Unlock()
	if len(prev) == 0 {
		return // first scan is the baseline
	}
	for key, fp := range next {
		if prev[key] == fp {
			continue
		}
		switch key[0] {
		case 'a':
			notes = append(notes, note{Kind: "activity", Room: key[2:]})
		case 'r':
			notes = append(notes, note{Kind: "thread", Room: key[2:]})
		case 't':
			rid, tid := key[2:14], key[15:]
			notes = append(notes, note{Kind: "messages", Room: rid, Thread: tid})
		case 'q':
			notes = append(notes, note{Kind: "quota"})
		case 'c':
			notes = append(notes, note{Kind: "committees"})
		}
	}
	for _, n := range notes {
		s.hub.publish(n)
	}
}

// sortedPeerNames returns the configured peer names in sorted order.
func sortedPeerNames(m map[string]*peer) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// sortedKeys returns m's keys in sorted order, for a stable fingerprint.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) dispatcher(room rooms.Room) *thread.Dispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.dispatchers[room.ID]; ok {
		return d
	}
	d := thread.New(s.opts(room))
	if err := d.Acquire(); err != nil {
		if !errors.Is(err, thread.ErrNotOwner) {
			s.logOnceLocked(room.ID, fmt.Sprintf("tincan web: %s: acquire: %v", room.Name, err))
		}
		return nil // another process owns this room, or acquiring failed; retry next tick
	}
	s.dispatchers[room.ID] = d
	return d
}

// dropDispatcher closes and forgets room's cached dispatcher, if any, so
// that a room later found missing and then recreated at the same path (and
// so the same room ID) is re-acquired from scratch rather than reusing a
// dispatcher whose lock file may no longer exist.
func (s *Server) dropDispatcher(id string) {
	s.mu.Lock()
	d, ok := s.dispatchers[id]
	if ok {
		delete(s.dispatchers, id)
	}
	s.mu.Unlock()
	if ok {
		d.Close()
	}
}

// logOnce writes msg to stderr unless it is identical to the last message
// logged under key, so a persistent error condition (a stuck registry file,
// a room whose dispatcher lock can't be acquired for a reason other than
// another process owning it) logs once instead of once per tick.
func (s *Server) logOnce(key, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logOnceLocked(key, msg)
}

// logOnceLocked is logOnce for a caller that already holds s.mu.
func (s *Server) logOnceLocked(key, msg string) {
	if s.lastLog[key] == msg {
		return
	}
	s.lastLog[key] = msg
	fmt.Fprintln(os.Stderr, msg)
}

// runRoomPass is the default reconcileRoom: it acquires (or reuses) the
// room's dispatcher and runs one Reconcile, plus a Janitor pass when due.
func (s *Server) runRoomPass(ctx context.Context, room rooms.Room, janitor bool) {
	d := s.dispatcher(room)
	if d == nil {
		return
	}
	// A persistent failure repeats every tick; log each distinct error once.
	if err := d.Reconcile(ctx); err != nil {
		s.logOnce(room.ID+"/reconcile", fmt.Sprintf("tincan web: %s: reconcile: %v", room.Name, err))
	}
	if janitor {
		if err := d.Janitor(ctx, s.cfg.IdleStop); err != nil {
			s.logOnce(room.ID+"/janitor", fmt.Sprintf("tincan web: %s: janitor: %v", room.Name, err))
		}
	}
}

// tick runs one dispatch-and-notify iteration over all registered rooms.
// Each room's reconcile-and-janitor pass runs in its own goroutine so that
// a slow room (host.Up/host.Down each wait up to 10s) cannot delay
// reconcile or SSE notes for every other room; a room whose previous pass
// is still running is skipped for this tick rather than re-entered.
// scan, which only stats files, always runs synchronously so notes keep
// flowing even while a room's pass is in flight.
func (s *Server) tick(ctx context.Context) {
	if s.cfg.StateDir != "" && time.Since(s.lastHeartbeat) >= 10*time.Second {
		now := time.Now()
		if err := rooms.WriteHeartbeat(s.cfg.StateDir, rooms.Heartbeat{PID: os.Getpid(), Machine: s.cfg.Machine, Started: s.started, Updated: now}); err != nil {
			s.logOnce("heartbeat", fmt.Sprintf("tincan web: heartbeat: %v", err))
		} else {
			s.lastHeartbeat = now
		}
	}
	list, err := s.cfg.Registry.List()
	if err != nil {
		s.logOnce("registry.list", fmt.Sprintf("tincan web: registry list: %v", err))
		return
	}
	janitor := time.Since(s.lastJanitor) > time.Minute
	if janitor {
		s.lastJanitor = time.Now()
	}
	for _, room := range list {
		if room.Missing {
			s.dropDispatcher(room.ID)
			continue
		}
		if _, err := os.Stat(filepath.Join(room.Path, ".tincan")); err != nil {
			continue // never used by tincan: nothing to coordinate
		}
		s.mu.Lock()
		if s.inFlight[room.ID] {
			s.mu.Unlock()
			continue
		}
		s.inFlight[room.ID] = true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.inFlight, room.ID)
				s.mu.Unlock()
			}()
			s.reconcileRoom(ctx, room, janitor)
		}()
	}
	s.scan(list)
	if time.Since(s.lastQuotaNote) >= time.Minute {
		s.lastQuotaNote = time.Now()
		s.hub.publish(note{Kind: "quota"})
	}
}

// shouldTrigger reports whether an fsnotify event is significant enough to
// arm an early tick. A host's log file is appended continuously while it
// runs and carries no dispatch-relevant state change, so a plain Write on a
// ".log" file is filtered out; every other event — including a Write
// elsewhere, or a Create/Remove/Rename touching a ".log" file — triggers.
func shouldTrigger(ev fsnotify.Event) bool {
	if ev.Has(fsnotify.Write) && strings.HasSuffix(ev.Name, ".log") {
		return false
	}
	return true
}

// debouncer coalesces a burst of triggers into a single delivery on C: arm
// (re)starts a single pending timer, so a delivery only happens once window
// has passed since the most recent call to arm. A storm of rapid fsnotify
// events therefore collapses into at most one early tick, rather than one
// tick per event or two ticks racing back-to-back.
type debouncer struct {
	window time.Duration
	C      chan struct{}

	mu    sync.Mutex
	timer *time.Timer
}

func newDebouncer(window time.Duration) *debouncer {
	return &debouncer{window: window, C: make(chan struct{}, 1)}
}

func (b *debouncer) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(b.window, func() {
		select {
		case b.C <- struct{}{}:
		default:
		}
	})
}

func (b *debouncer) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
}

func (s *Server) loop(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	var events chan fsnotify.Event
	if err == nil {
		defer watcher.Close()
		events = watcher.Events
		// Errors must be drained continuously: fsnotify's internal
		// goroutine blocks sending to it, so a single unread error would
		// otherwise wedge event delivery for the rest of the process.
		go func() {
			for e := range watcher.Errors {
				fmt.Fprintf(os.Stderr, "tincan web: watch: %v\n", e)
			}
		}()
	}
	watched := map[string]bool{}
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
	debounce := newDebouncer(500 * time.Millisecond)
	defer debounce.stop()
	for {
		s.tick(ctx)
		if watcher != nil {
			list, _ := s.cfg.Registry.List()
			for _, room := range list {
				dirs := []string{thread.Root(room.Path), filepath.Join(room.Path, ".tincan", "requests"), filepath.Join(room.Path, ".tincan", "hosts"), filepath.Join(room.Path, ".tincan", "present")}
				metas, _ := thread.List(room.Path)
				for _, m := range metas {
					dirs = append(dirs, filepath.Join(thread.Root(room.Path), m.ID))
				}
				for _, d := range dirs {
					if !watched[d] && watcher.Add(d) == nil {
						watched[d] = true
					}
				}
			}
		}
	waitForTrigger:
		for {
			select {
			case <-ctx.Done():
				s.wg.Wait() // let in-flight room passes finish before closing their dispatchers
				s.mu.Lock()
				for _, d := range s.dispatchers {
					d.Close()
				}
				s.mu.Unlock()
				return
			case <-ticker.C:
				break waitForTrigger
			case <-debounce.C:
				break waitForTrigger
			case ev := <-events:
				if shouldTrigger(ev) {
					debounce.arm()
				}
			}
		}
	}
}
