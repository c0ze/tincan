//go:build !windows

package host

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

// Unix fake agent modes whose descendant leaves the agent's process group by
// starting a new session, as a daemonizing tool or MCP server might:
//
//	escape <pidfile>   the escaped child inherits stdout; print ANSWER, exit 0
func init() {
	platformFakeModes["escape"] = func(args []string) int {
		exe, _ := os.Executable()
		child := exec.Command(exe, "sleep", "10")
		child.Stdout = os.Stdout
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(args[0], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			return 1
		}
		fmt.Println("ANSWER")
		return 0
	}
}

// killEscaped SIGKILLs a test's escaped descendant if it is still running.
func killEscaped(t *testing.T, pidfile string) {
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidfile); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}
