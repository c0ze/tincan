package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
)

func apiServer(t *testing.T) (*Server, string) {
	t.Helper()
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	room, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(t.TempDir(), "agent")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	presets := map[string]host.Preset{
		"claude":  {Exec: []string{bin}, Stdin: "none", Reply: "stdout"},
		"missing": {Exec: []string{filepath.Join(t.TempDir(), "absent")}},
	}
	s, err := New(Config{Owner: owner, Machine: "box", Registry: reg, ChainBudget: 6, Dispatch: dispatch.Options{Presets: presets}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := reg.Add(room)
	if err != nil {
		t.Fatal(err)
	}
	return s, r.ID
}

func mut() map[string]string {
	return map[string]string{"Tailscale-User-Login": owner, "X-Tincan-Request": "1"}
}

func decode(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestThreadLifecycleOverAPI(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"audit","primary":"claude","client_id":"c1"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var meta struct{ ID string }
	decode(t, rec.Body.String(), &meta)
	again := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"audit","primary":"claude","client_id":"c1"}`, mut())
	var meta2 struct{ ID string }
	decode(t, again.Body.String(), &meta2)
	if meta2.ID != meta.ID {
		t.Fatal("client_id did not dedupe thread creation")
	}
	rec = do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages", `{"text":"hello <b>","client_id":"m1"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages?after=0", "", ownerHdr())
	var page struct {
		Seq      int64
		Messages []struct{ Role, Text, HTML, State string }
	}
	decode(t, rec.Body.String(), &page)
	if page.Seq == 0 || len(page.Messages) != 2 || page.Messages[0].HTML != "<p>hello &lt;b&gt;</p>" || page.Messages[1].State != "pending" {
		t.Fatalf("page %+v", page)
	}
	rec = do(t, h, "GET", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages?after="+itoa(page.Seq), "", ownerHdr())
	decode(t, rec.Body.String(), &page)
	if len(page.Messages) != 0 {
		t.Fatalf("cursor returned old messages: %+v", page.Messages)
	}
}

func TestCreateThreadRejectsUnavailablePrimary(t *testing.T) {
	s, rid := apiServer(t)
	rec := do(t, s.Handler(), "POST", "/api/rooms/"+rid+"/threads", `{"title":"x","primary":"missing"}`, mut())
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "missing") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUnknownRoomAndThreadAre404(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	if rec := do(t, h, "GET", "/api/rooms/000000000000/threads", "", ownerHdr()); rec.Code != 404 {
		t.Fatalf("room: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/rooms/"+rid+"/threads/deadbeef/messages", "", ownerHdr()); rec.Code != 404 {
		t.Fatalf("thread: %d", rec.Code)
	}
}

func TestAgentsListsAvailablePresetsOnly(t *testing.T) {
	s, rid := apiServer(t)
	rec := do(t, s.Handler(), "GET", "/api/rooms/"+rid+"/agents", "", ownerHdr())
	var got struct{ Presets []string }
	decode(t, rec.Body.String(), &got)
	if len(got.Presets) != 1 || got.Presets[0] != "claude" {
		t.Fatalf("presets %q", got.Presets)
	}
}

func TestAgentLogRejectsTraversal(t *testing.T) {
	s, rid := apiServer(t)
	if rec := do(t, s.Handler(), "GET", "/api/rooms/"+rid+"/agents/..%2F..%2Fetc/log", "", ownerHdr()); rec.Code != 400 && rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

// TestPostMessageAllowsEscapedBodyUnderPostCap covers fix round 1 finding 1:
// JSON-escaping a legal, well-under-limit message can inflate the encoded
// request body past a naive byte cap. 120 KiB of alternating "\n\t" is well
// under thread.MaxPostBytes (128 KiB) decoded, but each byte encodes to two
// (\n -> `\n`, \t -> `\t`), pushing the JSON body past the old
// thread.MaxPostBytes+16<<10 (144 KiB) cap; it must still succeed under the
// new postBodyLimit.
func TestPostMessageAllowsEscapedBodyUnderPostCap(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"t","primary":"claude"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var meta struct{ ID string }
	decode(t, rec.Body.String(), &meta)

	// A leading non-whitespace prefix keeps Dispatcher.Post's
	// strings.TrimSpace(text) == "" emptiness check from tripping: TrimSpace
	// only trims runs at the very start/end, but a body that is *entirely*
	// whitespace trims away to "".
	text := "hi" + strings.Repeat("\n\t", (120*1024-2)/2)
	if len(text) != 120*1024 {
		t.Fatalf("test text is %d bytes, want exactly 120 KiB", len(text))
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 144<<10 {
		t.Fatalf("encoded body only %d bytes; want > 144 KiB to actually exercise the old cap", len(body))
	}
	if int64(len(body)) >= postBodyLimit {
		t.Fatalf("encoded body %d bytes; want it under postBodyLimit (%d) so this proves the cap, not an unrelated failure", len(body), postBodyLimit)
	}
	rec = do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages", string(body), mut())
	if rec.Code != 201 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body)
	}
}

// TestPostMessageOverPostCapReturns413 covers fix round 1 finding 1's other
// half: a body genuinely over the cap must fail loudly (413), not decode
// into a truncated, confusing "unexpected end of JSON input" 400.
func TestPostMessageOverPostCapReturns413(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"t","primary":"claude"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var meta struct{ ID string }
	decode(t, rec.Body.String(), &meta)

	huge := strings.Repeat("a", int(postBodyLimit)+1024)
	body, err := json.Marshal(map[string]string{"text": huge})
	if err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages", string(body), mut())
	if rec.Code != 413 {
		t.Fatalf("over cap: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("body: %s", rec.Body)
	}
}

// TestCreateThreadDedupIsRaceFree covers fix round 1 finding 2: without
// serializing the client_id dedup lookup and thread.Create, concurrent
// requests for the same client_id can each see "no existing thread" and each
// create one.
func TestCreateThreadDedupIsRaceFree(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"race","primary":"claude","client_id":"race1"}`, mut())
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 201 {
			t.Fatalf("goroutine %d: unexpected status %d", i, c)
		}
	}
	room, err := s.roomByID(rid)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := thread.List(room.Path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range metas {
		if m.ClientID == "race1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 thread for client_id race1, got %d", count)
	}
}

// TestPostMessageDedupByClientID covers fix round 1 finding 4's added
// coverage: two posts sharing a client_id return the same message and leave
// exactly one user message in the thread (Dispatcher.Post's per-thread
// journal lock already serializes this; this test exercises it through the
// HTTP layer).
func TestPostMessageDedupByClientID(t *testing.T) {
	s, rid := apiServer(t)
	h := s.Handler()
	rec := do(t, h, "POST", "/api/rooms/"+rid+"/threads", `{"title":"dedup","primary":"claude"}`, mut())
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var meta struct{ ID string }
	decode(t, rec.Body.String(), &meta)

	first := do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages", `{"text":"hi","client_id":"m1"}`, mut())
	if first.Code != 201 {
		t.Fatalf("first post: %d %s", first.Code, first.Body)
	}
	var m1 struct{ ID string }
	decode(t, first.Body.String(), &m1)

	second := do(t, h, "POST", "/api/rooms/"+rid+"/threads/"+meta.ID+"/messages", `{"text":"hi","client_id":"m1"}`, mut())
	if second.Code != 201 {
		t.Fatalf("second post: %d %s", second.Code, second.Body)
	}
	var m2 struct{ ID string }
	decode(t, second.Body.String(), &m2)
	if m1.ID == "" || m1.ID != m2.ID {
		t.Fatalf("client_id did not dedupe message post: %q vs %q", m1.ID, m2.ID)
	}

	room, err := s.roomByID(rid)
	if err != nil {
		t.Fatal(err)
	}
	th, err := thread.Open(room.Path, meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := th.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	userMsgs := 0
	for _, msg := range snap.Messages {
		if msg.Role == thread.RoleUser {
			userMsgs++
		}
	}
	if userMsgs != 1 {
		t.Fatalf("want exactly 1 user message, got %d", userMsgs)
	}
}

// TestAgentLogReturnsExactTail covers fix round 1 finding 3: a log larger
// than logTailBytes must return exactly its last logTailBytes, not 404 (the
// old fsutil.ReadFile(..., 64<<20) ceiling) and not a truncated-from-the-
// front slice.
func TestAgentLogReturnsExactTail(t *testing.T) {
	s, rid := apiServer(t)
	room, err := s.roomByID(rid)
	if err != nil {
		t.Fatal(err)
	}
	dir := host.Dir(room.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for b.Len() < logTailBytes+10000 {
		b.WriteString("0123456789")
	}
	content := b.String()
	if err := os.WriteFile(host.LogPath(room.Path, "claude"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := do(t, s.Handler(), "GET", "/api/rooms/"+rid+"/agents/claude/log", "", ownerHdr())
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	want := content[len(content)-logTailBytes:]
	if rec.Body.String() != want {
		t.Fatalf("got %d bytes, want %d bytes (exact tail mismatch)", rec.Body.Len(), len(want))
	}
}

func TestQuotasEndpoint(t *testing.T) {
	s, _ := apiServer(t)
	dir := t.TempDir()
	s.cfg.QuotaDir = dir
	s.cfg.QuotaConfig = filepath.Join(dir, "none.json")
	now := time.Now()
	os.WriteFile(filepath.Join(dir, "claude-quota.json"), []byte(fmt.Sprintf(`{"percent": 97, "reset_at": %d, "fetched_at": %d}`, now.Add(time.Hour).Unix(), now.Unix())), 0o600)
	rec := do(t, s.Handler(), "GET", "/api/quotas", "", ownerHdr())
	var got []struct {
		ID      string
		State   string
		Percent float64
	}
	decode(t, rec.Body.String(), &got)
	if len(got) != 1 || got[0].ID != "claude" || got[0].State != "ok" || got[0].Percent != 97 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
