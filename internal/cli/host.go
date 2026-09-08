package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/spool"
)

// positional splits a leading non-flag <name> off args so both
// `tincan up codex --room R` and `tincan up --room R codex` work: stdlib
// flag stops parsing at the first positional, so without this the former
// would parse no flags at all.
func positional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// hostFlags are the flags shared by `up` and `serve`.
type hostFlags struct {
	name, room, preset, execTpl, stdin, reply, session string
	execTimeout                                        int // -1 = not given
}

// parseHostFlags parses `<name> [flags]` for cmd; extra registers
// command-specific flags on the same FlagSet. Returns ExitUsage (with the
// message already printed) on any flag mistake, including an invalid name.
func parseHostFlags(cmd string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (hostFlags, int) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var h hostFlags
	fs.StringVar(&h.room, "room", ".", "room directory")
	fs.StringVar(&h.preset, "preset", "", "preset name (default: <name> when it is a known preset; see `tincan presets`)")
	fs.StringVar(&h.execTpl, "exec", "", "exec template, e.g. 'my-llm -p {body}' (overrides the preset's command)")
	fs.StringVar(&h.stdin, "stdin", "", "body|none: pipe the body to the agent's stdin (overrides the preset)")
	fs.StringVar(&h.reply, "reply", "", "stdout|file: where the agent's answer comes from (overrides the preset)")
	fs.StringVar(&h.session, "session", "", "persistent|stateless: conversation policy")
	fs.IntVar(&h.execTimeout, "exec-timeout", -1, "seconds before an agent run is killed; 0 = none (default: the preset's, or 3600)")
	if extra != nil {
		extra(fs)
	}
	var rest []string
	h.name, rest = positional(args)
	if err := fs.Parse(rest); err != nil {
		return h, ExitUsage
	}
	if h.name == "" && fs.NArg() > 0 {
		h.name, rest = fs.Arg(0), fs.Args()[1:]
	} else {
		rest = fs.Args()
	}
	if h.name == "" {
		fmt.Fprintf(stderr, "tincan %s: <name> is required\n", cmd)
		return h, ExitUsage
	}
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "tincan %s: unexpected argument %q\n", cmd, rest[0])
		return h, ExitUsage
	}
	if err := spool.ValidName(h.name); err != nil {
		fmt.Fprintf(stderr, "tincan %s: %v\n", cmd, err)
		return h, ExitUsage
	}
	if h.stdin != "" && h.stdin != "body" && h.stdin != "none" {
		fmt.Fprintf(stderr, "tincan %s: --stdin must be body or none\n", cmd)
		return h, ExitUsage
	}
	if h.reply != "" && h.reply != "stdout" && h.reply != "file" {
		fmt.Fprintf(stderr, "tincan %s: --reply must be stdout or file\n", cmd)
		return h, ExitUsage
	}
	return h, ExitOK
}

// resolveHost turns the flags into the effective preset (built-in ← config
// file ← flags). Exit codes: ExitUsage for a bad template or unknown preset,
// ExitError for an unreadable config file.
func resolveHost(cmd string, h hostFlags, stderr io.Writer) (string, host.Preset, int) {
	presets, err := host.Effective(host.ConfigPath())
	if err != nil {
		fmt.Fprintf(stderr, "tincan %s: %v\n", cmd, err)
		return "", host.Preset{}, ExitError
	}
	ov := host.Overrides{Stdin: h.stdin, Reply: h.reply, ExecTimeoutSec: h.execTimeout}
	if h.execTpl != "" {
		argv, err := host.SplitTemplate(h.execTpl)
		if err != nil {
			fmt.Fprintf(stderr, "tincan %s: %v\n", cmd, err)
			return "", host.Preset{}, ExitUsage
		}
		ov.Exec = argv
	}
	label, p, err := host.Resolve(presets, h.name, h.preset, ov)
	if err != nil {
		fmt.Fprintf(stderr, "tincan %s: %v\n", cmd, err)
		return "", host.Preset{}, ExitUsage
	}
	p, err = host.WithSession(p, label, h.session)
	if err != nil {
		fmt.Fprintf(stderr, "tincan %s: %v\n", cmd, err)
		return "", host.Preset{}, ExitUsage
	}
	return label, p, ExitOK
}

