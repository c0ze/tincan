package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/spool"
	"github.com/c0ze/tincan/v2/internal/thread"
)

var errNotFound = errors.New("not found")

const (
	// apiBodyLimit bounds every JSON request body except postMessage's.
	apiBodyLimit = 64 << 10
	// postBodyLimit leaves headroom for JSON string-escaping: a legal
	// 128 KiB (thread.MaxPostBytes) message can encode up to 6x larger when
	// it is mostly control characters, plus a small margin for the rest of
	// the envelope (client_id, JSON punctuation).
	postBodyLimit = 6*thread.MaxPostBytes + 16<<10
	// logTailBytes is how much of an agent log agentLog returns.
	logTailBytes = 64 << 10
)

func (s *Server) apiRoutes() {
	s.mux.HandleFunc("GET /api/rooms", s.listRooms)
	s.mux.HandleFunc("POST /api/rooms", s.addRoom)
	s.mux.HandleFunc("PATCH /api/rooms/{rid}", s.patchRoom)
	s.mux.HandleFunc("GET /api/rooms/{rid}/agents", s.listAgents)
	s.mux.HandleFunc("GET /api/rooms/{rid}/threads", s.listThreads)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads", s.createThread)
	s.mux.HandleFunc("GET /api/rooms/{rid}/threads/{tid}/messages", s.listMessages)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/messages", s.postMessage)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/stop", s.stopThread)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/archive", s.archiveThread)
	s.mux.HandleFunc("POST /api/rooms/{rid}/threads/{tid}/messages/{mid}/retry", s.retryMessage)
	s.mux.HandleFunc("GET /api/rooms/{rid}/activity", s.activity)
	s.mux.HandleFunc("GET /api/rooms/{rid}/requests/{id}/progress", s.progress)
	s.mux.HandleFunc("GET /api/rooms/{rid}/agents/{name}/log", s.agentLog)
	s.mux.HandleFunc("GET /api/quotas", s.apiQuotas)
	s.mux.HandleFunc("GET /api/presets", s.apiPresets)
	s.mux.HandleFunc("GET /api/presets/{preset}/quota-status", s.quotaStatus)
	s.mux.HandleFunc("GET /api/rooms/{rid}/reviews", s.listReviews)
	s.mux.HandleFunc("POST /api/rooms/{rid}/reviews", s.startReview)
	s.mux.HandleFunc("GET /api/rooms/{rid}/reviews/{id}", s.getReview)
	s.mux.HandleFunc("POST /api/rooms/{rid}/reviews/{id}/cancel", s.cancelReview)
	s.mux.HandleFunc("GET /api/committees", s.listCommittees)
	s.mux.HandleFunc("PUT /api/committees/{name}", s.putCommittee)
	s.mux.HandleFunc("DELETE /api/committees/{name}", s.deleteCommittee)
	s.mux.HandleFunc("POST /api/review-jobs", s.createJob)
	s.mux.HandleFunc("GET /api/review-jobs/{job}", s.getJob)
	s.mux.HandleFunc("POST /api/review-jobs/{job}/ack", s.ackJob)
	s.mux.HandleFunc("POST /api/review-jobs/{job}/cancel", s.cancelJob)
}

