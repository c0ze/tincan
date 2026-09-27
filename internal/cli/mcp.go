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
	"strings"
	"syscall"
	"time"

	"github.com/c0ze/tincan/v2/internal/buildinfo"
	"github.com/c0ze/tincan/v2/internal/mcpserver"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/spool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type writeCloser struct{ io.Writer }

func (writeCloser) Close() error { return nil }

func cmdMCP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", "", "room directory; tools cannot escape this scope (default: the project containing the working directory)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "tincan mcp: unexpected arguments")
		return ExitUsage
	}
	if *room == "" {
		inferred, err := inferRoom()
		if err != nil {
			fmt.Fprintf(stderr, "tincan mcp: %v\n", err)
			return ExitUsage
		}
		*room = inferred
	}
	server, err := mcpserver.New(mcpserver.Options{Room: *room})
	if err != nil {
		fmt.Fprintf(stderr, "tincan mcp: %v\n", err)
		return ExitError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, &mcp.IOTransport{Reader: os.Stdin, Writer: writeCloser{stdout}}); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "tincan mcp: %v\n", err)
		return ExitError
	}
	return ExitOK
}

// inferRoom picks the room for an MCP server registered once per user: clients
// such as Claude Code and Codex start stdio servers in the session's project
// directory, so the enclosing git work tree (or the directory itself) is the
// room. Filesystem roots, the home directory and its ancestors are refused and
// never climbed into, because a client started outside any project (a desktop
// app launched from /, say) would otherwise spool every project's work into one
// shared room.
func inferRoom() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// Compare canonical paths so a symlinked alias (macOS /var and
	// /private/var, say) cannot slip past the home check.
	if c, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = c
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		if h, err := filepath.EvalSymlinks(home); err == nil {
			home = h
		}
	}
	tooBroad := func(dir string) bool {
		if filepath.Dir(dir) == dir {
			return true // a filesystem or volume root, whatever home is
		}
		if home == "" {
			return false
		}
		rel, err := filepath.Rel(dir, home)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if tooBroad(cwd) {
		return "", fmt.Errorf("cannot infer a room from working directory %s; start the client in a project directory or pass --room /absolute/project", cwd)
	}
	for dir := cwd; !tooBroad(dir); dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
	}
	return cwd, nil
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 || (*format != "text" && *format != "json") {
		fmt.Fprintln(stderr, "tincan version: expected --format text|json")
		return ExitUsage
	}
	info := buildinfo.Current()
	var err error
	if *format == "json" {
		err = json.NewEncoder(stdout).Encode(info)
	} else {
		_, err = fmt.Fprintln(stdout, info.String())
	}
	if err != nil {
		fmt.Fprintf(stderr, "tincan version: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func cmdGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	room := fs.String("room", ".", "room directory")
	age := fs.Duration("older-than", 7*24*time.Hour, "retention for terminal records, progress, logs, and quarantine; minimum 1h")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 || *age < time.Hour {
		fmt.Fprintln(stderr, "tincan gc: --older-than must be at least 1h")
		return ExitUsage
	}
	before := time.Now().Add(-*age)
	count, err := request.GC(context.Background(), *room, before)
	if err != nil {
		fmt.Fprintf(stderr, "tincan gc: %v\n", err)
		return ExitError
	}
	sp, err := spool.Open(*room)
	if err != nil {
		fmt.Fprintf(stderr, "tincan gc: %v\n", err)
		return ExitError
	}
	stats, err := sp.GC(before)
	if err != nil {
		fmt.Fprintf(stderr, "tincan gc: %v\n", err)
		return ExitError
	}
	if err := json.NewEncoder(stdout).Encode(map[string]any{"requests": count, "spool": stats}); err != nil {
		fmt.Fprintf(stderr, "tincan gc: %v\n", err)
		return ExitError
	}
	return ExitOK
}
