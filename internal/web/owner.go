package web

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
)

var tailscaleCLIs = []string{"tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"}

func parseOwner(data []byte) (string, error) {
	var st struct {
		Self struct {
			UserID int64 `json:"UserID"`
		} `json:"Self"`
		User map[string]struct {
			LoginName string `json:"LoginName"`
		} `json:"User"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", err
	}
	login := st.User[strconv.FormatInt(st.Self.UserID, 10)].LoginName
	if login == "" {
		return "", errors.New("tailscale status has no login for this node's owner (is the node tagged?)")
	}
	return login, nil
}

// DetectOwner asks the local Tailscale CLI which user owns this node.
func DetectOwner(ctx context.Context) (string, error) {
	for _, name := range tailscaleCLIs {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, path, "status", "--json").Output()
		if err != nil {
			return "", err
		}
		return parseOwner(out)
	}
	return "", errors.New("tailscale CLI not found; pass --owner <login>")
}
