// Package mcpserver exposes tincan as native MCP tools over a persistent local
// connection. Hosts and requests outlive that connection and can be reattached.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/c0ze/tincan/internal/buildinfo"
	"github.com/c0ze/tincan/internal/host"
	"github.com/c0ze/tincan/internal/request"
	"github.com/c0ze/tincan/internal/spool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Options struct {
	Room       string
	Executable string
	Presets    map[string]host.Preset // nil loads the normal local configuration
}

type service struct{ Options }

// New binds every tool to one existing directory. Tool arguments cannot change
// the room, write agent configuration, or supply an arbitrary exec template.
func New(o Options) (*mcp.Server, error) {
	room, err := filepath.Abs(o.Room)
	if err != nil {
		return nil, err
	}
	room, err = filepath.EvalSymlinks(room)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(room)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("MCP room must be a directory")
	}
	o.Room = room
	s := &service{Options: o}
	server := mcp.NewServer(&mcp.Implementation{Name: "tincan", Version: buildinfo.Current().Version}, &mcp.ServerOptions{
		Instructions: "Coordinate local coding agents in room " + room + ". Send starts a configured agent automatically and returns a durable request_id. Use wait with that same ID until terminal; a timeout is not a reason to resubmit. Explicit request_id makes submissions idempotent. Conversation sessions survive calls and reconnects for supported presets. Cancel stops one request; stop ends a listener; reset stops it and clears its conversation. Hosts continue after the MCP connection closes. Agent tasks may read or modify files and use the network under the local preset's permissions.",
	})
	add(server, "tincan_presets", "List configured agent presets, executable availability, and conversation support.", true, true, false, s.presetsTool)
	add(server, "tincan_launch", "Launch or reconnect to a named agent listener in this room. Only locally configured presets are accepted.", false, true, true, s.launchTool)
	add(server, "tincan_send", "Submit a task and immediately return its durable request_id. Starts the listener if necessary. Reuse an explicit request_id only for identical work; collect timeouts with wait.", false, false, true, s.sendTool)
	add(server, "tincan_wait", "Wait up to 30 seconds for an existing request, returning status, result, and incremental progress. Pending work continues after this call or connection ends.", true, true, false, s.waitTool)
	add(server, "tincan_status", "Read listener status or inspect one durable request without waiting. Owner credentials and command arguments are never exposed.", true, true, false, s.statusTool)
	add(server, "tincan_cancel", "Cancel one queued or running request. Earlier file or external changes are not undone. Wait on the same request_id for acknowledgement.", false, true, false, s.cancelTool)
	add(server, "tincan_stop", "Stop a listener and interrupt its current request. Queued requests remain available for a future launch.", false, true, false, s.stopTool)
	add(server, "tincan_reset", "Stop a listener, interrupt any running request, and clear its saved conversation. Its next launch starts a new conversation.", false, true, false, s.resetTool)
	return server, nil
}

func add[I, O any](s *mcp.Server, name, description string, readOnly, idempotent, openWorld bool, handler func(context.Context, *mcp.CallToolRequest, I) (*mcp.CallToolResult, O, error)) {
	destructive := !readOnly
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, IdempotentHint: idempotent, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, handler)
}

func (s *service) presets() (map[string]host.Preset, error) {
	if s.Presets != nil {
		return s.Presets, nil
	}
	return host.Effective(host.ConfigPath())
}

type Empty struct{}
type PresetView struct {
	Name             string `json:"name"`
	Available        bool   `json:"available"`
	SessionSupported bool   `json:"session_supported"`
}
type PresetsOutput struct {
	Presets []PresetView `json:"presets"`
}

func (s *service) presetsTool(ctx context.Context, req *mcp.CallToolRequest, in Empty) (*mcp.CallToolResult, PresetsOutput, error) {
	all, err := s.presets()
	if err != nil {
		return nil, PresetsOutput{}, err
	}
	out := PresetsOutput{Presets: []PresetView{}}
	for _, name := range host.Names(all) {
		p := all[name]
		available := false
		if len(p.Exec) > 0 {
			_, err := exec.LookPath(p.Exec[0])
			available = err == nil
		}
		out.Presets = append(out.Presets, PresetView{Name: name, Available: available, SessionSupported: host.SupportsSessions(name)})
	}
	return nil, out, nil
}

