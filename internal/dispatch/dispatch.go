// Package dispatch is the single path by which tincan front ends (the MCP
// server and the web chat) submit work and start listeners, so routing,
// interactive-listener detection and idempotency stay identical.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
)

type Options struct {
	Room       string                 // canonical room directory
	Executable string                 // tincan executable for detached hosts; "" = current
	Presets    map[string]host.Preset // nil loads the user's effective presets
}

func (o Options) PresetMap() (map[string]host.Preset, error) {
	if o.Presets != nil {
		return o.Presets, nil
	}
	return host.Effective(host.ConfigPath())
}

type LaunchResult struct {
	State   host.State
	Already bool
}

// Launch starts or reconnects listener name. preset selects the configuration
// ("" means the name itself, or the live listener's own preset).
func Launch(ctx context.Context, o Options, name, preset, sessionMode string) (LaunchResult, error) {
	if err := spool.ValidName(name); err != nil {
		return LaunchResult{}, err
	}
	if st, alive := host.Existing(ctx, o.Room, name); alive && preset == "" {
		if sessionMode == "" {
			return LaunchResult{State: st, Already: true}, nil
		}
		// An alias keeps its existing provider when only the session mode is
		// specified. Up still rejects changing a live configuration.
		if st.Preset != "" {
			preset = st.Preset
		}
	}
	label, p, err := o.resolve(name, preset, sessionMode)
	if err != nil {
		return LaunchResult{}, err
	}
	res, err := host.Up(ctx, host.UpOptions{Room: o.Room, Name: name, Label: label, Preset: p, Wait: 10 * time.Second, Executable: o.Executable})
	if err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{State: res.State, Already: res.Already}, nil
}

// resolve picks the configuration a new listener would start with.
func (o Options) resolve(name, preset, sessionMode string) (string, host.Preset, error) {
	all, err := o.PresetMap()
	if err != nil {
		return "", host.Preset{}, err
	}
	label, p, err := host.Resolve(all, name, preset, host.Overrides{ExecTimeoutSec: -1})
	if err != nil {
		return "", host.Preset{}, err
	}
	p, err = host.WithSession(p, label, sessionMode)
	if err != nil {
		return "", host.Preset{}, err
	}
	return label, p, nil
}

type SendSpec struct {
	Agent     string // listener name
	Preset    string // preset used if the listener must be launched; "" = Agent
	From      string // sender name; "" = "mcp"
	Body      string
	RequestID string // optional idempotency key
}

// Send submits a durable request and launches its hosted listener when absent.
// A saved request stays retrievable when the launch fails. A blank prompt, or
// new work for an agent that is neither running nor configured, is refused
// before anything is saved.
func Send(ctx context.Context, o Options, s SendSpec) (request.Record, error) {
	if s.From == "" {
		s.From = "mcp"
	}
	if err := spool.ValidName(s.Agent); err != nil {
		return request.Record{}, err
	}
	if err := spool.ValidName(s.From); err != nil {
		return request.Record{}, err
	}
	if s.RequestID != "" {
		if err := request.ValidateID(s.RequestID); err != nil {
			return request.Record{}, err
		}
	}
	if len(s.Body) > request.MaxBodyBytes {
		return request.Record{}, fmt.Errorf("prompt exceeds %d bytes", request.MaxBodyBytes)
	}
	if strings.TrimSpace(s.Body) == "" {
		return request.Record{}, errors.New("prompt is empty")
	}
	// A cached result must remain retrievable even when the worker is stopped,
	// uninstalled, or named with an alias that is not itself a preset.
	saved := false
	if s.RequestID != "" {
		existing, err := request.Get(o.Room, s.RequestID)
		if err == nil && existing.Terminal() {
			return request.Submit(ctx, o.Room, s.Agent, s.From, s.Body, s.RequestID)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return request.Record{}, err
		}
		saved = err == nil
	}
	route, err := request.LockRoute(ctx, o.Room, s.Agent)
	if err != nil {
		return request.Record{}, err
	}
	defer route.Close()
	state, alive := host.Existing(ctx, o.Room, s.Agent)
	interactive := alive && state.Owner == ""
	if !alive {
		interactive, err = request.InteractivePending(ctx, o.Room, s.Agent)
		if err != nil {
			return request.Record{}, err
		}
	}
	// New work for a name that is neither running nor launchable (a typo) is
	// refused before it is saved: nothing could run it but a later listener
	// that happens to take that name. A saved request keeps its recovery:
	// launch the intended listener, then retry or collect the same ID.
	if !alive && !interactive && !saved {
		if _, _, err := o.resolve(s.Agent, s.Preset, ""); err != nil {
			return request.Record{}, fmt.Errorf("no listener %s is running and none can be launched; nothing was saved: %w", s.Agent, err)
		}
	}
	submit := request.Submit
	if interactive {
		submit = request.SubmitInteractive
	}
	r, err := submit(ctx, o.Room, s.Agent, s.From, s.Body, s.RequestID)
	if err != nil {
		return r, err
	}
	if !r.Terminal() && !alive && !interactive {
		if _, err := Launch(ctx, o, s.Agent, s.Preset, ""); err != nil {
			return r, fmt.Errorf("request %s is saved; launch its listener or retry this same request_id: %w", r.ID, err)
		}
	}
	return r, nil
}
