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

func newPeer(Peer) (*peer, error) { return &peer{}, nil }

// note and hub.publish are placeholders for Task 10's event stream; api.go
// calls publish to notify subscribers of thread/message changes.
type note struct{ Kind, Room, Thread string }

func (h *hub) publish(note) {}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {}
func (s *Server) proxyPeer(w http.ResponseWriter, r *http.Request) {}
func (s *Server) loop(ctx context.Context)                         {}
func (s *Server) watchPeers(ctx context.Context)                   {}
