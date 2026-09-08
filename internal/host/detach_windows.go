//go:build windows

package host

import (
	"errors"
	"os"
)

// StartDetached is unsupported on Windows. Use `tincan serve <name>` in a
// terminal to run a foreground listener; its agent process tree is managed
// separately through a Windows Job Object.
func StartDetached(exe string, args []string, dir, logPath string) (*os.Process, <-chan error, error) {
	return nil, nil, errors.New("detached hosted listeners are not supported on Windows; run `tincan serve <name>` in a terminal instead")
}
