package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/review"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func cmdReview(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		committee = fs.String("committee", "", "committee name")
		question  = fs.String("question", "", "what reviewers should answer")
		qfile     = fs.String("question-file", "", "read the question from a file")
		scope     = fs.String("scope", "", "uncommitted (default in Git), branch, commit:<rev>, range:<a>..<b>, none")
		requestID = fs.String("request-id", "", "idempotency key")
		room      = fs.String("room", ".", "room directory")
		wait      = fs.Bool("wait", false, "wait for the review to close and print its bundle")
		cancel    = fs.Bool("cancel", false, "cancel the review named by the argument")
		timeout   = fs.Int("timeout", 3600, "seconds to wait with --wait")
	)
	// Accept flags after the review id too (tincan review --wait <id> --timeout 60),
	// stopping flag parsing once "--" appears.
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return ExitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		// If fs.Parse stopped at "--", remaining args are all positional.
		parsed := len(args) - fs.NArg()
		if parsed > 0 && args[parsed-1] == "--" {
			positional = append(positional, fs.Args()...)
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if *timeout < 0 {
		fmt.Fprintln(stderr, "tincan review: --timeout must be >= 0")
		return ExitUsage
	}
	if err := requireRoom(*room); err != nil {
		fmt.Fprintf(stderr, "tincan review: %v\n", err)
		return ExitError
	}
	if w := roomRootWarning(*room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The review store refuses symlinked paths (macOS /tmp, /var, a
	// symlinked $PWD); Publish canonicalizes, so wait and cancel must too.
	canonical, err := rooms.Canonical(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan review: %v\n", err)
		return ExitError
	}
	*room = canonical
	if *cancel {
		if len(positional) != 1 {
			fmt.Fprintln(stderr, "tincan review: --cancel takes a review id")
			return ExitUsage
		}
		if _, err := review.RequestCancel(ctx, *room, positional[0]); err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ExitError
		}
		return ExitOK
	}
	id := ""
	if *wait && len(positional) == 1 && *committee == "" {
		id = positional[0]
	} else {
		if *committee == "" || len(positional) != 0 {
			fmt.Fprintln(stderr, "tincan review: --committee and --question (or --question-file) are required")
			return ExitUsage
		}
		q, ec, err := bodyFromFlags(*question, *qfile, "--question", "--question-file")
		if err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ec
		}
		in, _, err := review.Publish(ctx, review.PublishRequest{StateDir: rooms.StateDir(), Room: *room, Committee: *committee,
			Question: q, Scope: *scope, RequestID: *requestID, Origin: "cli"})
		if err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ExitError
		}
		id = in.ReviewID
		fmt.Fprintln(stdout, id)
		if !*wait {
			return ExitOK
		}
	}
	return waitReview(ctx, *room, id, time.Duration(*timeout)*time.Second, stdout, stderr)
}

func waitReview(ctx context.Context, room, id string, timeout time.Duration, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(timeout)
	for {
		st, err := review.ReadState(room, id)
		if err != nil {
			fmt.Fprintf(stderr, "tincan review: %v\n", err)
			return ExitError
		}
		if st.Status == "closed" || st.Status == "cancelled" {
			degraded := false
			for _, m := range st.Members {
				fmt.Fprintf(stderr, "%s: %s%s\n", m.Member, m.State, map[bool]string{true: " (late)"}[m.Late])
				degraded = degraded || m.Late || m.State == "unreachable"
			}
			b, ok, err := review.ReadBundle(room, id)
			if errors.Is(err, fsutil.ErrTooLarge) {
				fmt.Fprintf(stdout, "bundle_path=%s\n", filepath.Join(review.Dir(room, id), "bundle.md"))
			} else if err != nil {
				fmt.Fprintf(stderr, "tincan review: read bundle: %v\n", err)
				return ExitError
			} else if ok {
				fmt.Fprint(stdout, b)
			}
			if degraded {
				return ExitReviewDegraded
			}
			return ExitOK
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "tincan review: %s still %s; reattach with tincan review --wait %s\n", id, st.Status, id)
			return ExitTimeout
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "tincan review: interrupted")
			return ExitError
		case <-time.After(time.Second):
		}
	}
}
