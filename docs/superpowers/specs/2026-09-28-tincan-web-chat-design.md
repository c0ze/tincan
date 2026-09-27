# tincan web — chat interface for agents — Design Spec

Status: design approved section by section by the owner on 2026-09-28
(brainstorming in a Claude Code session), then revised after a Codex review
(gpt-6-astra, xhigh) whose 14 findings are addressed below (§14). Approved by
the owner; amended on 2026-09-28 for the two-way machine link, quota panels,
the dispatcher lock location and registry exclusions (§15), as required by the
committees design (`2026-09-28-tincan-committees-design.md`, phase 2).

## 1. Problem

tincan agents run headless: hosted listeners started by MCP clients, the CLI or
`/listen` sessions. The only ways to see what they are doing are status
commands, request records and host logs, per project, per machine. There is no
place to watch agents work, talk to them directly, or have them collaborate in
one conversation ("@codex review claude's code"). The owner works across two
machines (`macmini`, `cachyos`) that are both on a tailnet
(`brill-decibel.ts.net`) and wants one page, reachable from any of their
devices including an iPad, that shows and drives the agents on both.

## 2. Goals / Non-goals

Goals
- A web chat, served by the tincan binary (`tincan web`), reachable only over
  the owner's tailnet.
- Threads: group conversations in one project room, where the owner addresses
  agents with @mentions and agents can hand work to each other with @mentions.
- Visibility of the room's journaled tincan requests (everything sent through
  MCP, the web chat or hosted listeners) with live output, plus which listeners
  are present, including `/listen` participants.
- One page covering both machines: a hub instance proxies a peer instance.
- Dispatch that is crash-safe: a web-server crash or restart never loses,
  duplicates or orphans agent work.
- No change in behaviour for the existing CLI, MCP server or skills.
- No new runtime dependencies beyond the Go standard library and existing
  modules; the UI is static HTML/CSS/JS embedded in the binary, no build step.

Non-goals (v1)
- Mirroring interactive harness sessions (Claude Code, Codex TUI, …) that do
  not go through tincan.
- Dispatching thread work to interactive `/listen` participants. They are shown
  as present but cannot be mentioned: their replies are only collected by
  polling and their execution cannot be cancelled, which breaks Stop (§6.5).
- A complete activity history of CLI `send`/`ask` traffic, which is published
  straight to the spool without a request record. Recording it is a separate
  protocol change.
- Transcript deltas for session-capable agents; every agent receives the full
  bounded transcript (§6.4).
- A Tailscale Service hostname (`tincan.<tailnet>.ts.net`): Service hosts must
  be tagged, and tagged devices carry no user identity, which breaks the owner
  check for hub-to-peer calls (§9).
- File or image uploads, search, editing or deleting messages, push
  notifications, multiple human users, Codex persistent sessions.
- More than one peer in the UI (the code accepts several; two machines tested).
- A central store; each machine's `.tincan/` stays the only source of truth.
  (Phase 2 adds one exception: committee definitions live on the hub.)
- Fetching quota data: tincan only reads the caches that existing refreshers
  write (§15.2).

## 3. Decisions taken during design

| Question | Decision |
|---|---|
| Which agents | tincan agents; thread dispatch to hosted listeners only. |
| Reach | Both machines on one page (hub + peer). |
| Thread model | Shared thread; agents can call each other, with a chain-wide budget and Stop. |
| Agent memory | Fresh agent session per thread; full bounded transcript every turn. |
| Topology | A: `tincan web` on each machine; one hub proxies the peer. |
| Layout | B: sidebar + thread; running agents as header chips; per-room Activity tab. |
| URL | Path-based (`https://cachyos.brill-decibel.ts.net/tincan/`). |

## 4. Architecture

```
 browser (Mac, iPad, …)
    │ HTTPS, tailnet only
    ▼
 tailscale serve (cachyos) /tincan ──► unix socket ──► tincan web (hub)
                                                         ├─ local rooms on cachyos
                                                         └─ peer calls over HTTPS
                                                              tailscale serve (macmini) /tincan
                                                                ──► tincan web (peer)
                                                                      └─ local rooms on macmini
```

