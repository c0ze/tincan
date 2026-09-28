package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func TestReviewCLI(t *testing.T) {
	state := rooms.StateDir() // TestMain sets TINCAN_STATE_DIR
	room, _ := filepath.EvalSymlinks(t.TempDir())
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"claude@box"}})
	os.Remove(rooms.HeartbeatPath(state))
	if code, _, errOut := run("review", "--room", room, "--committee", "solo", "--question", "q"); code != ExitError || !strings.Contains(errOut, "tincan web is not running") {
		t.Fatalf("no coordinator: %d %q", code, errOut)
	}
	rooms.WriteHeartbeat(state, rooms.Heartbeat{PID: 1, Machine: "box", Updated: time.Now()})
	code, out, errOut := run("review", "--room", room, "--committee", "solo", "--question", "q", "--scope", "none", "--request-id", "k")
	id := strings.TrimSpace(out)
	if code != ExitOK || !strings.HasPrefix(id, "rv-") {
		t.Fatalf("review: %d %q %q", code, out, errOut)
	}
	// Simulate the coordinator closing it with a late member.
	st, _ := review.ReadState(room, id)
	st.Status, st.Members[0].Late, st.Members[0].State = "closed", true, "running"
	review.WriteState(room, id, st)
	os.WriteFile(filepath.Join(review.Dir(room, id), "bundle.md"), []byte("# bundle\n"), 0o600)
	code, out, _ = run("review", "--room", room, "--wait", id, "--timeout", "5")
	if code != 4 || !strings.Contains(out, "# bundle") {
		t.Fatalf("wait: %d %q", code, out)
	}
	if code, _, errOut := run("review", "--room", room, "--cancel", id); code != ExitOK {
		t.Fatalf("cancel: %d %q", code, errOut)
	}
	if code, _, _ := run("review", "--room", room); code != ExitUsage {
		t.Fatal("missing --committee accepted")
	}
}
