package packet

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// launcherDir holds a copy of the test binary named git, which acts like
// the git.exe that Git for Windows puts on PATH: a launcher that starts the
// real git with its own stdin, stdout and stderr and waits for it. Killing
// the launcher leaves the real git running and holding the pipes.
var launcherDir string

func TestMain(m *testing.M) {
	if mode := os.Getenv("TINCAN_TEST_GIT_CHILD"); mode != "" {
		os.Exit(fakeRealGit(mode))
	}
	if os.Getenv("TINCAN_TEST_GIT_LAUNCHER") != "" {
		os.Exit(fakeGitLauncher())
	}
	dir, err := os.MkdirTemp("", "tincan-fake-git-")
	if err != nil {
		panic(err)
	}
	launcherDir = dir
	code := m.Run()
	os.RemoveAll(dir) // best effort: on Windows a straggler may still run it
	os.Exit(code)
}

func fakeGitLauncher() int {
	self, err := os.Executable()
	if err != nil {
		return 2
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "TINCAN_TEST_GIT_CHILD="+os.Getenv("TINCAN_TEST_GIT_LAUNCHER"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return 1
	}
	return 0
}

// fakeRealGit writes to stdout until a write fails: as fast as it can
// (flood, like a huge diff) or a line every 50 ms (trickle). It gives up
// after two minutes, so that a regression fails the test instead of leaving
// a process behind for good.
func fakeRealGit(mode string) int {
	time.AfterFunc(2*time.Minute, func() { os.Exit(3) })
	line := []byte(strings.Repeat("x", 63) + "\n")
	for {
		if _, err := os.Stdout.Write(line); err != nil {
			return 1
		}
		if mode == "trickle" {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// useGitLauncher puts the launcher first on PATH; the real git it starts
// behaves as mode says.
func useGitLauncher(t *testing.T, mode string) Git {
	t.Helper()
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(launcherDir, name)
	if _, err := os.Stat(path); err != nil {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := copyFile(exe, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", launcherDir)
	t.Setenv("TINCAN_TEST_GIT_LAUNCHER", mode)
	// Not a t.TempDir: on Windows a directory cannot be removed while it is
	// a running process's working directory.
	return Git{Dir: launcherDir}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// within fails the test if f has not returned after a minute, instead of
// letting a hang run into the package timeout.
func within(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Minute):
		t.Fatalf("%s still blocked after a minute", what)
		return nil
	}
}

// A too-large output stops git; the real git behind the launcher must not
// keep the review waiting on the output pipes it inherited.
func TestRunLimitedStopsGitBehindLauncher(t *testing.T) {
	g := useGitLauncher(t, "flood")
	var over bool
	err := within(t, "runLimited", func() error {
		var err error
		_, over, err = g.runLimited(context.Background(), nil, 4096, "diff-tree")
		return err
	})
	if err != nil || !over {
		t.Fatalf("over=%v err=%v", over, err)
	}
}

// Cancelling a command kills the launcher; Run must return without waiting
// for the real git to finish.
func TestRunReturnsWhenCancelledGitBehindLauncherHoldsPipes(t *testing.T) {
	g := useGitLauncher(t, "trickle")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := within(t, "Run", func() error {
		_, err := g.Run(ctx, nil, "status")
		return err
	})
	if err == nil {
		t.Fatal("cancelled git reported success")
	}
}
