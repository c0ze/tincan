package dispatch_test

import (
	"context"
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
