package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/cli"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/mcpserver"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve", "mcp":
			os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
		case "fixture":
			body := os.Args[2]
			if body == "slow" {
				fmt.Println("working")
				time.Sleep(20 * time.Second)
			}
			fmt.Print(strings.ToUpper(body))
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func fixture(t *testing.T) host.Preset {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return host.Preset{Exec: []string{exe, "fixture", "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 30}
}

func connect(t *testing.T, room string) *mcp.ClientSession {
	t.Helper()
	server, err := mcpserver.New(mcpserver.Options{Room: room, Presets: map[string]host.Preset{"fixture": fixture(t)}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close(); host.Down(context.Background(), room, "fixture", 3*time.Second) })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("%s: %+v", name, result.Content)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err = json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestToolsAndDurableReconnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached host unsupported")
	}
	room := t.TempDir()
	cs := connect(t, room)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 8 {
		t.Fatalf("tools: %d", len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		if tool.InputSchema == nil || tool.OutputSchema == nil || tool.Annotations == nil {
			t.Fatalf("missing schema/annotation: %s", tool.Name)
		}
	}
	sent := call(t, cs, "tincan_send", map[string]any{"agent": "fixture", "body": "hello", "request_id": "stable-job"})
	if sent["request_id"] != "stable-job" {
		t.Fatal(sent)
	}
	result := call(t, cs, "tincan_wait", map[string]any{"request_id": "stable-job", "timeout_seconds": 5})
	if result["status"] != "completed" || result["result"] != "HELLO" {
		t.Fatal(result)
	}
	record, err := request.Get(room, "stable-job")
	if err != nil || record.Envelope.ReplyTo != "" {
		t.Fatalf("hosted work must not leave unused reply inboxes: %+v %v", record, err)
	}
	cs.Close()
	second := connect(t, room)
	replayed := call(t, second, "tincan_send", map[string]any{"agent": "fixture", "body": "hello", "request_id": "stable-job"})
	if replayed["status"] != "completed" || replayed["result"] != "HELLO" {
		t.Fatal(replayed)
	}
	status := call(t, second, "tincan_status", map[string]any{})
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "control_token") || strings.Contains(string(encoded), "control_address") {
		t.Fatal("leaked control credentials")
	}
	call(t, second, "tincan_stop", map[string]any{"name": "fixture"})
}

func TestCachedResultDoesNotRelaunchStoppedAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached host unsupported")
	}
	room := t.TempDir()
	cs := connect(t, room)
	t.Cleanup(func() { host.Down(context.Background(), room, "alias", 3*time.Second) })
	call(t, cs, "tincan_launch", map[string]any{"name": "alias", "preset": "fixture"})
	reconnected := call(t, cs, "tincan_launch", map[string]any{"name": "alias"})
	if reconnected["already"] != true || reconnected["agent"].(map[string]any)["session_mode"] != "stateless" {
		t.Fatalf("alias reconnect must retain its existing configuration: %+v", reconnected)
	}
	args := map[string]any{"agent": "alias", "body": "cached", "request_id": "alias-job"}
	call(t, cs, "tincan_send", args)
	result := call(t, cs, "tincan_wait", map[string]any{"request_id": "alias-job", "timeout_seconds": 5})
	if result["result"] != "CACHED" {
		t.Fatal(result)
	}
	call(t, cs, "tincan_stop", map[string]any{"name": "alias"})
	result = call(t, cs, "tincan_send", args)
	if result["status"] != "completed" || result["result"] != "CACHED" {
		t.Fatal(result)
	}
	status := call(t, cs, "tincan_status", map[string]any{"name": "alias"})
	for _, raw := range status["agents"].([]any) {
		if raw.(map[string]any)["alive"] == true {
			t.Fatal("collecting cached result restarted the host")
		}
	}
}

func TestResubmissionRecoversAbandonedRunningRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached host unsupported")
	}
	room := t.TempDir()
	ctx := context.Background()
	r, err := request.Submit(ctx, room, "fixture", "mcp", "must not replay", "abandoned-job")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	d, err := sp.ClaimContext(ctx, "fixture", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = request.Start(room, d.Envelope); err != nil {
		t.Fatal(err)
	}
	// This is the durable state left by a host that died after execution began.
	cs := connect(t, room)
	call(t, cs, "tincan_send", map[string]any{"agent": "fixture", "body": r.Envelope.Body, "request_id": r.ID})
	result := call(t, cs, "tincan_wait", map[string]any{"request_id": r.ID, "timeout_seconds": 5})
	if result["status"] != "interrupted" || !strings.Contains(result["result"].(string), "not replayed") {
		t.Fatal(result)
	}
}

func TestCancelAndInvalidInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached host unsupported")
	}
	room := t.TempDir()
	cs := connect(t, room)
	call(t, cs, "tincan_send", map[string]any{"agent": "fixture", "body": "slow", "request_id": "slow-job"})
	call(t, cs, "tincan_cancel", map[string]any{"request_id": "slow-job"})
	result := call(t, cs, "tincan_wait", map[string]any{"request_id": "slow-job", "timeout_seconds": 5})
	if result["status"] != "canceled" {
		t.Fatal(result)
	}
	for _, args := range []map[string]any{
		{"name": "../escape"},
		{"name": "fixture", "session_mode": "invalid"},
		{"name": "unknown", "preset": "nonexistent"},
	} {
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "tincan_launch", Arguments: args})
		if err == nil && !result.IsError {
			t.Fatalf("accepted invalid args %+v", args)
		}
	}
}

func TestStdioTransport(t *testing.T) {
	room := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(room, ".config", "tincan")
	if err = os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]host.Preset{"fixture": fixture(t)})
	if err = os.WriteFile(filepath.Join(configDir, "agents.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "mcp", "--room", room)
	cmd.Env = append(os.Environ(), "HOME="+room, "USERPROFILE="+room)
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if runtime.GOOS == "windows" {
		tools, err := cs.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != 8 {
			t.Fatalf("stdio discovery: %+v %v", tools, err)
		}
		return
	}
	defer host.Down(context.Background(), room, "fixture", 3*time.Second)
	call(t, cs, "tincan_send", map[string]any{"agent": "fixture", "body": "over stdio", "request_id": "stdio-job"})
	result := call(t, cs, "tincan_wait", map[string]any{"request_id": "stdio-job", "timeout_seconds": 5})
	if result["result"] != "OVER STDIO" {
		t.Fatal(result)
	}
	call(t, cs, "tincan_stop", map[string]any{"name": "fixture"})
}