type LaunchInput struct {
	Name        string `json:"name" jsonschema:"Listener name, for example claude or reviewer"`
	Preset      string `json:"preset,omitempty" jsonschema:"Configured preset; defaults to the listener name"`
	SessionMode string `json:"session_mode,omitempty" jsonschema:"persistent or stateless; defaults to persistent for supported presets"`
}

type AgentView struct {
	Name           string `json:"name"`
	Preset         string `json:"preset,omitempty"`
	Alive          bool   `json:"alive"`
	Busy           bool   `json:"busy"`
	Queued         int    `json:"queued"`
	PID            int    `json:"pid,omitempty"`
	CurrentRequest string `json:"current_request,omitempty"`
	SessionMode    string `json:"session_mode,omitempty"`
}
type LaunchOutput struct {
	Agent   AgentView `json:"agent"`
	Already bool      `json:"already"`
}

func (s *service) launch(ctx context.Context, in LaunchInput) (LaunchOutput, error) {
	if err := spool.ValidName(in.Name); err != nil {
		return LaunchOutput{}, err
	}
	if st, alive := host.Existing(ctx, s.Room, in.Name); alive && in.Preset == "" {
		if in.SessionMode == "" {
			return LaunchOutput{Agent: AgentView{Name: in.Name, Preset: st.Preset, Alive: true, Busy: st.State == "busy", PID: st.PID, CurrentRequest: st.CurrentID, SessionMode: st.Session}, Already: true}, nil
		}
		// An alias keeps its existing provider when only the session mode is
		// specified. Up still rejects changing a live configuration.
		if st.Preset != "" {
			in.Preset = st.Preset
		}
	}
	all, err := s.presets()
	if err != nil {
		return LaunchOutput{}, err
	}
	label, p, err := host.Resolve(all, in.Name, in.Preset, host.Overrides{ExecTimeoutSec: -1})
	if err != nil {
		return LaunchOutput{}, err
	}
	p, err = host.WithSession(p, label, in.SessionMode)
	if err != nil {
		return LaunchOutput{}, err
	}
	result, err := host.Up(ctx, host.UpOptions{Room: s.Room, Name: in.Name, Label: label, Preset: p, Wait: 10 * time.Second, Executable: s.Executable})
	if err != nil {
		return LaunchOutput{}, err
	}
	return LaunchOutput{Agent: AgentView{Name: in.Name, Preset: result.State.Preset, Alive: true, Busy: result.State.State == "busy", PID: result.State.PID, CurrentRequest: result.State.CurrentID, SessionMode: result.State.Session}, Already: result.Already}, nil
}

func (s *service) launchTool(ctx context.Context, req *mcp.CallToolRequest, in LaunchInput) (*mcp.CallToolResult, LaunchOutput, error) {
	out, err := s.launch(ctx, in)
	return nil, out, err
}

type SendInput struct {
	Agent     string `json:"agent" jsonschema:"Recipient listener name"`
	Body      string `json:"body" jsonschema:"Task text; maximum 1 MiB"`
	RequestID string `json:"request_id,omitempty" jsonschema:"Optional idempotency key; reuse only for the identical task"`
	From      string `json:"from,omitempty" jsonschema:"Sender name, defaults to mcp"`
}

type RequestView struct {
	RequestID       string          `json:"request_id"`
	Agent           string          `json:"agent"`
	Status          string          `json:"status"`
	Terminal        bool            `json:"terminal"`
	Result          string          `json:"result,omitempty"`
	CancelRequested bool            `json:"cancel_requested,omitempty"`
	Cancellable     bool            `json:"cancellable"`
	Created         time.Time       `json:"created"`
	Updated         time.Time       `json:"updated"`
	Events          []request.Event `json:"events,omitempty"`
	NextCursor      int64           `json:"next_cursor"`
}

