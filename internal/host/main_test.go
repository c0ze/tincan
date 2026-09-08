package host

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the fake agent: when TINCAN_FAKE_AGENT=1 is in the
// environment the test binary is being exec'd by the code under test, and it
// behaves like a tiny headless CLI (see fakeAgent) instead of running tests.
// Tests set the variable with t.Setenv so the child inherits it; the parent
// test process itself never has it set when it starts.
func TestMain(m *testing.M) {
	if os.Getenv("TINCAN_FAKE_AGENT") == "1" {
		os.Exit(fakeAgent(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// Low-level file helpers expect the canonical room used by the host boundary.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeAgent modes (args[0]):
//
//	echo <text…>            stdout "echo: <text>\n", exit 0
//	stdin                   stdout "stdin: " + all of stdin + "\n", exit 0
//	stdout <text>           stdout verbatim text, exit 0
//	fail <text…>            stderr "boom: <text>\n", stdout "partial", exit 3
//	sleep <sec> [pidfile]   write own pid to pidfile, then sleep <sec>
//	outfile <path> <text…>  write "file: <text>" to path, stdout "noise on stdout"
//	cwd                     stdout os.Getwd()
func fakeAgent(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake agent: no mode")
		return 2
	}
	switch args[0] {
	case "volume":
		fmt.Fprint(os.Stdout, strings.Repeat("o", 2*MaxCapturedOutput))
		fmt.Fprint(os.Stderr, strings.Repeat("e", 2*MaxCapturedOutput))
	case "progress":
		fmt.Println("first chunk")
		time.Sleep(30 * time.Second)
	case "fork", "fork-pipes":
		exe, _ := os.Executable()
		child := exec.Command(exe, "sleep", "30")
		if args[0] == "fork-pipes" {
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
		}
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			return 1
		}
	case "echo":
		fmt.Printf("echo: %s\n", strings.Join(args[1:], " "))
	case "stdout":
		fmt.Print(args[1])
	case "stdin":
		data, _ := io.ReadAll(os.Stdin)
		fmt.Printf("stdin: %s\n", data)
	case "fail":
		fmt.Fprintf(os.Stderr, "boom: %s\n", strings.Join(args[1:], " "))
		fmt.Print("partial")
		return 3
	case "sleep":
		secs, _ := strconv.Atoi(args[1])
		if len(args) > 2 {
			if err := os.WriteFile(args[2], []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		time.Sleep(time.Duration(secs) * time.Second)
	case "outfile":
		if err := os.WriteFile(args[1], []byte("file: "+strings.Join(args[2:], " ")), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Print("noise on stdout")
	case "cwd":
		wd, _ := os.Getwd()
		fmt.Print(wd)
	default:
		fmt.Fprintf(os.Stderr, "fake agent: unknown mode %q\n", args[0])
		return 2
	}
	return 0
}

// fakeExec returns the exec template that runs this test binary as the fake
// agent in the given mode. Callers must t.Setenv("TINCAN_FAKE_AGENT", "1").
func fakeExec(mode string, rest ...string) []string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return append([]string{exe, mode}, rest...)
}

// waitFor polls cond every 20ms until it is true or d elapses.
func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readPID reads the pid the fake agent's sleep mode wrote to path, waiting
// briefly for the file to appear.
func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	ok := waitFor(func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	}, 5*time.Second)
	if !ok {
		t.Fatalf("fake agent never wrote its pid to %s", path)
	}
	return pid
}