Every instance serves the same UI and API for its own machine. A hub is an
instance started with `--peer`; it forwards `/api/peers/<name>/…` to that peer.
The link is two-way: both machines may list each other as `--peer`, so either
page shows both (§15.1).
Agents always run on the machine that owns the room.

### 4.1 Units

1. **Room registry** — `internal/rooms`. File
   `$XDG_STATE_HOME/tincan/rooms.json` (default `~/.local/state/tincan/`;
   `%LocalAppData%\tincan\` on Windows). Entries: canonical path, display name
   (base name, disambiguated), `id` (first 12 hex of SHA-256 of the canonical
   path), `hidden`, `last_used`. Written under a file lock with atomic replace.
   - `Touch(room)` is called by `mcp` (at startup), `up`, `serve`, `send`,
     `ask` and `recv`. A registry failure is logged to stderr and never fails
     the command. Paths under `<state dir>/reviews/` (phase 2 review
     workspaces) are never registered.
   - First run of `tincan web` imports existing `.tincan/` directories at
     depth ≤ 3 under each `--scan` directory (default `~/projects`).
   - The UI can add a room by absolute path (it must exist and be a directory;
     home, its ancestors and filesystem roots are refused, as in `tincan mcp`)
     and hide a room.
2. **Thread store** — `internal/thread` (§5).
3. **Dispatcher** — `internal/thread`: intents, submission, collection, chain
   rules, Stop/archive barriers, restart recovery (§6).
4. **Shared send path** — package `internal/dispatch`, extracted from the MCP
   server's `sendTool`/`launch` (`internal/mcpserver/server.go`). Its contract
   takes the listener name and the preset **separately** (a fresh `codex.<tid>`
   is not itself a preset), keeps `request.LockRoute` around the
   route/submit/launch sequence, and keeps a saved request retrievable when
   launch fails. The MCP tools call it with `preset` = name, so their
   behaviour is unchanged.
5. **Activity reader** — lists a room's listeners (hosted states and `/listen`
   presence, as `tincan_status` does) and recent request records with progress.
   Read-only.
6. **HTTP API + live events** — `internal/web` (§7).
7. **UI** — `internal/web/ui/` (`index.html`, `app.js`, `app.css`), embedded
   with `go:embed`. Plain JS. Messages are rendered by a small in-repo Markdown
   subset renderer (paragraphs, fenced and inline code, lists, links); raw HTML
   is never rendered.
8. **Peer proxy** — `internal/web` (§7.3).

## 5. Thread store

Files in the room, under `.tincan/threads/<tid>/` (`<tid>`: 8 random hex):

- `thread.json` — `id`, `title`, `primary`, `created`, `status`
  (`open|stopping|archiving|archived`), `budget` (default from
  `--chain-budget`, 6), `listeners` (the per-thread listeners this thread
  created, with preset and session generation; used for janitor, Stop and
  archive — ownership is never inferred from names).
- `events.jsonl` — the append-only journal. Every line is one event with a
  strictly increasing `seq`. Event kinds:
  - `message` — creates a message: immutable `id`, `n` (display order, assigned
    at creation), `time`, `author`, `role` (`user|agent|system`), `text`,
    `reply_to` (message id), `chain` id.
  - `intent` — a planned agent turn (§6.2): `id`, `message` (the agent
    message it fills), `listener`, `preset`, `request_id`, `prompt_sha256`.
    The exact prompt is stored beside the journal as `prompts/<request_id>.txt`
    before the intent line is written.
  - `state` — a message's state change: `message` id, `state`
    (`pending|running|done|error|cancelled|suggested|uncollectable`), `text`
    (final reply for `done`/`error`).
  - `handoffs` — marks that an agent message's mentions have been processed,
    listing the intents or suggestions it produced (exactly-once continuation).
  - `chain` — chain budget reservations and `stopped` markers.
  - `thread` — title/status/budget changes.
- `lock` — the thread's file lock (persistent on disk, as tincan's other locks).

Rules:
- Every append takes the thread lock, **repairs an incomplete final line**
  (truncates to the last newline) before writing, writes the full line, and
  syncs the file. A torn tail can therefore only exist after a crash and is
  removed by the next writer; readers ignore it until then.
- Readers fold events into messages by message `id`: creation fields come from
  `message`, the latest `state` wins. Display order is `n`; `reply_to` refers to
  ids. The API cursor is the highest `seq` the client has seen.
- Browser posts carry a client-generated `client_id`; a repeated post with the
  same `client_id` returns the existing message instead of creating another.

## 6. Dispatch behaviour

### 6.1 Mentions and targets

- Syntax: `@name`, where `name` is `[A-Za-z0-9][A-Za-z0-9._-]{0,63}` and the
  `@` is at the start of text or preceded by whitespace or `(`, `[`, `,`.
  Mentions inside fenced code blocks and inline code are ignored. Trailing
  `.`, `,`, `:`, `;`, `)` and `]` are not part of the name. `@you` is reserved
  for the owner and never dispatches.
- Resolution (canonicalization), first match wins:
  1. A preset available on this machine (built-in or user presets such as
     `mimo`, `muse`). It canonicalizes to the thread listener
     `<preset>.<tid>`; the generated name must pass
     `envelope.ValidComponent`.
  2. The exact name of a thread listener of this thread (`codex.t7f2a9c1`).
  3. A **hosted** listener currently alive in the room (e.g. `codex-audit`),
     addressed directly with its existing preset and conversation.
  4. Anything else (unknown names, `/listen` participants, presets whose binary
     does not resolve) stays plain text and produces one `system` message
     naming the unresolved mentions.
- All deduplication and self-checks use the canonical listener name, so
  `@claude` written by `claude.<tid>` is a self-mention.
- A user message with no resolved mentions goes to the thread's primary; a user
  message whose only mentions are unresolved produces the `system` message and
  no dispatch.

### 6.2 Intents and submission (crash safety)

A turn is dispatched in this order; every step is idempotent:

1. Under the thread lock: append the agent `message` (state `pending`) and an
   `intent` with a fresh `request_id` (`<tid>-<message id>`), after writing the
   exact prompt file.
2. Outside the lock: call the shared send path with that `request_id`, the
   exact stored prompt, listener and preset. Because tincan's idempotency
   requires the same ID **and identical body**, a retry after a crash
   re-submits the stored prompt and can never create a second execution.
3. On success append `state: running`. On a launch failure the request is
   still saved; the message shows the error with Retry, which re-runs step 2
   with the same ID.

One dispatcher per room owns steps 2 onwards: `tincan web` holds
`.tincan/dispatcher.lock` for each room it serves (phase 2 reviews use the same
lock), so two web
processes (or a restarted one racing its predecessor) never collect or hand off
concurrently. A second instance serves reads and forwards writes' dispatch
work to the lock holder by leaving intents in the journal, which the holder
picks up on its next reconcile.

### 6.3 Collection, handoffs and the chain budget

- The dispatcher collects results with `request.Poll` for every `running`
  message (polling, not only file watching: some terminal transitions happen
  only inside `Poll`). A terminal record appends `state` `done` (reply text) or
  `error` (tincan `ERROR …` reply, cancellation, timeout). A request record that
  no longer exists (garbage-collected) becomes `uncollectable`.
- After `done`, the reply's mentions are resolved (§6.1) and, in one locked
  append, the dispatcher reserves budget and writes the new messages, intents
  and a `handoffs` marker for the source message. Restart recovery treats a
  `done` agent message without a `handoffs` marker as unprocessed, so each
  reply's handoffs happen exactly once.
- **Budget.** Each user message starts a chain. The chain has a single
  counter of automatic agent executions, shared by all branches; the user's
  own initial targets do not count. Reserving an execution appends a `chain`
  event under the thread lock and fails once the counter reaches the thread's
  `budget` (default 6). Mentions that cannot reserve become `suggested`
  messages with a Send button (which posts them as a new user message, a new
  chain), and one `system` message reports the budget was reached. Fan-out can
  therefore never exceed the budget in total.
- Error replies produce no handoffs.
- A listener runs one request at a time across **all** chains and threads:
  intents for a busy listener are submitted normally and queue in its inbox.
  The prompt for a queued turn is built when the intent is created, from the
  journal at that moment.

### 6.4 Context sent to an agent

The prompt for a turn is, in order, with a hard total limit of 256 KiB of
UTF-8 (well under `request.MaxBodyBytes`):

1. **Header** (≤ 2 KiB): "You are `<listener>` in tincan thread '`<title>`'
   (room `<path>`). Participants: … . All participants work in this same
   directory; use `git status`/`git diff` to see changes others made. Mention
   another participant with `@name` to hand them work; the owner is `@you`.
   Your reply is posted to the thread."
2. **Transcript**: the folded thread, oldest first, as `author: text` lines
   (errors as `author: [error] text`), excluding the triggering message.
   Truncated from the oldest end at message boundaries to fit, with a line
   `[N earlier messages omitted]`. Every agent, persistent-session or not,
   receives the transcript; persistent sessions additionally remember their own
   tool use.
3. **Triggering message** with its author. If it alone exceeds 128 KiB it is
   cut at a UTF-8 boundary with `[… truncated, full text in thread <tid>
   message <id>]`.

### 6.5 Stop and archive barriers

**Stop** (per thread):
1. Under the thread lock, append `chain stopped` for every active chain and set
   `status: stopping`. From here no intent is created or submitted: the
   dispatcher checks chain and thread status under the lock before steps 1 and
   2 of §6.2.
2. Mark every intent not yet submitted as `cancelled`.
3. Cancel every non-terminal request of the thread (`request.Cancel`, then
   `host.Cancel` for running ones) and keep polling until each is terminal.
4. Set `status: open` again. Completed work, including file edits, stays.

**Archive**: Stop steps 1–3 with `status: archiving`, then for each thread
listener in `thread.json`: `host.Down` (which waits for the host to exit), then
`host.ClearSession` (which requires a stopped host), then drain its inbox of
this thread's queued requests (already cancelled in step 3, so they cannot run
later). Finally `status: archived`. Unarchiving sets `status: open` and bumps
each listener's session generation; the next mention starts a fresh session.

**Janitor**: every minute, under the thread lock, for listeners in
`thread.json` idle for `--idle-stop` (default 30 min) with no pending or
running intent: `host.Down`. Session pointers remain, so the next mention
resumes. Listeners addressed by existing name (§6.1.3) are never stopped.

### 6.6 Restart recovery

On start, for each registered room (after taking its dispatcher lock) and each
non-archived thread:
- `stopping`/`archiving` status: finish that procedure.
- Intents without `running`/terminal state: re-run §6.2 step 2 (idempotent).
- `running` messages: resume collection.
- `done` agent messages without `handoffs`: process handoffs.

## 7. HTTP API

### 7.1 Endpoints

All paths are relative to the instance's base; room IDs are registry IDs and
no URL ever carries a filesystem path. Peer routes repeat the same paths under
`api/peers/<peer>/`.

| Method | Path | Purpose |
|---|---|---|
| GET | `api/self` | Machine name, version, peers with online state. |
| GET | `api/rooms` | Registered rooms (id, name, path, hidden, running count). |
| POST | `api/rooms` | Add a room `{path}`. |
| PATCH | `api/rooms/{rid}` | `{hidden}`. |
| GET | `api/rooms/{rid}/threads` | Threads with title, primary, status, last activity, running count. |
| POST | `api/rooms/{rid}/threads` | Create `{title, primary, client_id}`. |
| GET | `api/rooms/{rid}/threads/{tid}/messages?after=<seq>` | Folded messages changed after `seq`, plus the new high-water `seq`. |
| POST | `api/rooms/{rid}/threads/{tid}/messages` | Post `{text, client_id}` as the owner (text ≤ 128 KiB). |
| POST | `api/rooms/{rid}/threads/{tid}/stop` | Stop (§6.5). |
| POST | `api/rooms/{rid}/threads/{tid}/archive` | `{archived: bool}`. |
| POST | `api/rooms/{rid}/threads/{tid}/messages/{mid}/retry` | Re-run §6.2 step 2 for a failed submission, or re-dispatch a failed turn as a new intent. |
| GET | `api/rooms/{rid}/activity` | Listeners (hosted + presence) and the last 50 request records. |
| GET | `api/rooms/{rid}/requests/{id}/progress?cursor=` | Progress events. |
| GET | `api/rooms/{rid}/agents/{name}/log` | Last 64 KiB of a hosted listener's log. |
| GET | `api/events` | SSE change notifications (§7.2). |

Errors are `{"error": "…"}` with a 4xx/5xx status.

### 7.2 Live updates

- The server watches each relevant directory individually (fsnotify is not
  recursive): every open thread directory, each room's `.tincan/requests`,
  `.tincan/hosts` and `.tincan/present`. It **also reconciles every 2 s**
  regardless of watch success, as the spool does.
- SSE events are notifications only — `{"kind": "thread"|"messages"|
  "activity"|"peer", "room": …, "thread": …, "seq": …}` — never the data
  itself. The client refetches with its cursor. On any reconnect (including
  after a server restart or a peer coming back), the client refetches
  everything it displays; there is no event replay to get wrong.

### 7.3 Peer proxy, base path and CSRF

- **Base path.** `tailscale serve --set-path /tincan` strips the prefix before
  forwarding, so the server cannot tell `/tincan` from `/tincan/`. The server
  therefore takes `--public-path` (default `/`; `/tincan` in deployment) and
  renders `index.html` with absolute URLs under it (`/tincan/app.js`,
  `/tincan/api/…`); `app.js` reads the base from a `<meta>` tag and builds every
  URL from it. Routing inside the server ignores the public path. Peer data is
  always fetched through the hub's `api/peers/<name>/…`, so a peer's own
  public path never reaches the browser.
- **Proxy.** `--peer macmini=https://macmini.brill-decibel.ts.net/tincan/`.
  The hub joins `api/peers/macmini/<rest>` to the peer base exactly once
  (`<base>api/<rest>`), streams SSE through, uses a 5 s connect timeout and a
  health check every 15 s.
- **CSRF.** Mutating requests (POST/PATCH) require `X-Tincan-Request: 1` and,
  when `Origin` is present, an Origin equal to the scheme+host of the request
  as seen by the client. The **hub** performs this check before proxying. The
  hub's outbound peer request carries its own `X-Tincan-Request: 1`, no
  browser `Origin`, and the peer's host as `Host`; browser cookies and identity
  headers are never forwarded.

## 8. UI (layout B)

- **Sidebar**: machines (with online dot), their rooms, each room's threads
  plus "Activity (n running)", and "+ new thread" (title, primary agent picker
  listing available presets). Hidden rooms are behind a toggle. Each machine
  also has a **Limits** panel (§15.2).
- **Thread view**: header with title, one chip per participant
  (idle/busy/elapsed) and Stop. Messages in `n` order; running agent messages
  show "working… (k/budget used)" with a collapsible live-output block; errors
  are red cards with Retry and Open log; `suggested` handoffs show Send;
  `uncollectable` shows "result expired".
- **Composer**: multiline, Enter sends, Shift+Enter newline, `@` autocompletes
  presets, this thread's listeners and hosted room listeners.
- **Activity tab**: listeners (hosted and `/listen` presence) and journaled
  requests from any source with sender, agent, status, elapsed, live output;
  "Message this agent" (hosted listeners only) opens a new thread with that
  listener as primary.
- **Responsive**: below 768 px the sidebar is a drawer; works on iPad and
  phone widths. Light and dark themes follow the system.

## 9. Security

Threat model: anyone who can call the API can run agents with the local
presets' permissions (e.g. `claude --dangerously-skip-permissions`) in
registered rooms, which is equivalent to a shell as the owner. The design
therefore trusts exactly the owner's own devices and processes and nothing
else, and the docs say so.

- **Listener.** Default `--listen unix:$XDG_RUNTIME_DIR/tincan/web.sock`
  (fallback `~/.local/state/tincan/web.sock`), in a `0700` directory with the
  socket `0600`, so other OS users cannot connect. `tailscale serve` forwards to
  the socket (`tailscale serve --set-path /tincan unix:<path>`, supported on
  Linux/macOS). `--listen 127.0.0.1:<port>` exists for platforms where serve
  cannot reach the socket (e.g. a sandboxed macOS Tailscale app — verified per
  machine at deployment); with TCP, any local process of any OS user can reach
  the server, which the docs call out. There is no non-loopback TCP option.
- **Owner check.** Every request must carry `Tailscale-User-Login` equal to
  `--owner` (default: this node's owner login from `tailscale status --json`,
  via `tailscale` on `PATH` or `/Applications/Tailscale.app/Contents/MacOS/Tailscale`;
  startup fails clearly if neither is available). Serve replaces any
  client-supplied identity headers. For node-to-node calls the header names
  the calling device's owner, so calls from **any process on any of the
  owner's untagged devices** pass: this is accepted (such processes could
  already run agents locally) and documented. Other users, shared-in users and
  tagged devices are refused with 403.