func (s *Server) apiQuotas(w http.ResponseWriter, r *http.Request) {
	entries, err := quota.Load(s.cfg.QuotaDir, s.cfg.QuotaConfig, time.Now())
	if entries == nil {
		// Load only returns a nil slice when it could not even list
		// cacheDir; that is a real failure, unlike a malformed
		// (optional) quotas.json, which Load reports alongside the
		// entries it could still produce.
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if err != nil {
		s.logOnce("quota.config", fmt.Sprintf("tincan web: quota config: %v", err))
	}
	s.writeJSON(w, http.StatusOK, entries)
}

func (s *Server) roomByID(id string) (rooms.Room, error) {
	room, ok, err := s.cfg.Registry.Get(id)
	if err != nil {
		return rooms.Room{}, err
	}
	if !ok || room.Missing {
		return rooms.Room{}, errNotFound
	}
	return room, nil
}

func (s *Server) opts(room rooms.Room) dispatch.Options {
	o := s.cfg.Dispatch
	o.Room = room.Path
	return o
}

// dispatcherFor returns a non-owning dispatcher for journal writes (post,
// stop, archive, retry); submission is left to the room's lock holder.
func (s *Server) dispatcherFor(room rooms.Room) *thread.Dispatcher { return thread.New(s.opts(room)) }

func (s *Server) room(w http.ResponseWriter, r *http.Request) (rooms.Room, bool) {
	room, err := s.roomByID(r.PathValue("rid"))
	if errors.Is(err, errNotFound) {
		s.fail(w, http.StatusNotFound, fmt.Errorf("room %s not found", r.PathValue("rid")))
		return room, false
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return room, false
	}
	return room, true
}

func (s *Server) threadOf(w http.ResponseWriter, r *http.Request) (rooms.Room, *thread.Thread, bool) {
	room, ok := s.room(w, r)
	if !ok {
		return room, nil, false
	}
	t, err := thread.Open(room.Path, r.PathValue("tid"))
	if err != nil {
		s.fail(w, http.StatusNotFound, errors.New("thread not found"))
		return room, nil, false
	}
	return room, t, true
}

type roomView struct {
	rooms.Room
	Running int `json:"running"`
}

func running(roomPath string) int {
	states, _ := host.ListStates(roomPath)
	n := 0
	for _, st := range states {
		if st.State == "busy" {
			n++
		}
	}
	return n
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Registry.List()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []roomView{}
	for _, room := range list {
		v := roomView{Room: room}
		if !room.Missing {
			v.Running = running(room.Path)
		}
		out = append(out, v)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) addRoom(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(w, r, apiBodyLimit, &in); err != nil {
		s.failBody(w, err)
		return
	}
	if !filepath.IsAbs(in.Path) {
		s.fail(w, http.StatusBadRequest, errors.New("expected {\"path\": \"/absolute/dir\"}"))
		return
	}
	room, err := s.cfg.Registry.Add(in.Path)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, room)
}

func (s *Server) patchRoom(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hidden bool `json:"hidden"`
	}
	if err := readJSON(w, r, apiBodyLimit, &in); err != nil {
		s.failBody(w, err)
		return
	}
	if err := s.cfg.Registry.SetHidden(r.PathValue("rid"), in.Hidden); err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"hidden": in.Hidden})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	res, err := s.dispatcherFor(room).Resolver()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	presets := []string{}
	for _, name := range host.Names(res.Presets) {
		if targets, _ := res.Resolve(thread.Meta{ID: "00000000"}, []string{name}); len(targets) == 1 {
			presets = append(presets, name)
		}
	}
	listeners := []string{}
	states, _ := host.ListStates(room.Path)
	for name, st := range states {
		if !strings.Contains(name, ".") && st.Owner != "" && st.Ready(r.Context()) {
			listeners = append(listeners, name)
		}
	}
	sort.Strings(listeners)
	s.writeJSON(w, http.StatusOK, map[string][]string{"presets": presets, "listeners": listeners})
}

type threadView struct {
	thread.Meta
	Running  int       `json:"running"`
	LastTime time.Time `json:"last_time"`
}

func (s *Server) listThreads(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	metas, err := thread.List(room.Path)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []threadView{}
	for _, m := range metas {
		v := threadView{Meta: m, LastTime: m.Created}
		if t, err := thread.Open(room.Path, m.ID); err == nil {
			if snap, err := t.Snapshot(); err == nil {
				v.Meta = snap.Meta
				for _, msg := range snap.Messages {
					if msg.Active() {
						v.Running++
					}
					if msg.Updated.After(v.LastTime) {
						v.LastTime = msg.Updated
					}
				}
			}
		}
		out = append(out, v)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) createThread(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	var in struct {
		Title    string `json:"title"`
		Primary  string `json:"primary"`
		ClientID string `json:"client_id"`
	}
	if err := readJSON(w, r, apiBodyLimit, &in); err != nil {
		s.failBody(w, err)
		return
	}
	if strings.TrimSpace(in.Title) == "" || in.Primary == "" {
		s.fail(w, http.StatusBadRequest, errors.New("expected {\"title\", \"primary\"}"))
		return
	}
	res, err := s.dispatcherFor(room).Resolver()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if targets, _ := res.Resolve(thread.Meta{ID: "00000000"}, []string{in.Primary}); len(targets) == 0 {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("agent %q is not available on %s", in.Primary, s.cfg.Machine))
		return
	}

	// createMu serializes the dedup lookup and thread.Create below: without
	// it, two concurrent requests for the same client_id could both find no
	// existing thread and both create one (thread.Create always makes a
	// fresh directory; there is no per-client_id lock to arbitrate between
	// distinct not-yet-created threads the way Dispatcher.Post's per-thread
	// journal lock arbitrates duplicate messages on an existing thread).
	s.createMu.Lock()
	defer s.createMu.Unlock()

	if in.ClientID != "" {
		metas, _ := thread.List(room.Path)
		for _, m := range metas {
			if m.ClientID == in.ClientID {
				s.writeJSON(w, http.StatusCreated, m)
				return
			}
		}
	}
	t, err := thread.Create(room.Path, strings.TrimSpace(in.Title), in.Primary, in.ClientID, s.cfg.ChainBudget)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	snap, err := t.Snapshot()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.hub.publish(note{Kind: "thread", Room: room.ID, Thread: t.ID})
	s.writeJSON(w, http.StatusCreated, snap.Meta)
}

type messageView struct {
	thread.Message
	HTML string `json:"html"`
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	_, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	snap, err := t.Snapshot()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []messageView{}
	for _, m := range snap.Messages {
		if m.Seq > after {
			out = append(out, messageView{Message: m, HTML: RenderMarkdown(m.Text)})
		}
	}
	chains := map[string]*thread.Chain{}
	for id, c := range snap.Chains {
		chains[id] = c
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"seq": snap.MaxSeq, "meta": snap.Meta, "messages": out, "chains": chains})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	var in struct {
		Text     string `json:"text"`
		ClientID string `json:"client_id"`
	}
	if err := readJSON(w, r, postBodyLimit, &in); err != nil {
		s.failBody(w, err)
		return
	}
	m, err := s.dispatcherFor(room).Post(r.Context(), t.ID, in.Text, in.ClientID)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "messages", Room: room.ID, Thread: t.ID})
	s.writeJSON(w, http.StatusCreated, messageView{Message: m, HTML: RenderMarkdown(m.Text)})
}

