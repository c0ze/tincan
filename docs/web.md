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
- Prefer the default unix socket. With `--listen 127.0.0.1:<port>`, **any
  local process of any OS user on that machine can connect to the port and
  forge the `Tailscale-User-Login` header** — the identity header is only
  trustworthy when `tailscale serve` is the sole way in, which a private
  `0700`/`0600` socket guarantees and a loopback port does not. Use TCP only
  where serve cannot reach the socket, and only on a machine whose other
  local users and processes you trust as much as yourself.
- Never enable Tailscale **Funnel** for this hostname or port: Funnel puts it
  on the public internet. tincan refuses any request carrying the
  `Tailscale-Funnel-Request` header as a second line of defence, but do not
  rely on that.
- Other apps served on the same `tailscale serve` hostname (for example `/`
  and `/comics` beside `/tincan` on cachyos) share its browser origin, so the
  browser treats their pages as same-origin with tincan: a page they serve can
  call tincan's API with your identity. Only co-host apps you trust as much
  as tincan itself, or give tincan a hostname of its own.
- Every request's `Host` (and `X-Forwarded-Host`, when present) must be on
  the [Host allowlist](#host-allowlist); this blocks DNS-rebinding pages that
  resolve their own name to `127.0.0.1`.
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

The same `tailscale status --json` call supplies this node's Tailscale DNS
name for the [Host allowlist](#host-allowlist). With an explicit `--owner`,
tincan still runs it for the name but does not fail if it errors; it then
allows only loopback hosts and the `--origin` host, and prints a one-line
note that remote access needs `--origin https://<host>`.

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

### Host allowlist

Every request, whatever its method, is refused with `403 {"error": "host
not allowed"}` unless its `Host` header names this server:

- a loopback literal — `127.0.0.1`, `[::1]` or `localhost` — on any port;
- this node's Tailscale DNS name (e.g. `cachyos.<tailnet>.ts.net`, as
  `tailscale status --json` reports it), on any port;
- the host of `--origin` when it is set (host and port, case-insensitive).

When `X-Forwarded-Host` is present, it must pass the same list. `tailscale
serve` keeps the browser's `Host` (the node's DNS name), so a normal deployment
needs no flag. If the DNS name could not be detected (for example with an
explicit `--owner` and no working Tailscale CLI), or you reach tincan under a
different name, pass `--origin https://<host>`.

### CSRF and `--origin`

