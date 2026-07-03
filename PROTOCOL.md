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

## Rooms — always pass the repo root

`--room` defaults to the **current working directory, literally** — there is no
auto-climb to a repo root. An agent that `cd`s into a subdirectory and runs tincan
bare will silently create a *second* room there. Always pass the room explicitly:

```
--room "$(git rev-parse --show-toplevel)"     # or an absolute path
```

## CLI

```
tincan recv  --as <name>   [--timeout <sec=570>] [--format json|body] [--log]
tincan ask   --to <name> --from <name> (--body <s> | --body-file <f>)
             [--timeout <sec=570>] [--format json|body] [--artifact <path>]...
tincan reply --channel <reply_to> (--body <s> | --body-file <f>)
             [--from <name>] [--artifact <path>]...
tincan send  --to <name> --from <name> (--body <s> | --body-file <f>)
             [--corr <id>] [--reply-to <channel>] [--artifact <path>]...
```

All commands take `--room <path>` (see above; default `.`).

- `recv --log` keeps a copy of each consumed message in `<room>/.tincan/log/`
  (default: consumed messages are deleted; chat transcripts are the record).
- `--format body` prints just the body text; `json` (default) prints the full
  envelope (you need `json` on `recv` to see `reply_to`).
- `ask --timeout` bounds how long the orchestrator waits for the reply.
- `send --corr/--reply-to` exist for hand-rolled request/reply metadata; normally
  `ask`/`reply` manage these for you.
- `reply --from` records who answered; recommended.

**Exit codes: 0 ok, 1 error, 2 usage, 3 timeout.** Branch on these: 3 means
"nothing arrived / no reply yet", never an error.

## LISTEN loop — an agent-level loop, not a shell loop

The loop below is **your** loop (the agent's), not a `while true` in bash. A shell
loop cannot hand the task back to your reasoning context. Each iteration: run one
blocking `recv`, act on what it returns, reply, then run the next `recv`.

If your harness caps foreground command duration, run the `recv` **in the
background / async** and yield; act when it completes. Otherwise a foreground call
is fine. Either way, pick `--timeout` to fit under your harness's cap.

1. `tincan recv --as <me> --room "$(git rev-parse --show-toplevel)" --timeout 280`
2. **Exit 3 (timeout):** run step 1 again. Do not report status in between.
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
- **Gemini CLI / Antigravity:** `--yolo` / `--approval-mode`, or use the prompt's
  "always allow this command" option on `tincan recv` and `tincan reply`.

Caveat: an auto-approving listener executes whatever lands in its inbox — anything
that can write to `<room>/.tincan/inbox/` drives that agent. Fine on a single-user
machine with a trusted orchestrator; think before widening that boundary.

## Troubleshooting

- `ask` printed `pending channel=r-<id>`: the target didn't reply in time. The
  request is still queued and the channel persists — collect later with
  `tincan recv --as r-<id>`, or re-ask.
- Nothing happens: confirm both sides use the **same room path** (see Rooms above)
  and the listener is actually parked on `recv`.
- `invalid name`: names are single path components; no slashes or dots.
