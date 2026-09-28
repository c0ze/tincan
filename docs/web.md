# tincan web — chat interface for agents

## What it is

`tincan web` is a small web chat UI, served by the tincan binary itself, for
talking to tincan agents. Each project room gets one or more **threads**: you
post messages, `@mention` a preset or a listener to hand it work, and agents
can `@mention` each other to hand off within the same conversation, bounded by
a per-thread chain budget so fan-out can never run away. Every thread shows
Stop (cancel everything running or queued) and an Activity tab listing the
room's listeners and recent requests, hosted or not. When you run `tincan web`
on two machines and point each at the other with `--peer`, either machine's
page is a single hub showing both.

## Threat model

**Anyone who can call this API can run agents with your presets' permissions**
(several built-in presets pass flags like `claude --dangerously-skip-permissions`)
**in every room this instance serves — which is equivalent to a shell as you.**
`tincan web` therefore listens on a private unix socket (or loopback TCP only)
and accepts a request only when its `Tailscale-User-Login` header equals
`--owner`. Because Tailscale identifies the *device*, not the process, any
process on any of your own **untagged** devices is trusted once it can reach
the socket over `tailscale serve`. Concretely:

- Do not tag the machines that run `tincan web` or that reach it — a tagged
  device carries no user identity, so its calls would either fail the owner
  check or (worse, for a tagged hub) silently drop the identity it needs to
  authenticate calls to a peer.
- Do not expose the listening socket or port any other way (no port-forward,
  no reverse proxy other than `tailscale serve`, no binding to a non-loopback
  address). There is deliberately no non-loopback TCP option.
- Keep the room list to projects you are comfortable running agents in from
  any of your own devices, including hosts you occasionally lend to others.

## Run it

