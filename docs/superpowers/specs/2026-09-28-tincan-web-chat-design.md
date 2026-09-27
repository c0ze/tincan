# tincan web — chat interface for agents — Design Spec

Status: design approved section by section by the owner on 2026-09-28
(brainstorming in a Claude Code session). This document awaits the owner's
review before an implementation plan is written.

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
- Visibility of all tincan activity in a room, including work started outside
  the web page (MCP, CLI, `/listen`), with live output.
- One page covering both machines: a hub instance proxies a peer instance.
- No change in behaviour for the existing CLI, MCP server or skills.
- No new runtime dependencies beyond the Go standard library and the existing
  modules; the UI is static HTML/CSS/JS embedded in the binary, no build step.

Non-goals (v1)
- Mirroring interactive harness sessions (Claude Code, Codex TUI, …) that do
  not go through tincan.
- File or image uploads, search, editing or deleting messages, push
  notifications.
- Multiple human users; accounts or auth beyond Tailscale identity.
- Codex persistent sessions (the thread transcript substitutes; see §6.3).
- More than one peer in the UI (the code accepts several; only two machines are
  tested).
- A central store; each machine's `.tincan/` stays the only source of truth.

## 3. Decisions taken during design

| Question | Decision |
|---|---|
| Which agents | tincan agents only (hosted listeners and `/listen` participants). |
| Reach | Both machines on one page (hub + peer). |
| Thread model | Shared thread; agents can call each other, with a chain budget and Stop. |
| Agent memory | Fresh agent session per thread. |
| Topology | A: `tincan web` on each machine; one hub proxies the peer. |
| Layout | B: sidebar + thread; running agents as header chips; per-room Activity tab. |
| URL | Path-based by default; optional Tailscale Service hostname. |

## 4. Architecture

```
 browser (Mac, iPad, …)
    │ HTTPS, tailnet only
    ▼
 tailscale serve (cachyos)  ──►  tincan web (127.0.0.1:7788, hub)
                                   ├─ local rooms on cachyos
                                   └─ /api/peers/macmini/* ──► HTTPS over tailnet
                                                               tailscale serve (macmini)
                                                                 ──► tincan web (127.0.0.1:7788)
                                                                       └─ local rooms on macmini
```

Every instance serves the same UI and API for its own machine. A hub is an
instance started with `--peer`; it forwards `/api/peers/<name>/…` to that peer.
Agents always run on the machine that owns the room, where the project files
are.

### 4.1 Units

Each unit has one purpose and is testable on its own.

