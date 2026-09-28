package reviewjob

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tincanBin is a tincan binary built for these tests; detached hosts are
// started from it.
var tincanBin string

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fake-review" {
		os.Exit(fakeReview(os.Args[2:]))
	}
	dir, err := os.MkdirTemp("", "tincan-bin-")
	if err != nil {
		panic(err)
	}
	tincanBin = filepath.Join(dir, "tincan")
	if out, err := exec.Command("go", "build", "-o", tincanBin, "../../cmd/tincan").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building tincan: %v\n%s", err, out)
		tincanBin = ""
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeReview is the reviewer: mode echo (default) appends a line to
// runs.log in its working directory and prints the prompt; pwd prints the
// working directory; sleep sleeps 30 s.
func fakeReview(args []string) int {
	mode := ""
	if len(args) > 0 {
		mode, args = args[0], args[1:]
	}
	switch mode {
	case "sleep":
		time.Sleep(30 * time.Second)
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Print(wd)
	default:
		f, _ := os.OpenFile("runs.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintln(f, "run")
		f.Close()
		fmt.Printf("REVIEW: %s", strings.Join(args, " "))
	}
	return 0
}
