// Package web serves the tincan chat UI and its JSON API. It trusts only the
// owner's Tailscale identity as set by `tailscale serve`.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/buildinfo"
	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/quota"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/thread"
)

//go:embed ui
var uiFS embed.FS

// publicPathRe restricts Config.PublicPath (after trimming leading/trailing
// slashes) to a safe set of URL-path characters — no spaces, backslashes, or
// other characters that could confuse a proxy or client into treating the
// path as something other than a plain literal prefix.
var publicPathRe = regexp.MustCompile(`^[A-Za-z0-9/_.-]*$`)

type Peer struct {
	Name string
	URL  string
}

type Config struct {
	PublicPath string // URL prefix the browser sees, e.g. "/tincan"
	Owner      string // required Tailscale-User-Login
	Origin     string // optional pinned origin for mutations; its host is also allowed as Host
	// AllowedHosts lists the Host values accepted besides loopback and the
	// Origin host: this node's Tailscale DNS name and any extra hosts. An
	// entry without a port matches any port; "host:port" matches exactly.
	AllowedHosts []string
	Machine      string
	Peers        []Peer
	ChainBudget  int
	IdleStop     time.Duration
	Tick         time.Duration
	Registry     *rooms.Registry
	Dispatch     dispatch.Options // Executable and Presets; Room is set per room
	QuotaDir     string           // default quota.DefaultCacheDir()
	QuotaConfig  string           // default quota.DefaultConfigPath()
	// StateDir is the shared tincan state directory (rooms.StateDir()).
	// Empty disables the heartbeat and committee storage (tests).
	StateDir string
	// CommitteesFrom names the peer that hosts committee definitions; ""
	// makes this machine the hub.
	CommitteesFrom string
}

type Server struct {
	cfg   Config
	hosts []hostRule // Host allowlist besides loopback
	base  string     // public path with trailing slash
	mux   *http.ServeMux
	index *template.Template
	hub   *hub
	peers map[string]*peer

	mu            sync.Mutex
	dispatchers   map[string]*thread.Dispatcher // room ID → owned dispatcher
	inFlight      map[string]bool               // room ID → a reconcile/janitor pass is running
	lastLog       map[string]string             // dedup key → last message logged to stderr
	lastJanitor   time.Time                     // touched only by tick
	lastQuotaNote time.Time                     // touched only by tick
	wg            sync.WaitGroup                // tracks room passes spawned by tick, for a clean shutdown
	started       time.Time                     // for the heartbeat
	lastHeartbeat time.Time                     // touched only by tick

	// reconcileRoom runs one room's Reconcile-and-Janitor pass; it defaults
	// to runRoomPass and is overridden in tests to exercise tick's
	// concurrency without a real dispatcher.
	reconcileRoom func(ctx context.Context, room rooms.Room, janitor bool)

	// createMu serializes thread creation across all rooms, closing the
	// check-then-act race between the client_id dedup lookup and
	// thread.Create in createThread (thread.Create always makes a fresh
	// thread directory, so two concurrent requests for the same client_id
	// would otherwise both see "not found" and both create one).
	createMu sync.Mutex

	jobs *reviewjob.Service // reviewer jobs; nil without a state directory

	committeesMu       sync.Mutex // serializes peer committee-cache refreshes
	committeesAttempt  time.Time  // last hub fetch attempt (guarded by committeesMu)
	committeesAttempts int        // hub fetch attempts, for tests (guarded by committeesMu)
}

