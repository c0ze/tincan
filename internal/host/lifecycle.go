package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/spool"
)

func lifetimePath(room, name string) string {
	return filepath.Join(Dir(room), "locks", "lifetime", name+".lock")
}
func launchPath(room, name string) string {
	return filepath.Join(Dir(room), "locks", "launch", name+".lock")
}

type UpOptions struct {
	Room       string
	Name       string
	Label      string
	Preset     Preset
	Wait       time.Duration
	Executable string // defaults to the current tincan executable
}

type UpResult struct {
	State   State `json:"state"`
	Already bool  `json:"already"`
}

// Existing identifies a live hosted lifetime or an interactive receiver.
func Existing(ctx context.Context, room, name string) (State, bool) {
	if st, ok, _ := ReadState(room, name); ok && st.Owner != "" && st.Ready(ctx) {
		return st, true
	}
	sp, err := spool.Open(room)
	if err == nil {
		if p, ok, _ := sp.Present(name); ok {
			return State{PID: p.PID}, true
		}
	}
	return State{}, false
}

// Up serializes starts and waits for the exact owner's control endpoint.
// Receiving a queued job immediately does not prevent readiness.
func Up(ctx context.Context, o UpOptions) (UpResult, error) {
	if err := spool.ValidName(o.Name); err != nil {
		return UpResult{}, err
	}
	room, err := canonicalRoom(o.Room)
	if err != nil {
		return UpResult{}, err
	}
	o.Room = room
	if len(o.Preset.Exec) > 0 {
		if err := o.Preset.normalize(); err != nil {
			return UpResult{}, err
		}
		o.Preset, err = WithSession(o.Preset, o.Label, "")
		if err != nil {
			return UpResult{}, err
		}
	}
	if o.Wait < 0 {
		return UpResult{}, errors.New("wait cannot be negative")
	}
	if o.Wait == 0 {
		o.Wait = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Wait)
	defer cancel()
	launch, err := filelock.Acquire(ctx, launchPath(room, o.Name))
	if err != nil {
		return UpResult{}, err
	}
	defer launch.Close()
	for {
		if st, ok := Existing(ctx, room, o.Name); ok {
			return existingResult(st, o)
		}
		owner, err := filelock.Try(lifetimePath(room, o.Name))
		if err == nil {
			owner.Close()
			break
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return UpResult{}, err
		}
		if err := pause(ctx); err != nil {
			return UpResult{}, fmt.Errorf("listener %s owns its lock but is not ready: %w", o.Name, err)
		}
	}
	if len(o.Preset.Exec) == 0 {
		return UpResult{}, errors.New("exec is required to launch a new host")
	}
	if _, err := exec.LookPath(o.Preset.Exec[0]); err != nil {
		return UpResult{}, fmt.Errorf("agent binary %q not found on PATH (preset %s): %w", o.Preset.Exec[0], o.Label, err)
	}
	exe := o.Executable
	if exe == "" {
		exe, err = os.Executable()
		if err != nil {
			return UpResult{}, err
		}
	}
	data, err := json.Marshal(o.Preset)
	if err != nil {
		return UpResult{}, err
	}
	owner := envelope.NewID()
	args := []string{"serve", o.Name, "--room", room, "--daemon", "--owner", owner, "--resolved-label", o.Label, "--resolved-preset", string(data)}
	_, exited, err := StartDetached(exe, args, room, LogPath(room, o.Name))
	if err != nil {
		return UpResult{}, err
	}
	for {
		if st, ok, _ := ReadState(room, o.Name); ok && st.Owner == owner && st.Ready(ctx) {
			return UpResult{State: st}, nil
		}
		select {
		case err := <-exited:
			if st, ok := Existing(ctx, room, o.Name); ok {
				return existingResult(st, o)
			}
			return UpResult{}, fmt.Errorf("serve exited before readiness (%v); see %s", err, LogPath(room, o.Name))
		case <-ctx.Done():
			if st, ok, _ := ReadState(room, o.Name); ok && st.Owner == owner {
				stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
				_ = st.controlCall(stopCtx, "/stop")
				stop()
			}
			return UpResult{}, fmt.Errorf("listener %s did not become ready: %w; see %s", o.Name, ctx.Err(), LogPath(room, o.Name))
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func existingResult(st State, o UpOptions) (UpResult, error) {
	result := UpResult{State: st, Already: true}
	if len(o.Preset.Exec) > 0 && (st.Config == nil || st.Preset != o.Label || !reflect.DeepEqual(*st.Config, o.Preset)) {
		return result, fmt.Errorf("listener %s is already running with a different configuration (preset=%s session=%s); stop it before relaunching with new settings", o.Name, st.Preset, st.Session)
	}
	return result, nil
}

func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(25 * time.Millisecond):
		return nil
	}
}

// Down stops the authenticated host lifetime and waits for its lock to be
// released. It never sends an operating-system signal to an on-disk PID.
func Down(ctx context.Context, room, name string, wait time.Duration) error {
	canonical, err := canonicalRoom(room)
	if err != nil {
		return err
	}
	room = canonical
	if err := spool.ValidName(name); err != nil {
		return err
	}
	if wait < 0 {
		return errors.New("wait cannot be negative")
	}
	if wait == 0 {
		wait = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	launch, err := filelock.Acquire(ctx, launchPath(room, name))
	if err != nil {
		return err
	}
	defer launch.Close()
	st, ok, readErr := ReadState(room, name)
	lock, lockErr := filelock.Try(lifetimePath(room, name))
	if lockErr == nil {
		if ok {
			err = RemoveOwnedState(room, name, st.Owner)
		} else if readErr != nil {
			err = RemoveState(room, name)
		}
		lock.Close()
		if err != nil {
			return err
		}
		return stopInteractive(ctx, room, name)
	}
	if !errors.Is(lockErr, filelock.ErrLocked) {
		return lockErr
	}
	if readErr != nil {
		return readErr
	}
	if !ok || !st.Ready(ctx) {
		return errors.New("listener owns its lock but its authenticated control endpoint is unavailable")
	}
	if err := st.controlCall(ctx, "/stop"); err != nil {
		return err
	}
	for {
		lock, err := filelock.Try(lifetimePath(room, name))
		if err == nil {
			defer lock.Close()
			return RemoveOwnedState(room, name, st.Owner)
		}
		if !errors.Is(err, filelock.ErrLocked) {
			return err
		}
		if err := pause(ctx); err != nil {
			return fmt.Errorf("listener %s has not stopped: %w", name, err)
		}
	}
}

func stopInteractive(ctx context.Context, room, name string) error {
	sp, err := spool.Open(room)
	if err != nil {
		return err
	}
	if _, ok, _ := sp.Present(name); !ok {
		return nil
	}
	e := &envelope.Envelope{ID: envelope.NewID(), From: "orch", To: name, TS: time.Now().UTC(), Kind: "stop"}
	if err := sp.Send(e); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(sp.InboxDir(name), envelope.Filename(e)))
	for {
		if _, ok, _ := sp.Present(name); !ok {
			return nil
		}
		if err := pause(ctx); err != nil {
			return fmt.Errorf("listener %s has not stopped: %w", name, err)
		}
	}
}