- **CSRF** as in §7.3.
- **Headers**: `Content-Security-Policy: default-src 'self'`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`.
- **Tagging**: neither machine may be tagged; a tagged hub would lose the
  identity header on peer calls. This is why a Tailscale Service hostname is
  out of scope for v1.

## 10. Deployment

- `tincan web [--listen unix:<path>|127.0.0.1:<port>] [--peer name=<url>]
  [--public-path /tincan] [--owner <login>] [--scan <dir>] [--chain-budget 6]
  [--idle-stop 30m]`.
- cachyos (hub): systemd user unit `tincan-web.service` running
  `tincan web --peer macmini=https://macmini.brill-decibel.ts.net/tincan/`;
  `tailscale serve --bg --set-path /tincan unix:<socket>`, giving
  `https://cachyos.brill-decibel.ts.net/tincan/` beside the existing `/` and
  `/comics` routes.
- macmini (peer): launchd agent `net.tincan.web.plist` running
  `tincan web --peer cachyos=https://cachyos.brill-decibel.ts.net/tincan/`
  (two-way link, §15.1); the same serve route via the Tailscale app's CLI,
  using the socket if the app can reach it and loopback TCP otherwise.
- `docs/web.md` documents flags, both service files, the serve commands and
  the threat model. `install.sh` does not install services in v1.

