package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
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
	if o.Preset.Pinned {
		if err := PinnedExecutable(o.Preset.Exec[0]); err != nil {
			return UpResult{}, err
		}
	} else if _, err := ResolveExecutable(o.Preset.Exec[0], room); err != nil {
		return UpResult{}, fmt.Errorf("agent binary %q not found on PATH or in user bin directories (preset %s; fix the exec path in %s or install the CLI): %w", o.Preset.Exec[0], o.Label, ConfigPath(), err)
	}
	exe := o.Executable
	if exe == "" {
		exe, err = os.Executable()
		if err != nil {
			return UpResult{}, err
		}
	}
	owner := envelope.NewID()
	if err := removeStalePresetFiles(room, o.Name); err != nil {
		return UpResult{}, err
	}
	presetFile, err := writePresetFile(room, o.Name, owner, o.Preset)
	if err != nil {
		return UpResult{}, err
	}
	// serve removes the file once it has read it; this covers a daemon that
	// never got that far.
	defer os.Remove(presetFile)
	args := []string{"serve", o.Name, "--room", room, "--daemon", "--owner", owner, "--resolved-label", o.Label, "--resolved-preset-file", presetFile}
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

// presetFileDir holds the private hand-off of a launch's resolved preset from
// Up to the daemon it starts (spec §5.2). One directory per listener, because
// listener names may contain dots.
func presetFileDir(room, name string) string { return filepath.Join(Dir(room), "config", name) }

// writePresetFile stores p for the daemon of lifetime owner: 0700 directory,
// 0600 file, written atomically. The argv carries only the path.
func writePresetFile(room, name, owner string, p Preset) (string, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	path := filepath.Join(presetFileDir(room, name), owner+".json")
	if err := fsutil.WriteFileAtomic(path, data); err != nil {
		return "", fmt.Errorf("write preset hand-off: %w", err)
	}
	return path, nil
}

// ReadPresetFile loads and removes the hand-off file at path, which must be in
// listener name's hand-off directory of room.
func ReadPresetFile(room, name, path string) (Preset, error) {
	if err := spool.ValidName(name); err != nil {
		return Preset{}, err
	}
	room, err := canonicalRoom(room)
	if err != nil {
		return Preset{}, err
	}
	dir := presetFileDir(room, name)
	if filepath.Dir(filepath.Clean(path)) != dir {
		return Preset{}, fmt.Errorf("preset hand-off %s is not in %s", path, dir)
	}
	data, err := fsutil.ReadFile(path, 1<<20)
	if err != nil {
		return Preset{}, fmt.Errorf("read preset hand-off: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Preset{}, err
	}
	_ = fsutil.SyncDir(dir)
	var p Preset
	if err := json.Unmarshal(data, &p); err != nil {
		return Preset{}, fmt.Errorf("read preset hand-off: %w", err)
	}
	if err := p.normalize(); err != nil {
		return Preset{}, err
	}
	return p, nil
}

// removeStalePresetFiles empties name's hand-off directory. Callers hold the
// launch lock with the lifetime lock free, so no daemon is reading one.
func removeStalePresetFiles(room, name string) error {
	dir := presetFileDir(room, name)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
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
