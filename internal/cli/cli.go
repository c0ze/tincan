// Package cli implements the tincan command-line interface.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/c0ze/tincan/internal/envelope"
	"github.com/c0ze/tincan/internal/host"
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
  tincan send   --to <name> --from <name> (--body <s> | --body-file <f>) [flags]
  tincan recv   --as <name> [--timeout <sec>] [--format json|body] [--log] [flags]
  tincan ask    --to <name> --from <name> (--body <s> | --body-file <f>) [--timeout <sec>] [flags]
  tincan reply  --channel <id> (--body <s> | --body-file <f>) [--from <name>] [flags]
  tincan status [--format table|json] [flags]
  tincan ping   --to <name> [flags]
  tincan stop   --to <name> --from <name> [flags]

Hosted listeners (tincan runs a headless agent CLI for you):
  tincan up      <name> [--preset <p>] [--exec '<tpl>'] [--stdin body|none] [--reply stdout|file]
                        [--exec-timeout <sec>] [--wait <sec>] [flags]
  tincan serve   <name> [same flags as up] [--daemon]
  tincan down    <name> [--wait <sec>] [flags]
  tincan presets [--format table|json]

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
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "ping":
		return cmdPing(args[1:], stdout, stderr)
	case "stop":
		return cmdStop(args[1:], stdout, stderr)
	case "up":
		return cmdUp(args[1:], stdout, stderr)
	case "serve":
		return cmdServe(args[1:], stdout, stderr)
	case "down":
		return cmdDown(args[1:], stdout, stderr)
	case "presets":
		return cmdPresets(args[1:], stdout, stderr)
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

// bodyFrom resolves --body / --body-file (exactly one required). On error the
// second return value is the exit code: ExitUsage for flag mistakes,
// ExitError for I/O failures reading --body-file.
func bodyFrom(body, bodyFile string) (string, int, error) {
	switch {
	case body != "" && bodyFile != "":
		return "", ExitUsage, errors.New("use --body or --body-file, not both")
	case body != "":
		return body, ExitOK, nil
	case bodyFile != "":
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", ExitError, err
		}
		return string(data), ExitOK, nil
	default:
		return "", ExitUsage, errors.New("--body or --body-file is required (--body must be non-empty)")
	}
}