## 11. Error handling

| Situation | Behaviour |
|---|---|
| Agent turn fails / times out / cancelled | `error` card with the `ERROR …` text; Retry and Open log; no handoffs. |
| Launch fails after the request is saved | `error` card; Retry re-submits the same request ID and prompt. |
| Preset missing on this machine | One `system` message; no dispatch. |
| Unknown or `/listen` mention | Plain text + one `system` message. |
| Request record garbage-collected | `uncollectable` ("result expired"). |
| Browser disconnects | SSE reconnects; client refetches with its cursor. |
| Duplicate browser post | Same `client_id` returns the existing message. |
| Concurrent writers | Thread lock + one dispatcher per room (§6.2). |
| Peer unreachable | Peer rooms greyed "offline"; hub keeps serving; health check retries. |
| `tincan web` crash at any point | §6.6 recovery; no lost, duplicated or orphaned turns. |
| Registry unreadable | Web starts empty with a banner; commands never fail on it. |
| Room path removed | Room shown missing; can be hidden. |

## 12. Testing

Unit
- Mention parsing (code spans, punctuation, `@you`, length and
  `ValidComponent` limits) and canonical resolution order, including
  `@claude` from `claude.<tid>` counting as a self-mention.
- Journal: concurrent appends from several processes keep `seq` strict; an
  append after an injected torn tail repairs it; folding by id with display
  order `n`; `client_id` deduplication.
