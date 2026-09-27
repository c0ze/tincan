// internal/web/proxy.go
package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type peer struct {
	name   string
	base   *url.URL
	online atomic.Bool
	proxy  *httputil.ReverseProxy
	client *http.Client
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
		ResponseHeaderTimeout: 30 * time.Second,
	}
	pp := &peer{name: p.Name, base: base, client: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	pp.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // stream SSE
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := pr.In.PathValue("rest")
			pr.Out.URL = base.ResolveReference(&url.URL{Path: "api/" + rest, RawQuery: pr.In.URL.RawQuery})
			pr.Out.Host = base.Host
			for _, h := range []string{"Origin", "Cookie", "Referer", "Tailscale-User-Login", "Tailscale-User-Name", "Tailscale-User-Profile-Pic", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-Tincan-Request", "1")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			pp.online.Store(false)
			http.Error(w, "peer "+p.Name+" is offline", http.StatusBadGateway)
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
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." || seg == "." {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}
	if strings.HasPrefix(rest, "peers/") {
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