func view(r request.Record) RequestView {
	return RequestView{RequestID: r.ID, Agent: r.Agent, Status: r.Status, Terminal: r.Terminal(), Result: r.Result, CancelRequested: r.CancelRequested, Cancellable: !r.Interactive && !r.Terminal(), Created: r.Created, Updated: r.Updated}
}

func (s *service) sendTool(ctx context.Context, req *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, RequestView, error) {
	if in.From == "" {
		in.From = "mcp"
	}
	if err := spool.ValidName(in.Agent); err != nil {
		return nil, RequestView{}, err
	}
	if err := spool.ValidName(in.From); err != nil {
		return nil, RequestView{}, err
	}
	if in.RequestID != "" {
		if err := request.ValidateID(in.RequestID); err != nil {
			return nil, RequestView{}, err
		}
	}
	if len(in.Body) > request.MaxBodyBytes {
		return nil, RequestView{}, fmt.Errorf("prompt exceeds %d bytes", request.MaxBodyBytes)
	}
	// A cached result must remain retrievable even when the worker is stopped,
	// uninstalled, or named with an alias that is not itself a preset.
	if in.RequestID != "" {
		existing, err := request.Get(s.Room, in.RequestID)
		if err == nil && existing.Terminal() {
			r, err := request.Submit(ctx, s.Room, in.Agent, in.From, in.Body, in.RequestID)
			return nil, view(r), err
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, RequestView{}, err
		}
	}
	route, err := request.LockRoute(ctx, s.Room, in.Agent)
	if err != nil {
		return nil, RequestView{}, err
	}
	defer route.Close()
	state, alive := host.Existing(ctx, s.Room, in.Agent)
	interactive := alive && state.Owner == ""
	if !alive {
		interactive, err = request.InteractivePending(ctx, s.Room, in.Agent)
		if err != nil {
			return nil, RequestView{}, err
		}
	}
	submit := request.Submit
	if interactive {
		submit = request.SubmitInteractive
	}
	r, err := submit(ctx, s.Room, in.Agent, in.From, in.Body, in.RequestID)
	if err != nil {
		return nil, view(r), err
	}
	if !r.Terminal() && !alive && !interactive {
		if _, err := s.launch(ctx, LaunchInput{Name: in.Agent}); err != nil {
			return nil, view(r), fmt.Errorf("request %s is saved; launch its listener or retry this same request_id: %w", r.ID, err)
		}
	}
	return nil, view(r), err
}

type WaitInput struct {
	RequestID      string `json:"request_id" jsonschema:"ID returned by tincan_send"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"0 polls immediately; 1–30 waits for completion"`
	Cursor         int64  `json:"cursor,omitempty" jsonschema:"Progress byte cursor from the previous next_cursor; defaults to zero"`
}

func (s *service) waitTool(ctx context.Context, req *mcp.CallToolRequest, in WaitInput) (*mcp.CallToolResult, RequestView, error) {
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 30 {
		return nil, RequestView{}, fmt.Errorf("timeout_seconds must be between 0 and 30")
	}
	r, err := s.waitWithProgress(ctx, req, in)
	if err != nil {
		return nil, RequestView{}, err
	}
	out := view(r)
	out.Events, out.NextCursor, err = request.Progress(s.Room, in.RequestID, in.Cursor, 100)
	return nil, out, err
}

func (s *service) waitWithProgress(ctx context.Context, req *mcp.CallToolRequest, in WaitInput) (request.Record, error) {
	if req.Params.GetProgressToken() == nil || in.TimeoutSeconds == 0 {
		return request.Wait(ctx, s.Room, in.RequestID, time.Duration(in.TimeoutSeconds)*time.Second)
	}
	cursor := in.Cursor
	deadline := time.NewTimer(time.Duration(in.TimeoutSeconds) * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		r, err := request.Poll(ctx, s.Room, in.RequestID)
		if err != nil {
			return r, err
		}
		events, next, err := request.Progress(s.Room, in.RequestID, cursor, 100)
		if err != nil {
			return r, err
		}
		if len(events) > 0 {
			message := events[len(events)-1].Text
			if len(message) > 4096 {
				message = message[:4096]
			}
			if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: req.Params.GetProgressToken(), Progress: float64(next), Message: message}); err != nil {
				return r, err
			}
			cursor = next
		}
		if r.Terminal() {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-deadline.C:
			return request.Get(s.Room, in.RequestID)
		case <-tick.C:
		}
	}
}