- Prompt building: header, transcript truncation with the omitted count, the
  256 KiB total cap, UTF-8-safe cut of an oversized trigger.
- Budget: a branching chain (one reply mentioning three agents, each
  mentioning two more) never exceeds the budget in total executions; overflow
  becomes `suggested`.
- Owner/CSRF middleware: missing, wrong and correct identity; mutation without
  `X-Tincan-Request`; mismatched Origin at the hub.

Integration (Go tests using the existing fake agent as presets `fake` and
`fake2`)
- Round trip: "@fake do x" → fake replies "@fake2 check" → fake2 runs → both
  replies in the thread.
- **Crash boundaries**: kill the dispatcher (a test hook that exits the
  process) after the intent append, after submission, after the terminal state,
  and after writing handoffs; restart; assert each fake agent executed exactly
  once per turn (execution counter file) and every handoff happened once.
- Stop racing a completion that would hand off: no new execution starts after
  Stop returns; archive leaves no queued request that runs after unarchive.
- Two `tincan web` processes on one room: only the lock holder dispatches.
- Memory: a session-capable adapter test (in the style of
  `internal/host/session_test.go`) checks that the second turn of a thread
  listener resumes the saved session ID.
- Peer proxy against a second in-process server: path joining, SSE
  pass-through, mutation forwarding with the hub's headers, peer going offline
  and back.
