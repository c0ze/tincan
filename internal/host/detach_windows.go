//go:build windows

package host

import (
	"errors"
	"os"
)

// StartDetached is not available on Windows yet. Making a serve process
// survive its console (new process group, DETACHED_PROCESS, redirected
// stdio) has not been verified on a Windows machine in this iteration, so
// `up` fails loudly here instead of half-working (spec §10).
// `tincan serve <name>` in a terminal works on Windows.
func StartDetached(exe string, args []string, dir, logPath string) (*os.Process, <-chan error, error) {
	return nil, nil, errors.New("detached hosted listeners are not supported on Windows yet; run `tincan serve <name>` in a terminal instead")
}