type StatusInput struct {
	Name      string `json:"name,omitempty" jsonschema:"Optional listener filter"`
	RequestID string `json:"request_id,omitempty" jsonschema:"Inspect a request instead of listing listeners"`
}
type StatusOutput struct {
	Room    string       `json:"room"`
	Agents  []AgentView  `json:"agents,omitempty"`
	Request *RequestView `json:"request,omitempty"`
}

func (s *service) statusTool(ctx context.Context, req *mcp.CallToolRequest, in StatusInput) (*mcp.CallToolResult, StatusOutput, error) {
	out := StatusOutput{Room: s.Room}
	if in.Name != "" {
		if err := spool.ValidName(in.Name); err != nil {
			return nil, out, err
		}
	}
	if in.RequestID != "" {
		r, err := request.Poll(ctx, s.Room, in.RequestID)
		if err != nil {
			return nil, out, err
		}
		v := view(r)
		out.Request = &v
		return nil, out, nil
	}
	sp, err := spool.Open(s.Room)
	if err != nil {
		return nil, out, err
	}
	presence, err := sp.ListPresence()
	if err != nil {
		return nil, out, err
	}
	states, err := host.ListStates(s.Room)
	if err != nil {
		return nil, out, err
	}
	names := map[string]AgentView{}
	for _, p := range presence {
		names[p.Name] = AgentView{Name: p.Name, Alive: p.Alive, PID: p.PID, Queued: p.Queued}
	}
	for name, st := range states {
		v := names[name]
		v.Name = name
		if st.Ready(ctx) {
			v.Alive = true
			v.Preset = st.Preset
			v.Busy = st.State == "busy"
			v.PID = st.PID
			v.CurrentRequest = st.CurrentID
			v.SessionMode = st.Session
		}
		names[name] = v
	}
	for name, v := range names {
		if in.Name == "" || in.Name == name {
			out.Agents = append(out.Agents, v)
		}
	}
	sort.Slice(out.Agents, func(i, j int) bool { return out.Agents[i].Name < out.Agents[j].Name })
	return nil, out, nil
}

type RequestInput struct {
	RequestID string `json:"request_id"`
}

func (s *service) cancelTool(ctx context.Context, req *mcp.CallToolRequest, in RequestInput) (*mcp.CallToolResult, RequestView, error) {
	r, err := request.Cancel(ctx, s.Room, in.RequestID)
	if err != nil {
		return nil, RequestView{}, err
	}
	if !r.Terminal() {
		if err := host.Cancel(ctx, s.Room, r.Agent, r.ID); err != nil {
			return nil, view(r), err
		}
	}
	return nil, view(r), nil
}

type NameInput struct {
	Name string `json:"name"`
}
type StopOutput struct {
	Name    string `json:"name"`
	Stopped bool   `json:"stopped"`
	Reset   bool   `json:"reset,omitempty"`
}

func (s *service) stopTool(ctx context.Context, req *mcp.CallToolRequest, in NameInput) (*mcp.CallToolResult, StopOutput, error) {
	err := host.Down(ctx, s.Room, in.Name, 10*time.Second)
	return nil, StopOutput{Name: in.Name, Stopped: err == nil}, err
}
func (s *service) resetTool(ctx context.Context, req *mcp.CallToolRequest, in NameInput) (*mcp.CallToolResult, StopOutput, error) {
	if err := host.Down(ctx, s.Room, in.Name, 10*time.Second); err != nil {
		return nil, StopOutput{}, err
	}
	err := host.ClearSession(ctx, s.Room, in.Name)
	return nil, StopOutput{Name: in.Name, Stopped: true, Reset: err == nil}, err
}