// roomRootWarning returns a non-empty advisory string iff room is inside a
// git repo but is not that repo's root — the "cd into a subdir silently
// creates a second room" footgun. It never errors: any failure to stat while
// walking (permissions, races) is treated as "not a root" and yields "".
//
// A directory counts as a repo root if it has a `.git` entry, whether that
// entry is a directory (a normal clone) or a file (a git worktree, whose
// `.git` is a pointer file to the real git-dir elsewhere).
func roomRootWarning(room string) string {
	abs, err := filepath.Abs(room)
	if err != nil {
		return ""
	}
	if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
		return "" // room is itself a repo root
	}
	dir := abs
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached filesystem root; not inside a repo
		}
		dir = parent
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			// Floor: don't warn when the discovered gitroot is $HOME or an
			// ancestor of $HOME (e.g. a dotfiles repo at ~/.git, or / or
			// /home). Advising "pass --room ~" would be actively wrong.
			// Best-effort: if home can't be determined, don't floor.
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				if rel, err := filepath.Rel(dir, home); err == nil {
					goesUp := rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
					if !goesUp { // home == dir or nested under it ⇒ dir is $HOME or an ancestor
						return ""
					}
				}
			}
			return fmt.Sprintf(
				"tincan: warning: room %s is inside a repo but not its root; "+
					"peers using the repo root won't see these messages (pass --room %s)",
				abs, dir)
		}
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
	b, ec, err := bodyFrom(*body, *bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tincan send: %v\n", err)
		return ec
	}
	if w := roomRootWarning(*room); w != "" {
		fmt.Fprintln(stderr, w)
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
		timeout = fs.Int("timeout", 570, "seconds to wait; 0 or negative blocks forever (no re-arm cost)")
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
	if w := roomRootWarning(*room); w != "" {
		fmt.Fprintln(stderr, w)
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
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		to       = fs.String("to", "", "recipient name")
		from     = fs.String("from", "", "sender name")
		room     = fs.String("room", ".", "room directory")
		timeout  = fs.Int("timeout", 570, "seconds to wait for the reply")
		format   = fs.String("format", "json", "output format: json|body")
		body     = fs.String("body", "", "message body")
		bodyFile = fs.String("body-file", "", "read body from file")
	)
	var artifacts stringList
	fs.Var(&artifacts, "artifact", "artifact path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *to == "" || *from == "" {
		fmt.Fprintln(stderr, "tincan ask: --to and --from are required")
		return ExitUsage
	}
	if *format != "json" && *format != "body" {
		fmt.Fprintln(stderr, "tincan ask: --format must be json or body")
		return ExitUsage
	}
	b, ec, err := bodyFrom(*body, *bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ec
	}
	if w := roomRootWarning(*room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ExitError
	}
	channel := "r-" + envelope.NewID()
	e := &envelope.Envelope{
		ID: envelope.NewID(), CorrID: channel, From: *from, To: *to,
		ReplyTo: channel, TS: time.Now().UTC(), Body: b, Artifacts: artifacts,
	}
	if err := sp.Send(e); err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ExitError
	}
	reply, err := sp.Recv(channel, time.Duration(*timeout)*time.Second, false)
	if errors.Is(err, spool.ErrTimeout) {
		// Leave the channel in place: a late reply still lands there and is
		// collected with `tincan recv --as <channel>`.
		fmt.Fprintf(stdout, "pending channel=%s\n", channel)
		return ExitTimeout
	}
	if err != nil {
		fmt.Fprintf(stderr, "tincan ask: %v\n", err)
		return ExitError
	}
	code := printEnvelope(reply, *format, stdout, stderr)
	if err := sp.RemoveInbox(channel); err != nil {
		// Non-fatal: the answer is already delivered; a leaked r-<id> channel
		// is reclaimed by the Phase-2 gc sweep.
		fmt.Fprintf(stderr, "tincan ask: cleanup warning: %v\n", err)
	}
	return code
}

func cmdReply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		channel  = fs.String("channel", "", "reply channel (the request's reply_to)")
		from     = fs.String("from", "", "sender name (optional)")
		room     = fs.String("room", ".", "room directory")
		body     = fs.String("body", "", "message body")
		bodyFile = fs.String("body-file", "", "read body from file")
	)
	var artifacts stringList
	fs.Var(&artifacts, "artifact", "artifact path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *channel == "" {
		fmt.Fprintln(stderr, "tincan reply: --channel is required")
		return ExitUsage
	}
	b, ec, err := bodyFrom(*body, *bodyFile)
	if err != nil {
		fmt.Fprintf(stderr, "tincan reply: %v\n", err)
		return ec
	}
	if w := roomRootWarning(*room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan reply: %v\n", err)
		return ExitError
	}
	e := &envelope.Envelope{
		ID: envelope.NewID(), CorrID: *channel, From: *from, To: *channel,
		TS: time.Now().UTC(), Body: b, Artifacts: artifacts,
	}
	if err := sp.Send(e); err != nil {
		fmt.Fprintf(stderr, "tincan reply: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "replied channel=%s\n", *channel)
	return ExitOK
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		roomFlag = fs.String("room", ".", "room directory")
		format   = fs.String("format", "table", "output format: table|json")
	)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *format != "table" && *format != "json" {
		fmt.Fprintln(stderr, "tincan status: --format must be table or json")
		return ExitUsage
	}
	room, err := filepath.Abs(*roomFlag)
	if err != nil {
		fmt.Fprintf(stderr, "tincan status: %v\n", err)
		return ExitError
	}
	if w := roomRootWarning(room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	sp, err := spool.Open(room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan status: %v\n", err)
		return ExitError
	}
	rows, err := statusRows(sp, room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan status: %v\n", err)
		return ExitError
	}
	if *format == "json" {
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "tincan status: %v\n", err)
			return ExitError
		}
		fmt.Fprintln(stdout, string(data))
		return ExitOK
	}
	printStatusTable(rows, stdout)
	return ExitOK
}

