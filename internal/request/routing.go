package request

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/c0ze/tincan/internal/filelock"
	"github.com/c0ze/tincan/internal/fsutil"
	"github.com/c0ze/tincan/internal/spool"
)

// LockRoute serializes an agent's route selection and submission across MCP
// connections. Callers hold it until publication and any hosted launch finish.
// Request result collectors do not acquire it, so an interactive reply remains
// free to complete while another submission chooses its destination.
func LockRoute(ctx context.Context, room, agent string) (*filelock.Lock, error) {
	if err := spool.ValidName(agent); err != nil {
		return nil, err
	}
	return filelock.Acquire(ctx, filepath.Join(Dir(room), "routes", agent+".lock"))
}

// InteractivePending retains routing while a legacy receiver is processing a
// request and consequently has no parked recv presence. The request journal is
// the durable source of truth, including after an MCP reconnect or crash.
//
// Call while holding LockRoute. Directory reads and metadata reads are bounded;
// prompts/results are never loaded. Unreadable relevant metadata is an error,
// never permission to start a different hosted agent under the same name.
func InteractivePending(ctx context.Context, room, agent string) (bool, error) {
	if err := spool.ValidName(agent); err != nil {
		return false, err
	}
	dir := Dir(room)
	if err := fsutil.CheckDir(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		entries, err := f.ReadDir(64)
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if validID(id) != nil {
				continue
			}
			pending, readErr := interactiveHeader(filepath.Join(dir, entry.Name()), id, agent)
			if errors.Is(readErr, os.ErrNotExist) {
				continue // terminal-record GC may remove an entry during the scan
			}
			if readErr != nil {
				return false, fmt.Errorf("read request routing %s: %w", id, readErr)
			}
			if pending {
				return true, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
	}
}

func interactiveHeader(path, id, agent string) (bool, error) {
	f, err := fsutil.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	// save writes routing fields before the potentially large request body.
	// Terminal records can be skipped before their potentially large result.
	decoder := json.NewDecoder(io.LimitReader(f, 16<<10))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false, errors.New("invalid request metadata")
	}
	var foundID, foundAgent, foundStatus bool
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false, err
		}
		key, ok := token.(string)
		if !ok {
			return false, errors.New("invalid request metadata key")
		}
		switch key {
		case "id":
			var value string
			if err := decoder.Decode(&value); err != nil {
				return false, err
			}
			if value != id {
				return false, errors.New("request metadata ID mismatch")
			}
			foundID = true
		case "agent":
			var value string
			if err := decoder.Decode(&value); err != nil {
				return false, err
			}
			if value != agent {
				return false, nil
			}
			foundAgent = true
		case "status":
			var status string
			if err := decoder.Decode(&status); err != nil {
				return false, err
			}
			switch status {
			case "completed", "failed", "interrupted", "canceled":
				return false, nil
			case "queued", "running":
				foundStatus = true
			default:
				return false, errors.New("unknown request status")
			}
		case "interactive":
			var interactive bool
			if err := decoder.Decode(&interactive); err != nil {
				return false, err
			}
			if !foundID || !foundAgent || !foundStatus {
				return false, errors.New("incomplete request routing metadata")
			}
			return interactive, nil
		case "request":
			if !foundID || !foundAgent || !foundStatus {
				return false, errors.New("incomplete request routing metadata")
			}
			return false, nil // Interactive was omitted: native hosted request
		default:
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return false, err
			}
		}
	}
	return false, errors.New("missing request routing metadata")
}
