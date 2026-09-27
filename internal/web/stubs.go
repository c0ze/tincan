// internal/web/stubs.go — deleted piecemeal as Task 11 implements the real
// versions.
package web

import (
	"context"
	"net/http"
	"sync/atomic"
)

type peer struct{ online atomic.Bool }

func newPeer(Peer) (*peer, error) { return &peer{}, nil }

func (s *Server) proxyPeer(w http.ResponseWriter, r *http.Request) {}
func (s *Server) watchPeers(ctx context.Context)                   {}
