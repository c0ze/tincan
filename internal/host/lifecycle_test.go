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

func TestListenerNamesCannotCollideWithLifecycleLocks(t *testing.T) {
	room, _, _, stopped, _ := startServe(t, echoPreset(), "fake")
	ctx, cancel := context.WithCancel(context.Background())
	other := make(chan error, 1)
	go func() {
		other <- Serve(ctx, ServeOptions{Room: room, Name: "agent.launch", Label: "fake", Preset: echoPreset()})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-other:
			if err != nil {
				t.Errorf("second host: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("second host did not stop")
		}
	})
	if !waitFor(func() bool {
		st, ok, _ := ReadState(room, "agent.launch")
		return ok && st.Alive()
	}, 5*time.Second) {
		t.Fatal("second host did not become ready")
	}
	if err := Down(context.Background(), room, "agent", 2*time.Second); err != nil {
		t.Fatalf("other listener's name blocked shutdown: %v", err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("original host did not stop")
	}
	st, ok, err := ReadState(room, "agent.launch")
	if err != nil || !ok || !st.Alive() {
		t.Fatalf("shutdown affected other listener: %+v %v", st, err)
	}
}

func TestPresetFileHandOff(t *testing.T) {
	room := canonicalTempDir(t)
	p := Preset{Exec: []string{"codex"}, Stdin: "none", Reply: "stdout", Env: map[string]string{"CODEX_HOME": "/secret"}}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	path, err := writePresetFile(room, "agent", "owner1", p)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != presetFileDir(room, "agent") {
		t.Fatalf("hand-off file outside its directory: %s", path)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("hand-off file mode: %v %v", info, err)
		}
		dir, err := os.Stat(presetFileDir(room, "agent"))
		if err != nil || dir.Mode().Perm() != 0o700 {
			t.Fatalf("hand-off dir mode: %v %v", dir, err)
		}
	}
	got, err := ReadPresetFile(room, "agent", path)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("read back %+v %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hand-off file not removed after reading: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "x.json")
	os.WriteFile(outside, []byte(`{"exec":["x"]}`), 0o600)
	if _, err := ReadPresetFile(room, "agent", outside); err == nil {
		t.Fatal("read a preset file outside the listener's hand-off directory")
	}
	stale, _ := writePresetFile(room, "agent", "old", p)
	if err := removeStalePresetFiles(room, "agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale hand-off file survived")
	}
}
