package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/rooms"
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
