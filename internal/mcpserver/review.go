package mcpserver

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReviewInput struct {
	Committee string `json:"committee" jsonschema:"Committee name"`
	Question  string `json:"question" jsonschema:"What the reviewers should answer; the change itself is attached automatically"`
	Scope     string `json:"scope,omitempty" jsonschema:"uncommitted (default in a Git room), branch, commit:<rev>, range:<a>..<b>, or none"`
	RequestID string `json:"request_id,omitempty" jsonschema:"Optional idempotency key"`
}

type ReviewMember struct {
	Member string `json:"member"`
	State  string `json:"state"`
	Late   bool   `json:"late,omitempty"`
}

type ReviewOutput struct {
	ReviewID string         `json:"review_id"`
	Status   string         `json:"status"`
	Members  []ReviewMember `json:"members"`
}

type ReviewWaitInput struct {
	ReviewID       string `json:"review_id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"0 polls; 1–30 waits"`
}

type ReviewWaitOutput struct {
	ReviewOutput
	Closed     bool   `json:"closed"`
	Settled    bool   `json:"settled"`
	Bundle     string `json:"bundle,omitempty"`
	BundlePath string `json:"bundle_path,omitempty"`
}

func reviewOut(st review.State) ReviewOutput {
	o := ReviewOutput{ReviewID: st.ReviewID, Status: st.Status, Members: []ReviewMember{}}
	for _, m := range st.Members {
		o.Members = append(o.Members, ReviewMember{Member: m.Member, State: m.State, Late: m.Late})
	}
	return o
}

func (s *service) reviewTool(ctx context.Context, req *mcp.CallToolRequest, in ReviewInput) (*mcp.CallToolResult, ReviewOutput, error) {
	_, st, err := review.Publish(ctx, review.PublishRequest{StateDir: s.StateDir, Room: s.Room, Committee: in.Committee,
		Question: in.Question, Scope: in.Scope, RequestID: in.RequestID, Origin: "mcp"})
	if err != nil {
		return nil, ReviewOutput{}, err
	}
	return nil, reviewOut(st), nil
}

func (s *service) reviewWaitTool(ctx context.Context, req *mcp.CallToolRequest, in ReviewWaitInput) (*mcp.CallToolResult, ReviewWaitOutput, error) {
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 30 {
		return nil, ReviewWaitOutput{}, fmt.Errorf("timeout_seconds must be between 0 and 30")
	}
	deadline := time.Now().Add(time.Duration(in.TimeoutSeconds) * time.Second)
	for {
		st, err := review.ReadState(s.Room, in.ReviewID)
		if err != nil {
			return nil, ReviewWaitOutput{}, err
		}
		closed := st.Status == "closed" || st.Status == "cancelled"
		if closed || !time.Now().Before(deadline) {
			out := ReviewWaitOutput{ReviewOutput: reviewOut(st), Closed: closed, Settled: st.Settled}
			if b, ok, _ := review.ReadBundle(s.Room, in.ReviewID); ok {
				if len(b) <= 256<<10 {
					out.Bundle = b
				} else {
					out.BundlePath = filepath.Join(review.Dir(s.Room, in.ReviewID), "bundle.md")
				}
			}
			return nil, out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ReviewWaitOutput{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *service) reviewCancelTool(ctx context.Context, req *mcp.CallToolRequest, in ReviewWaitInput) (*mcp.CallToolResult, ReviewOutput, error) {
	st, err := review.RequestCancel(ctx, s.Room, in.ReviewID)
	if err != nil {
		return nil, ReviewOutput{}, err
	}
	return nil, reviewOut(st), nil
}
