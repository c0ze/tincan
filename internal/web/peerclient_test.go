package web

import (
	"context"
	"errors"
	"testing"
)

func TestPeerCallPassesThePeersChecks(t *testing.T) {
	hub, last, peerHTTP, _ := peerPair(t)
	p := hub.peers["macmini"]
	var self struct {
		Machine string `json:"machine"`
	}
	if err := p.call(context.Background(), "GET", "self", nil, 1<<20, &self); err != nil || self.Machine != "macmini" {
		t.Fatalf("GET self: %+v %v", self, err)
	}
	// A mutation reaches the handler (400 for a bad body), proving the
	// Host, owner and CSRF checks passed rather than a 403 refusal.
	err := p.call(context.Background(), "POST", "rooms", map[string]string{"path": ""}, 1<<20, nil)
	var pe *PeerError
	if !errors.As(err, &pe) || pe.Status != 400 {
		t.Fatalf("POST rooms: %v", err)
	}
	if last.Header.Get("X-Tincan-Request") != "1" || last.Header.Get("Origin") != "" || last.Header.Get("X-Forwarded-Host") != "" {
		t.Fatalf("headers: %v", last.Header)
	}
	if last.Host != peerHTTP.Listener.Addr().String() {
		t.Fatalf("Host = %q", last.Host)
	}
	if IsTransient(err) {
		t.Fatal("a 400 is not transient")
	}
}

func TestPeerCallCapsResponseAndReportsOffline(t *testing.T) {
	hub, _, peerHTTP, _ := peerPair(t)
	p := hub.peers["macmini"]
	if err := p.call(context.Background(), "GET", "self", nil, 8, nil); err == nil {
		t.Fatal("oversized response accepted")
	}
	peerHTTP.Close()
	err := p.call(context.Background(), "GET", "self", nil, 1<<20, nil)
	if err == nil || !IsTransient(err) {
		t.Fatalf("offline peer: %v", err)
	}
}
