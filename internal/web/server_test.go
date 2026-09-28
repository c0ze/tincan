package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

const owner = "owner@example.com"

// testHosts allows httptest.NewRequest's default Host, example.com.
var testHosts = []string{"example.com"}

func testServer(t *testing.T) *Server {
	t.Helper()
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	qdir := t.TempDir()
	s, err := New(Config{Owner: owner, AllowedHosts: testHosts, PublicPath: "/tincan", Machine: "testbox", Registry: reg, ChainBudget: 6, Dispatch: dispatch.Options{}, QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ownerHdr() map[string]string { return map[string]string{"Tailscale-User-Login": owner} }

// shortSocketDir returns a short, symlink-free base directory for unix
// socket tests. t.TempDir() lives under macOS's long
// /var/folders/.../T/<test name>/... hierarchy, which combined with a
// nested socket path can exceed the 104-byte sockaddr_un limit; /tmp itself
// is a symlink to /private/tmp on macOS, which fsutil.MkdirPrivate rejects
// as an ancestor, so resolve it to its canonical form too.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "tw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func TestOwnerCheck(t *testing.T) {
	h := testServer(t).Handler()
	if rec := do(t, h, "GET", "/api/self", "", nil); rec.Code != 403 {
		t.Fatalf("no identity: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/self", "", map[string]string{"Tailscale-User-Login": "intruder@example.com"}); rec.Code != 403 {
		t.Fatalf("wrong identity: %d", rec.Code)
	}
	rec := do(t, h, "GET", "/api/self", "", ownerHdr())
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"machine":"testbox"`) {
		t.Fatalf("owner: %d %s", rec.Code, rec.Body)
	}
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy"} {
		if rec.Header().Get(k) == "" {
			t.Errorf("missing header %s", k)
		}
	}
}

func TestMutationsNeedCSRFHeaderAndSameOrigin(t *testing.T) {
	h := testServer(t).Handler()
	hdr := ownerHdr()
	if rec := do(t, h, "POST", "/api/rooms", `{"path":"/nope"}`, hdr); rec.Code != 403 {
		t.Fatalf("without X-Tincan-Request: %d", rec.Code)
	}
	hdr["X-Tincan-Request"] = "1"
	hdr["Origin"] = "https://evil.example"
	if rec := do(t, h, "POST", "/api/rooms", `{"path":"/nope"}`, hdr); rec.Code != 403 {
		t.Fatalf("foreign origin: %d", rec.Code)
	}
	hdr["Origin"] = "http://example.com" // httptest's default Host
	if rec := do(t, h, "POST", "/api/rooms", `{"path":"/nope"}`, hdr); rec.Code == 403 {
		t.Fatalf("same origin refused: %s", rec.Body)
	}
}

func TestIndexUsesPublicPath(t *testing.T) {
	h := testServer(t).Handler()
	rec := do(t, h, "GET", "/", "", ownerHdr())
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.Contains(string(body), `content="/tincan/"`) || !strings.Contains(string(body), `src="/tincan/app.js"`) {
		t.Fatalf("index: %d %s", rec.Code, body)
	}
}

func TestListenRefusesNonLoopbackTCPAndSecuresSocket(t *testing.T) {
	if _, err := Listen("0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback TCP accepted")
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if runtime.GOOS == "windows" {
		return
	}
	base := shortSocketDir(t)
	dir := filepath.Join(base, "run")
	sock := filepath.Join(dir, "web.sock")
	ln, err = Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, sock: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode %v want %v", path, info.Mode().Perm(), want)
		}
	}
	// While the listener is still live, a second Listen on the same path
	// must refuse rather than steal or remove the socket.
	if _, err := Listen("unix:" + sock); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("listener still open: got %v, want an \"in use\" error", err)
	}
	// A stale socket left behind by a crashed run (no listener attached, but
	// the socket file itself still on disk) is detected and replaced. Disable
	// unlink-on-close so Close leaves the file behind, simulating the crash.
	unixLn, ok := ln.(*net.UnixListener)
	if !ok {
		t.Fatalf("Listen(unix:...) returned %T, want *net.UnixListener", ln)
	}
	unixLn.SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket file missing after close: %v", err)
	}
	ln2, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln2.Close()
}

func TestListenRefusesDestructivePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-socket only")
	}
	t.Run("relative path refused", func(t *testing.T) {
		if _, err := Listen("unix:relative/web.sock"); err == nil {
			t.Fatal("relative path accepted")
		}
	})
	t.Run("regular file at socket path is left alone", func(t *testing.T) {
		base := shortSocketDir(t)
		path := filepath.Join(base, "notes.txt")
		if err := os.WriteFile(path, []byte("do not delete me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Listen("unix:" + path); err == nil {
			t.Fatal("non-socket file accepted")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "do not delete me" {
			t.Fatalf("file was removed or altered: %q %v", data, err)
		}
	})
	t.Run("group/other-accessible parent dir refused, mode unchanged", func(t *testing.T) {
		base := shortSocketDir(t)
		dir := filepath.Join(base, "run")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Listen("unix:" + filepath.Join(dir, "web.sock")); err == nil {
			t.Fatal("wide-open parent dir accepted")
		}
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("parent dir mode changed: %v %v", info.Mode().Perm(), err)
		}
	})
	t.Run("missing parent dir is created 0700", func(t *testing.T) {
		base := shortSocketDir(t)
		dir := filepath.Join(base, "fresh")
		ln, err := Listen("unix:" + filepath.Join(dir, "web.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("fresh dir mode %v %v", info.Mode().Perm(), err)
		}
	})
}

func TestDetectOwnerWrapsCLIFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shebang script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho not running >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := DetectOwner(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not running") || !strings.Contains(err.Error(), "--owner") {
		t.Fatalf("got %v, want an error mentioning \"not running\" and \"--owner\"", err)
	}
}

func TestNewValidatesPublicPath(t *testing.T) {
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	base := Config{Owner: owner, Machine: "testbox", Registry: reg, ChainBudget: 6}
	for _, bad := range []string{"/\\evil.com", "/a b"} {
		cfg := base
		cfg.PublicPath = bad
		if _, err := New(cfg); err == nil {
			t.Errorf("public path %q accepted", bad)
		}
	}
	good := base
	good.PublicPath = "/tincan"
	if _, err := New(good); err != nil {
		t.Errorf("public path %q rejected: %v", good.PublicPath, err)
	}
}

func TestParseOwnerFromStatusJSON(t *testing.T) {
	got, err := parseOwner([]byte(`{"Self":{"UserID":42},"User":{"42":{"LoginName":"me@example.com"}}}`))
	if err != nil || got != "me@example.com" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := parseOwner([]byte(`{"Self":{"UserID":1},"User":{}}`)); err == nil {
		t.Fatal("missing user accepted")
	}
}

func doHost(t *testing.T, h http.Handler, method, path, host string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(`{"path":"/nope"}`))
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func hostServer(t *testing.T, origin string, allowed ...string) http.Handler {
	t.Helper()
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	qdir := t.TempDir()
	s, err := New(Config{Owner: owner, Machine: "testbox", Registry: reg, Origin: origin, AllowedHosts: allowed, QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json")})
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func wantForbiddenJSON(t *testing.T, rec *httptest.ResponseRecorder, msg, what string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s: got %d %s, want 403", what, rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s: content-type %q, want application/json", what, ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	decode(t, rec.Body.String(), &body)
	if body.Error != msg {
		t.Fatalf("%s: error %q, want %q", what, body.Error, msg)
	}
}

// A DNS-rebinding page on attacker.example resolves to 127.0.0.1 and sends
// same-origin requests with forged identity and CSRF headers; only the Host
// header gives it away, so every method must be refused on it.
func TestHostAllowlistRefusesRebindingHosts(t *testing.T) {
	h := hostServer(t, "", "node.tailnet.ts.net")
	evil := mut()
	evil["Origin"] = "http://attacker.example:7788"
	wantForbiddenJSON(t, doHost(t, h, "GET", "/api/self", "attacker.example:7788", ownerHdr()), "host not allowed", "GET foreign host")
	wantForbiddenJSON(t, doHost(t, h, "POST", "/api/rooms", "attacker.example:7788", evil), "host not allowed", "POST foreign host")
	wantForbiddenJSON(t, doHost(t, h, "GET", "/api/self", "node.tailnet.ts.net.evil.example", ownerHdr()), "host not allowed", "suffix trick")
	wantForbiddenJSON(t, doHost(t, h, "GET", "/api/self", "", ownerHdr()), "host not allowed", "empty host")

	fwd := ownerHdr()
	fwd["X-Forwarded-Host"] = "evil.example"
	wantForbiddenJSON(t, doHost(t, h, "GET", "/api/self", "127.0.0.1:7788", fwd), "host not allowed", "foreign X-Forwarded-Host")

	for _, host := range []string{"127.0.0.1:7788", "127.0.0.1", "localhost:7788", "LOCALHOST", "[::1]:7788", "node.tailnet.ts.net", "Node.Tailnet.ts.net:443", "node.tailnet.ts.net."} {
		if rec := doHost(t, h, "GET", "/api/self", host, ownerHdr()); rec.Code != 200 {
			t.Errorf("host %q refused: %d %s", host, rec.Code, rec.Body)
		}
	}
	fwd["X-Forwarded-Host"] = "node.tailnet.ts.net"
	if rec := doHost(t, h, "GET", "/api/self", "127.0.0.1:7788", fwd); rec.Code != 200 {
		t.Fatalf("allowed X-Forwarded-Host refused: %d %s", rec.Code, rec.Body)
	}
	// A loopback-hosted mutation with a matching Origin still works.
	ok := mut()
	ok["Origin"] = "http://127.0.0.1:7788"
	if rec := doHost(t, h, "POST", "/api/rooms", "127.0.0.1:7788", ok); rec.Code == 403 {
		t.Fatalf("loopback mutation refused: %s", rec.Body)
	}
}

func TestHostAllowlistIncludesPinnedOrigin(t *testing.T) {
	h := hostServer(t, "https://tincan.example:8443")
	if rec := doHost(t, h, "GET", "/api/self", "tincan.example:8443", ownerHdr()); rec.Code != 200 {
		t.Fatalf("origin host refused: %d %s", rec.Code, rec.Body)
	}
	if rec := doHost(t, h, "GET", "/api/self", "TINCAN.example:8443", ownerHdr()); rec.Code != 200 {
		t.Fatalf("origin host (case) refused: %d %s", rec.Code, rec.Body)
	}
	wantForbiddenJSON(t, doHost(t, h, "GET", "/api/self", "tincan.example:9999", ownerHdr()), "host not allowed", "origin host, other port")
	wantForbiddenJSON(t, doHost(t, h, "GET", "/api/self", "node.tailnet.ts.net", ownerHdr()), "host not allowed", "undetected DNS name")
}

func TestOriginValidatedAndDefaultPortDropped(t *testing.T) {
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	for _, bad := range []string{"tincan.example", "ftp://tincan.example", "https://"} {
		if _, err := New(Config{Owner: owner, Registry: reg, Origin: bad}); err == nil {
			t.Errorf("origin %q accepted", bad)
		}
	}
	h := hostServer(t, "https://tincan.example:443")
	if rec := doHost(t, h, "GET", "/api/self", "tincan.example", ownerHdr()); rec.Code != 200 {
		t.Fatalf("default-port origin host refused: %d %s", rec.Code, rec.Body)
	}
}

func TestSelfListsPeersSortedByName(t *testing.T) {
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	qdir := t.TempDir()
	var peers []Peer
	for _, n := range []string{"zeta", "alpha", "mid", "beta"} {
		peers = append(peers, Peer{Name: n, URL: "https://" + n + ".example/tincan/"})
	}
	s, err := New(Config{Owner: owner, AllowedHosts: testHosts, Machine: "box", Registry: reg, Peers: peers, QuotaDir: qdir, QuotaConfig: filepath.Join(qdir, "quotas.json")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // map iteration order varies between calls
		rec := do(t, s.Handler(), "GET", "/api/self", "", ownerHdr())
		var self struct {
			Peers []struct {
				Name string `json:"name"`
			} `json:"peers"`
		}
		decode(t, rec.Body.String(), &self)
		var names []string
		for _, p := range self.Peers {
			names = append(names, p.Name)
		}
		if strings.Join(names, ",") != "alpha,beta,mid,zeta" {
			t.Fatalf("peers %v", names)
		}
	}
}

// Stop's wait must end before the hub's proxy gives up on the response
// headers, or a Stop through the hub reports a 502 instead of the thread.
func TestStopWaitIsBelowProxyHeaderTimeout(t *testing.T) {
	if stopWait != 25*time.Second || stopWait >= peerResponseHeaderTimeout {
		t.Fatalf("stopWait %v, proxy ResponseHeaderTimeout %v", stopWait, peerResponseHeaderTimeout)
	}
}

func TestFunnelRequestsRefused(t *testing.T) {
	h := hostServer(t, "", "node.tailnet.ts.net")
	hdr := ownerHdr()
	hdr["Tailscale-Funnel-Request"] = "?1"
	wantForbiddenJSON(t, doHost(t, h, "GET", "/api/self", "node.tailnet.ts.net", hdr), "funnel requests are refused", "funnel")
}

func TestRefusalsAreJSON(t *testing.T) {
	h := testServer(t).Handler()
	wantForbiddenJSON(t, do(t, h, "GET", "/api/self", "", nil), "forbidden", "no identity")
	wantForbiddenJSON(t, do(t, h, "POST", "/api/rooms", `{}`, ownerHdr()), "missing X-Tincan-Request", "no CSRF header")
	hdr := mut()
	hdr["Origin"] = "https://evil.example"
	wantForbiddenJSON(t, do(t, h, "POST", "/api/rooms", `{}`, hdr), "cross-origin request refused", "foreign origin")
}

func TestParseNodeFromStatusJSON(t *testing.T) {
	login, dns, err := parseNode([]byte(`{"Self":{"UserID":42,"DNSName":"cachyos.brill-decibel.ts.net."},"User":{"42":{"LoginName":"me@example.com"}}}`))
	if err != nil || login != "me@example.com" || dns != "cachyos.brill-decibel.ts.net" {
		t.Fatalf("%q %q %v", login, dns, err)
	}
}

func TestDetectNodeReadsDNSName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix shebang script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "tailscale")
	status := `{"Self":{"UserID":7,"DNSName":"box.tailnet.ts.net."},"User":{"7":{"LoginName":"me@example.com"}}}`
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho '"+status+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	login, dns, err := DetectNode(context.Background())
	if err != nil || login != "me@example.com" || dns != "box.tailnet.ts.net" {
		t.Fatalf("%q %q %v", login, dns, err)
	}
	if got, err := DetectOwner(context.Background()); err != nil || got != "me@example.com" {
		t.Fatalf("DetectOwner %q %v", got, err)
	}
}
