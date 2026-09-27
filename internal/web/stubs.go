// internal/web/stubs.go — deleted piecemeal as Tasks 9–11 implement the real
// versions.
package web

import (
	"context"
	"net/http"
	"sync/atomic"
)

type hub struct{}

func newHub() *hub { return &hub{} }

type peer struct{ online atomic.Bool }

func newPeer(Peer) (*peer, error)                                  { return &peer{}, nil }
func (s *Server) apiRoutes()                                       {}
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {}
func (s *Server) proxyPeer(w http.ResponseWriter, r *http.Request) {}
func (s *Server) loop(ctx context.Context)                         {}
func (s *Server) watchPeers(ctx context.Context)                   {}
