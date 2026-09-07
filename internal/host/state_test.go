package host

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestHostFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	room := t.TempDir()
	if err := WriteState(room, "agent", State{PID: 1}); err != nil {
		t.Fatal(err)
	}
	f, err := OpenLog(LogPath(room, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, path := range []string{StatePath(room, "agent"), LogPath(room, "agent")} {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteState(room, "agent", State{PID: 2}); err != nil {
		t.Fatal(err)
	}
	f, err = OpenLog(LogPath(room, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, path := range []string{StatePath(room, "agent"), LogPath(room, "agent")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", path, info.Mode().Perm())
		}
	}
	info, err := os.Stat(Dir(room))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("hosts directory mode %o", info.Mode().Perm())
	}
}

func TestOpenLogRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may need privileges")
	}
	room := t.TempDir()
	target := filepath.Join(room, "target")
	if err := os.WriteFile(target, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(Dir(room), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, LogPath(room, "agent")); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenLog(LogPath(room, "agent")); err == nil {
		f.Close()
		t.Fatal("followed a log symlink")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatal("changed symlink target permissions")
	}
}

func TestStateWriteReadRoundTrip(t *testing.T) {
	room := t.TempDir()
	in := State{PID: os.Getpid(), Preset: "codex", Exec: []string{"codex", "exec", "-"}, Started: time.Now().UTC().Truncate(time.Second), State: "busy", CurrentID: "m1"}
	if err := WriteState(room, "codex", in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadState(room, "codex")
	if err != nil || !ok {
		t.Fatalf("ReadState = %+v, %v, %v", got, ok, err)
	}
	if got.PID != in.PID || got.Preset != in.Preset || !reflect.DeepEqual(got.Exec, in.Exec) || !got.Started.Equal(in.Started) || got.State != "busy" || got.CurrentID != "m1" {
		t.Fatalf("round trip mismatch:\n in %+v\ngot %+v", in, got)
	}
}

func TestStateWriteOverwritesAndLeavesNoResidue(t *testing.T) {
	room := t.TempDir()
	if err := WriteState(room, "x", State{PID: 1, State: "parked"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(room, "x", State{PID: 2, State: "busy"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := ReadState(room, "x")
	if got.PID != 2 || got.State != "busy" {
		t.Fatalf("second write not visible: %+v", got)
	}
	entries, err := os.ReadDir(Dir(room))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "x.json" {
		t.Fatalf("hosts dir should hold only x.json, got %v", entries)
	}
}

func TestReadStateMissingIsNotAnError(t *testing.T) {
	st, ok, err := ReadState(t.TempDir(), "ghost")
	if err != nil || ok {
		t.Fatalf("ReadState missing = %+v, %v, %v; want zero, false, nil", st, ok, err)
	}
}

func TestReadStateCorruptIsAnError(t *testing.T) {
	room := t.TempDir()
	if err := os.MkdirAll(Dir(room), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(room, "bad"), []byte("{torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadState(room, "bad"); err == nil {
		t.Fatal("want error for corrupt state file")
	}
}

func TestRemoveStateIsIdempotent(t *testing.T) {
	room := t.TempDir()
	if err := WriteState(room, "x", State{PID: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := RemoveState(room, "x"); err != nil {
			t.Fatalf("RemoveState #%d: %v", i+1, err)
		}
	}
	if _, ok, _ := ReadState(room, "x"); ok {
		t.Fatal("state file still present after RemoveState")
	}
}

func TestStateAliveRequiresExactOwner(t *testing.T) {
	if (State{PID: os.Getpid()}).Alive() {
		t.Fatal("a live PID without an owner identity was trusted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
	ctl, err := newControl("test-owner", cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.server.Close()
	st := State{PID: os.Getpid(), Owner: "test-owner", ControlAddress: ctl.listener.Addr().String(), ControlToken: ctl.token}
	if !st.Alive() {
		t.Fatal("authenticated live owner not recognized")
	}
	st.Owner = "another-owner"
	if st.Alive() {
		t.Fatal("wrong owner was trusted")
	}
	if runtime.GOOS == "windows" {
		t.Skip("dead-pid detection is unix-only (see spool/alive_other.go)")
	}
	if (State{PID: 1 << 30}).Alive() {
		t.Fatal("huge unused pid reported alive")
	}
}

func TestListStatesSkipsTempAndCorruptFiles(t *testing.T) {
	room := t.TempDir()
	if err := WriteState(room, "a", State{PID: 1, Preset: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(room, "b", State{PID: 2, Preset: "kimi"}); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{".a.tmp": "{}", "bad.json": "{torn", "notes.txt": "x"} {
		if err := os.WriteFile(filepath.Join(Dir(room), name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListStates(room)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"].Preset != "codex" || got["b"].Preset != "kimi" {
		t.Fatalf("ListStates = %+v", got)
	}
	empty, err := ListStates(t.TempDir())
	if err != nil || len(empty) != 0 {
		t.Fatalf("ListStates on a room without hosts dir = %v, %v", empty, err)
	}
}
