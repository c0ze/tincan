// internal/web/proxy.go
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// peerResponseHeaderTimeout bounds how long the hub waits for a peer's
// response headers (SSE bodies stream on afterwards).
const peerResponseHeaderTimeout = 30 * time.Second

type peer struct {
	name   string
	base   *url.URL
	online atomic.Bool
	proxy  *httputil.ReverseProxy
	client *http.Client
	rpc    *http.Client // machine-link calls; per-call deadlines
}

func newPeer(p Peer) (*peer, error) {
	base, err := url.Parse(p.URL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return nil, errors.New("peer " + p.Name + ": URL must be http(s)://host/path/")
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: peerResponseHeaderTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	pp := &peer{name: p.Name, base: base, client: &http.Client{Transport: transport, Timeout: 5 * time.Second}, rpc: &http.Client{Transport: transport}}
	pp.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // stream SSE
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := pr.In.PathValue("rest")
			pr.Out.URL = base.ResolveReference(&url.URL{Path: "api/" + rest, RawQuery: pr.In.URL.RawQuery})
			pr.Out.Host = base.Host
			for h := range pr.Out.Header {
				if strings.HasPrefix(h, "Tailscale-") {
					pr.Out.Header.Del(h)
				}
			}
			for _, h := range []string{"Origin", "Cookie", "Referer", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Authorization", "X-Real-Ip"} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-Tincan-Request", "1")
		},
		// ErrorHandler never flips the health flag: it fires for every
		// round-trip error, including a client-cancelled fetch or a slow
		// endpoint hitting ResponseHeaderTimeout — neither means the peer
		// is down, and checkPeers is the sole owner of online. It's quiet
		// on cancellation (the client going away isn't a peer problem)
		// and otherwise logs to stderr and replies with the API's usual
		// JSON error shape.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) && r.Context().Err() == nil {
				fmt.Fprintf(os.Stderr, "tincan web: peer %s: proxy error: %v\n", p.Name, err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": "peer " + p.Name + " is unreachable"})
		},
	}
	return pp, nil
}

func (s *Server) proxyPeer(w http.ResponseWriter, r *http.Request) {
	p, ok := s.peers[r.PathValue("peer")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	rest := r.PathValue("rest")
	if strings.HasPrefix(rest, "/") || strings.Contains(rest, "//") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." || seg == "." {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}
	if strings.HasPrefix(strings.TrimLeft(rest, "/"), "peers/") {
		http.Error(w, "peers are not chained", http.StatusBadRequest)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

func (s *Server) checkPeers(ctx context.Context) {
	for _, p := range s.peers {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", p.base.ResolveReference(&url.URL{Path: "api/self"}).String(), nil)
		resp, err := p.client.Do(req)
		online := err == nil && resp.StatusCode == http.StatusOK
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		cancel()
		if p.online.Swap(online) != online {
			s.hub.publish(note{Kind: "peer"})
		}
	}
}

func (s *Server) watchPeers(ctx context.Context) {
	if len(s.peers) == 0 {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.checkPeers(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