// stopWait bounds how long POST …/stop waits for the thread to leave
// stopping; it stays below the hub proxy's peerResponseHeaderTimeout so a
// Stop sent through a hub gets the thread back instead of a 502.
const stopWait = 25 * time.Second

func (s *Server) stopThread(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	if err := s.dispatcherFor(room).RequestStop(r.Context(), t.ID); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "messages", Room: room.ID, Thread: t.ID})
	ctx, cancel := context.WithTimeout(r.Context(), stopWait)
	defer cancel()
	meta, err := thread.WaitStatus(ctx, t, thread.StatusStopping)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, meta)
}

func (s *Server) archiveThread(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	var in struct {
		Archived bool `json:"archived"`
	}
	if err := readJSON(w, r, apiBodyLimit, &in); err != nil {
		s.failBody(w, err)
		return
	}
	if err := s.dispatcherFor(room).SetArchived(r.Context(), t.ID, in.Archived); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "thread", Room: room.ID, Thread: t.ID})
	snap, err := t.Snapshot()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, snap.Meta)
}

func (s *Server) retryMessage(w http.ResponseWriter, r *http.Request) {
	room, t, ok := s.threadOf(w, r)
	if !ok {
		return
	}
	if err := s.dispatcherFor(room).Retry(r.Context(), t.ID, r.PathValue("mid")); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.hub.publish(note{Kind: "messages", Room: room.ID, Thread: t.ID})
	s.writeJSON(w, http.StatusOK, map[string]bool{"retried": true})
}

type listenerView struct {
	Name    string `json:"name"`
	Preset  string `json:"preset,omitempty"`
	Mode    string `json:"mode"` // hosted | interactive
	Busy    bool   `json:"busy"`
	Current string `json:"current,omitempty"`
	Since   string `json:"since,omitempty"`
}

type requestView struct {
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	From    string    `json:"from"`
	Status  string    `json:"status"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Result  string    `json:"result,omitempty"`
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	listeners := []listenerView{}
	states, _ := host.ListStates(room.Path)
	for name, st := range states {
		if st.Ready(r.Context()) {
			listeners = append(listeners, listenerView{Name: name, Preset: st.Preset, Mode: "hosted", Busy: st.State == "busy", Current: st.CurrentID})
		}
	}
	if sp, err := spool.Open(room.Path); err == nil {
		if present, err := sp.ListPresence(); err == nil {
			for _, p := range present {
				if _, hosted := states[p.Name]; !hosted && p.Alive {
					listeners = append(listeners, listenerView{Name: p.Name, Mode: "interactive", Since: p.Since.Format(time.RFC3339)})
				}
			}
		}
	}
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].Name < listeners[j].Name })
	entries, _ := os.ReadDir(request.Dir(room.Path))
	var reqs []requestView
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		rec, err := request.Get(room.Path, id)
		if err != nil {
			continue
		}
		v := requestView{ID: rec.ID, Agent: rec.Agent, Status: rec.Status, Created: rec.Created, Updated: rec.Updated, Result: rec.Result}
		if rec.Envelope != nil {
			v.From = rec.Envelope.From
		}
		if len(v.Result) > 2048 {
			v.Result = truncateUTF8(v.Result, 2048) + "…"
		}
		reqs = append(reqs, v)
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Updated.After(reqs[j].Updated) })
	if len(reqs) > 50 {
		reqs = reqs[:50]
	}
	if reqs == nil {
		reqs = []requestView{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"listeners": listeners, "requests": reqs})
}

func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := request.ValidateID(id); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	events, next, err := request.Progress(room.Path, id, cursor, 200)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"events": events, "next_cursor": next})
}

// truncateUTF8 returns the longest prefix of s that is at most n bytes and
// does not split a multi-byte rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// agentLog streams the tail of a hosted listener's log. Host logs are never
// rotated and can grow well past logTailBytes, so this seeks to the last
// logTailBytes instead of fsutil.ReadFile's whole-file read (which would
// both reject any log over its size ceiling and, for everything under that
// ceiling, load the entire file just to keep its last chunk).
func (s *Server) agentLog(w http.ResponseWriter, r *http.Request) {
	room, ok := s.room(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := spool.ValidName(name); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	f, err := fsutil.OpenFile(host.LogPath(room.Path, name), os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			s.fail(w, http.StatusNotFound, errors.New("no log for this agent"))
			return
		}
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	size := info.Size()
	offset := int64(0)
	if size > logTailBytes {
		offset = size - logTailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.CopyN(w, f, size-offset)
}

func (s *Server) quotaStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, quota.Decide(s.cfg.QuotaDir, s.cfg.QuotaConfig, r.PathValue("preset"), time.Now()))
}
