// internal/web/events.go
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
	"github.com/fsnotify/fsnotify"
)

type note struct {
	Kind   string `json:"kind"` // thread | messages | activity | peer
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
		}
	}
	for _, n := range notes {
		s.hub.publish(n)
	}
}

func (s *Server) dispatcher(room rooms.Room) *thread.Dispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.dispatchers[room.ID]; ok {
		return d
	}
	d := thread.New(s.opts(room))
	if err := d.Acquire(); err != nil {
		return nil // another process owns this room; retry next tick
	}
	s.dispatchers[room.ID] = d
	return d
}

// tick runs one dispatch-and-notify iteration over all registered rooms.
func (s *Server) tick(ctx context.Context) {
	list, err := s.cfg.Registry.List()
	if err != nil {
		return
	}
	janitor := time.Since(s.lastJanitor) > time.Minute
	for _, room := range list {
		if room.Missing {
			continue
		}
		if _, err := os.Stat(filepath.Join(room.Path, ".tincan")); err != nil {
			continue // never used by tincan: nothing to coordinate
		}
		d := s.dispatcher(room)
		if d == nil {
			continue
		}
		if err := d.Reconcile(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "tincan web: %s: %v\n", room.Name, err)
		}
		if janitor {
			if err := d.Janitor(ctx, s.cfg.IdleStop); err != nil {
				fmt.Fprintf(os.Stderr, "tincan web: %s: %v\n", room.Name, err)
			}
		}
	}
	if janitor {
		s.lastJanitor = time.Now()
	}
	s.scan(list)
}

func (s *Server) loop(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	var events chan fsnotify.Event
	if err == nil {
		defer watcher.Close()
		events = watcher.Events
	}
	watched := map[string]bool{}
	ticker := time.NewTicker(s.cfg.Tick)
	defer ticker.Stop()
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
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, d := range s.dispatchers {
				d.Close()
			}
			s.mu.Unlock()
			return
		case <-ticker.C:
		case <-events:
			time.Sleep(100 * time.Millisecond) // coalesce bursts
			for len(events) > 0 {
				<-events
			}
		}
	}
}