- MCP regression: existing `internal/mcpserver` tests pass unchanged after the
  shared send path extraction.

Deployment verification (manual, recorded in the plan): through real
`tailscale serve` on both machines, check the identity header is present and
correct from a browser and from the hub, a request from another identity is
refused, the mounted path loads assets and API, and a proxied POST to the peer
succeeds. Then a UI smoke test in a real browser: create a thread, post, live
progress, a handoff, Stop, archive, phone-width drawer.

CI: existing Linux/macOS/Windows matrix; tests that launch detached listeners
skip on Windows, as existing ones do.

## 13. Configuration defaults

- `--chain-budget 6`, per-thread override in `thread.json`.
- `--idle-stop 30m`.
- `--scan ~/projects`.
- `--listen unix:$XDG_RUNTIME_DIR/tincan/web.sock`.
- `--public-path /`.

## 14. Review record

Codex review (2026-09-28) and resolution:

| # | Finding | Resolution |
|---|---|---|
| 1 | Local processes could forge identity on a loopback port | Unix socket default (§9); remaining trust in the owner's devices stated. |
| 2 | Dispatch not crash-safe | Durable intents with stored prompts, deterministic request IDs, `handoffs` markers, one dispatcher per room, `client_id` (§6.2, §6.3, §6.6). |
| 3 | Stop/archive lacked a dispatch barrier | Status barrier under the lock before cancelling; archive order Down → ClearSession; listener membership persisted (§6.5). |
| 4 | Interactive listeners break collection/Stop | Out of scope for dispatch in v1 (§2, §6.1). |
| 5 | Session capability ≠ delivery checkpoint | Full bounded transcript for every agent (§6.4). |
| 6 | Budget allowed exponential fan-out and alias self-dispatch | Chain-wide execution counter reserved atomically; canonical names (§6.1, §6.3). |
| 7 | Activity cannot show all traffic | Scoped to journaled requests + presence (§2, §8). |
| 8 | Shared sender needed name and preset separately | §4.1 item 4; `ValidComponent` and `@you` in §6.1. |
| 9 | Append/folding semantics incomplete | Immutable ids, display order `n`, `seq` cursor, tail repair + sync (§5). |
| 10 | 64 KiB cap did not bound the body | 256 KiB total prompt budget, trigger cap (§6.4). |
| 11 | Non-recursive watching could miss updates | Per-directory watches + 2 s reconcile; SSE as notifications only (§7.2). |
| 12 | Proxy CSRF and base path underspecified | `--public-path`, hub-side Origin check, outbound header rules, path join (§7.3). |
| 13 | Service hostname conflicts with peer auth | Deferred (§2, §9). |
| 14 | Tests could pass without proving behaviour | Crash-boundary, race, branching-budget, torn-tail, session-resume and real-Serve checks (§12). |

