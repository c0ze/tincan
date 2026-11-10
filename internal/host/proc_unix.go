//go:build !windows

package host

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// stopGrace bounds how long a cancelled or timed-out agent may take to exit
// after SIGTERM before its whole group is killed.
const stopGrace = 2 * time.Second

// runProcess leaves the child unreaped until its entire group is killed.
// The unreaped group leader pins the PID/PGID and prevents a reuse race.
// This also cleans up descendants when their direct parent exits normally.
//
// Stopping a running agent (cancel, timeout, shutdown) sends SIGTERM to its
// group first and SIGKILL once the agent exits or stopGrace elapses: only the
// agent can reach descendants that left its group (their own session or
// group), and it needs its shutdown path to do so. A descendant that left the
// group and ignores its parent's death remains out of reach.
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
		if syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) == nil {
			grace := time.NewTimer(stopGrace)
			select {
			case observeErr = <-exited:
				observed = true
			case <-grace.C:
			}
			grace.Stop()
		}
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
