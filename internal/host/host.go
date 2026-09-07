// Package host implements hosted listeners: a detached `tincan serve`
// process that parks on an inbox, runs a configured headless agent CLI once
// per message with the body as the prompt, and sends the output back as the
// reply. See PROTOCOL.md "Hosted listeners".
package host

import (
	"os"
	"path/filepath"
)

// Dir is <room>/.tincan/hosts, where state files and host logs live.
func Dir(room string) string {
	if canonical, err := filepath.EvalSymlinks(room); err == nil {
		room = canonical
	}
	return filepath.Join(room, ".tincan", "hosts")
}

func canonicalRoom(room string) (string, error) {
	abs, err := filepath.Abs(room)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// StatePath is the state file of the hosted listener name (see State).
func StatePath(room, name string) string { return filepath.Join(Dir(room), name+".json") }

// LogPath is the host log of the hosted listener name.
func LogPath(room, name string) string { return filepath.Join(Dir(room), name+".log") }
