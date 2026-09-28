package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// PeerError is a non-2xx answer from a peer's machine-link API.
type PeerError struct {
	Status int
	Msg    string
}

func (e *PeerError) Error() string { return fmt.Sprintf("peer answered %d: %s", e.Status, e.Msg) }

// IsTransient reports whether a peer call may succeed if retried: transport
// failures and 5xx answers (committees §6.4).
func IsTransient(err error) bool {
	var pe *PeerError
	if errors.As(err, &pe) {
		return pe.Status >= 500
	}
	return err != nil
}

// call sends one machine-link request to the peer's public URL (committees
// §6.7). tailscale serve on the peer attaches this device owner's identity,
// so the peer's Host, forwarded-Host, owner and Funnel checks apply
// unchanged; the client adds only X-Tincan-Request and never forwards
// browser or identity headers. Without a caller deadline it allows 10 s.
func (p *peer) call(ctx context.Context, method, path string, in any, limit int64, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	u := p.base.ResolveReference(&url.URL{Path: "api/" + path})
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-Tincan-Request", "1")
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.rpc.Do(req)
	if err != nil {
		return fmt.Errorf("peer %s: %w", p.name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("peer %s: %w", p.name, err)
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("peer %s: response exceeds %d bytes", p.name, limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := string(data)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &PeerError{Status: resp.StatusCode, Msg: msg}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("peer %s: %w", p.name, err)
		}
	}
	return nil
}
