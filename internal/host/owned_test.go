package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
)

// A foreground Serve that starts while a probe holds the listener's locks
// (as Owned, Up, Down and settlements do, under the launch lock) waits for
// the launch lock instead of failing on the lifetime lock.
func TestForegroundServeWaitsOutAProbe(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	room := canonicalTempDir(t)
	launch, err := filelock.Try(launchPath(room, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	lifetime, err := filelock.Try(lifetimePath(room, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	p := echoPreset()
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var serveErr error
	exited := make(chan struct{})
	go func() {
		serveErr = Serve(ctx, ServeOptions{Room: room, Name: "agent", Label: "fake", Preset: p})
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not exit")
		}
	})
	select {
	case <-exited:
		t.Fatalf("foreground serve failed during a probe: %v", serveErr)
	case <-time.After(100 * time.Millisecond):
	}
	lifetime.Close()
	launch.Close()
	if !waitFor(func() bool { st, ok, _ := ReadState(room, "agent"); return ok && st.Alive() }, 5*time.Second) {
		t.Fatal("foreground serve never became ready")
	}
}

// WithoutHost runs its callback only while no host owns the listener, and
// holds the listener's locks meanwhile.
func TestWithoutHostRunsOnlyWhenNoHostOwns(t *testing.T) {
	ctx := context.Background()
	room, sp, _, done, _ := startServe(t, echoPreset(), "fake")
	called := false
	if ran, err := WithoutHost(ctx, room, "agent", func() error { called = true; return nil }); err != nil || ran || called {
		t.Fatalf("live host: ran=%v called=%v err=%v", ran, called, err)
	}
	stopServe(t, sp, done)
	ran, err := WithoutHost(ctx, room, "agent", func() error {
		called = true
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if owned, err := Owned(short, room, "agent"); err == nil {
			t.Errorf("probe ran inside WithoutHost (owned=%v): the launch lock was not held", owned)
		}
		if l, err := filelock.Try(lifetimePath(room, "agent")); err == nil {
			l.Close()
			t.Error("the lifetime lock was not held")
		}
		return nil
	})
	if err != nil || !ran || !called {
		t.Fatalf("no host: ran=%v called=%v err=%v", ran, called, err)
	}
}

func TestOwnedReportsTheLifetimeLock(t *testing.T) {
	ctx := context.Background()
	empty := canonicalTempDir(t)
	if owned, err := Owned(ctx, empty, "agent"); err != nil || owned {
		t.Fatalf("no host ever: owned=%v err=%v", owned, err)
	}
	if _, err := os.Stat(filepath.Join(Dir(empty), "locks")); err == nil {
		t.Fatal("probing a name that never ran created its locks")
	}

	room, sp, _, done, _ := startServe(t, echoPreset(), "fake")
	if owned, err := Owned(ctx, room, "agent"); err != nil || !owned {
		t.Fatalf("live host: owned=%v err=%v", owned, err)
	}
	if st, ok := Existing(ctx, room, "agent"); !ok || st.Owner == "" {
		t.Fatal("the probe disturbed the live host")
	}
	stopServe(t, sp, done)
	if owned, err := Owned(ctx, room, "agent"); err != nil || owned {
		t.Fatalf("stopped host: owned=%v err=%v", owned, err)
	}

	// While a launch is in progress the answer is unknown, never a guess.
	launch, err := filelock.Try(launchPath(room, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := Owned(short, room, "agent"); err == nil {
		t.Fatal("probed while a launch held the launch lock")
	}
}
