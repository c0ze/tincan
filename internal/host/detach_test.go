package host

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStartDetachedRunsChildWithStdioInLog(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".tincan", "hosts", "agent.log")
	proc, exited, err := StartDetached(exe, []string{"echo", "detached"}, dir, logPath)
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "tincan serve") {
			t.Fatalf("want a loud unsupported error naming `tincan serve` on Windows, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if proc == nil || proc.Pid <= 0 {
		t.Fatalf("no process handle: %+v", proc)
	}
	select {
	case werr := <-exited:
		if werr != nil {
			t.Fatalf("child exited with %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "echo: detached") {
		t.Fatalf("child stdout not in log: %q", data)
	}
}
