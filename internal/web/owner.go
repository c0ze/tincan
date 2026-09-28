package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

var tailscaleCLIs = []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"}

func parseOwner(data []byte) (string, error) {
	login, _, err := parseNode(data)
	return login, err
}

// parseNode extracts this node's owner login and its Tailscale DNS name
// (trailing dot removed) from `tailscale status --json` output.
func parseNode(data []byte) (string, string, error) {
	var st struct {
		Self struct {
			UserID  int64  `json:"UserID"`
			DNSName string `json:"DNSName"`
		} `json:"Self"`
		User map[string]struct {
			LoginName string `json:"LoginName"`
		} `json:"User"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", "", err
	}
	login := st.User[strconv.FormatInt(st.Self.UserID, 10)].LoginName
	if login == "" {
		return "", "", errors.New("tailscale status has no login for this node's owner (is the node tagged?)")
	}
	return login, strings.TrimSuffix(st.Self.DNSName, "."), nil
}

// DetectOwner asks the local Tailscale CLI which user owns this node.
func DetectOwner(ctx context.Context) (string, error) {
	login, _, err := DetectNode(ctx)
	return login, err
}

// DetectNode asks the local Tailscale CLI, in one `tailscale status --json`
// call, which user owns this node and what its Tailscale DNS name is (e.g.
// "host.tailnet.ts.net"; "" when the status carries none).
func DetectNode(ctx context.Context) (owner, dnsName string, err error) {
	for _, name := range tailscaleCLIs {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, path, "status", "--json").Output()
		if err != nil {
			stderr := ""
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				stderr = strings.TrimSpace(string(exitErr.Stderr))
			}
			return "", "", fmt.Errorf("%s status --json: %v: %s; pass --owner <login>", path, err, stderr)
		}
		return parseNode(out)
	}
	return "", "", errors.New("tailscale CLI not found; pass --owner <login>")
}