```
tincan web [--listen unix:<path>|127.0.0.1:<port>] [--public-path /tincan]
           [--owner <login>] [--origin <url>] [--peer name=<url>]...
           [--scan <dir>]... [--chain-budget 6] [--idle-stop 30m]
```

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `unix:$XDG_RUNTIME_DIR/tincan/web.sock` (falls back to `<state dir>/web.sock`; `127.0.0.1:7788` on Windows) | `unix:<absolute path>` or a loopback `host:port`. See [Listener rules](#listener-rules). |
| `--public-path` | `/` | URL prefix the browser uses (e.g. `/tincan`); only `[A-Za-z0-9/_.-]` is accepted. |
| `--owner` | this node's Tailscale owner login | The `Tailscale-User-Login` every request must carry. |
| `--origin` | *(unset)* | Pin the browser origin mutations must match; see [CSRF and `--origin`](#csrf-and---origin). |
| `--peer` | *(none)* | `name=https://peer-host.tailnet.ts.net/tincan/`, repeatable; see [Two machines](#two-machines). |
| `--scan` | `~/projects` | Directory to import existing `.tincan/` rooms from at startup (depth ≤ 3), repeatable. |
| `--chain-budget` | `6` | Automatic agent executions per user message across a whole handoff chain; per-thread override in `thread.json`. |
| `--idle-stop` | `30m` | Stop a thread listener with no pending or running work anywhere in the room after this long idle. |

`--owner` defaults to the login `tailscale status --json` reports for this
node (via `tailscale` on `PATH`, or
`/Applications/Tailscale.app/Contents/MacOS/Tailscale`). If neither CLI is
found, or the command fails (for example because the node is tagged and has
no owning user), startup fails and prints that CLI's own error appended with
`; pass --owner <login>` — pass `--owner` explicitly in that case.

### Listener rules

`--listen unix:<path>` requires an absolute path. A missing parent directory
is created private (`0700`); an existing parent directory that is group- or
other-accessible is refused rather than silently re-chmodded. An existing
non-socket file at the target path is refused. The socket itself is created
`0600`. `--listen host:port` is accepted only for a loopback address
(`127.0.0.1`, `::1`); any other host is refused — remote reach is always
through `tailscale serve`, never a directly exposed port.

## Expose it with Tailscale

Linux:

```sh
tailscale serve --bg --set-path /tincan unix:$XDG_RUNTIME_DIR/tincan/web.sock
```

macOS, using the Tailscale app's own CLI:

```sh
/Applications/Tailscale.app/Contents/MacOS/Tailscale serve --bg --set-path /tincan \
  unix:$HOME/.local/state/tincan/web.sock
```

If the sandboxed macOS app cannot reach the unix socket (this varies by
machine; verify it at deployment), fall back to loopback TCP on both sides:

```sh
tincan web --listen 127.0.0.1:7788 ...
/Applications/Tailscale.app/Contents/MacOS/Tailscale serve --bg --set-path /tincan http://127.0.0.1:7788
```

### CSRF and `--origin`

Mutating requests (`POST`/`PATCH`) must carry `X-Tincan-Request: 1`; when they
also carry an `Origin` header, it is checked against the host the browser is
actually using. Without `--origin`, only the *host* of `Origin` is compared
(case-insensitively) — against `X-Forwarded-Host` as `tailscale serve` sets
it, falling back to `Host`; the scheme is not checked. Behind a plain
`tailscale serve --set-path` this works without any flag. If some other proxy
rewrites `Host` without adding `X-Forwarded-Host`, pass
`--origin https://<host>`: with `--origin` set, the whole `Origin` value
(scheme and host together) must match it exactly instead.

## Two machines

Point each machine's `tincan web` at the other with `--peer`:

```sh
# on cachyos
tincan web --peer macmini=https://macmini.<tailnet>.ts.net/tincan/

# on macmini
tincan web --peer cachyos=https://cachyos.<tailnet>.ts.net/tincan/
```

The link is two-way and symmetric: either machine's page then shows both
machines' rooms, each proxied read/write through the hub you happen to be
looking at. Peers are never chained — an instance only ever forwards to the
peers it was started with, so `api/peers/<a>/peers/...` is refused; there is
no path from one peer to a peer-of-a-peer.

### Quota panels

Each machine's sidebar can show a **Limits** panel: a read-only view of usage
caches your own refreshers already write to
`~/.cache/<provider>-quota.json` or `~/.cache/<provider>-quota-<profile>.json`
(for example a conky script polling every 60 s, or a LaunchAgent). `tincan
web` never fetches quota data itself, never calls a provider API and never
reads credentials — it only reads those cache files. A cache file larger than
64 KiB, or that is not a regular file, is ignored. Both the newer schema
(`percent`, `reset_at`, `short_percent`, `short_reset_at`, `fetched_at`,
`attempted_at`, optional `error`) and the legacy one (`percent`, `reset_secs`,
`fetched_at`, optional `short_percent`) are understood. A reading is `stale`
after 15 minutes without a fresh fetch, `error` when the last attempt failed,
`unknown` with no valid reading yet, otherwise `ok`; bars turn red at 95% or
more.

An optional `~/.config/tincan/quotas.json` maps cache entries to labels and
presets:

```json
{
  "codex-default": {"label": "Codex gand", "presets": ["codex"]}
}
```

Without an entry, `codex-default` maps to preset `codex` and any other ID
maps to the preset with the same name. A malformed `quotas.json` is logged
and the default labels/mapping are used instead — it never breaks the panel.

## Services

Two starting points live under [`deploy/`](../deploy/); edit the peer URL,
paths and user before installing either.

**Linux (systemd user unit)** — [`deploy/systemd/tincan-web.service`](../deploy/systemd/tincan-web.service):

```sh
cp deploy/systemd/tincan-web.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now tincan-web
loginctl enable-linger $USER   # so it keeps running without a login session
```

**macOS (launchd agent)** — [`deploy/launchd/net.tincan.web.plist`](../deploy/launchd/net.tincan.web.plist):

```sh
cp deploy/launchd/net.tincan.web.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/net.tincan.web.plist
```

`install.sh` does not install either service file; copy and edit them by
hand as above.

## Using threads

Each thread has a **primary** agent (chosen when the thread is created) that
receives a user message with no resolved `@mention`. Writing `@name` hands
that turn to `name` instead — code spans (fenced or inline) are never
scanned for mentions, and `@you` is reserved for the owner and never
dispatches. You can `@mention`:

- a preset available on this machine (built in, or from
  `~/.config/tincan/agents.json`) — this starts (or reuses) that thread's own
  listener, named `<preset>.<tid>`;
- this thread's own listener by its existing name (e.g. `codex.t7f2a9c1`);
- a hosted listener already running elsewhere in the room (e.g.
  `codex-audit`), addressed directly.

You cannot dispatch to an interactive `/listen` participant — it is shown as
present in Activity, but a mention naming it stays plain text and produces a
`system` message calling out the unresolved name.

Every agent turn receives the thread's full bounded transcript (oldest first,
truncated only from the oldest end if the 256&nbsp;KiB total prompt budget is
exceeded), not just a delta — there is no persistent memory across turns
beyond what a session-capable preset's provider already remembers.

Replies can themselves `@mention` other participants, handing work onward in
the same thread. Each user message opens one **chain budget** (default 6,
shown as "working... (k/budget used)"): every automatic hop the chain
triggers spends one unit, shared across every branch it fans out into; your
own initial targets do not count against it. A mention that would exceed the
budget becomes a `suggested` card with a **Send** button instead of running
automatically — clicking it posts that suggestion as a new user message,
opening a fresh chain and budget.

**Stop** (per thread) cancels every running and queued turn in that thread
immediately; completed work, including any file edits already made, stays.
**Archive** does the same and additionally stops and clears each of the
thread's own listeners, so the next mention after unarchiving starts a fresh
session. Left alone, a thread's own listener that has no pending or running
work anywhere in the room, for `--idle-stop` (default 30 min), is stopped by
an idle janitor; its session pointer is kept, so the next mention resumes
where it left off.

An agent turn that fails, times out, is cancelled, or is collected but prints
nothing at all shows as a red error card (an empty reply is reported as
"empty reply") with **Retry** and **Open log**. Retry re-runs that one turn:
it re-uses the same request when it never actually ran, or starts a fresh
one in a brand-new chain otherwise (for example after the thread was
Stopped). A given failed turn can be retried only once from the same card.

## Limits

- Dispatch never reaches interactive `/listen` participants (see above); they
  are visible in Activity but not addressable from a thread.
- CLI `tincan send`/`tincan ask` traffic is not recorded as a request and so
  never appears in Activity — only work that goes through a durable request
  record (MCP, the web chat, hosted listeners) does.
- There is no Tailscale Service hostname (`tincan.<tailnet>.ts.net`) option:
  Service hosts must be tagged, and a tagged device carries no user identity,
  which breaks the owner check on hub-to-peer calls.
- On Windows, hosted `up`/detached listeners are unsupported (as elsewhere in
  tincan): a thread's listeners need a `tincan serve <name>` running in the
  foreground on that machine to be reachable from the web UI.
