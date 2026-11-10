package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// `send --to Codex` then `recv --as codex` on a case-folding filesystem used to
// time out and quarantine the message. The second spelling is now refused by
// name and the message stays deliverable under the first.
func TestCaseAliasedRecvIsRefusedAndMessageKept(t *testing.T) {
	room := t.TempDir()
	if code, _, err := run("send", "--room", room, "--to", "Codex", "--from", "orch", "--body", "kept"); code != 0 {
		t.Fatal(err)
	}
	upper, err1 := os.Lstat(filepath.Join(room, ".tincan", "inbox", "Codex"))
	lower, err2 := os.Lstat(filepath.Join(room, ".tincan", "inbox", "codex"))
	if err1 != nil || err2 != nil || !os.SameFile(upper, lower) {
		t.Skip("case-sensitive filesystem")
	}
	code, _, stderr := run("recv", "--room", room, "--as", "codex", "--timeout", "1")
	if code != ExitError || !strings.Contains(stderr, `use "Codex"`) {
		t.Fatalf("aliased recv: exit %d, stderr=%q", code, stderr)
	}
	code, out, stderr := run("recv", "--room", room, "--as", "Codex", "--timeout", "1", "--format", "body")
	if code != 0 || out != "kept\n" {
		t.Fatalf("message lost: %d %q %q", code, out, stderr)
	}
}

// cliProcess starts this test binary as the tincan CLI (see TestMain).
func cliProcess(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "TINCAN_AS_CLI=1")
	return cmd
}

// waitForToken waits until a receiver parked as name has published its
// presence token.
func waitForToken(t *testing.T, room, name string) {
	t.Helper()
	dir := filepath.Join(room, ".tincan", "present", name)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no presence token under %s", dir)
}

// A parked recv or ask stopped by SIGINT, SIGTERM or SIGHUP removes its own
// presence token before exiting, instead of leaving it for a later process
// that reuses its PID to masquerade as a listener.
func TestSignaledReceiveClearsItsPresence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals")
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		for _, command := range []string{"recv", "ask"} {
			t.Run(command+"-"+sig.String(), func(t *testing.T) {
				room := t.TempDir()
				args := []string{"recv", "--room", room, "--as", "parked", "--timeout", "0"}
				name := "parked"
				if command == "ask" {
					args = []string{"ask", "--room", room, "--to", "worker", "--from", "orch", "--body", "q", "--timeout", "0"}
					name = ""
				}
				cmd := cliProcess(t, args...)
				var stdout bytes.Buffer
				cmd.Stdout = &stdout
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				if name == "" {
					// ask parks on its own r-<id> reply channel.
					deadline := time.Now().Add(10 * time.Second)
					for name == "" && time.Now().Before(deadline) {
						entries, _ := os.ReadDir(filepath.Join(room, ".tincan", "present"))
						for _, ent := range entries {
							if strings.HasPrefix(ent.Name(), "r-") {
								name = ent.Name()
							}
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				waitForToken(t, room, name)
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				err := cmd.Wait()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != ExitError {
					t.Fatalf("exit = %v, want code %d", err, ExitError)
				}
				if entries, _ := os.ReadDir(filepath.Join(room, ".tincan", "present", name)); len(entries) != 0 {
					t.Fatalf("token left behind: %v", entries)
				}
				if command == "ask" && !strings.Contains(stdout.String(), "pending channel="+name) {
					t.Fatalf("interrupted ask did not name its channel: %q", stdout.String())
				}
			})
		}
	}
}

// `tincan recv | true` used to die of SIGPIPE between claim and Ack and strand
// the message in flight. The write now fails, the claim is requeued, and the
// next receive gets it.
func TestReceiveIntoClosedPipeRequeues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGPIPE")
	}
	room := t.TempDir()
	if code, _, err := run("send", "--room", room, "--to", "worker", "--from", "test", "--body", "recover me"); code != 0 {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	cmd := cliProcess(t, "recv", "--room", room, "--as", "worker", "--timeout", "5")
	cmd.Stdout = w
	err = cmd.Run()
	w.Close()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != ExitError {
		t.Fatalf("recv into a closed pipe: %v, want exit %d", err, ExitError)
	}
	code, out, stderr := run("recv", "--room", room, "--as", "worker", "--timeout", "1", "--format", "body")
	if code != 0 || out != "recover me\n" {
		t.Fatalf("message stranded: %d %q %q", code, out, stderr)
	}
}

func TestFailedOutputLeavesMessageAvailable(t *testing.T) {
	for _, format := range []string{"body", "json"} {
		t.Run(format, func(t *testing.T) {
			room := t.TempDir()
			if code, _, err := run("send", "--room", room, "--to", "worker", "--from", "test", "--body", "recover me"); code != 0 {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			if code := Run([]string{"recv", "--room", room, "--as", "worker", "--timeout", "1", "--format", format}, brokenOutput{}, &stderr); code != ExitError {
				t.Fatalf("got %d", code)
			}
			code, out, err := run("recv", "--room", room, "--as", "worker", "--timeout", "1", "--format", "body")
			if code != 0 || out != "recover me\n" {
				t.Fatalf("message lost: %d %q %q", code, out, err)
			}
		})
	}
}
