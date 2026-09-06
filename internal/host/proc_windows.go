//go:build windows

package host

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// childAttr starts the agent in a new process group, mirroring Setpgid on
// unix, so killGroup can address it and its descendants as a tree.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup ends the agent and its descendants with `taskkill /T /F`; if
// taskkill is unavailable or refuses, fall back to killing the direct child.
// Best-effort: not verified on a Windows machine in this iteration.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// Terminate has no SIGTERM equivalent on Windows: the serve process is ended
// outright, so it cannot run its own cleanup — down removes the state file.
func Terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// Kill is the same as Terminate on Windows (there is only one way to stop).
func Kill(pid int) error { return Terminate(pid) }
