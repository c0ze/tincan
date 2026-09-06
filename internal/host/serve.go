package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/spool"
)

// ServeOptions configures one hosted listener loop.
type ServeOptions struct {
	Room   string    // absolute room directory; also the agent's cwd
	Name   string    // inbox to park on
	Label  string    // preset label recorded in the state file / status
	Preset Preset    // resolved preset (see Resolve)
	Log    io.Writer // host log sink; nil discards
}

// Serve parks on <room>/.tincan/inbox/<name> with the normal Recv (so
// presence, status and ping work unchanged) and answers every ordinary
// message by running the preset once with the body as the prompt, sending
// the output to the message's reply_to channel. Each message is a fresh
// single-turn run; nothing is remembered between messages.
//
// It returns nil when a kind=="stop" message arrives or ctx is cancelled
// (SIGTERM/SIGINT), after killing any in-flight agent and removing the state
// file. Any other error is a spool failure that ends the loop.
func Serve(ctx context.Context, o ServeOptions) error {
	if err := spool.ValidName(o.Name); err != nil {
		return err
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	sp, err := spool.Open(o.Room)
	if err != nil {
		return err
	}
	st := State{PID: os.Getpid(), Preset: o.Label, Exec: o.Preset.Exec, Started: time.Now().UTC(), State: "parked"}
	if err := WriteState(o.Room, o.Name, st); err != nil {
		return err
	}
	defer RemoveState(o.Room, o.Name)
	logf(o.Log, "serve name=%s preset=%s pid=%d exec=%q", o.Name, o.Label, st.PID, o.Preset.Exec)
	for {
		e, err := sp.RecvContext(ctx, o.Name, 0, false)
		if err != nil {
			if ctx.Err() != nil {
				logf(o.Log, "serve name=%s stopping: %v", o.Name, ctx.Err())
				return nil
			}
			return err
		}
		switch e.Kind {
		case "":
		case "stop":
			logf(o.Log, "=== %s from=%s kind=stop: stopping", e.ID, e.From)
			return nil
		default:
			// Unknown control kinds are ignored and logged, never answered
			// (there is no reply_to on control messages by convention).
			logf(o.Log, "=== %s from=%s kind=%q ignored", e.ID, e.From, e.Kind)
			continue
		}
		st.State, st.CurrentID = "busy", e.ID
		if err := WriteState(o.Room, o.Name, st); err != nil {
			logf(o.Log, "state: %v", err)
		}
		body := handle(ctx, o, e)
		if e.ReplyTo != "" {
			reply := &envelope.Envelope{
				ID: envelope.NewID(), CorrID: e.ReplyTo, From: o.Name, To: e.ReplyTo,
				TS: time.Now().UTC(), Body: body,
			}
			if err := sp.Send(reply); err != nil {
				// A spool failure on the reply must not take the listener
				// down; the asker's timeout covers it.
				logf(o.Log, "reply %s: send failed: %v", e.ID, err)
			}
		} else {
			logf(o.Log, "--- %s has no reply_to; output logged only", e.ID)
		}
		if ctx.Err() != nil {
			return nil
		}
		st.State, st.CurrentID = "parked", ""
		if err := WriteState(o.Room, o.Name, st); err != nil {
			logf(o.Log, "state: %v", err)
		}
	}
}

// handle runs the preset for one message and returns the post-processed
// reply body, writing the per-message log block:
//
//	=== <id> from=<from> started=<ts>
//	<agent stdout+stderr>
//	=== exit=<code> duration=<s>s
func handle(ctx context.Context, o ServeOptions, e *envelope.Envelope) string {
	started := time.Now()
	logf(o.Log, "=== %s from=%s started=%s", e.ID, e.From, started.UTC().Format(time.RFC3339))
	spec := RunSpec{Dir: o.Room, Timeout: time.Duration(o.Preset.ExecTimeoutSec) * time.Second}
	vars := Vars{Body: e.Body, Room: o.Room, Name: o.Name, ID: e.ID}
	if o.Preset.Reply == "file" {
		f, err := os.CreateTemp("", "tincan-reply-*.md")
		if err != nil {
			logf(o.Log, "=== exit=error duration=%.1fs reply file: %v", time.Since(started).Seconds(), err)
			return "ERROR exec: cannot create reply file: " + err.Error()
		}
		f.Close()
		spec.OutFile = f.Name()
		defer os.Remove(spec.OutFile)
		vars.Out = spec.OutFile
	}
	spec.Argv = Render(o.Preset.Exec, vars)
	if o.Preset.Stdin == "body" {
		body := e.Body
		spec.Stdin = &body
	}
	res := Run(ctx, spec)
	if len(res.Stdout) > 0 {
		o.Log.Write(res.Stdout)
		if res.Stdout[len(res.Stdout)-1] != '\n' {
			io.WriteString(o.Log, "\n")
		}
	}
	if len(res.Stderr) > 0 {
		o.Log.Write(res.Stderr)
		if res.Stderr[len(res.Stderr)-1] != '\n' {
			io.WriteString(o.Log, "\n")
		}
	}
	logf(o.Log, "=== exit=%s duration=%.1fs", exitLabel(res), res.Duration.Seconds())
	return PostProcess(o.Label, ReplyBody(spec, res))
}

func exitLabel(res Result) string {
	switch {
	case res.TimedOut:
		return "timeout"
	case res.Killed:
		return "killed"
	case res.Err != nil:
		return "error"
	default:
		return strconv.Itoa(res.ExitCode)
	}
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}
