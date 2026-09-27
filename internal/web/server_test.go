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
