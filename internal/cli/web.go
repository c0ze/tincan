package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/web"
)

func cmdWeb(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", web.DefaultListen(), "unix:<path> or a loopback host:port")
	public := fs.String("public-path", "/", "URL path the browser uses, e.g. /tincan")
	owner := fs.String("owner", "", "Tailscale login allowed to use the UI (default: this node's owner)")
	origin := fs.String("origin", "", "pinned browser origin for mutations, e.g. https://host.tailnet.ts.net")
	budget := fs.Int("chain-budget", 6, "automatic agent executions per user message")
	idle := fs.Duration("idle-stop", 30*time.Minute, "stop idle thread listeners after this long")
	var peers, scans stringList
	fs.Var(&peers, "peer", "name=https://peer/tincan/ (repeatable)")
	fs.Var(&scans, "scan", "directory to scan for existing rooms (repeatable; default ~/projects)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "tincan web: unexpected arguments")
		return ExitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *owner == "" {
		detected, err := web.DetectOwner(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "tincan web: %v\n", err)
			return ExitUsage
		}
		*owner = detected
	}
	var peerList []web.Peer
	for _, p := range peers {
		name, u, ok := strings.Cut(p, "=")
		if !ok || name == "" || u == "" {
			fmt.Fprintf(stderr, "tincan web: --peer %q: expected name=url\n", p)
			return ExitUsage
		}
		peerList = append(peerList, web.Peer{Name: name, URL: u})
	}
	if len(scans) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			scans = stringList{filepath.Join(home, "projects")}
		}
	}
	reg := rooms.Default()
	if n, err := reg.Import(scans, 3); err != nil {
		fmt.Fprintf(stderr, "tincan web: room import: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(stderr, "tincan web: imported %d rooms\n", n)
	}
	machine, _ := os.Hostname()
	machine, _, _ = strings.Cut(machine, ".")
	srv, err := web.New(web.Config{PublicPath: *public, Owner: *owner, Origin: *origin, Machine: machine, Peers: peerList,
		ChainBudget: *budget, IdleStop: *idle, Registry: reg, Dispatch: dispatch.Options{}})
	if err != nil {
		fmt.Fprintf(stderr, "tincan web: %v\n", err)
		return ExitError
	}
	ln, err := web.Listen(*listen)
	if err != nil {
		fmt.Fprintf(stderr, "tincan web: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stderr, "tincan web: serving %s for %s on %s\n", *public, *owner, *listen)
	if err := srv.Run(ctx, ln); err != nil {
		fmt.Fprintf(stderr, "tincan web: %v\n", err)
		return ExitError
	}
	return ExitOK
}