1. **Room registry** — `internal/rooms`. File
   `$XDG_STATE_HOME/tincan/rooms.json` (default `~/.local/state/tincan/`;
   `%LocalAppData%\tincan\` on Windows). Entries: canonical path, display name
   (base name, disambiguated), `id` (first 12 hex of SHA-256 of the canonical
   path), `hidden`, `last_used`. Written under a file lock with atomic replace.
   - `Touch(room)` is called by `mcp` (at startup), `up`, `send`, `ask` and
     `serve`. A registry write failure is logged to stderr and never fails the
     command.
   - First run of `tincan web` imports existing `.tincan/` directories found
     at depth ≤ 3 under `~/projects` (configurable `--scan <dir>`, repeatable).
   - The UI can add a room by absolute path (it must exist and be a
     directory; home and filesystem roots are refused, matching `tincan mcp`)
     and hide a room.
2. **Thread store** — `internal/thread`, files in the room:
   - `.tincan/threads/<tid>/thread.json`: `id`, `title`, `primary` (agent
     name), `created`, `archived`, `budget` (default 6).
   - `.tincan/threads/<tid>/messages.jsonl`: append-only messages (§5).
   - Appends take `.tincan/threads/<tid>/lock` and assign a monotonically
     increasing `seq`. Readers tolerate a torn final line by ignoring it.
   - `<tid>` is 8 random hex characters.
3. **Dispatcher** — `internal/thread`. Parses mentions, resolves targets,
   builds agent context, submits requests, collects results, enforces chain
   rules, and reconciles after restart (§6).
4. **Shared send path** — extract the MCP server's "submit a request and
   launch the listener if needed" logic (`sendTool`/`launch` in
   `internal/mcpserver/server.go`) into an exported function (package
   `internal/dispatch`) used by both the MCP server and the dispatcher, so
   routing, interactive-listener detection and idempotency stay identical.
   The MCP tool behaviour does not change.
5. **Activity reader** — lists a room's listeners (host states + presence, as
   `tincan_status` does) and recent request records with their progress
   events. Read-only; no new on-disk format.
6. **HTTP API + live events** — `internal/web`. JSON endpoints (§7) and one
   Server-Sent Events stream. Change detection uses the existing fsnotify
   dependency on each registered room's `.tincan/threads` and
   `.tincan/requests`, with a 2 s polling fallback where watching fails.
7. **UI** — `internal/web/ui/` (`index.html`, `app.js`, `app.css`), embedded
   with `go:embed`. Plain JS, no framework, no build. Markdown in messages is
   rendered by a small in-repo renderer (paragraphs, code blocks, inline code,
   lists, links); raw HTML is never rendered.
8. **Peer proxy** — `internal/web`. Reverse proxy for
   `/api/peers/<name>/*` to the configured base URL, with a 5 s connect
   timeout, SSE pass-through, and a background health check every 15 s that
   marks the peer online/offline.

## 5. Data model

Message (one JSON object per line in `messages.jsonl`; comments are
annotations only):

```jsonc
{
  "seq": 12,
  "id": "m-4f1c…",
  "time": "2026-09-28T10:03:11Z",
  "author": "codex.t7f2a9c1",
  "role": "agent",                // "user" | "agent" | "system"
  "text": "…",
  "mentions": ["claude"],         // resolved targets, in order
  "request_id": "…",              // the tincan request that produced/carries this
  "reply_to": 11,                 // seq of the message that triggered it
  "chain": {"id": "c-…", "hop": 2, "budget": 6},
  "state": "pending|running|done|error|cancelled|suggested"
}
```

- A user message has `role:"user"`, `author:"you"`, and one `pending` agent
  message is appended per dispatched target, carrying its `request_id`. The
  dispatcher later appends a state change for that message (same `id`, new
  `seq`, updated `state`/`text`); readers fold by `id`, last write wins. This
  keeps the log append-only.
- `system` messages report non-agent events (unknown preset, chain budget
  reached, Stop pressed, agent unavailable on this machine).
- `state:"suggested"` marks a mention that was not dispatched because the chain
  budget was exhausted; the UI offers a Send button that posts it as a user
  message.

## 6. Dispatch behaviour

### 6.1 Mentions

- Syntax: `@name` where `name` matches `[A-Za-z0-9][A-Za-z0-9._-]*`, preceded by
  start of text or whitespace/punctuation. Mentions inside fenced code blocks
  and inline code are ignored. Trailing `.`, `,`, `:` and `)` are not part of
  the name.
- A thread has a `primary` agent. A user message with no mentions is sent to
  the primary. A message with mentions is sent to each distinct mentioned
  target in parallel, in order of first appearance.
- Target resolution, first match wins:
  1. A preset name available on this machine (`claude`, `codex`, `grok`,
     `agy`, `kimi`, `gemini`, user presets such as `mimo`, `muse`). The
     target listener is `<preset>.<tid>` (e.g. `codex.t7f2a9c1`), launched
     with that preset and the preset's default session mode.
  2. A listener currently known in the room (hosted state or `/listen`
     presence), e.g. `@codex-audit`. It is addressed directly and keeps its own
     conversation, which is shared with whoever else uses it.
  3. Otherwise the mention stays plain text and a `system` message says the
     agent is unknown or unavailable on this machine (e.g. a preset whose
     binary does not resolve).

### 6.2 Agents calling agents

- After an agent message completes, its text is parsed with the same rules.
  Each resolved target other than the author is dispatched automatically as
  the next hop of the same chain.
- Each user message starts a new chain with the thread's `budget` (default 6)
  automatic hops. A hop that would exceed the budget is recorded as
  `suggested` instead of dispatched, and one `system` message notes that the
  budget was reached.
- An agent never dispatches to itself. If a target already has a running or
  pending request in this chain, the new mention queues behind it (the
  listener is serial) rather than creating a parallel request.
- A failed, timed-out or cancelled agent turn ends its branch of the chain:
  nothing is parsed from an error reply.
- **Stop** (per thread) cancels every non-terminal request in the thread with
  `request.Cancel` + `host.Cancel` (as `tincan_cancel` does), marks the active
  chains stopped, and appends a `system` message. Completed work, including
  file edits, is not undone.

### 6.3 Context sent to an agent

The request body for a turn is:

1. **Header**: "You are `<name>` in tincan thread '`<title>`' (room
   `<path>`). Participants: … . All participants share this working tree; use
   `git status`/`git diff` to see changes others made. Mention another
   participant with `@name` to hand them work; the owner is `@you`. Reply with
   your answer for the thread."
2. **Transcript**:
   - Session-capable listeners (`host.SupportsSessions`: claude, grok, agy,
     kimi) receive messages with `seq` greater than the last message this
     listener already received in this thread.
   - Stateless listeners (codex, gemini, custom presets) receive the whole
     thread, newest last, truncated from the oldest end to fit 64 KiB, with a
     line "[N earlier messages omitted]".
   - Existing room listeners addressed by name (§6.1.2) are treated as
     stateless.
   - Each transcript line is `author: text`; agent error cards are included
     as `author: [error] …`.
3. **The triggering message**, repeated at the end, with its author.

The body must stay below `request.MaxBodyBytes`; the 64 KiB transcript cap
keeps it well under.

### 6.4 Collection and restart

- The dispatcher watches request records for every `pending`/`running`
  message in open threads. On a terminal record it appends the final message
  state with the reply text (tincan's `ERROR …` replies become `error`), then
  runs §6.2 on success.
- Progress events (`request.Progress`) are streamed to the UI as the
  "working…" block; they are not copied into `messages.jsonl`.
- On `tincan web` start, every non-terminal agent message in non-archived
  threads is re-attached by its `request_id`; results that completed while the
  server was down are posted then, and chains continue.
- Agent-to-agent continuation happens only while `tincan web` is running;
  requests themselves complete regardless.

### 6.5 Thread listener lifecycle

- Thread listeners are launched on first dispatch via the shared send path.
- A janitor in `tincan web` stops (`host.Down`) thread listeners (names ending
  in `.<tid>`) that have been idle for 30 minutes. Their session pointers
  remain, so the next mention resumes the same conversation.
- Archiving a thread stops its listeners and clears their sessions
  (`host.ClearSession`), then sets `archived`. Archived threads are read-only
  in the UI and can be unarchived (new sessions start fresh).
- Listeners addressed by existing name (§6.1.2) are never stopped by the
  janitor or by archiving.

## 7. HTTP API

All paths are relative to the instance base path (the UI never assumes `/`).
Room IDs are registry IDs; the server maps them to paths and never accepts a
filesystem path in a URL. Peer routes are the same paths under
`/api/peers/<peer>/`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/self` | Machine name, version, peers with online state. |
| GET | `/api/rooms` | Registered rooms (id, name, path, hidden, running agent count). |
| POST | `/api/rooms` | Add a room `{path}`. |
| PATCH | `/api/rooms/{rid}` | `{hidden}`. |
| GET | `/api/rooms/{rid}/threads` | Threads with title, primary, last activity, running count. |
| POST | `/api/rooms/{rid}/threads` | Create `{title, primary}`. |
| GET | `/api/rooms/{rid}/threads/{tid}/messages?after=<seq>` | Folded messages after `seq`. |
| POST | `/api/rooms/{rid}/threads/{tid}/messages` | Post `{text}` as the owner. |
| POST | `/api/rooms/{rid}/threads/{tid}/stop` | Stop (§6.2). |
| POST | `/api/rooms/{rid}/threads/{tid}/archive` | `{archived: bool}`. |
| POST | `/api/rooms/{rid}/messages/{mid}/retry` | Re-dispatch a failed agent message as a new request. |
| GET | `/api/rooms/{rid}/activity` | Listeners and recent requests (last 50) with status. |
| GET | `/api/rooms/{rid}/requests/{id}/progress?cursor=` | Progress events. |
| GET | `/api/rooms/{rid}/agents/{name}/log?tail=` | Last 64 KiB of the host log. |
| GET | `/api/events` | SSE: `thread`, `message`, `progress`, `activity`, `peer` events with IDs for resume via `Last-Event-ID`. |

Request and response bodies are JSON; errors are `{"error": "…"}` with 4xx/5xx.
Message text is limited to 256 KiB.

## 8. UI (layout B)

- **Sidebar**: machines (with online dot), their rooms, each room's threads
  plus an "Activity (n running)" entry, and "+ new thread" (title, primary
  agent picker listing available presets). Hidden rooms are behind a toggle.
- **Thread view**: header with title, a chip per participant showing
  idle/busy/elapsed, and Stop. Messages in order; agent messages show
  "working… (handoff h/budget)" with a collapsible live-output block while
  running; errors are red cards with Retry and Open log; `suggested` mentions
  show a Send button.
- **Composer**: multiline, Enter sends, Shift+Enter newline, `@` opens an
  autocomplete of presets and room listeners.
- **Activity tab**: running and recent requests in the room from any source,
  with sender, agent, status, elapsed, live output, and "Message this agent"
  which opens a new thread whose primary is that listener.
- **Responsive**: below 768 px the sidebar becomes a drawer; the layout works
  on iPad and phone widths. Light and dark themes follow the system.

## 9. Security

- `tincan web` binds `127.0.0.1` only (flag `--listen`, which refuses non-
  loopback addresses unless `--insecure-listen` is also given). Remote access
  is only through `tailscale serve`.
- Every request must carry `Tailscale-User-Login` equal to the owner
  (`--owner`, default: the node owner's login from `tailscale status --json`
  at startup, using `tailscale` on `PATH` or, on macOS,
  `/Applications/Tailscale.app/Contents/MacOS/Tailscale`; startup fails with a
  clear message if neither `--owner` nor the CLI is available). Requests without the header or with another identity get 403.
  This excludes other tailnet users, shared-in users and tagged devices.
- State-changing requests (POST/PATCH) additionally require the header
  `X-Tincan-Request: 1` and, when present, an `Origin` matching the request's
  host, so other websites cannot trigger actions through the browser.
- The hub calls the peer through the peer's `tailscale serve` URL; Tailscale
  sets the identity header for the calling device's owner, so the peer's owner
  check passes without shared secrets. The hub never forwards the browser's
  identity header.
- Anyone passing these checks can run agents with the local presets'
  permissions (e.g. `claude --dangerously-skip-permissions`) in registered
  rooms; the docs state this plainly.
- Responses set `Content-Security-Policy: default-src 'self'`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`.

## 10. Deployment

- `tincan web --listen 127.0.0.1:7788 [--peer macmini=https://macmini.brill-decibel.ts.net/tincan] [--owner …] [--scan ~/projects]`.
- cachyos (hub): systemd user unit `tincan-web.service`;
  `tailscale serve --bg --set-path /tincan http://127.0.0.1:7788`, giving
  `https://cachyos.brill-decibel.ts.net/tincan/` beside the existing `/` and
  `/comics` routes.
- macmini (peer): launchd agent `net.tincan.web.plist` running
  `tincan web`; the same `tailscale serve` path via the Tailscale app's CLI.
- Optional dedicated name: a Tailscale Service `svc:tincan` for
  `https://tincan.brill-decibel.ts.net`, which needs the owner's approval in the
  admin console; the base-path handling makes this a configuration change only.
- `docs/web.md` documents flags, both service files and the serve commands.
  Service files are not installed by `install.sh` in v1.

## 11. Error handling

| Situation | Behaviour |
|---|---|
| Agent turn fails / times out / cancelled | Message becomes an `error` card with the `ERROR …` text; Retry and Open log; chain branch ends. |
| Preset missing on this machine | `system` message; no dispatch. |
| Unknown mention | Plain text + `system` message. |
| Browser disconnects | SSE reconnects with `Last-Event-ID`; client refetches `messages?after=`. |
| Concurrent posts | Serialized by the thread lock; ordered by `seq`. |
| Peer unreachable | Peer rooms greyed "offline"; hub keeps serving local rooms; health check retries. |
| `tincan web` restarts mid-request | §6.4 re-attach. |
| Registry unreadable | Web starts with an empty registry and a banner; commands never fail on it. |
| Room path removed | Room shown as missing; hidden on request. |

## 12. Testing

- Unit: mention parsing (code spans, punctuation, self, unknown), target
  resolution order, context building (persistent vs stateless, 64 KiB cap,
  omitted-count line), chain budget and queueing, message folding, thread log
  ordering under concurrent appends, torn-line tolerance, registry
  touch/import/hide, owner and CSRF checks (missing, wrong, correct), base-path
  handling.
- Integration (Go tests with the existing fake agent as presets `fake`,
  `fake2`): post "@fake do x" → fake replies "@fake2 check" → fake2 dispatched
  → both replies in the thread; budget exhaustion yields `suggested`; Stop
  mid-chain cancels the running request; server restart mid-request
  re-attaches and posts the result; peer proxy against a second in-process
  server, including the peer going offline and returning.
- MCP regression: existing `internal/mcpserver` tests pass unchanged after
  the shared send path extraction.
- UI smoke (manual, in a real browser): create thread, post, live progress,
  handoff, Stop, archive, phone-width drawer.
- CI: existing Linux/macOS/Windows matrix. On Windows, thread listeners need a
  foreground `tincan serve` as today; tests that launch detached listeners
  skip there, as existing ones do.

## 13. Configuration defaults

- `--idle-stop 30m`: janitor threshold for thread listeners (§6.5).
- `--chain-budget 6`: default budget for new threads (§6.2); each thread can
  override it in `thread.json`.
- `recv` also calls the registry `Touch`, so rooms used only by interactive
  `/listen` participants appear in the UI.
