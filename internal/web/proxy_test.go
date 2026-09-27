// internal/web/proxy_test.go
package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/rooms"
)

// servedLike emulates `tailscale serve --set-path /tincan`: it strips the
// mount prefix and overwrites the identity header for the calling device.
func servedLike(h http.Handler, login string) http.Handler {
	return http.StripPrefix("/tincan", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Tailscale-User-Login", login)
		h.ServeHTTP(w, r)
	}))
}

func peerPair(t *testing.T) (*Server, *http.Request, *httptest.Server) {
	t.Helper()
	peerReg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	peerReg.Add(t.TempDir())
	peerServer, err := New(Config{Owner: owner, Machine: "macmini", Registry: peerReg})
	if err != nil {
		t.Fatal(err)
	}
	last := &http.Request{}
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*last = *r.Clone(context.Background())
		peerServer.Handler().ServeHTTP(w, r)
	})
	peerHTTP := httptest.NewServer(servedLike(record, owner))
	t.Cleanup(peerHTTP.Close)
	hubServer, err := New(Config{Owner: owner, Machine: "cachyos", Registry: rooms.Open(filepath.Join(t.TempDir(), "rooms.json")),
		Peers: []Peer{{Name: "macmini", URL: peerHTTP.URL + "/tincan/"}}})
	if err != nil {
		t.Fatal(err)
	}
	return hubServer, last, peerHTTP
}

func TestProxyForwardsReadsWithPathJoinedOnce(t *testing.T) {
	hub, seen, _ := peerPair(t)
	rec := do(t, hub.Handler(), "GET", "/api/peers/macmini/rooms", "", ownerHdr())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if seen.URL.Path != "/api/rooms" {
		t.Fatalf("peer saw path %q", seen.URL.Path)
	}
}

func TestProxyMutationUsesHubHeaders(t *testing.T) {
	hub, seen, _ := peerPair(t)
	hdr := mut()
	hdr["Origin"] = "http://example.com"
	hdr["Cookie"] = "secret=1"
	rec := do(t, hub.Handler(), "POST", "/api/peers/macmini/rooms", `{"path":"/nonexistent"}`, hdr)
	if rec.Code == 403 {
		t.Fatalf("mutation refused: %s", rec.Body)
	}
	if seen.Header.Get("Origin") != "" || seen.Header.Get("Cookie") != "" || seen.Header.Get("X-Tincan-Request") != "1" {
		t.Fatalf("peer headers %v", seen.Header)
	}
}

func TestProxyRejectsTraversalAndUnknownPeer(t *testing.T) {
	hub, _, _ := peerPair(t)
	// Go's ServeMux path-cleans "../.." segments itself before our handler
	// ever runs, redirecting away from the peers route entirely; on wildcard
	// patterns it does so with 307 (not 301 as on the old fixed-pattern
	// mux), so that's an acceptable outcome here alongside our own checks.
	if rec := do(t, hub.Handler(), "GET", "/api/peers/macmini/../../comics", "", ownerHdr()); rec.Code != 400 && rec.Code != 404 && rec.Code != 301 && rec.Code != 307 {
		t.Fatalf("traversal: %d", rec.Code)
	}
	if rec := do(t, hub.Handler(), "GET", "/api/peers/nobody/rooms", "", ownerHdr()); rec.Code != 404 {
		t.Fatalf("unknown peer: %d", rec.Code)
	}
	if rec := do(t, hub.Handler(), "GET", "/api/peers/macmini/peers/cachyos/rooms", "", ownerHdr()); rec.Code != 400 {
		t.Fatalf("chained peer: %d", rec.Code)
	}
}

func TestPeerHealthTracksOffline(t *testing.T) {
	hub, _, peerSrv := peerPair(t)
	hub.checkPeers(context.Background())
	if !hub.peers["macmini"].online.Load() {
		t.Fatal("peer not online")
	}
	peerSrv.Close()
	hub.checkPeers(context.Background())
	if hub.peers["macmini"].online.Load() {
		t.Fatal("peer still online after close")
	}
	rec := do(t, hub.Handler(), "GET", "/api/peers/macmini/rooms", "", ownerHdr())
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("offline peer: %d %s", rec.Code, body)
	}
}
