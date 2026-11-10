package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/filelock"
)

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