func cmdPresets(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("presets", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "table", "output format: table|json")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *format != "table" && *format != "json" {
		fmt.Fprintln(stderr, "tincan presets: --format must be table or json")
		return ExitUsage
	}
	presets, err := host.Effective(host.ConfigPath())
	if err != nil {
		fmt.Fprintf(stderr, "tincan presets: %v\n", err)
		return ExitError
	}
	if *format == "json" {
		data, err := json.MarshalIndent(presets, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "tincan presets: %v\n", err)
			return ExitError
		}
		fmt.Fprintln(stdout, string(data))
		return ExitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTDIN\tREPLY\tTIMEOUT\tEXEC")
	for _, n := range host.Names(presets) {
		p := presets[n]
		timeout := "none"
		if p.ExecTimeoutSec > 0 {
			timeout = strconv.Itoa(p.ExecTimeoutSec) + "s"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n, p.Stdin, p.Reply, timeout, strings.Join(p.Exec, " "))
	}
	tw.Flush()
	return ExitOK
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	var daemon bool
	var owner, resolved, label string
	h, code := parseHostFlags("serve", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&daemon, "daemon", false, "started by `up`: stdio already points at the host log; print no banner")
		fs.StringVar(&owner, "owner", "", "internal host lifetime identity")
		fs.StringVar(&resolved, "resolved-preset", "", "internal resolved preset JSON")
		fs.StringVar(&label, "resolved-label", "", "internal resolved preset label")
	})
	if code != ExitOK {
		return code
	}
	var p host.Preset
	if resolved != "" {
		if err := json.Unmarshal([]byte(resolved), &p); err != nil {
			fmt.Fprintf(stderr, "tincan serve: invalid resolved preset: %v\n", err)
			return ExitUsage
		}
	} else {
		label, p, code = resolveHost("serve", h, stderr)
		if code != ExitOK {
			return code
		}
	}
	room, err := filepath.Abs(h.room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan serve: %v\n", err)
		return ExitError
	}
	if w := roomRootWarning(room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	// In daemon mode `up` already pointed our stdio at the host log, so
	// stderr is the log. In the foreground we append to the log file
	// ourselves and tee to the terminal for debugging.
	logw := stderr
	if !daemon {
		f, err := host.OpenLog(host.LogPath(room, h.name))
		if err != nil {
			fmt.Fprintf(stderr, "tincan serve: %v\n", err)
			return ExitError
		}
		defer f.Close()
		logw = io.MultiWriter(f, stderr)
		fmt.Fprintf(stdout, "serving name=%s preset=%s room=%s (Ctrl-C to stop)\n", h.name, label, room)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := host.Serve(ctx, host.ServeOptions{Room: room, Name: h.name, Label: label, Preset: p, Log: logw, Owner: owner}); err != nil {
		fmt.Fprintf(stderr, "tincan serve: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func cmdUp(args []string, stdout, stderr io.Writer) int {
	var wait int
	h, code := parseHostFlags("up", args, stderr, func(fs *flag.FlagSet) {
		fs.IntVar(&wait, "wait", 10, "seconds to wait for the listener to park")
	})
	if code != ExitOK {
		return code
	}
	room, err := filepath.Abs(h.room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan up: %v\n", err)
		return ExitError
	}
	if w := roomRootWarning(room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	if h.preset == "" && h.execTpl == "" && h.stdin == "" && h.reply == "" && h.session == "" && h.execTimeout < 0 {
		if st, ok := host.Existing(context.Background(), room, h.name); ok {
			fmt.Fprintf(stdout, "already up name=%s pid=%d\n", h.name, st.PID)
			return ExitOK
		}
	}
	label, p, code := resolveHost("up", h, stderr)
	if code != ExitOK {
		return code
	}
	result, err := host.Up(context.Background(), host.UpOptions{Room: room, Name: h.name, Label: label, Preset: p, Wait: time.Duration(wait) * time.Second})
	if err != nil {
		fmt.Fprintf(stderr, "tincan up: %v\n", err)
		return ExitError
	}
	if result.Already {
		fmt.Fprintf(stdout, "already up name=%s pid=%d\n", h.name, result.State.PID)
	} else {
		fmt.Fprintf(stdout, "up name=%s pid=%d preset=%s\n", h.name, result.State.PID, result.State.Preset)
	}
	return ExitOK
}

func cmdDown(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	roomFlag := fs.String("room", ".", "room directory")
	wait := fs.Int("wait", 15, "seconds to wait for the listener to stop")
	name, rest := positional(args)
	if err := fs.Parse(rest); err != nil {
		return ExitUsage
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		fmt.Fprintln(stderr, "tincan down: <name> is required")
		return ExitUsage
	}
	if err := spool.ValidName(name); err != nil {
		fmt.Fprintf(stderr, "tincan down: %v\n", err)
		return ExitUsage
	}
	room, err := filepath.Abs(*roomFlag)
	if err != nil {
		fmt.Fprintf(stderr, "tincan down: %v\n", err)
		return ExitError
	}
	if w := roomRootWarning(room); w != "" {
		fmt.Fprintln(stderr, w)
	}
	if err := host.Down(context.Background(), room, name, time.Duration(*wait)*time.Second); err != nil {
		fmt.Fprintf(stderr, "tincan down: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "down name=%s\n", name)
	return ExitOK
}
