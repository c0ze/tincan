package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

// openStream GETs an event stream and returns its body once the first
// line has arrived.
func openStream(t *testing.T, url string) io.ReadCloser {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Tailscale-User-Login", owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d", url, resp.StatusCode)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return resp.Body
}

// streamEnded reports whether body hits EOF or an error within d.
func streamEnded(body io.Reader, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, body)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Run must not return when shutdown starts: the room passes still running,
// the dispatcher locks and the heartbeat are all handled before it returns.
// Event streams, local and proxied, end at once rather than holding
// Shutdown for its whole grace.
func TestRunWaitsForShutdown(t *testing.T) {
	peerSSE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": connected\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(peerSSE.Close)
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	state, _ := filepath.EvalSymlinks(t.TempDir())
	qdir := t.TempDir()
	s, err := New(Config{Owner: owner, Machine: "box", Registry: reg, QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json"), StateDir: state,
		Peers: []Peer{{Name: "peer", URL: peerSSE.URL + "/"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Add(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".tincan"), 0o700); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	s.reconcileRoom = func(ctx context.Context, room rooms.Room, janitor bool) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no room pass started")
	}
	base := "http://" + ln.Addr().String()
	local := openStream(t, base+"/api/events")
	defer local.Close()
	proxied := openStream(t, base+"/api/peers/peer/events")
	defer proxied.Close()
	waitFor(t, func() bool { _, err := os.Stat(rooms.HeartbeatPath(state)); return err == nil })

	cancel()
	if !streamEnded(local, 2*time.Second) {
		t.Error("the local event stream was not closed at shutdown")
	}
	if !streamEnded(proxied, 2*time.Second) {
		t.Error("the proxied event stream was not closed at shutdown")
	}
	select {
	case <-done:
		t.Fatal("Run returned while a room pass was still running")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(rooms.HeartbeatPath(state)); err != nil {
		t.Fatalf("heartbeat removed before shutdown finished: %v", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the room pass finished")
	}
	if _, err := os.Stat(rooms.HeartbeatPath(state)); !os.IsNotExist(err) {
		t.Fatalf("heartbeat left behind: %v", err)
	}
}

// A job creation whose request body arrives after shutdown began would
// start a background creation that nothing joins: it is refused instead.
func TestRunRefusesJobsOnceShuttingDown(t *testing.T) {
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	state, _ := filepath.EvalSymlinks(t.TempDir())
	qdir := t.TempDir()
	// An executable that does not exist: a creation fails before launching.
	presets := map[string]host.Preset{"reviewer": {Exec: []string{filepath.Join(t.TempDir(), "absent")}, Stdin: "none", Reply: "stdout"}}
	s, err := New(Config{Owner: owner, Machine: "box", Registry: reg, QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json"), StateDir: state,
		Dispatch: dispatch.Options{Presets: presets}})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "look"
	sum := sha256.Sum256([]byte(prompt))
	body, _ := json.Marshal(reviewjob.CreateRequest{JobID: "rv-3333333333333333-0", ReviewID: "rv-3333333333333333", Requester: "hub", Preset: "reviewer",
		ExpiresAt: time.Now().Add(time.Hour).UTC(), Prompt: prompt, PromptSHA: hex.EncodeToString(sum[:]),
		Packet: &packet.Packet{Question: "q", Manifest: packet.Manifest{Scope: "none", BaseKind: "none", Changes: []packet.Change{}}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /api/review-jobs HTTP/1.1\r\nHost: 127.0.0.1\r\nTailscale-User-Login: %s\r\nX-Tincan-Request: 1\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", owner, len(body))
	conn.Write(body[:10])
	time.Sleep(200 * time.Millisecond) // the handler is reading the body
	cancel()
	time.Sleep(200 * time.Millisecond) // shutdown has begun
	conn.Write(body[10:])
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("job created during shutdown: %d", resp.StatusCode)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, ok, _ := (reviewjob.Store{Dir: filepath.Join(state, "reviews")}).Get("rv-3333333333333333-0"); ok {
		t.Error("a job was recorded during shutdown")
	}
}

// A request that never finishes cannot hold Run past the shutdown grace.
func TestRunShutdownIsBounded(t *testing.T) {
	defer func(g time.Duration) { shutdownGrace = g }(shutdownGrace)
	shutdownGrace = 100 * time.Millisecond
	s := testServer(t)
	unblock, entered := make(chan struct{}), make(chan struct{})
	defer close(unblock)
	s.mux.HandleFunc("GET /hang", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-unblock
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()
	go func() {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/hang", nil)
		req.Header.Set("Tailscale-User-Login", owner)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung on a request that never finishes")
	}
}
