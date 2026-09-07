# tincan — operating protocol

tincan passes messages between AI coding agents on one machine, scoped to a repo,
over a filesystem spool. No network service or background coordinator; messages,
hosted logs and request state are stored locally. Near-zero tokens while idle.
Design rationale lives in `docs/superpowers/specs/`; this file is the operating guide
that the `/tell` and `/listen` skills point to.

## Model

- Each participant has a **name** (`orch`, `codex`, `gemini`, …) and an inbox under
  `<room>/.tincan/inbox/<name>/`. Names and message IDs are portable path
  components, at most 128 bytes. Empty names, `.`/`..`, path separators, control
  characters, Windows-reserved characters/device names, and trailing dots/spaces
  are rejected. Interior dots are allowed.
- **Send** = publish a synced message file into the recipient's inbox with an
  atomic hard link, without overwriting existing work. The local spool filesystem
  must support hard links (for example POSIX filesystems or NTFS).
- **Receive** = block until a file appears, claim it, print it, acknowledge it.
  Hosted listeners retain claims until delivery. Blocking is free:
  the shell call is suspended, so the model spends no tokens while waiting.
- Unread messages **queue**; a receive that times out and re-arms loses nothing.
- CLI and interactive **replies** use a per-request channel (`r-<id>`) so parallel
  requests never cross. Hosted MCP requests store results directly in the durable
  request journal.
- While a receive is parked, its process writes a small per-receiver presence
  file under `<room>/.tincan/present/<name>/` (separate from the inbox) so
  `status`/`ping` can answer "is a listener parked?" without sending a
  message — see below. One file per parked receive (tincan allows several
  receivers to park as the same name) so one returning never erases another
  still-parked receiver's presence.

## Rooms — always pass the repo root

`--room` defaults to the **current working directory, literally** — there is no
auto-climb to a repo root. An agent that `cd`s into a subdirectory and runs tincan
bare will silently create a *second* room there. Always pass the room explicitly:

```
--room "$(git rev-parse --show-toplevel)"     # or an absolute path
```

If `--room` resolves to a directory that's inside a git repo but isn't that
repo's root, messaging and hosted lifecycle commands print an advisory to stderr naming the
repo root it found — the warning never fails the command or changes its exit
code, it just flags the footgun before it silently splits your room in two.

This warning is floored at `$HOME`: if the repo root it finds is your home
directory or an ancestor of it (a `~/.git` dotfiles repo is the common case),
nothing is printed — advising `--room ~` would be actively wrong. A real
project repo below `$HOME` (e.g. `~/projects/foo`) still warns normally.

## CLI

```
tincan recv   --as <name>   [--timeout <sec=570>] [--format json|body] [--log]
tincan ask    --to <name> --from <name> (--body <s> | --body-file <f>)
              [--timeout <sec=570>] [--format json|body] [--artifact <path>]...
tincan reply  --channel <reply_to> (--body <s> | --body-file <f>)
              [--from <name>] [--artifact <path>]...
tincan send   --to <name> --from <name> (--body <s> | --body-file <f>)
              [--corr <id>] [--reply-to <channel>] [--artifact <path>]...
tincan status [--format table|json]
tincan ping   --to <name>
tincan stop   --to <name> --from <name>

tincan up     <name> [--preset <p>] [--exec '<template>'] [--stdin body|none]
              [--reply stdout|file] [--session persistent|stateless]
              [--exec-timeout <sec=3600>] [--wait <sec=10>]
tincan serve  <name> [same flags as up] [--daemon]
tincan down   <name> [--wait <sec=15>]
tincan presets [--format table|json]
tincan version [--format json]
tincan mcp --room <absolute-path>
tincan gc [--older-than 168h] [--room <path>]
```

Messaging, hosted lifecycle and cleanup commands take `--room <path>` (default
`.`); MCP binds its room at startup. `presets` and `version` are room-independent.
`up`/`serve`/`down` are the **hosted listener** commands — tincan runs a headless
agent CLI for you so no human terminal is needed per agent; see below.

- `recv --log` keeps a copy of each consumed message in `<room>/.tincan/log/`
  (default: consumed messages are deleted; chat transcripts are the record).