## 15. Amendments (2026-09-28)

### 15.1 Two-way machine link

- Each `tincan web` may list the other machine as `--peer`. Either page then
  shows both machines; phase 2 needs this so a Mac room can reach cachyos
  reviewers and vice versa.
- Peers are never chained: `api/peers/<a>/peers/…` is refused (400), and an
  instance lists only its own rooms plus its direct peers' rooms, so two
  instances that list each other never loop.
- The owner check is unchanged: each side's `tailscale serve` identifies calls
  from the other machine as the owner's device.

### 15.2 Quota panels (read-only)

- **Source.** `tincan web` reads the cache files existing refreshers write:
  `~/.cache/<provider>-quota.json` and `~/.cache/<provider>-quota-<profile>.json`
  (cachyos: conky `texeci` scripts every 60 s; macmini: the `tr.gand.aiquota`
  LaunchAgent). It never runs quota scripts, calls provider APIs or reads
  credentials. Entry ID: `<provider>` or `<provider>-<profile>` from the file
  name (`claude`, `codex-default`, `codex-gmail`, …). Files larger than 64 KiB
  or not regular files are ignored.
- **Normalization.** Two schemas exist:
  - schema 2 (cachyos): `percent`, `reset_at`, `short_percent`,
    `short_reset_at`, `fetched_at`, `attempted_at`, optional `error`;
  - legacy (macmini): `percent`, `reset_secs`, `fetched_at`, optional
    `short_percent` (reset = `fetched_at + reset_secs`; no short reset time).
  The normalized entry keeps nullable values and records last success
  (`fetched_at`) and last attempt (`attempted_at`, or `fetched_at` when absent)
  separately, plus `error`. Timestamps outside 2020-01-01 … now + 400 days and
  percentages outside 0–1000 are treated as missing.