// statusRow is one `status` line: the spool presence plus the hosted-listener
// view from <room>/.tincan/hosts/<name>.json. Mode is "hosted:<preset>" for a
// live hosted listener, "agent" for a plain parked listener (an interactive
// /listen session), and "" when nothing is listening.
type statusRow struct {
	spool.Presence
	Mode      string `json:"mode"`
	Preset    string `json:"preset,omitempty"`
	Busy      bool   `json:"busy"`
	CurrentID string `json:"current_id,omitempty"`
}

// statusRows joins presence with live hosted state. A state file whose pid is
// dead is stale (serve was killed without cleanup) and is ignored, so the row
// falls back to plain presence and never reports a phantom "busy". A hosted
// listener that is busy is not parked (no presence), so its pid comes from
// the state file.
func statusRows(sp *spool.Spool, room string) ([]statusRow, error) {
	list, err := sp.ListPresence()
	if err != nil {
		return nil, err
	}
	states, err := host.ListStates(room)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	rows := make([]statusRow, 0, len(list)+len(states))
	for _, p := range list {
		seen[p.Name] = true
		rows = append(rows, statusRow{Presence: p})
	}
	for name := range states {
		if seen[name] {
			continue
		}
		p, _, err := sp.Present(name)
		if err != nil {
			continue // not a legal participant name; nothing else could be listening as it
		}
		rows = append(rows, statusRow{Presence: p})
	}
	for i := range rows {
		r := &rows[i]
		switch st, ok := states[r.Name]; {
		case ok && st.Alive():
			r.Mode = "hosted:" + st.Preset
			r.Preset = st.Preset
			r.PID = st.PID
			r.Busy = st.State == "busy"
			r.CurrentID = st.CurrentID
		case r.Alive:
			r.Mode = "agent"
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// printStatusTable renders rows as a tab-aligned table: name, queued count,
// mode (hosted:<preset> / agent / —), listener state (parked / busy / —),
// pid (or -), and parked-since age.
func printStatusTable(rows []statusRow, stdout io.Writer) {
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tQUEUED\tMODE\tSTATE\tPID\tSINCE")
	for _, r := range rows {
		mode, state, pid, since := "—", "—", "-", "-"
		if r.Mode != "" {
			mode = r.Mode
		}
		switch {
		case r.Busy:
			state, pid = "busy", strconv.Itoa(r.PID)
		case r.Alive:
			state, pid = "parked", strconv.Itoa(r.PID)
			since = time.Since(r.Since).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", r.Name, r.Queued, mode, state, pid, since)
	}
	tw.Flush()
}

func cmdPing(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		to   = fs.String("to", "", "name to check")
		room = fs.String("room", ".", "room directory")
	)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *to == "" {
		fmt.Fprintln(stderr, "tincan ping: --to is required")
		return ExitUsage
	}
	if w := roomRootWarning(*room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan ping: %v\n", err)
		return ExitError
	}
	p, ok, err := sp.Present(*to)
	if err != nil {
		fmt.Fprintf(stderr, "tincan ping: %v\n", err)
		return ExitError
	}
	if !ok {
		fmt.Fprintln(stdout, "absent")
		return ExitError
	}
	fmt.Fprintf(stdout, "present pid=%d\n", p.PID)
	return ExitOK
}

func cmdStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		to   = fs.String("to", "", "recipient name")
		from = fs.String("from", "", "sender name")
		room = fs.String("room", ".", "room directory")
	)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *to == "" || *from == "" {
		fmt.Fprintln(stderr, "tincan stop: --to and --from are required")
		return ExitUsage
	}
	if w := roomRootWarning(*room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan stop: %v\n", err)
		return ExitError
	}
	e := &envelope.Envelope{
		ID: envelope.NewID(), From: *from, To: *to,
		TS: time.Now().UTC(), Kind: "stop",
	}
	if err := sp.Send(e); err != nil {
		fmt.Fprintf(stderr, "tincan stop: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "stop sent to=%s\n", e.To)
	return ExitOK
}
