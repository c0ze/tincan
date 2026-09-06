//go:build !windows

package host

import (
	"os/exec"
	"syscall"
)

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

// Terminate asks a hosted serve process to wind down (SIGTERM): it kills any
// in-flight agent, removes its state file and exits 0.
func Terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// Kill forcibly ends a serve process that ignored Terminate.
func Kill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
