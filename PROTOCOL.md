# tincan — operating protocol

tincan passes messages between AI coding agents on one machine, scoped to a repo,
over a filesystem spool. No daemon, no message store, near-zero tokens while idle.
Design rationale lives in `docs/superpowers/specs/`; this file is the operating guide
that the `/tell` and `/listen` skills point to.

## Model

- Each participant has a **name** (`orch`, `codex`, `gemini`, …) and an inbox under
  `<room>/.tincan/inbox/<name>/`. Names must be a single path component — no `/`,
  `\`, `.`, or `..` (the CLI rejects anything else).
- **Send** = drop a message file into the recipient's inbox (atomic).
- **Receive** = block until a file appears, print it, delete it. Blocking is free:
  the shell call is suspended, so the model spends no tokens while waiting.
- Unread messages **queue**; a receive that times out and re-arms loses nothing.
- **Replies** use an ephemeral per-request channel (`r-<id>`) so parallel requests
  never cross.
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
repo's root, every command prints a one-line advisory to stderr naming the
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
              [--reply stdout|file] [--exec-timeout <sec=3600>] [--wait <sec=10>]
tincan serve  <name> [same flags as up] [--daemon]
tincan down   <name> [--wait <sec=15>]
tincan presets [--format table|json]
```

All commands take `--room <path>` (see above; default `.`). `up`/`serve`/`down`/
`presets` are the **hosted listener** commands — tincan runs a headless agent CLI
for you so no human terminal is needed per agent; see below.

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
their next `recv`. `--to` and `--from` are required. tincan itself does not
enforce shutdown — it only carries the typed message. The `listen` skill is
what honors it: on receiving a message with `kind == "stop"`, the listener
acks briefly, then exits its loop instead of re-arming. **You must pass
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
3. sends that output as the reply on the message's `reply_to` channel (a
   message without `reply_to` is run and logged only);
4. loops. A `kind == "stop"` message (`tincan stop` / `tincan down`) or
   SIGTERM/SIGINT ends the loop.

Each message is a **fresh single-turn run** of the agent CLI — no memory
between messages, so briefs must be self-contained. One hosted process is one
serial worker; run `up` with different names for fan-out.

```
tincan up codex --room "$ROOM"          # up name=codex pid=12345 preset=codex
tincan ask --to codex --from orch --room "$ROOM" --body-file brief.md
tincan status --room "$ROOM"            # MODE hosted:codex, STATE parked|busy
tincan down codex --room "$ROOM"        # down name=codex
```

