package mcpserver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/committee"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/mcpserver"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectState(t *testing.T, room, state string) *mcp.ClientSession {
	t.Helper()
	server, err := mcpserver.New(mcpserver.Options{Room: room, StateDir: state, Presets: map[string]host.Preset{}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return cs
}

func TestReviewToolsPublishWaitAndCancel(t *testing.T) {
	state, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TINCAN_STATE_DIR", state)
	room, _ := filepath.EvalSymlinks(t.TempDir())
	committee.NewStore(state).Put(context.Background(), committee.Committee{Name: "solo", Members: []string{"claude@box"}})
	cs := connectState(t, room, state)

	res, _ := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "tincan_review", Arguments: map[string]any{"committee": "solo", "question": "q"}})
	if !res.IsError || !strings.Contains(textOf(res), "tincan web is not running") {
		t.Fatalf("without a coordinator: %+v", res)
	}
	if _, err := os.Stat(review.Root(room)); err == nil {
		t.Fatal("refused review wrote files")
	}

	rooms.WriteHeartbeat(state, rooms.Heartbeat{PID: 1, Machine: "box", Updated: time.Now()})
	out := call(t, cs, "tincan_review", map[string]any{"committee": "solo", "question": "q", "scope": "none", "request_id": "k"})
	id, _ := out["review_id"].(string)
	if !strings.HasPrefix(id, "rv-") || out["status"] != "running" {
		t.Fatalf("review: %v", out)
	}
	waited := call(t, cs, "tincan_review_wait", map[string]any{"review_id": id, "timeout_seconds": 0})
	if waited["closed"] != false {
		t.Fatalf("wait: %v", waited)
	}
	cancelled := call(t, cs, "tincan_review_cancel", map[string]any{"review_id": id})
	if cancelled["status"] != "cancelled" {
		t.Fatalf("cancel: %v", cancelled)
	}
}

func textOf(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