Mutating requests (`POST`/`PATCH`) must carry `X-Tincan-Request: 1`; when they
also carry an `Origin` header, it is checked against the host the browser is
actually using. Without `--origin`, only the *host* of `Origin` is compared
(case-insensitively) — against `X-Forwarded-Host` as `tailscale serve` sets
it, falling back to `Host`; the scheme is not checked. Behind a plain
`tailscale serve --set-path` this works without any flag. If some other proxy
rewrites `Host` without adding `X-Forwarded-Host`, pass
`--origin https://<host>`: with `--origin` set, the whole `Origin` value
(scheme and host together) must match it exactly instead, and its host joins
the [Host allowlist](#host-allowlist). Refused requests get a JSON `403`
(`{"error": "..."}`).

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

## Committees

A committee is a named group of reviewers, each a `preset@machine`, such as
`codex-gmail@cachyos` or `claude-personal@macmini`. Define committees on the
**Committees** page. Reviews that use them arrive in a later release.

- **One hub.** Definitions live on the hub: the machine started *without*
  `--committees-from`, in `<state>/committees.json`. Its peer runs
  `tincan web --committees-from <hub peer name>`. It keeps a cached copy,
  refreshed every 60 s and whenever the page loads, and shows the cache's age.
  Edits always go to the hub; a peer refuses them with 409. If both machines
  point at each other, the page says "not a committees hub".
- **Machine names.** A member's machine is the name that machine's
  `tincan web` uses for itself: `--machine`, by default the first label of
  its Tailscale DNS name (`macmini` for `macmini.<tailnet>.ts.net`). That
  matches the names used in `--peer name=url`.
- **Checks when saving.** Every member must exist on its machine, be
  installed, and use a bare command name or an absolute path; relative
  executables are refused. A member on an offline peer cannot be saved. Presets
  with a permission bypass (`--dangerously-skip-permissions`,
  `--always-approve`, `--yolo`, `-s danger-full-access`) are saved with a
  warning, because such a reviewer could modify files. A committee may not
  share a name with a preset on any reachable machine.
- **Limits.** 1–8 members, a deadline of 1–240 minutes (default 30),
  instructions up to 8 KiB. Each save increments the version.

### Reviewer jobs

When a review has a member on this machine, the requesting coordinator
creates a job through `api/review-jobs`. For each job:

- **Workspace.** It gets a private workspace under
  `<state>/reviews/ws/<job_id>`. If a registered room is a clone of the same
  repository (same `origin`, not a partial clone) and already has the base
  commit, the workspace is a disposable clone at that commit with the change
  staged (`git diff --cached`), verified against the requester's tree. Nothing
  is ever fetched. Otherwise the workspace holds the packet alone:
  `packet/manifest.json`, `packet/diff.patch`, `packet/question.md` and
  post-change contents under `packet/files/`.
- **What is never sent.** Secret-looking files (`.env*`, `*.pem`, `*.key`,
  `id_rsa*`, `credentials*`, `*secret*`, …), symlinks, submodules and files over
  1 MiB. The manifest lists each one with the reason.
- **Execution.** The member preset runs once, without a session. Its
  executable is resolved *before* the workspace exists and run by absolute
  path, so a reviewed change cannot substitute its own binary. The run is
  bounded by the review's deadline.
- **Results and cleanup.** A result is kept until the requester acknowledges
  it. A janitor runs every minute: it resumes jobs interrupted by a restart
  (adopting a run that already happened), removes workspaces after
  acknowledgement or cancellation, and forgets records a week after expiry.

**Residual risk.** A reviewer with a permission bypass, or a CLI that honours
configuration inside the reviewed repository, can still act outside its
workspace. Use read-only presets for committee members, for example
`claude -p {body} --permission-mode plan --setting-sources user` or
`codex exec -s read-only`.

Each machine also serves `api/presets`, its preset catalogue with
availability, warnings and quota, but never env values. `tincan web` writes
`<state>/web.json` every 10 s so the CLI and MCP can tell that a coordinator
is running.

## Reviews

Ask a committee to review a room's change, in any of three ways:

- **Web UI:** the room's **Activity** page ("Start a review").
- **Terminal:** `tincan review --committee reviewers --question "…" --wait`.
- **An agent:** the MCP tool `tincan_review`, then `tincan_review_wait`.

`tincan web` on the requesting machine coordinates the review:

- It captures the change as a packet: uncommitted work by default, or
  `--scope branch`, `commit:<rev>`, `range:<a>..<b>` or `none` for a
  question with no code.
- It sends each member a job on that member's machine, and records results
  as they arrive.
- It closes the review when every member has finished, or at the
  committee's deadline. Members still running then are marked *late*: their
  results are kept but not added to the bundle.

The bundle (`.tincan/reviews/<id>/bundle.md`) collects every member's review;
whoever asked synthesizes it. Cancelling stops members that are still running.

Unreachable machines are retried until the deadline, and an unanswered
member ends as `unreachable` or `expired`, so a review always finishes. With
`skip_exhausted` set on the committee, a member whose `quotas.json` entry
declares `blocking` and whose quota is at 100% is skipped with a note naming
the window and reset time.

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
it re-uses the same request when that request was never saved or never
started, or starts a fresh one in a brand-new chain otherwise (for example
after the thread was Stopped, or after a launch failure). A fresh retry can
be made only once from the same card; a same-request retry that fails again
can be retried again.
When a listener fails to launch after its request was saved, the saved
request is cancelled at once (and Stop/Archive cancel any such leftovers), so
a later launch of that listener never runs the stale prompt.

## Rooms in the sidebar

Rooms come from the registry (`--scan` at startup, and every room a tincan
command runs in, except your home directory, its ancestors and filesystem
roots). Each machine has **+ add room**, which asks for an absolute directory
path. Each room has **hide**/**unhide**; hidden rooms appear, dimmed, only
with **show hidden rooms** checked. A room whose directory is gone stays
listed, greyed and marked "(missing)", so you can hide it. A room with
archived threads offers **show archived**, listing them so you can open one
and **Unarchive** it.

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
