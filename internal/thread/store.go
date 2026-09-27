// internal/thread/store.go
package thread

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/request"
)

var ErrNotFound = errors.New("thread not found")

func Root(room string) string { return filepath.Join(room, ".tincan", "threads") }

type Thread struct {
	Room string
	ID   string
	dir  string
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func validTID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (t *Thread) metaPath() string   { return filepath.Join(t.dir, "thread.json") }
func (t *Thread) eventsPath() string { return filepath.Join(t.dir, "events.jsonl") }
func (t *Thread) lockPath() string   { return filepath.Join(t.dir, "lock") }

func Create(room, title, primary, clientID string, budget int) (*Thread, error) {
	id := randHex(4)
	t := &Thread{Room: room, ID: id, dir: filepath.Join(Root(room), id)}
	if err := fsutil.MkdirPrivate(t.dir); err != nil {
		return nil, err
	}
	m := Meta{ID: id, Title: title, Primary: primary, Created: time.Now().UTC(), Status: StatusOpen, Budget: budget, ClientID: clientID}
	if err := t.writeMeta(m); err != nil {
		return nil, err
	}
	return t, nil
}

func Open(room, id string) (*Thread, error) {
	if !validTID(id) {
		return nil, ErrNotFound
	}
	t := &Thread{Room: room, ID: id, dir: filepath.Join(Root(room), id)}
	if _, err := os.Stat(t.metaPath()); err != nil {
		return nil, ErrNotFound
	}
	return t, nil
}

func List(room string) ([]Meta, error) {
	entries, err := os.ReadDir(Root(room))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() || !validTID(e.Name()) {
			continue
		}
		t := &Thread{Room: room, ID: e.Name(), dir: filepath.Join(Root(room), e.Name())}
		m, err := t.readMeta()
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

func (t *Thread) readMeta() (Meta, error) {
	var m Meta
	data, err := os.ReadFile(t.metaPath())
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data, &m)
}

func (t *Thread) writeMeta(m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(t.metaPath(), append(data, '\n'))
}

// readEvents reads complete journal lines. Each line is one committed
// transaction (a JSON array of events), so a torn final line drops that whole
// transaction and nothing else. It returns the byte length of the complete
// prefix.
func (t *Thread) readEvents() ([]Event, int64, error) {
	data, err := os.ReadFile(t.eventsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	valid := int64(bytes.LastIndexByte(data, '\n') + 1)
	var out []Event
	for _, line := range bytes.Split(data[:valid], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var batch []Event
		if err := json.Unmarshal(line, &batch); err != nil {
			return nil, 0, fmt.Errorf("%s: corrupt journal line: %w", t.eventsPath(), err)
		}
		out = append(out, batch...)
	}
	return out, valid, nil
}

func (t *Thread) events() ([]Event, error) {
	evs, _, err := t.readEvents()
	return evs, err
}

// load reads the thread.json cache and the journal, and folds the journal
// over the cached meta. It also returns the cached meta on its own (before
// any journal-derived thread event overrides it) so Update can tell whether
// the cache is stale relative to the journal, which is authoritative.
func (t *Thread) load() (s *Snapshot, cached Meta, valid int64, err error) {
	cached, err = t.readMeta()
	if err != nil {
		return nil, Meta{}, 0, err
	}
	evs, valid, err := t.readEvents()
	if err != nil {
		return nil, Meta{}, 0, err
	}
	s = newSnapshot(cached)
	for _, e := range evs {
		s.apply(e)
	}
	s.sortMessages()
	return s, cached, valid, nil
}

// Snapshot reads the thread without locking; it never sees a torn line.
func (t *Thread) Snapshot() (Snapshot, error) {
	s, _, _, err := t.load()
	if err != nil {
		return Snapshot{}, err
	}
	return *s, nil
}

type Tx struct {
	Meta         Meta
	Snap         *Snapshot
	appended     []Event
	metaSave     bool
	metaEventIdx int
	nextSeq      int64
}

// Append assigns seq and time (and N and ID for messages), applies the event
// to Snap so later logic in the same transaction sees it, and stages it.
func (tx *Tx) Append(e Event) Event {
	e.Seq = tx.nextSeq
	tx.nextSeq++
	e.Time = time.Now().UTC()
	if e.Kind == KindMessage {
		if e.ID == "" {
			e.ID = "m" + randHex(6)
		}
		e.N = tx.Snap.NextN
	}
	tx.Snap.apply(e)
	tx.appended = append(tx.appended, e)
	return e
}

// SaveMeta records a thread event once per transaction. Its Meta payload is
// filled in at commit time (see Update), from the final value of tx.Meta
// after fn returns, so any changes made after calling SaveMeta are included.
func (tx *Tx) SaveMeta() {
	if !tx.metaSave {
		tx.metaSave = true
		tx.metaEventIdx = len(tx.appended)
		tx.Append(Event{Kind: KindThread})
	}
}

func deepCopyMeta(m Meta) Meta {
	cp := m
	if m.Listeners != nil {
		cp.Listeners = make(map[string]string, len(m.Listeners))
		for k, v := range m.Listeners {
			cp.Listeners[k] = v
		}
	}
	return cp
}

// metaEqual compares two Meta values field-by-field, using time.Time.Equal
// for Created so wall-clock/monotonic representation differences never cause
// a spurious cache repair.
func metaEqual(a, b Meta) bool {
	if a.ID != b.ID || a.Title != b.Title || a.Primary != b.Primary || a.Status != b.Status || a.Budget != b.Budget || a.ClientID != b.ClientID {
		return false
	}
	if !a.Created.Equal(b.Created) {
		return false
	}
	if len(a.Listeners) != len(b.Listeners) {
		return false
	}
	for k, v := range a.Listeners {
		if bv, ok := b.Listeners[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// Update runs fn under the thread lock against a fresh snapshot and commits
// its staged events (after repairing any torn tail) and meta. The journal is
// authoritative for meta: thread.json is only a cache, and Update rewrites it
// whenever it is stale relative to the journal-derived meta, in addition to
// whenever fn itself calls SaveMeta.
func (t *Thread) Update(ctx context.Context, fn func(*Tx) error) error {
	l, err := filelock.Acquire(ctx, t.lockPath())
	if err != nil {
		return err
	}
	defer l.Close()
	s, cached, valid, err := t.load()
	if err != nil {
		return err
	}
	tx := &Tx{Meta: s.Meta, Snap: s, nextSeq: s.MaxSeq + 1}
	if err := fn(tx); err != nil {
		return err
	}
	if tx.metaSave {
		final := deepCopyMeta(tx.Meta)
		tx.appended[tx.metaEventIdx].Meta = &final
	}
	if len(tx.appended) > 0 {
		if err := t.appendEvents(valid, tx.appended); err != nil {
			return err
		}
	}
	if tx.metaSave || !metaEqual(cached, tx.Meta) {
		return t.writeMeta(tx.Meta)
	}
	return nil
}

func (t *Thread) appendEvents(valid int64, evs []Event) error {
	var buf bytes.Buffer
	line, err := json.Marshal(evs) // one line = one atomic transaction
	if err != nil {
		return err
	}
	buf.Write(line)
	buf.WriteByte('\n')
	f, err := fsutil.OpenFile(t.eventsPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > valid {
		if err := f.Truncate(valid); err != nil {
			return err
		}
	}
	if _, err := f.Seek(valid, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return err
	}
	return f.Sync()
}

func (t *Thread) promptPath(requestID string) (string, error) {
	if err := request.ValidateID(requestID); err != nil {
		return "", err
	}
	return filepath.Join(t.dir, "prompts", requestID+".txt"), nil
}

func (t *Thread) WritePrompt(requestID, prompt string) error {
	p, err := t.promptPath(requestID)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(p, []byte(prompt))
}

func (t *Thread) ReadPrompt(requestID string) (string, error) {
	p, err := t.promptPath(requestID)
	if err != nil {
		return "", err
	}
	data, err := fsutil.ReadFile(p, request.MaxBodyBytes)
	return string(data), err
}
