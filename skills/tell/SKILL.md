---
name: tell
description: Delegate a task to another running agent via tincan and act on its reply. Use for "/tell <agent> <task>" (e.g. "/tell codex review PR 56", "/tell gemini generate an image from prompt.md") or when asked to consult/hand work to another agent. Supports parallel fan-out to several agents at once.
---

# tincan: tell

You are the **orchestrator** side of tincan. Send a task to a named agent that is
running `/listen`, then act on its reply. See `PROTOCOL.md` in the tincan repo for
the full model. Your name defaults to `orch`.

## Setup

Resolve the room once: `ROOM="$(git rev-parse --show-toplevel)"` (absolute CWD if
not in a repo). tincan does NOT auto-climb — always pass `--room "$ROOM"`; the
listener must use the same path.

## Parse

From the request, extract each **target** (`codex`, `gemini`, …) and its **task**.
Put anything long (a prompt, instructions) in a file and pass `--body-file`.

## One target — blocking consult

```
tincan ask --to <who> --from orch --room "$ROOM" --body-file task.md [--timeout <sec>]
```
Blocks (no token cost) until the reply arrives; then act on it. `--format body`
prints just the answer text; default `json` gives the full envelope. If the reply
lists artifacts, read them from the repo. Size `--timeout` to the task (default
570s; reviews and generation can need more).

## Several targets / fire-and-continue — fan-out

Run each `ask` as a **background** shell command (`run_in_background`) so they work
in parallel and you stay free:

- Dispatch one background `tincan ask --to <who> --from orch --room "$ROOM" …` per target.
- Keep doing your own work. When each courier finishes, you're re-invoked with
  that agent's reply — integrate it, then continue.
- Each `ask` waits on its own `r-<id>` reply channel, so replies never cross.

**Large replies:** instead of a bare background `ask`, spawn a background subagent
whose only job is to run the `ask`, digest the big reply, and return a short
summary + artifact pointer — keeping your context clean.

## Notes

- Exit 3 with `pending channel=r-<id>` = no reply in time; the request is still
  queued. Retry, or collect later with `tincan recv --as r-<id> --room "$ROOM"`.
- Exit 1 = real error (read stderr); exit 2 = your flags were wrong.
- Ensure the target is actually running `/listen` in the same room.
