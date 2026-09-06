//go:build !windows

package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// StartDetached launches `exe args...` as a daemon: a new session (so it has
// no controlling terminal and never gets the shell's SIGHUP), stdin from
// /dev/null, stdout+stderr appended to logPath, cwd = dir. The returned
// channel yields the child's exit status once it exits — up uses it to fail
// fast when serve dies before parking. The process handle is deliberately
// not released: keeping a Wait running lets the caller reap the child while
// it is still alive; the daemon outlives the caller either way.
func StartDetached(exe string, args []string, dir, logPath string) (*os.Process, <-chan error, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, nil, err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	defer logf.Close() // the child holds its own descriptor after Start
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd.Process, exited, nil
}
