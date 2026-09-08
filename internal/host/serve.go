package host

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
)

// ServeOptions configures one hosted listener loop.
type ServeOptions struct {
	Owner  string    // optional launch identity supplied by Up
	Room   string    // absolute room directory; also the agent's cwd
	Name   string    // inbox to park on
	Label  string    // preset label recorded in the state file / status
	Preset Preset    // resolved preset (see Resolve)
	Log    io.Writer // host log sink; nil discards
}

// Serve parks on <room>/.tincan/inbox/<name> with the normal Recv (so
// presence, status and ping work unchanged) and answers every ordinary
// message by running the preset once with the body as the prompt, sending
// the output to the message's reply_to channel. Supported session adapters
// preserve the provider conversation ID for this room and listener.
//
// It returns nil when a kind=="stop" message arrives or ctx is cancelled
// (SIGTERM/SIGINT), after killing any in-flight agent and removing the state
// file. Any other error is a spool failure that ends the loop.
func Serve(ctx context.Context, o ServeOptions) error {
	room, err := canonicalRoom(o.Room)
	if err != nil {
		return err
	}
	o.Room = room
	if err := spool.ValidName(o.Name); err != nil {
		return err
	}
	if err := o.Preset.normalize(); err != nil {
		return err
	}
	o.Preset, err = WithSession(o.Preset, o.Label, "")
	if err != nil {
		return err
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Owner == "" {
		o.Owner = envelope.NewID()
	}
	if len(o.Owner) > 128 {
		return errors.New("invalid host owner")
	}
	if err := spool.ValidName(o.Owner); err != nil {
		return fmt.Errorf("invalid host owner: %w", err)
	}
	lifetime, err := filelock.Try(lifetimePath(o.Room, o.Name))
	if err != nil {
		return fmt.Errorf("listener %s already owns its lifetime or cannot be locked: %w", o.Name, err)
	}
	defer lifetime.Close()
	// The lock excludes the previous lifetime and all replacement writers.
	if previous, ok, err := ReadState(o.Room, o.Name); err != nil {
		if err := RemoveState(o.Room, o.Name); err != nil {
			return err
		}
	} else if ok {
		if err := RemoveOwnedState(o.Room, o.Name, previous.Owner); err != nil {
			return err
		}
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	ctl, err := newControl(o.Owner, stop)
	if err != nil {
		return err
	}
	defer ctl.server.Close()
	sp, err := spool.Open(o.Room)
	if err != nil {
		return err
	}
	st := State{PID: os.Getpid(), Owner: o.Owner, ControlAddress: ctl.listener.Addr().String(), ControlToken: ctl.token, Preset: o.Label, Session: o.Preset.Session, Config: &o.Preset, Exec: o.Preset.Exec, Started: time.Now().UTC(), State: "parked"}
	// Exclusive ownership makes these claims abandoned. Persist uncertainty;
	// never execute a possibly completed side effect for a second time.
	abandoned, err := sp.RecoverInFlight(o.Name)
	if err != nil {
		return err
	}
	for _, d := range abandoned {
		if d.Envelope.Kind != "" {
			if err := d.Ack(false); err != nil {
				return err
			}
			continue
		}
		r, err := request.Finish(o.Room, d.Envelope, "ERROR interrupted: previous hosted listener exited; execution outcome is uncertain and was not replayed")
		if err != nil {
			return err
		}
		if err := complete(sp, o, d, r.Result); err != nil {
			return err
		}
	}
	if err := WriteState(o.Room, o.Name, st); err != nil {
		return err
	}
	defer RemoveOwnedState(o.Room, o.Name, o.Owner)
	logf(o.Log, "serve name=%s preset=%s pid=%d exec=%q", o.Name, o.Label, st.PID, o.Preset.Exec)
	for {
		d, err := sp.ClaimContext(ctx, o.Name, 0)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		e := d.Envelope
		switch e.Kind {
		case "":
		case "stop":
			logf(o.Log, "=== %s from=%s kind=stop: stopping", e.ID, e.From)
			return d.Ack(false)
		default:
			logf(o.Log, "=== %s from=%s kind=%q ignored", e.ID, e.From, e.Kind)
			if err := d.Ack(false); err != nil {
				return err
			}
			continue
		}
		jobCtx, endJob := ctl.begin(ctx, e.ID)
		// Start preserves terminal records and atomically observes queued cancellation.
		r, err := request.Start(o.Room, e)
		if err != nil {
			endJob()
			return err
		}
		if !r.Terminal() {
			st.State, st.CurrentID = "busy", e.ID
			if err := WriteState(o.Room, o.Name, st); err != nil {
				endJob()
				return err
			}
			body := handle(jobCtx, o, e)
			r, err = request.Finish(o.Room, e, body)
		}
		endJob()
		if err != nil {
			return err
		}
		if err := complete(sp, o, d, r.Result); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		st.State, st.CurrentID = "parked", ""
		if err := WriteState(o.Room, o.Name, st); err != nil {
			return err
		}
	}
}

// complete leaves the delivery recoverable until its durable terminal result
// has been delivered. A send failure cannot silently drop accepted work.
func complete(sp *spool.Spool, o ServeOptions, d *spool.Delivery, body string) error {
	e := d.Envelope
	if e.ReplyTo != "" {
		replyID := fmt.Sprintf("%x", sha256.Sum256([]byte("reply:"+e.ID)))
		reply := &envelope.Envelope{ID: replyID, CorrID: e.ReplyTo, From: o.Name, To: e.ReplyTo, TS: e.TS, Body: body}
		if err := sp.Send(reply); err != nil {
			return fmt.Errorf("reply %s: %w", e.ID, err)
		}
	} else {
		logf(o.Log, "--- %s has no reply_to; result retained in request journal", e.ID)
	}
	return d.Ack(false)
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
	session, err := PrepareSession(o.Room, o.Name, o.Label, o.Preset)
	if err != nil {
		logf(o.Log, "=== exit=error session: %v", err)
		return "ERROR session: " + err.Error()
	}
	p := session.Preset
	spec := RunSpec{Dir: o.Room, Timeout: time.Duration(p.ExecTimeoutSec) * time.Second,
		Output: func(stream string, data []byte) error {
			if _, err := o.Log.Write(data); err != nil {
				return err
			}
			filtered, err := session.Output(stream, data)
			if err != nil {
				return err
			}
			if len(filtered) > 0 {
				return request.AppendProgress(o.Room, e.ID, stream, filtered)
			}
			return nil
		},
	}
	vars := Vars{Body: e.Body, Room: o.Room, Name: o.Name, ID: e.ID}
	if p.Reply == "file" {
		f, err := newReplyFile()
		if err != nil {
			logf(o.Log, "=== exit=error duration=%.1fs reply file: %v", time.Since(started).Seconds(), err)
			return "ERROR exec: cannot create reply file: " + err.Error()
		}
		f.Close()
		spec.OutFile = f.Name()
		defer os.Remove(spec.OutFile)
		vars.Out = spec.OutFile
	}
	spec.Argv = Render(p.Exec, vars)
	if p.Stdin == "body" {
		body := e.Body
		spec.Stdin = &body
	}
	res := Run(ctx, spec)
	logf(o.Log, "=== exit=%s duration=%.1fs", exitLabel(res), res.Duration.Seconds())
	body, err := session.Reply(spec, res)
	if err != nil {
		return "ERROR session: " + err.Error()
	}
	return PostProcess(o.Label, body)
}

// Canonicalize the system temporary directory before creating the private
// reply file. On macOS it commonly contains /var -> /private/var; the returned
// path must pass the regular-file reader's checks on every ancestor.
func newReplyFile() (*os.File, error) {
	dir, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, "tincan-reply-*.md")
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
