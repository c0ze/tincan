//go:build !windows

package host

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

// runProcess leaves the child unreaped until its entire group is killed.
// The unreaped group leader pins the PID/PGID and prevents a reuse race.
// This also cleans up descendants when their direct parent exits normally.
func runProcess(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- waitChildExit(cmd.Process.Pid) }()
	var observeErr error
	observed := false
	select {
	case observeErr = <-exited:
		observed = true
	case <-ctx.Done():
	}
	killErr := killGroup(cmd)
	if !observed {
		<-exited
	}
	err := cmd.Wait()
	if err != nil {
		return err
	}
	return errors.Join(observeErr, killErr)
}

// childAttr puts the agent in its own process group (pgid == its pid) so a
// timeout or shutdown can kill it together with everything it spawned,
// without touching the serve process that started it.
func childAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// killGroup SIGKILLs the whole process group of cmd's process. A group that
// is already gone (ESRCH) is success; any other failure falls back to killing
// just the direct child so a run can never hang on an unkillable group.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil || err == syscall.ESRCH {
		return nil
	}
	return cmd.Process.Kill()
}
