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

	"github.com/c0ze/tincan/internal/host"
	"github.com/c0ze/tincan/internal/spool"
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
	name, room, preset, execTpl, stdin, reply string
	execTimeout                               int // -1 = not given
}

// forward re-emits the preset-shaping flags so `up` can hand them verbatim
// to the `serve --daemon` child, which resolves the preset the same way.
func (h hostFlags) forward() []string {
	var out []string
	if h.preset != "" {
		out = append(out, "--preset", h.preset)
	}
	if h.execTpl != "" {
		out = append(out, "--exec", h.execTpl)
	}
	if h.stdin != "" {
		out = append(out, "--stdin", h.stdin)
	}
	if h.reply != "" {
		out = append(out, "--reply", h.reply)
	}
	if h.execTimeout >= 0 {
		out = append(out, "--exec-timeout", strconv.Itoa(h.execTimeout))
	}
	return out
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
	h, code := parseHostFlags("serve", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&daemon, "daemon", false, "started by `up`: stdio already points at the host log; print no banner")
	})
	if code != ExitOK {
		return code
	}
	label, p, code := resolveHost("serve", h, stderr)
	if code != ExitOK {
		return code
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
		if err := os.MkdirAll(host.Dir(room), 0o755); err != nil {
			fmt.Fprintf(stderr, "tincan serve: %v\n", err)
			return ExitError
		}
		f, err := os.OpenFile(host.LogPath(room, h.name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	if err := host.Serve(ctx, host.ServeOptions{Room: room, Name: h.name, Label: label, Preset: p, Log: logw}); err != nil {
		fmt.Fprintf(stderr, "tincan serve: %v\n", err)
		return ExitError
	}
	return ExitOK
}
