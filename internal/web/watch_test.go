package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/fsnotify/fsnotify"
)

// writeRecords writes n request records to room whose file times and
// Updated times both step back a minute per record, rec-000 newest.
func writeRecords(t *testing.T, room string, n int) {
	t.Helper()
	dir := request.Dir(room)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range n {
		id := fmt.Sprintf("rec-%03d", i)
		when := now.Add(-time.Duration(i) * time.Minute)
		data, _ := json.Marshal(request.Record{ID: id, Agent: "claude", Status: "completed", Created: when, Updated: when,
			Envelope: &envelope.Envelope{ID: id, From: "you", To: "claude"}})
		path := filepath.Join(dir, id+".json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("no /dev/fd")
	}
	return len(entries)
}

// kqueue opens every file in a watched directory, so watching requests/
// held one fd per record and progress file, growing toward the limit.
func TestWatchDirsHoldNoFDPerRequestFile(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	writeRecords(t, dir, 100)
	room := rooms.Room{ID: "abcdef123456", Path: dir}
	dirs := watchDirs(room)
	if got := slices.Contains(dirs, request.Dir(dir)); got == kqueueWatch {
		t.Fatalf("requests/ watched = %v with kqueue = %v", got, kqueueWatch)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skip(err)
	}
	defer w.Close()
	before := openFDs(t)
	for _, d := range dirs {
		w.Add(d)
	}
	if grew := openFDs(t) - before; grew >= 100 {
		t.Fatalf("watching the room opened %d fds", grew)
	}
}

// Activity returns the newest records and parses only those, not every
// record the room has ever had.
func TestActivityReadsOnlyNewestRequests(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	writeRecords(t, room.Path, 120)
	reads := 0
	defer func(get func(string, string) (request.Record, error)) { getRequest = get }(getRequest)
	getRequest = func(room, id string) (request.Record, error) {
		reads++
		return request.Get(room, id)
	}
	rec := do(t, s.Handler(), "GET", "/api/rooms/"+rid+"/activity", "", ownerHdr())
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var out struct {
		Requests []requestView `json:"requests"`
	}
	decode(t, rec.Body.String(), &out)
	if len(out.Requests) != activityRequests {
		t.Fatalf("%d requests, want %d", len(out.Requests), activityRequests)
	}
	for i, r := range out.Requests {
		if want := fmt.Sprintf("rec-%03d", i); r.ID != want {
			t.Fatalf("request %d is %s, want %s", i, r.ID, want)
		}
	}
	if reads != activityRequests {
		t.Fatalf("parsed %d records to return %d", reads, activityRequests)
	}
}