func New(cfg Config) (*Server, error) {
	if cfg.Owner == "" {
		return nil, errors.New("owner login is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("room registry is required")
	}
	if cfg.ChainBudget <= 0 {
		cfg.ChainBudget = 6
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 2 * time.Second
	}
	if cfg.IdleStop <= 0 {
		cfg.IdleStop = 30 * time.Minute
	}
	if cfg.QuotaDir == "" {
		cfg.QuotaDir = quota.DefaultCacheDir()
	}
	if cfg.QuotaConfig == "" {
		cfg.QuotaConfig = quota.DefaultConfigPath()
	}
	trimmed := strings.Trim(cfg.PublicPath, "/")
	if !publicPathRe.MatchString(trimmed) {
		return nil, fmt.Errorf("invalid --public-path %q", cfg.PublicPath)
	}
	base := "/" + trimmed + "/"
	if base == "//" {
		base = "/"
	}
	var hosts []hostRule
	if cfg.Origin != "" {
		u, err := url.Parse(cfg.Origin)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("invalid --origin %q: want scheme://host[:port]", cfg.Origin)
		}
		r := parseHostRule(u.Host)
		if (u.Scheme == "https" && r.port == "443") || (u.Scheme == "http" && r.port == "80") {
			r.port = "" // browsers omit the default port from Host
		}
		hosts = append(hosts, r)
	}
	for _, h := range cfg.AllowedHosts {
		if r := parseHostRule(h); r.name != "" {
			hosts = append(hosts, r)
		}
	}
	idx, err := template.ParseFS(uiFS, "ui/index.html")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, hosts: hosts, base: base, mux: http.NewServeMux(), index: idx, hub: newHub(), peers: map[string]*peer{}, dispatchers: map[string]*thread.Dispatcher{}, inFlight: map[string]bool{}, lastLog: map[string]string{}, started: time.Now()}
	s.reconcileRoom = s.runRoomPass
	for _, p := range cfg.Peers {
		pp, err := newPeer(p)
		if err != nil {
			return nil, err
		}
		s.peers[p.Name] = pp
	}
	if cfg.CommitteesFrom != "" {
		if _, ok := s.peers[cfg.CommitteesFrom]; !ok {
			return nil, fmt.Errorf("--committees-from %q is not a configured --peer", cfg.CommitteesFrom)
		}
	}
	s.routes()
	s.initJobs()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.serveIndex)
	s.mux.HandleFunc("GET /app.js", s.serveAsset("ui/app.js", "text/javascript; charset=utf-8"))
	s.mux.HandleFunc("GET /app.css", s.serveAsset("ui/app.css", "text/css; charset=utf-8"))
	s.mux.HandleFunc("GET /api/self", s.apiSelf)
	s.apiRoutes()                                                // Task 9
	s.mux.HandleFunc("GET /api/events", s.apiEvents)             // Task 10
	s.mux.HandleFunc("/api/peers/{peer}/{rest...}", s.proxyPeer) // Task 11
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.index.Execute(w, map[string]string{"Base": s.base})
}

func (s *Server) serveAsset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := uiFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(data)
	}
}

