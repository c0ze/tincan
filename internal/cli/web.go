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
	machineFlag := fs.String("machine", "", "this machine's name in committees and peers' --peer (default: first label of its Tailscale DNS name)")
	from := fs.String("committees-from", "", "peer that hosts committee definitions (default: this machine is the hub)")
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
	who, hosts, err := webIdentity(ctx, *owner, *origin, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "tincan web: %v\n", err)
		return ExitError
	}
	*owner = who
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
	// webIdentity's only extra host is this node's Tailscale DNS name.
	dnsName := ""
	if len(hosts) > 0 {
		dnsName = hosts[0]
	}
	hostname, _ := os.Hostname()
	machine := machineName(*machineFlag, dnsName, hostname)
	// The --origin host joins the Host allowlist inside web.New.
	srv, err := web.New(web.Config{PublicPath: *public, Owner: *owner, Origin: *origin, AllowedHosts: hosts, Machine: machine, Peers: peerList,
		ChainBudget: *budget, IdleStop: *idle, Registry: reg, Dispatch: dispatch.Options{}, StateDir: rooms.StateDir(), CommitteesFrom: *from})
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

// webIdentity returns the owner login and the extra Host allowlist (this
// node's Tailscale DNS name) from one `tailscale status --json` call. Without
// --owner a detection failure is fatal; with it, detection only supplies the
// DNS name, and a failure leaves loopback and --origin as the only hosts.
func webIdentity(ctx context.Context, owner, origin string, stderr io.Writer) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	detected, dnsName, err := web.DetectNode(ctx)
	if owner == "" {
		if err != nil {
			return "", nil, err
		}
		owner = detected
	}
	var hosts []string
	if err == nil && dnsName != "" {
		hosts = append(hosts, dnsName)
	}
	if len(hosts) == 0 && origin == "" {
		fmt.Fprintln(stderr, "tincan web: this node's Tailscale DNS name is unknown, so only loopback hosts are allowed; remote access needs --origin https://<host>")
	}
	return owner, hosts, nil
}

// machineName picks this machine's name: the flag, else the first label of
// its Tailscale DNS name (what peers' --peer URLs use), else the host name.
func machineName(flag, dnsName, hostname string) string {
	if flag != "" {
		return flag
	}
	if dnsName != "" {
		label, _, _ := strings.Cut(dnsName, ".")
		return label
	}
	label, _, _ := strings.Cut(hostname, ".")
	return label
}
