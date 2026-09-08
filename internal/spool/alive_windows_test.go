//go:build windows

package spool

import (
	"io"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProcessAliveExitedProcessWithOpenHandle(t *testing.T) {
	if os.Getenv("TINCAN_TEST_WAIT_STDIN") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessAliveExitedProcessWithOpenHandle$")
	cmd.Env = append(os.Environ(), "TINCAN_TEST_WAIT_STDIN=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if !processAlive(pid) {
		t.Fatal("running child reported dead")
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Keeping h open pins this process object and prevents PID reuse. Opening
	// the process can still succeed, but its signaled state proves it exited.
	if processAlive(pid) {
		t.Fatal("exited child with a retained handle reported alive")
	}
}
