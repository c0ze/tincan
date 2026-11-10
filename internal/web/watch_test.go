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
	now := time.Now().UTC()
	for i := range n {
		when := now.Add(-time.Duration(i) * time.Minute)
		writeRecord(t, room, fmt.Sprintf("rec-%03d", i), when, when, true)
	}
}

// writeRecord writes one request record with the given Updated and file
// times, and its progress file; an invalid one does not parse.
func writeRecord(t *testing.T, room, id string, updated, mtime time.Time, valid bool) {
	t.Helper()
	dir := request.Dir(room)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(request.Record{ID: id, Agent: "claude", Status: "completed", Created: updated, Updated: updated,
		Envelope: &envelope.Envelope{ID: id, From: "you", To: "claude"}})
	if !valid {
		data = []byte("not json")
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// activityIDs fetches the Activity page and returns its request IDs, and
// how many records it parsed to build them.
func activityIDs(t *testing.T, s *Server, rid string) ([]string, int) {
	t.Helper()
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
	ids := []string{}
	for _, r := range out.Requests {
		ids = append(ids, r.ID)
	}
	return ids, reads
}

// File times only approximate Updated: Updated is stamped before the write,
// and coarse timestamps tie. A record whose file time is a little older
// than the cut-off, or ties with it, is still listed by its Updated time.
func TestActivityToleratesMtimeDisorderAndTies(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	// 60 records sharing one file time, Updated a second apart: tie-000 newest.
	for i := range 60 {
		writeRecord(t, room.Path, fmt.Sprintf("tie-%03d", i), base.Add(-time.Duration(i)*time.Second), base, true)
	}
	// Written after its Updated stamp by a stalled writer: newest by Updated,
	// but its file time is a second older than all the others.
	writeRecord(t, room.Path, "late", base.Add(time.Minute), base.Add(-time.Second), true)
	ids, _ := activityIDs(t, s, rid)
	want := []string{"late"}
	for i := range activityRequests - 1 {
		want = append(want, fmt.Sprintf("tie-%03d", i))
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("got %v\nwant %v", ids, want)
	}
}

// A writer paused between stamping Updated and writing leaves a file time
// much newer than its Updated: that record must not push a genuinely newer
// request out of the list.
func TestActivityToleratesPausedWriter(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	var want []string
	for i := range activityRequests - 1 {
		id := fmt.Sprintf("rec-%03d", i)
		when := base.Add(-time.Duration(i) * time.Second)
		writeRecord(t, room.Path, id, when, when, true)
		want = append(want, id)
	}
	writeRecord(t, room.Path, "paused", base.Add(-100*time.Second), base.Add(time.Second), true)
	older := base.Add(-51 * time.Second)
	writeRecord(t, room.Path, "older", older, older, true)
	want = append(want, "older")
	if ids, _ := activityIDs(t, s, rid); !slices.Equal(ids, want) {
		t.Fatalf("got %v\nwant %v", ids, want)
	}
}

// Malformed records cost reads too: however many recent ones fail to
// parse, one Activity call parses a bounded number of records.
func TestActivityBoundsReadsOfMalformedRecords(t *testing.T) {
	s, rid := apiServer(t)
	room, _ := s.roomByID(rid)
	now := time.Now().UTC()
	for i := range 300 {
		when := now.Add(-time.Duration(i) * time.Second)
		writeRecord(t, room.Path, fmt.Sprintf("bad-%03d", i), when, when, false)
	}
	for i := range 20 { // valid records, older than every malformed one

		when := now.Add(-time.Hour - time.Duration(i)*time.Minute)
		writeRecord(t, room.Path, fmt.Sprintf("rec-%03d", i), when, when, true)
	}
	_, reads := activityIDs(t, s, rid)
	if reads > activityReads {
		t.Fatalf("parsed %d records; the bound is %d", reads, activityReads)
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
