// Package cli implements the tincan command-line interface.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/spool"
)

// Exit codes (CLI contract; skills branch on these).
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitTimeout = 3
)

const usageText = `tincan — local message passing between AI coding agents

Usage:
  tincan send  --to <name> --from <name> (--body <s> | --body-file <f>) [flags]
  tincan recv  --as <name> [--timeout <sec>] [--format json|body] [--log] [flags]
  tincan ask   --to <name> --from <name> (--body <s> | --body-file <f>) [--timeout <sec>] [flags]
  tincan reply --channel <id> (--body <s> | --body-file <f>) [--from <name>] [flags]

Common flags:
  --room <path>       room directory (default: current directory)
  --artifact <path>   artifact pointer, repeatable (send/ask/reply)

Exit codes: 0 ok, 1 error, 2 usage, 3 timeout.
`

// Run executes a tincan command and returns its exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}
	switch args[0] {
	case "send":
		return cmdSend(args[1:], stdout, stderr)
	case "recv":
		return cmdRecv(args[1:], stdout, stderr)
	case "ask":
		return cmdAsk(args[1:], stdout, stderr)
	case "reply":
		return cmdReply(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "tincan: unknown command %q\n%s", args[0], usageText)
		return ExitUsage
	}
}

// stringList is a repeatable string flag (--artifact a --artifact b).
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// bodyFrom resolves --body / --body-file (exactly one required).
func bodyFrom(body, bodyFile string) (string, error) {
	switch {
	case body != "" && bodyFile != "":
		return "", errors.New("use --body or --body-file, not both")
	case body != "":
		return body, nil
	case bodyFile != "":
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", err
		}
		return string(data), nil
	default:
		return "", errors.New("--body or --body-file is required (--body must be non-empty)")
	}
}

func printEnvelope(e *envelope.Envelope, format string, stdout, stderr io.Writer) int {
	if format == "body" {
		fmt.Fprintln(stdout, e.Body)
		return ExitOK
	}
	data, err := envelope.Marshal(e)
	if err != nil {
		fmt.Fprintf(stderr, "tincan: %v\n", err)
		return ExitError
	}
	fmt.Fprintln(stdout, string(data))
	return ExitOK
}

func cmdSend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		to       = fs.String("to", "", "recipient name")
		from     = fs.String("from", "", "sender name")
		room     = fs.String("room", ".", "room directory")
		corr     = fs.String("corr", "", "correlation id")
		replyTo  = fs.String("reply-to", "", "reply channel")
		body     = fs.String("body", "", "message body")
		bodyFile = fs.String("body-file", "", "read body from file")
	)
	var artifacts stringList
	fs.Var(&artifacts, "artifact", "artifact path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *to == "" || *from == "" {
		fmt.Fprintln(stderr, "tincan send: --to and --from are required")
		return ExitUsage
	}
	b, err := bodyFrom(*body, *bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tincan send: %v\n", err)
		return ExitUsage
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan send: %v\n", err)
		return ExitError
	}
	e := &envelope.Envelope{
		ID: envelope.NewID(), CorrID: *corr, From: *from, To: *to,
		ReplyTo: *replyTo, TS: time.Now().UTC(), Body: b, Artifacts: artifacts,
	}
	if err := sp.Send(e); err != nil {
		fmt.Fprintf(stderr, "tincan send: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "sent id=%s to=%s\n", e.ID, e.To)
	return ExitOK
}

func cmdRecv(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		as      = fs.String("as", "", "my inbox name")
		room    = fs.String("room", ".", "room directory")
		timeout = fs.Int("timeout", 570, "seconds to wait")
		format  = fs.String("format", "json", "output format: json|body")
		logMsgs = fs.Bool("log", false, "keep consumed messages in .tincan/log/")
	)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *as == "" {
		fmt.Fprintln(stderr, "tincan recv: --as is required")
		return ExitUsage
	}
	if *format != "json" && *format != "body" {
		fmt.Fprintln(stderr, "tincan recv: --format must be json or body")
		return ExitUsage
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan recv: %v\n", err)
		return ExitError
	}
	e, err := sp.Recv(*as, time.Duration(*timeout)*time.Second, *logMsgs)
	if errors.Is(err, spool.ErrTimeout) {
		return ExitTimeout
	}
	if err != nil {
		fmt.Fprintf(stderr, "tincan recv: %v\n", err)
		return ExitError
	}
	return printEnvelope(e, *format, stdout, stderr)
}

func cmdAsk(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "tincan ask: not implemented yet")
	return ExitError
}

func cmdReply(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "tincan reply: not implemented yet")
	return ExitError
}