- `--format body` prints just the body text; `json` (default) prints the full
  envelope (you need `json` on `recv` to see `reply_to`).
- `ask --timeout` bounds how long the orchestrator waits for the reply.
- `send --corr/--reply-to` exist for hand-rolled request/reply metadata; normally
  `ask`/`reply` manage these for you.
- `reply --from` records who answered; recommended.
- `--timeout 0` (or negative) on `recv`/`ask` means **block until a message
  arrives — no deadline, no re-arm.** fsnotify already wakes instantly on a real
  message, so a timeout buys no latency, only cost: every re-arm is a fresh agent
  turn that re-reads full context. Use `--timeout 0` wherever your harness can run
  the receive in the background or as a genuinely long-running foreground call;
  otherwise pick the longest timeout your harness's cap allows.
- `status`/`ping` are read-only observability: they answer "is a listener
  actually parked on `recv`?" without sending a message (zero agent wake). See
  below.
- `stop` sends a typed wind-down control message (see below); it is otherwise
  an ordinary send — the message queues and is delivered on the recipient's
  next `recv`.

**Exit codes: 0 ok, 1 error, 2 usage, 3 timeout.** Branch on these: 3 means
"nothing arrived / no reply yet", never an error.

## Presence — `status` / `ping`

Before `status`/`ping`, checking "is a listener alive?" meant `ps` plus a
round-trip `ask`/`reply` — a real message, a real agent wake. `status` and
`ping` answer it for free: a `Recv` writes a small presence file (holding its
pid and a since-timestamp) for the duration it is parked, under
`<room>/.tincan/present/<name>/<token>` — one file per parked receive, keyed
by a token unique to that `Recv` call, since more than one receiver can be
parked as the same name at once. `status`/`ping` just read that directory
(a name is "present" if any token's pid is still alive) plus a pid-liveness
check per token. **No message is sent; nothing wakes an agent.**

```
tincan status [--room <path>] [--format table|json]
tincan ping   --to <name> [--room <path>]
```

- `status` lists every name that has a queue and/or a live listener: name,
  queued count, `MODE` (`hosted:<preset>` for a listener tincan hosts, `agent`
  for an interactive `/listen` session, `—` for none), `STATE` (`parked`,
  `busy` — a hosted listener running its agent right now — or `—`), pid, and
  how long it's been parked. `--format json` prints the same data as a JSON
  array (one object per name) for scripting; hosted rows add `mode`, `preset`,
  `busy` and `current_id`.
- `ping --to <name>` is the single-name yes/no form: prints `present
  pid=<n>` and exits `0` if a live listener is parked for `<name>`, or
  prints `absent` and exits `1` otherwise.
- "Parked" means a `recv` (or the receive half of `ask`) is blocked waiting
  right now — not "the agent is running", "busy processing a message" also
  shows as not-parked. A listener that re-arms in a loop (see below) is
  "parked" during each wait and briefly "—" between messages; that's
  expected, not a bug.
- A presence file surviving its process (e.g. the listener was killed rather
  than exiting normally) is detected and reported as absent: `status`/`ping`
  also check that the recorded pid is actually alive, so a crashed listener
  doesn't linger as a false "parked" row.

## Control messages — `kind` / `stop`

Every envelope carries a `kind` field (`"kind"` in JSON, `omitempty`): `""`
(the default, omitted from JSON entirely) marks an ordinary message; `"stop"`
marks a wind-down control message. Existing/legacy JSON without a `kind` key
still unmarshals to `Kind == ""` — this is purely additive, no on-disk or
protocol break.

```
tincan stop --to <name> --from <name> [--room <path>]
```

`stop` builds an envelope with `Kind: "stop"` and an empty body, and sends it
like any other message: it queues in `<name>`'s inbox and is delivered on
their next `recv`. `--to` and `--from` are required. Hosted listeners honor it
inside tincan. Interactive listeners honor it through the `listen` skill: the
agent acknowledges briefly, then exits its loop instead of re-arming. **You must pass
`--format json` on `recv` to see `kind`** — `--format body` only prints the
message body and drops the envelope, including `kind`.

## Hosted listeners — `up` / `serve` / `down`

A listener used to mean an *interactive* agent session running `/listen`, so a
human had to open one terminal per agent before an orchestrator could `/tell`
it anything. A **hosted listener** removes the human: `tincan up <name>` starts
a detached `tincan serve` process that

1. parks on `<room>/.tincan/inbox/<name>/` with the normal receive (so
   presence, `status` and `ping` work unchanged);
2. for each ordinary message, renders the configured agent command with the
   message body as the prompt, runs it with `cwd = room`, and captures its
   output;
3. persists the terminal result, sends a reply if `reply_to` is present, then
   acknowledges the claimed message;
4. loops. A `kind == "stop"` message (`tincan stop`), authenticated shutdown
   (`tincan down`), or SIGTERM/SIGINT ends the loop.

Each request runs one provider CLI process. Claude, Grok, Agy and Kimi native
executable presets resume a conversation scoped to the room and listener name by
default. Codex, Gemini and custom executables default to stateless runs and need
self-contained briefs. One hosted process is one serial worker; run `up` with
different names for fan-out. Select a policy with `--session persistent|stateless`.

```
tincan up codex --room "$ROOM"          # up name=codex pid=12345 preset=codex
tincan ask --to codex --from orch --room "$ROOM" --body-file brief.md
tincan status --room "$ROOM"            # MODE hosted:codex, STATE parked|busy
tincan down codex --room "$ROOM"        # down name=codex
```

- `up` reconnects to a live listener named `<name>` and prints
  `already up name=<name> pid=<n>`. Explicit settings must match the running
  hosted configuration; stop it before selecting another preset or session mode.
  For a new listener it resolves the preset (`--preset`, else `<name>` if that is a
  known preset, else `--exec` is required → exit 2), checks the agent binary
  is on `PATH` (exit 1 if not), starts `serve … --daemon` detached (own
  session, stdio on the host log) and waits up to `--wait` seconds for authenticated
  readiness. A listener can become ready while already processing queued work.
  Failure reports the host log path.
- `serve` is the loop itself, in the foreground — handy for debugging a
  preset (`Ctrl-C` stops it). `--daemon` is what `up` passes.
- `down` sends an authenticated shutdown request to a hosted listener,
  immediately cancels its active run and waits for its ownership lock to be
  released. It does not signal a process based on a stored PID. For an
  interactive listener it sends the ordinary typed stop envelope. A queued
  `tincan stop` waits until the listener next receives; use `down` to interrupt
  an active hosted run.
- `presets` lists the effective presets (`--format json` for scripts).

### Presets and `~/.config/tincan/agents.json`

Base preset commands (session adapters add the explicit session and output flags
described below):

| preset | exec template | stdin | reply |
|---|---|---|---|
| `codex`  | `codex exec -s workspace-write --skip-git-repo-check -o {out} -` | body | file |
| `grok`   | `grok -p {body} --always-approve` | none | stdout |
| `kimi`   | `kimi -p {body}` | none | stdout |
| `agy`    | `agy -p {body} --dangerously-skip-permissions --model gemini-3.8-flash-high --print-timeout 150m` | none | stdout |
| `gemini` | `gemini -p {body}` | none | stdout |
| `claude` | `claude -p {body} --dangerously-skip-permissions` | none | stdout |

Placeholders: `{body}` (the message body, passed as **one argv element** —
never through a shell), `{out}` (a per-message temp file whose contents become
the reply when `reply = file`), `{room}`, `{name}`, `{id}`. `stdin = body` pipes
the body to the process' stdin instead. Templates given with `--exec` are
split on unquoted whitespace (single/double quotes group a token) *before*
substitution, so a body can never be re-tokenized.

`~/.config/tincan/agents.json` overrides or adds presets (an entry replaces
the built-in of the same name wholesale); merge order is built-in ← config
file ← command-line flags:

```json
{
  "agy":   { "exec": ["agy", "-p", "{body}", "--dangerously-skip-permissions", "--model", "gemini-3.8-flash-high", "--print-timeout", "150m"], "stdin": "none", "reply": "stdout" },
  "myllm": { "exec": ["my-llm", "--prompt-file", "-"], "stdin": "body", "reply": "stdout", "exec_timeout_sec": 1800 }
}
```

Persistent adapters use structured output and return only the final assistant
answer. Assistant text becomes progress; startup inventories, thoughts and
provider stderr remain in the private host log. Empty, invalid, incomplete or
oversized structured output produces an explicit error. Stateless commands keep
their legacy text/file replies; trailing whitespace and Kimi's resume-hint footer
are trimmed.

### Conversations and reset

The `session` field in a preset can be `persistent` or `stateless`. An omitted
field selects persistent mode only for the native `claude`, `grok`, `agy` and
`kimi` executables under their respective preset labels. A custom wrapper must
explicitly opt into a supported adapter; its executable and other arguments are
preserved. Persistent adapters manage output/resume flags, so remove conflicting
`--continue`, `--resume` or session-ID flags from the configured command.

| Provider | Structured format | Explicit resume argument |
|---|---|---|
| Claude | `stream-json` with `--verbose` | `--resume <UUID>` |
| Grok | `streaming-json` | `--resume <UUID>` |
| Agy | `stream-json` | `--conversation <UUID>` |
| Kimi | `stream-json` | `--session <session_UUID>` |

Saved pointers in `.tincan/sessions/<name>.json` survive host and MCP restarts.
Each pointer records its provider and preset identity; changing those requires
an explicit reset. The adapters never resume a provider's most recent global
conversation. If initialization is interrupted before a usable ID is observed,
the next request fails with a reset instruction. A failed resume preserves the
saved pointer and error; it never silently starts a replacement conversation.

MCP `tincan_reset` stops the listener, interrupts its active request and atomically
removes the saved pointer under the launch/lifetime locks. Its next launch starts
fresh. Provider-side transcripts are preserved. Stop and reset both leave queued
requests available for the next launch, so cancel unwanted queued work explicitly
before resetting. Stopping alone preserves the conversation.

### Files under the room

```
.tincan/hosts/<name>.json   # private owner/control credentials, preset, session_mode, state, current_id
.tincan/hosts/<name>.log    # per message: "=== <id> from=<from> started=<ts>" … agent stdout+stderr … "=== exit=<code> duration=<s>s"
.tincan/sessions/<name>.json # saved provider conversation pointer and preset identity
.tincan/requests/<id>.json # durable request status and result
.tincan/requests/<id>.jsonl # bounded progress events
```

New runtime directories use `0700` and regular files use `0600` on POSIX.
Send and claim also tighten an existing `.tincan` root to `0700`; room/ancestor
permissions and existing child file modes are left alone. On Windows, protect
the room with the appropriate user ACLs.
Spool paths reject symlinks. Inbox input must be a regular file containing a
valid envelope no larger than 8 MiB; malformed, mismatched or unsafe input is
quarantined so later valid work can continue.

Unacknowledged claims live under `.tincan/inflight/<name>/`. A host restart treats
leftover work as interrupted/uncertain. A process may have changed the workspace
before it died, so tincan does not automatically execute that work again.
Inspect the log and files before deliberately submitting a new task. This is
durable delivery bookkeeping, not a guarantee of exactly-once external effects.

The state file is written atomically (tmp + rename) and removed on clean
exit; status validates the authenticated host lifetime before reporting it as
alive or busy. Add `.tincan/` to your project's `.gitignore`
(or global excludes) — it now holds logs, not just transient messages.

An OS lock gives each hosted name one owner for its lifetime. Readiness and
shutdown authenticate to that owner through a private loopback control endpoint;
the stored PID alone never authorizes shutdown. Output streams to the log and
bounded request progress, with at most a 1 MiB in-memory tail per stream.

### Error handling and interrupted work

- A structured provider failure → `ERROR session: provider failed …` with the
  parsed reason, including when the provider exits nonzero. Other nonzero exits
  return `ERROR exit=<code>` and the last 4 KiB of stderr; stateless commands also
  include captured stdout. Raw structured startup metadata is excluded.
- `--exec-timeout` elapsed (default 3600 s; `0` = none) → the agent's whole
  process group is killed; reply `ERROR timeout after <sec>s`.
- Agent binary missing at run time (removed after `up`) → reply
  `ERROR exec: <message>`; the listener keeps running.
- The listener is stopped (`down`/SIGTERM) mid-run → the agent is killed and
  the asker gets `ERROR interrupted: hosted listener was stopped while running`.
- Malformed envelopes are quarantined; an unknown `kind` is not executed.
- A hard crash may prevent an immediate reply. Retained claims and request state
  allow recovery to report interrupted work; a missing reply is not permission
  to repeat an agent run automatically.
- Room rules are unchanged: pass `--room` explicitly; non-root rooms warn.

A hosted `ERROR ` reply marks failed or interrupted work. Inspect its details to
distinguish provider, session, process and storage failures before deciding what
to do next.

### Retention

`tincan gc --room "$ROOM" --older-than 168h` removes old terminal request records
and progress once they are no longer needed by a queued/in-flight delivery, plus
old consumed-message audit logs and quarantine entries. The default age is seven
days; the minimum is one hour. It retains active work, queued messages, claims,
session pointers and host logs. After a request record is collected, its result
and idempotency protection are gone; do not reuse old request IDs for new work.

## MCP stdio

Start a server with `tincan mcp --room /absolute/path/to/repo`. The room is fixed
for that server's lifetime; tools cannot override it. MCP uses stdin/stdout and
diagnostics use stderr. The binary's `tincan version --format json` reports
version, commit, date, modification status, Go version and module identity.
Build this checkout for the interface below: the `v0.1.0` release does not
contain it, and `@main` includes only changes published to the main branch.

Clients with an `mcpServers` JSON configuration can use:

```json
{
  "mcpServers": {
    "tincan": {
      "command": "/absolute/path/to/tincan",
      "args": ["mcp", "--room", "/absolute/path/to/repo"]
    }
  }
}
```

Tools expose structured inputs/results:

| Tool | Arguments | Result / behavior |
|---|---|---|
| `tincan_presets` | `{}` | Preset names, executable availability and adapter support. |
| `tincan_launch` | `name`, optional `preset`, `session_mode` | Agent status and `already`; matching live names reconnect. |
| `tincan_send` | `agent`, `body`, optional `request_id`, `from` | Durable request view; starts an absent configured listener. |
| `tincan_wait` | `request_id`, optional `timeout_seconds`, `cursor` | Request status/result, progress `events` and `next_cursor`. |
| `tincan_status` | Optional `name` or `request_id` | Room/listener views, or one request. |
| `tincan_cancel` | `request_id` | Requests cancellation of hosted work; earlier changes remain. |
| `tincan_reset` | `name` | Stops the listener, interrupts active work and clears its pointer. |
| `tincan_stop` | `name` | Stops the listener and interrupts active work; keeps its pointer. |

The MCP interface accepts configured presets, without an arbitrary execution
template. Preset configuration is a local trust decision: a configured worker
can edit files or perform other actions permitted by its provider CLI.

For example, invoke the tools with these JSON argument objects in order:

```json
{"name":"reviewer","preset":"claude","session_mode":"persistent"}
```

Pass that object to `tincan_launch`, then submit through `tincan_send`:

```json
{"agent":"reviewer","from":"mcp","body":"Review the current diff","request_id":"review-1"}
```

Collect through `tincan_wait`:

```json
{"request_id":"review-1","timeout_seconds":30,"cursor":0}
```

The wait timeout is `0`–`30` seconds; omitted/zero polls immediately. Pass the
returned `next_cursor` as the next `cursor` to receive subsequent progress, and
keep waiting on the same ID while `terminal` is false. States are `queued`,
`running`, `completed`, `failed`, `interrupted` and `canceled`; the final four are
terminal. Progress is bounded and advisory; `result` is the authoritative answer.
Clients that supply an MCP progress token also receive progress notifications.

An explicit `request_id` is an idempotency key for identical agent/from/body
values while the record is retained. Different work with the same ID is rejected.
New or omitted IDs submit new work. The prompt limit is 1 MiB; `from` defaults to
`mcp`. If startup fails after saving work, retain the ID from the error, launch
the intended listener and collect or retry that same ID. A custom listener name
such as `reviewer` needs an explicit launch with its preset after being stopped.

Hosts and retained requests continue after the MCP connection closes. Reconnect
to the same room and use the same ID with wait/status. A wait deadline does not
cancel work. Cancellation or a crash may leave changes already made by the
worker; inspect state and workspace before submitting new work. Interrupted work
is never replayed automatically. Stop/reset preserve queued work for a later
launch; cancel each unwanted queued request first.

MCP can also send to an existing interactive `/listen` receiver. Its ordinary
reply is collected into the durable journal by wait or status. Outstanding
interactive requests keep routing to that receiver while it is busy and has no
parked presence. The interactive
receiver does not report hosted running/progress state, and request cancellation
is unsupported for that legacy loop; use the interactive session's own controls.
Publication intent is saved before dispatch, and identical retries never republish
confirmed interactive work. If a crash prevents confirming publication and the
envelope is no longer queued or in flight, the request becomes `interrupted` with
an uncertain outcome. Inspect the receiver and workspace before submitting new
work: delivery or side effects may already have occurred.
A later validated reply can resolve this publication uncertainty and supply the
actual result. Other terminal results remain unchanged by duplicate replies.
MCP marks this case with `outcome_uncertain: true`. It is terminal for dispatch
(the task will never be replayed), but wait/status can still collect an actual
reply if the receiver handled it. A bounded wait uses its timeout to await that
evidence; inspect the receiver and workspace if the uncertainty remains.

### Security caveat

Same trust boundary as before, stated more loudly: **anything that can write
into `<room>/.tincan/inbox/<name>/` drives the hosted agent**, and the presets
bake in auto-approval flags — that is the point. Mitigations: presets use the
narrowest mode that works unattended (`codex -s workspace-write`, never
`danger-full-access`); `cwd` is the room; the body is an argv element or
stdin, never interpolated into a shell string. Fine on a single-user machine
with a trusted orchestrator; think before widening that boundary.

### Platform note

Detaching (`up`) is implemented for Linux/macOS (new session, stdio to the
log). On Windows `up` fails with a clear message in this iteration; `tincan
serve <name>` in a terminal works there. Agent processes run in a Windows Job
Object so interruption terminates their descendants too.

## Starting a listener, per agent

tincan listeners are implemented using the `listen` skill. Although all three supported agents execute the same underlying `recv` loop defined in `SKILL.md`, how they discover and invoke the skill varies:

| Agent | Skill Discovery | How to Invoke | Recommended Auto-Approve Setup |
| :--- | :--- | :--- | :--- |
| **Claude Code** | User-level skill path: `~/.claude/skills/listen` | Slash command `/listen` (optionally `as <name>`) | Allowlist `Bash(tincan *)` in the project's `.claude/settings.json` |
| **Antigravity** | Workspace skill path: `.agents/skills/` (repo ships `.agents/skills` -> `skills` symlink) | Slash command `/listen` | Start with `agy --dangerously-skip-permissions` or use prompt-level "always allow this command" |
| **Codex** | User skills: `~/.agents/skills/listen/SKILL.md`; workspace `.agents/skills/` also works | Plain query `"listen as codex"` or matching request | Run with `--full-auto` or `--ask-for-approval never --sandbox workspace-write` |

### Onboarding & Best Practices

- **Backgrounding `recv`:** If your agent harness caps the duration of foreground commands, run the `recv` command in the background/async (e.g., in Antigravity, by executing the command with `WaitMsBeforeAsync` and yielding) and process the output when the background task wakes you up.
- **Audit Logging (optional):** add `--log` to `tincan recv` to keep an audit copy
  of consumed messages in `<room>/.tincan/log/`. Default is off for interactive
  receives; hosted logs, active claims and MCP request records have separate
  persistence requirements.

## LISTEN loop — an agent-level loop, not a shell loop

The loop below is **your** loop (the agent's), not a `while true` in bash. A shell
loop cannot hand the task back to your reasoning context. Each iteration: run one
blocking `recv`, act on what it returns, reply, then run the next `recv`.

If your harness can run the `recv` **in the background / long-running** (async, or
a foreground call with no duration cap), use `--timeout 0`: it blocks until a
message arrives, with no re-arm in between. A positive timeout costs one agent
re-arm — a full-context re-read — per timeout window, for zero latency benefit
(fsnotify wakes instantly on a real message regardless). Only use a positive
timeout, sized to fit under the cap, when your harness caps foreground command
duration and you cannot background the call.

1. `tincan recv --as <me> --room "$(git rev-parse --show-toplevel)" --timeout 0`
2. **Exit 3 (timeout, positive-timeout mode only):** run step 1 again. Do not
   report status in between.
3. **On a message** (JSON): inspect `kind` first. `"stop"` means acknowledge in
   chat and exit without replying or re-arming; ignore unknown kinds. For an
   absent/empty `kind`, `body` is the task; note `reply_to` (`r-<hex>`). Do the work.
4. If `reply_to` is present:
   `tincan reply --room "$ROOM" --channel <reply_to> --from <me> --body-file answer.md`
   (add `--artifact out/x.png` for produced files; keep the body a short pointer,
   not the payload).
5. Go to step 1. Continue until the user stops the session.

## TELL / orchestrate

- **One target (blocking consult):**
  `tincan ask --to codex --from orch --body "review PR 56"` → act on the reply.
- **Several targets / fire-and-continue:** run each `ask` in the **background**
  (e.g. Claude Code `run_in_background`). Keep working; act on each reply as its
  courier completes. Each `ask` waits on its own `r-<id>` channel, so parallel
  couriers never collide.
- **Big replies:** spawn a background subagent whose only job is to run the `ask`,
  digest the (large) reply, and hand back a short summary + artifact pointer.
- Any agent without background execution: run `ask` calls sequentially in the
  foreground.

## Artifacts

Messages carry coordination + pointers, never large payloads. A worker writes its
output into the repo and returns "done, wrote `out/img.png`" with
`--artifact out/img.png`. The orchestrator reads the file from the repo.

## Running listeners unattended (approvals)

A listener runs the same two commands forever; per-command approval prompts break
the flow. Prefer the narrowest grant your agent supports:

- **Claude Code:** allowlist `Bash(tincan *)` in the project's `.claude/settings.json`
  (plus normal edit permissions). No yolo mode needed.
- **Codex:** `codex --full-auto` (sandboxed to workspace writes, auto-approves) is
  usually enough; `--ask-for-approval never --sandbox workspace-write` is the
  granular form. Avoid `--yolo` unless the sandbox blocks you.
- **Antigravity / Gemini CLI:** Start the session with `agy --dangerously-skip-permissions` to skip command and file permissions check, or use the CLI/prompt's "always allow this command" option on `tincan recv` and `tincan reply`.

Caveat: an auto-approving listener executes whatever lands in its inbox — anything
that can write to `<room>/.tincan/inbox/` drives that agent. Fine on a single-user
machine with a trusted orchestrator; think before widening that boundary.

## Troubleshooting

- `ask` printed `pending channel=r-<id>`: no reply arrived before the deadline.
  The original request may be queued or running. Collect it with
  `tincan recv --as r-<id> --room "$ROOM" --timeout 0`. Do not re-ask the same
  task: that submits duplicate work. A waiting timeout does not cancel work.
- Nothing happens: confirm both sides use the **same room path** (see Rooms above)
  and the listener is actually parked on `recv` — check with
  `tincan ping --to <name> --room <path>` or `tincan status --room <path>`
  instead of guessing from `ps`.
- `invalid name`: use a portable path component; see the naming rules above.
- `up` said `did not park` / `exited before parking`: read
  `<room>/.tincan/hosts/<name>.log` — the agent CLI's own usage/auth errors
  land there. `tincan serve <name> --room <path>` runs the same loop in the
  foreground for a closer look.
- A reply body starting `ERROR exit=` / `ERROR timeout` / `ERROR exec:` came
  from a hosted listener whose agent failed; the details are in the same log.
