# tincan — operating protocol

tincan passes messages between AI coding agents on one machine, scoped to a repo,
over a filesystem spool. No daemon, no message store, near-zero tokens while idle.
Design rationale lives in `docs/superpowers/specs/`; this file is the operating guide
that the `/tell` and `/listen` skills point to.

## Model

- Each participant has a **name** (`orch`, `codex`, `gemini`, …) and an inbox under
  `<repo>/.tincan/inbox/<name>/`.
- **Send** = drop a message file into the recipient's inbox (atomic).
- **Receive** = block until a file appears, print it, delete it. Blocking is free:
  the shell call is suspended, so the model spends no tokens while waiting.
- Unread messages **queue**; a receive that times out and re-arms loses nothing.
- **Replies** use an ephemeral per-request channel so parallel requests never cross.

## CLI

```
tincan recv  --as <name>   [--timeout 570] [--format json|body]   # block for one msg
tincan ask   --to <name> --from <name> (--body S | --body-file F)  # send + wait for reply
tincan reply --channel <reply_to> (--body S | --body-file F)       # answer a received msg
tincan send  --to <name> --from <name> (--body S | --body-file F)  # fire-and-forget
```
Common flags: `--room <path>` (default: CWD / repo root), `--artifact <path>` (repeatable).

## LISTEN loop (consultant / worker)

1. `tincan recv --as <me> --timeout 570`
2. On a message: read `body` (the task) and `reply_to`; do the work — review a diff,
   answer a question, generate a file, etc.
3. `tincan reply --channel <reply_to> --body-file answer.md` (add `--artifact out/x.png`
   for produced files; keep the body a short pointer, not the payload).
4. Re-arm: go to 1. On timeout with no message: also go to 1.

Keep looping in one turn — do not stop between iterations until the user ends the session.

## TELL / orchestrate

- **One target (blocking consult):**
  `tincan ask --to codex --from orch --body "review PR 56"` → act on the reply.
- **Several targets / fire-and-continue (Claude Code):** run each `ask` as a
  **background** shell command (`run_in_background`). Keep working; the harness
  re-invokes you when each courier returns its reply. Each `ask` waits on its own
  reply channel, so parallel couriers never collide.
- **Big replies:** spawn a background subagent whose only job is to run the `ask`,
  digest the (large) reply, and hand back a short summary + artifact pointer.
- Any non-Claude orchestrator: just run `ask` calls sequentially.

## Artifacts

Messages carry coordination + pointers, never large payloads. A worker writes its
output into the repo and returns "done, wrote `out/img.png`" with `--artifact out/img.png`.
The orchestrator reads the file from the repo.

## Troubleshooting

- `ask` printed `pending channel=<id>`: the target didn't reply in time. The request is
  still queued and the reply channel persists — re-collect with `tincan recv --as <id>`
  or re-issue.
- Nothing happens: confirm both sides share the same `--room` (repo root) and the
  listener is running `/listen`.