- `up` is idempotent: if any listener named `<name>` is already live in the
  room (hosted or interactive), it prints `already up name=<name> pid=<n>` and
  exits 0. It resolves the preset (`--preset`, else `<name>` if that is a
  known preset, else `--exec` is required → exit 2), checks the agent binary
  is on `PATH` (exit 1 if not), starts `serve … --daemon` detached (own
  session, stdio on the host log) and waits up to `--wait` seconds for the
  listener to park (exit 1 with the log path if it doesn't).
- `serve` is the loop itself, in the foreground — handy for debugging a
  preset (`Ctrl-C` stops it). `--daemon` is what `up` passes.
- `down` sends `stop` (from `orch`), waits up to `--wait` seconds for the
  process to exit, then SIGTERMs the recorded pid and, 5 s later, SIGKILLs it.
  Exit 0 if the process is gone, 1 otherwise. A `stop` arrives only when the
  current run finishes; `--wait` is how long you are prepared to let it.
- `presets` lists the effective presets (`--format json` for scripts).

### Presets and `~/.config/tincan/agents.json`

Built-in presets (the headless invocations verified on 2026-09-07):

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

Reply post-processing is minimal and preset-level: trailing whitespace is
trimmed; for `kimi` the trailing `To resume this session: …` line is dropped.
Nothing else is rewritten.

### Files under the room

```
.tincan/hosts/<name>.json   # {"pid", "preset", "exec", "started", "state": "parked|busy", "current_id"}
.tincan/hosts/<name>.log    # per message: "=== <id> from=<from> started=<ts>" … agent stdout+stderr … "=== exit=<code> duration=<s>s"
```

The state file is written atomically (tmp + rename) and removed on clean
exit; a state file whose pid is dead is stale and `status` ignores it (shows
`—`, never a phantom `busy`). Add `.tincan/` to your project's `.gitignore`
(or global excludes) — it now holds logs, not just transient messages.

### Error handling — the asker always gets a reply

- Agent exits non-zero → reply `ERROR exit=<code>` + newline + the last 4 KiB
  of its stderr, then any stdout.
- `--exec-timeout` elapsed (default 3600 s; `0` = none) → the agent's whole
  process group is killed; reply `ERROR timeout after <sec>s`.
- Agent binary missing at run time (removed after `up`) → reply
  `ERROR exec: <message>`; the listener keeps running.
- The listener is stopped (`down`/SIGTERM) mid-run → the agent is killed and
  the asker gets `ERROR interrupted: hosted listener was stopped while running`.
- Malformed/unknown `kind` → ignored and logged, no reply. A failed reply
  send (spool error) is logged; the loop continues.
- Room rules are unchanged: pass `--room` explicitly; non-root rooms warn.

A reply body starting with `ERROR ` is therefore the *agent* failing, not
tincan — treat it as such in the orchestrator.

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
serve <name>` in a terminal works there, and the per-run process-group kill
uses `taskkill /T`.

## Starting a listener, per agent

tincan listeners are implemented using the `listen` skill. Although all three supported agents execute the same underlying `recv` loop defined in `SKILL.md`, how they discover and invoke the skill varies:

| Agent | Skill Discovery | How to Invoke | Recommended Auto-Approve Setup |
| :--- | :--- | :--- | :--- |
| **Claude Code** | User-level skill path: `~/.claude/skills/listen` | Slash command `/listen` (optionally `as <name>`) | Allowlist `Bash(tincan *)` in the project's `.claude/settings.json` |
| **Antigravity** | Workspace skill path: `.agents/skills/` (repo ships `.agents/skills` -> `skills` symlink) | Slash command `/listen` | Start with `agy --dangerously-skip-permissions` or use prompt-level "always allow this command" |
| **Codex** | Workspace skill path: `skills/listen/SKILL.md` (via native registry) | Plain query `"listen"` or matching request (no `/listen` command exists) | Run with `--full-auto` or `--ask-for-approval never --sandbox workspace-write` |

### Onboarding & Best Practices

- **Backgrounding `recv`:** If your agent harness caps the duration of foreground commands, run the `recv` command in the background/async (e.g., in Antigravity, by executing the command with `WaitMsBeforeAsync` and yielding) and process the output when the background task wakes you up.
- **Audit Logging (optional):** add `--log` to `tincan recv` to keep an audit copy of consumed messages in `<room>/.tincan/log/`. Default is off — tincan deliberately stores nothing, and the agents' chat transcripts are the canonical record.

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
3. **On a message** (JSON): `body` is the task; note `reply_to` (`r-<hex>`). Do the
   work — review the diff, answer the question, generate the file.
4. `tincan reply --channel <reply_to> --from <me> --body-file answer.md`
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

- `ask` printed `pending channel=r-<id>`: the target didn't reply in time. The
  request is still queued and the channel persists — collect later with
  `tincan recv --as r-<id>`, or re-ask.
- Nothing happens: confirm both sides use the **same room path** (see Rooms above)
  and the listener is actually parked on `recv` — check with
  `tincan ping --to <name> --room <path>` or `tincan status --room <path>`
  instead of guessing from `ps`.
- `invalid name`: names are single path components; no slashes or dots.
- `up` said `did not park` / `exited before parking`: read
  `<room>/.tincan/hosts/<name>.log` — the agent CLI's own usage/auth errors
  land there. `tincan serve <name> --room <path>` runs the same loop in the
  foreground for a closer look.
- A reply body starting `ERROR exit=` / `ERROR timeout` / `ERROR exec:` came
  from a hosted listener whose agent failed; the details are in the same log.
