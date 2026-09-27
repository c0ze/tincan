package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

const owner = "owner@example.com"

func testServer(t *testing.T) *Server {
	t.Helper()
	reg := rooms.Open(filepath.Join(t.TempDir(), "rooms.json"))
	s, err := New(Config{Owner: owner, PublicPath: "/tincan", Machine: "testbox", Registry: reg, ChainBudget: 6, Dispatch: dispatch.Options{}})
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
	// t.TempDir() paths can exceed the 104-byte unix socket path limit on
	// macOS; use a short base for the socket directory in this test only.
	// fsutil.MkdirPrivate also rejects a symlinked ancestor (macOS /tmp is a
	// symlink to /private/tmp), so resolve it to its canonical form too.
	base, err := os.MkdirTemp("/tmp", "tw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "run")
	sock := filepath.Join(dir, "web.sock")
	ln, err = Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for path, want := range map[string]os.FileMode{dir: 0o700, sock: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode %v %v", path, info.Mode().Perm(), err)
		}
	}
	// A stale socket from a crashed run is replaced.
	ln.Close()
	ln2, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln2.Close()
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