- **State**, recomputed on every read: `error` when the last attempt failed and
  is newer than the last success; `stale` when the last success is older than
  15 minutes or its reset time has passed without a newer success; otherwise
  `ok`. Bars are red at ≥ 95 %.
- **Mapping.** Optional `~/.config/tincan/quotas.json`:
  `{"codex-default": {"label": "Codex gand", "presets": ["codex"]}, …}`.
  Without it, `codex-default` maps to preset `codex` and every other entry to
  the preset with the same ID. Phase 2 extends entries with `blocking`.
- **API.** `GET api/quotas` → `[{id, label, presets, percent, reset_at,
  short_percent, short_reset_at, fetched_at, attempted_at, error, state}]`,
  also through the hub for the peer.
- **Updates.** The change scanner fingerprints the cache files; a `quota` note
  is also published every minute so time-based staleness reaches the UI
  without file changes.
- **UI.** A Limits panel per machine in the sidebar: label, bar, percent, time
  to reset ("2d 4h"), the short window in small type, grey when stale, "error"
  when failing, red at ≥ 95 %. Agent chips and the @-autocomplete show the
  mapped preset's percent.
- **Tests.** Both schemas, failure-only records, missing and invalid values,
  staleness transitions over time with a fixed clock, mapping defaults, the
  per-minute note.

### 15.3 Dispatcher lock and registry exclusion

- The per-room dispatcher lock is `.tincan/dispatcher.lock` (not under
  `threads/`), so phase 2 review coordination shares it.
- `rooms.Touch` ignores paths under `<state dir>/reviews/`.