func (s *Server) apiSelf(w http.ResponseWriter, r *http.Request) {
	type peerView struct {
		Name   string `json:"name"`
		Online bool   `json:"online"`
	}
	peers := []peerView{}
	for name, p := range s.peers {
		peers = append(peers, peerView{Name: name, Online: p.online.Load()})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	s.writeJSON(w, http.StatusOK, map[string]any{"machine": s.cfg.Machine, "version": buildinfo.Current().Version, "peers": peers})
}

// hostRule is one allowed Host value, lowercased with any trailing dot
// removed. An empty port matches any port.
type hostRule struct{ name, port string }

func parseHostRule(h string) hostRule {
	h = strings.ToLower(strings.TrimSpace(h))
	var r hostRule
	if name, port, err := net.SplitHostPort(h); err == nil {
		r = hostRule{name, port}
	} else {
		r.name = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	}
	r.name = strings.TrimSuffix(r.name, ".")
	return r
}

// hostAllowed reports whether a Host (or X-Forwarded-Host) value names this
// server: a loopback literal on any port, or an allowlisted host. It is the
// DNS-rebinding defence: a rebound page's requests carry its own hostname.
func (s *Server) hostAllowed(host string) bool {
	h := parseHostRule(host)
	switch h.name {
	case "":
		return false
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	for _, r := range s.hosts {
		if r.name == h.name && (r.port == "" || r.port == h.port) {
			return true
		}
	}
	return false
}

func (s *Server) hostsOK(r *http.Request) bool {
	if !s.hostAllowed(r.Host) {
		return false
	}
	for _, v := range r.Header.Values("X-Forwarded-Host") {
		for _, h := range strings.Split(v, ",") {
			if !s.hostAllowed(h) {
				return false
			}
		}
	}
	return true
}

// Handler wraps every route with security headers and the Funnel, Host,
// owner and CSRF checks, in that order.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		refuse := func(msg string) { s.fail(w, http.StatusForbidden, errors.New(msg)) }
		if _, funnel := r.Header["Tailscale-Funnel-Request"]; funnel {
			refuse("funnel requests are refused")
			return
		}
		if !s.hostsOK(r) {
			refuse("host not allowed")
			return
		}
		if r.Header.Get("Tailscale-User-Login") != s.cfg.Owner {
			refuse("forbidden")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Tincan-Request") != "1" {
				refuse("missing X-Tincan-Request")
				return
			}
			if o := r.Header.Get("Origin"); o != "" && !s.originOK(o, r) {
				refuse("cross-origin request refused")
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

// originOK compares a mutation's Origin with the pinned --origin or, when
// none is set, with the request's host as the client saw it; Handler has
// already checked that host (and X-Forwarded-Host) against the allowlist.
func (s *Server) originOK(origin string, r *http.Request) bool {
	if s.cfg.Origin != "" {
		return strings.EqualFold(strings.TrimRight(origin, "/"), strings.TrimRight(s.cfg.Origin, "/"))
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return strings.EqualFold(u.Host, host)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	s.writeJSON(w, status, map[string]string{"error": err.Error()})
}

// readJSON decodes the JSON request body into v, capping the body at limit
// bytes via http.MaxBytesReader rather than truncating it outright: JSON
// escaping can grow a legal payload well past its decoded size (\n, \t, \",
// \\ cost 2 bytes each; other control characters cost 6), so a hard byte cap
// on the raw body must leave headroom for that, and callers size limit
// accordingly (see postMessage).
func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// failBody classifies a readJSON error: an oversized body is reported as 413
// (distinct from a merely malformed one, which is a client bug and stays
// 400), detected via the *http.MaxBytesError sentinel readJSON's underlying
// reader produces once the cap is exceeded.
func (s *Server) failBody(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		s.fail(w, http.StatusRequestEntityTooLarge, errors.New("request body too large"))
		return
	}
	s.fail(w, http.StatusBadRequest, err)
}

// DefaultListen is the platform default listen address.
func DefaultListen() string {
	if runtime.GOOS == "windows" {
		return "127.0.0.1:7788"
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return "unix:" + filepath.Join(d, "tincan", "web.sock")
	}
	return "unix:" + filepath.Join(rooms.StateDir(), "web.sock")
}

// Listen opens a private unix socket ("unix:<path>") or a loopback TCP
// address. Non-loopback TCP is refused: remote access is via tailscale serve.
func Listen(spec string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(spec, "unix:"); ok {
		if path == "" || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("refusing unix socket path %q: must be an absolute path", path)
		}
		dir := filepath.Dir(path)
		switch dirInfo, err := os.Lstat(dir); {
		case os.IsNotExist(err):
			if err := fsutil.MkdirPrivate(dir); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		default:
			if !dirInfo.IsDir() {
				return nil, fmt.Errorf("%s is not a directory", dir)
			}
			if dirInfo.Mode().Perm()&0o077 != 0 {
				return nil, fmt.Errorf("refusing unix socket directory %s: mode %o allows group or other access", dir, dirInfo.Mode().Perm())
			}
		}
		switch info, err := os.Lstat(path); {
		case err == nil:
			if info.Mode()&os.ModeSocket == 0 {
				return nil, fmt.Errorf("refusing to remove %s: not a socket", path)
			}
			if c, dialErr := net.Dial("unix", path); dialErr == nil {
				c.Close()
				return nil, fmt.Errorf("%s is in use by another tincan web", path)
			}
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// Nothing to remove.
		default:
			return nil, err
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			ln.Close()
			return nil, err
		}
		return ln, nil
	}
	host, _, err := net.SplitHostPort(spec)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("refusing to listen on %s: use a unix socket or a loopback address with tailscale serve", spec)
	}
	return net.Listen("tcp", spec)
}

// Run serves until ctx is done, running the dispatch loop and peer checks.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	go s.loop(ctx)       // Task 10
	go s.watchPeers(ctx) // Task 11
	go s.syncCommittees(ctx)
	go s.runJobJanitor(ctx)
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	defer func() {
		if s.cfg.StateDir != "" {
			rooms.RemoveHeartbeat(s.cfg.StateDir, os.Getpid())
		}
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
