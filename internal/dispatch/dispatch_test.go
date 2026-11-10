package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/cli"
	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
		case "fixture":
			fmt.Print(strings.ToUpper(os.Args[2]))
			os.Exit(0)
		}
	}
	state, _ := os.MkdirTemp("", "tincan-state-")
	os.Setenv("TINCAN_STATE_DIR", state)
	code := m.Run()
	os.RemoveAll(state)
	os.Exit(code)
}

func opts(t *testing.T) dispatch.Options {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts are not supported on Windows")
	}
	exe, _ := os.Executable()
	room, _ := filepath.EvalSymlinks(t.TempDir())
	fixture := host.Preset{Exec: []string{exe, "fixture", "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 30}
	return dispatch.Options{Room: room, Presets: map[string]host.Preset{"fixture": fixture}}
}

func TestSendLaunchesNamedListenerWithSeparatePreset(t *testing.T) {
	o := opts(t)
	name := "fixture.t0000001"
	t.Cleanup(func() { host.Down(context.Background(), o.Room, name, 5*time.Second) })
	r, err := dispatch.Send(context.Background(), o, dispatch.SendSpec{Agent: name, Preset: "fixture", From: "web", Body: "hi", RequestID: "t0000001-m1"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = request.Wait(context.Background(), o.Room, r.ID, 20*time.Second)
	if err != nil || r.Result != "HI" {
		t.Fatalf("result %+v %v", r, err)
	}
	st, ok := host.Existing(context.Background(), o.Room, name)
	if !ok || st.Preset != "fixture" {
		t.Fatalf("listener state %+v %v", st, ok)
	}
}

func TestSendSameIDAndBodyDoesNotExecuteTwice(t *testing.T) {
	o := opts(t)
	name := "fixture.t0000002"
	t.Cleanup(func() { host.Down(context.Background(), o.Room, name, 5*time.Second) })
	spec := dispatch.SendSpec{Agent: name, Preset: "fixture", From: "web", Body: "once", RequestID: "t0000002-m1"}
	first, err := dispatch.Send(context.Background(), o, spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dispatch.Send(context.Background(), o, spec)
	if err != nil || second.ID != first.ID {
		t.Fatalf("second send %+v %v", second, err)
	}
	spec.Body = "different"
	if _, err := dispatch.Send(context.Background(), o, spec); err == nil {
		t.Fatal("same ID with a different body was accepted")
	}
}

func TestLaunchUnknownPresetFails(t *testing.T) {
	o := opts(t)
	if _, err := dispatch.Launch(context.Background(), o, "x.t0000003", "nope", ""); err == nil {
		t.Fatal("unknown preset accepted")
	}
}

// A send to a name that is neither running nor a preset (a typo) must not
// leave a queued record or message that a later listener of that name runs.
func TestSendUnknownAgentSavesNothing(t *testing.T) {
	room, _ := filepath.EvalSymlinks(t.TempDir())
	missing := host.Preset{Exec: []string{filepath.Join(t.TempDir(), "not-installed"), "{body}"}, Stdin: "none", Reply: "stdout"}
	o := dispatch.Options{Room: room, Presets: map[string]host.Preset{"claude": missing}}
	for _, spec := range []dispatch.SendSpec{
		{Agent: "cluade", Body: "typo", RequestID: "typo-1"},
		{Agent: "reviewer", Preset: "nope", Body: "typo", RequestID: "typo-2"},
	} {
		if _, err := dispatch.Send(context.Background(), o, spec); err == nil {
			t.Fatalf("%+v accepted", spec)
		}
		if _, err := request.Get(room, spec.RequestID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left a record: %v", spec.RequestID, err)
		}
		sp, _ := spool.Open(room)
		if p, _, err := sp.Present(spec.Agent); err != nil || p.Queued != 0 {
			t.Fatalf("%s left a message: %+v %v", spec.Agent, p, err)
		}
	}
}

// A launch failure a same-ID retry can cure keeps the request saved.
func TestSendTransientLaunchFailureKeepsRequest(t *testing.T) {
	room, _ := filepath.EvalSymlinks(t.TempDir())
	missing := host.Preset{Exec: []string{filepath.Join(t.TempDir(), "not-installed"), "{body}"}, Stdin: "none", Reply: "stdout"}
	o := dispatch.Options{Room: room, Presets: map[string]host.Preset{"missing": missing}}
	_, err := dispatch.Send(context.Background(), o, dispatch.SendSpec{Agent: "missing", Body: "work", RequestID: "transient-1"})
	if err == nil || !strings.Contains(err.Error(), "retry this same request_id") {
		t.Fatalf("launch error %v", err)
	}
	if r, err := request.Get(room, "transient-1"); err != nil || r.Status != "queued" {
		t.Fatalf("saved request %+v %v", r, err)
	}
}

func TestSendRejectsBlankBody(t *testing.T) {
	room, _ := filepath.EvalSymlinks(t.TempDir())
	// The executable is missing, so nothing can run even if the check fails.
	missing := host.Preset{Exec: []string{filepath.Join(t.TempDir(), "not-installed"), "{body}"}, Stdin: "none", Reply: "stdout"}
	o := dispatch.Options{Room: room, Presets: map[string]host.Preset{"missing": missing}}
	for _, body := range []string{"", " \n\t"} {
		if _, err := dispatch.Send(context.Background(), o, dispatch.SendSpec{Agent: "missing", Body: body, RequestID: "blank-1"}); err == nil {
			t.Fatalf("blank body %q accepted", body)
		}
		if _, err := request.Get(room, "blank-1"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("blank body saved: %v", err)
		}
	}
}

func TestLaunchThreadListenerUsesPresetSessionMode(t *testing.T) {
	o := opts(t)
	// A fake executable named like a session-capable provider selects the
	// persistent adapter; each thread listener has its own session file.
	dir := t.TempDir()
	exe, _ := os.Executable()
	claude := filepath.Join(dir, "claude")
	if err := os.Symlink(exe, claude); err != nil {
		t.Skip(err)
	}
	o.Presets["claude"] = host.Preset{Exec: []string{claude, "fixture", "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 30}
	name := "claude.t0000004"
	t.Cleanup(func() { host.Down(context.Background(), o.Room, name, 5*time.Second) })
	res, err := dispatch.Launch(context.Background(), o, name, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.State.Session != "persistent" || res.State.Preset != "claude" {
		t.Fatalf("state %+v", res.State)
	}
}
