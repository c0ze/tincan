package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func hubWithState(t *testing.T) (*Server, string) {
	t.Helper()
	s, _ := apiServer(t) // presets: claude (available), missing
	state, _ := filepath.EvalSymlinks(t.TempDir())
	s.cfg.StateDir = state
	return s, state
}

func TestCommitteeCRUDOnHub(t *testing.T) {
	s, _ := hubWithState(t)
	h := s.Handler()
	rec := do(t, h, "PUT", "/api/committees/reviewers", `{"members":["claude@box"],"instructions":"cite file:line"}`, mut())
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	var saved struct {
		Committee struct {
			Version int `json:"version"`
		} `json:"committee"`
	}
	decode(t, rec.Body.String(), &saved)
	if saved.Committee.Version != 1 {
		t.Fatalf("version: %s", rec.Body)
	}
	rec = do(t, h, "GET", "/api/committees", "", ownerHdr())
	var view committeesView
	decode(t, rec.Body.String(), &view)
	if view.Role != "hub" || len(view.Committees) != 1 || view.Committees[0].Instructions != "cite file:line" {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec := do(t, h, "DELETE", "/api/committees/reviewers", "", mut()); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "DELETE", "/api/committees/reviewers", "", mut()); rec.Code != 404 {
		t.Fatalf("second delete: %d", rec.Code)
	}
}

func TestCommitteeValidationAtSave(t *testing.T) {
	s, _ := hubWithState(t)
	h := s.Handler()
	for body, want := range map[string]string{
		`{"members":["missing@box"]}`:      "not available",
		`{"members":["nosuch@box"]}`:       "no preset",
		`{"members":["claude@elsewhere"]}`: "unknown machine",
		`{"members":[]}`:                   "1 to 8",
	} {
		rec := do(t, h, "PUT", "/api/committees/reviewers", body, mut())
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s (want %q)", body, rec.Code, rec.Body, want)
		}
	}
	if rec := do(t, h, "PUT", "/api/committees/claude", `{"members":["claude@box"]}`, mut()); rec.Code != 400 || !strings.Contains(rec.Body.String(), "preset name") {
		t.Errorf("committee named like a preset: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/committees/reviewers", strings.Repeat(" ", 17<<10)+`{}`, mut()); rec.Code != 413 {
		t.Errorf("oversized body: %d", rec.Code)
	}
}

func TestCommitteeMemberOnOfflinePeerIsRefused(t *testing.T) {
	hub, _, peerHTTP, _ := peerPair(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	hub.cfg.StateDir = state
	peerHTTP.Close()
	rec := do(t, hub.Handler(), "PUT", "/api/committees/reviewers", `{"members":["claude@macmini"]}`, mut())
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "macmini") {
		t.Fatalf("offline peer member: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, hub.Handler(), "GET", "/api/committees", "", ownerHdr()); !strings.Contains(rec.Body.String(), `"committees":[]`) {
		t.Fatalf("saved anyway: %s", rec.Body)
	}
}

func TestCommitteesDisabledWithoutStateDir(t *testing.T) {
	s, _ := apiServer(t)
	if rec := do(t, s.Handler(), "GET", "/api/committees", "", ownerHdr()); rec.Code != 503 {
		t.Fatalf("no state dir: %d", rec.Code)
	}
}

func TestMalformedCommitteesFileIs500(t *testing.T) {
	s, state := hubWithState(t)
	writeFile(t, filepath.Join(state, "committees.json"), "{broken")
	rec := do(t, s.Handler(), "GET", "/api/committees", "", ownerHdr())
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "committees.json") {
		t.Fatalf("malformed: %d %s", rec.Code, rec.Body)
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}
func TestPeerCachesHubCommittees(t *testing.T) {
	// hub = the httptest peer ("macmini") holding committees; the local
	// server ("cachyos") runs with --committees-from macmini.
	local, _, peerHTTP, hubServer := peerPair(t)
	hubState, _ := filepath.EvalSymlinks(t.TempDir())
	hubServer.cfg.StateDir = hubState
	committee.NewStore(hubState).Put(context.Background(), committee.Committee{Name: "reviewers", Members: []string{"x@macmini"}})
	localState, _ := filepath.EvalSymlinks(t.TempDir())
	local.cfg.StateDir, local.cfg.CommitteesFrom = localState, "macmini"

	rec := do(t, local.Handler(), "GET", "/api/committees", "", ownerHdr())
	var view committeesView
	decode(t, rec.Body.String(), &view)
	if view.Role != "peer" || view.From != "macmini" || view.FetchedAt == nil || len(view.Committees) != 1 || view.Error != "" {
		t.Fatalf("peer view: %s", rec.Body)
	}
	if rec := do(t, local.Handler(), "PUT", "/api/committees/x", `{"members":["x@macmini"]}`, mut()); rec.Code != 409 || !strings.Contains(rec.Body.String(), "edited on macmini") {
		t.Fatalf("edit on peer: %d %s", rec.Code, rec.Body)
	}
	// Hub goes away: the cache is kept and the error is reported.
	peerHTTP.Close()
	c, _, _ := committee.LoadCache(localState)
	c.FetchedAt = c.FetchedAt.Add(-time.Minute) // force a refresh attempt
	committee.SaveCache(localState, c)
	rec = do(t, local.Handler(), "GET", "/api/committees", "", ownerHdr())
	decode(t, rec.Body.String(), &view)
	if len(view.Committees) != 1 || view.Error == "" {
		t.Fatalf("stale cache view: %s", rec.Body)
	}
}

func TestPeerRefusesANonHub(t *testing.T) {
	local, _, _, hubServer := peerPair(t)
	hubServer.cfg.StateDir, _ = filepath.EvalSymlinks(t.TempDir())
	hubServer.cfg.CommitteesFrom = "cachyos" // misconfigured: also a peer
	local.cfg.StateDir, _ = filepath.EvalSymlinks(t.TempDir())
	local.cfg.CommitteesFrom = "macmini"
	rec := do(t, local.Handler(), "GET", "/api/committees", "", ownerHdr())
	if !strings.Contains(rec.Body.String(), "not a committees hub") {
		t.Fatalf("non-hub accepted: %s", rec.Body)
	}
}

func TestNewRejectsUnknownCommitteesFrom(t *testing.T) {
	_, err := New(Config{Owner: owner, Registry: rooms.Open(filepath.Join(t.TempDir(), "rooms.json")), CommitteesFrom: "nowhere"})
	if err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("unknown hub accepted: %v", err)
	}
}
