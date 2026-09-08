package host

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
)

// control is kept responsive while the listener is executing a job. Its
// random owner credential identifies one lifetime, without ever signalling
// a process selected from an on-disk PID.
type control struct {
	mu       sync.Mutex
	current  string
	cancel   context.CancelFunc
	stop     context.CancelFunc
	server   *http.Server
	listener net.Listener
	token    string
}

func newControl(owner string, stop context.CancelFunc) (*control, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	c := &control{stop: stop, listener: l, token: envelope.NewID()}
	c.server = &http.Server{ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	c.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+c.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		stopAfterReply := false
		switch r.URL.Path {
		case "/ready":
		case "/stop":
			stopAfterReply = true
		case "/cancel":
			c.mu.Lock()
			if c.current == r.URL.Query().Get("id") && c.cancel != nil {
				c.cancel()
			}
			c.mu.Unlock()
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(owner)))
		io.WriteString(w, owner)
		if stopAfterReply {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			c.stop()
		}
	})
	go c.server.Serve(l)
	return c, nil
}

func (c *control) begin(ctx context.Context, id string) (context.Context, func()) {
	c.mu.Lock()
	jobCtx, cancel := context.WithCancel(ctx)
	c.current, c.cancel = id, cancel
	c.mu.Unlock()
	return jobCtx, func() { c.mu.Lock(); c.current, c.cancel = "", nil; cancel(); c.mu.Unlock() }
}

func (st State) controlCall(ctx context.Context, action string) error {
	if st.Owner == "" || st.ControlToken == "" {
		return errors.New("host has no authenticated control identity")
	}
	host, _, err := net.SplitHostPort(st.ControlAddress)
	if err != nil || host != "127.0.0.1" {
		return errors.New("invalid host control address")
	}
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+st.ControlAddress+action, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+st.ControlToken)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 257))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK || subtle.ConstantTimeCompare(data, []byte(st.Owner)) != 1 {
		return fmt.Errorf("host control identity rejected (%d)", resp.StatusCode)
	}
	return nil
}

// Ready verifies the exact host lifetime, including while it is busy.
func (st State) Ready(ctx context.Context) bool { return st.controlCall(ctx, "/ready") == nil }

// Cancel interrupts the named in-flight job while leaving its host running.
// A caller cancelling queued work must persist its terminal request record
// first, so a job that has not started cannot subsequently execute.
func Cancel(ctx context.Context, room, name, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("request id is required")
	}
	st, ok, err := ReadState(room, name)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("host is not running")
	}
	return st.controlCall(ctx, "/cancel?id="+url.QueryEscape(id))
}
