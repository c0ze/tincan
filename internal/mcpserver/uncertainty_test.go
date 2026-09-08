package mcpserver_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/mcpserver"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProgressWaitCollectsReplyAfterPublicationUncertainty(t *testing.T) {
	room := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := request.SubmitInteractive(ctx, room, "legacy", "mcp", "one task", "uncertain-job")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(room)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sp.Recv("legacy", time.Second, false); err != nil {
		t.Fatal(err)
	}
	// Model a submitter that exited after dispatch but before confirmation.
	r.InteractivePublished = false
	data, _ := json.Marshal(r)
	if err = fsutil.WriteFileAtomic(filepath.Join(request.Dir(room), r.ID+".json"), data); err != nil {
		t.Fatal(err)
	}
	if _, err = request.SubmitInteractive(ctx, room, r.Agent, "mcp", "one task", r.ID); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, room)
	status := call(t, cs, "tincan_status", map[string]any{"request_id": r.ID})
	if status["request"].(map[string]any)["outcome_uncertain"] != true {
		t.Fatalf("missing uncertainty marker: %+v", status)
	}
	params := &mcp.CallToolParams{Name: "tincan_wait", Arguments: map[string]any{"request_id": r.ID, "timeout_seconds": 2}}
	params.SetProgressToken("uncertain-progress")
	type response struct {
		result *mcp.CallToolResult
		err    error
	}
	done := make(chan response, 1)
	go func() {
		result, err := cs.CallTool(ctx, params)
		done <- response{result, err}
	}()
	select {
	case early := <-done:
		t.Fatalf("bounded wait ended before collecting evidence: %+v", early)
	case <-time.After(100 * time.Millisecond):
	}
	if err := sp.Send(&envelope.Envelope{ID: envelope.NewID(), From: r.Agent, To: r.Envelope.ReplyTo, TS: time.Now().UTC(), Body: "actual answer"}); err != nil {
		t.Fatal(err)
	}
	var got response
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got.err != nil || got.result.IsError {
		t.Fatalf("wait: %+v", got)
	}
	data, _ = json.Marshal(got.result.StructuredContent)
	var result mcpserver.RequestView
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Result != "actual answer" || result.Status != "completed" || result.OutcomeUncertain {
		t.Fatalf("actual reply did not resolve uncertainty: %+v", result)
	}
}
