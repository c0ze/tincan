package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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
	room := fs.String("room", ".", "room directory; tools cannot escape this scope")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "tincan mcp: unexpected arguments")
		return ExitUsage
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
