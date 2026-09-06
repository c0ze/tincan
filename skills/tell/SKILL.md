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

## Ensure the target is up (hosted listeners)

Before the first `ask` to `<who>` in this session, check that a listener is parked:

```
tincan ping --to <who> --room "$ROOM"
```

- Exit 0 (`present pid=…`): proceed.
- Exit 1 (`absent`): if `<who>` is a known preset (`tincan presets` lists them —
  built-ins are codex, grok, kimi, agy, gemini, claude; `~/.config/tincan/agents.json`
  can add more) or the user gave you an `--exec` template, bring it up yourself:
  `tincan up <who> --room "$ROOM"` (add `--exec '<template>'` for a custom command).
  It prints `up name=<who> pid=… preset=…` and returns once the listener is parked.
  If it exits non-zero, stop and report its stderr (it names the host log,
  `.tincan/hosts/<who>.log`) — do not fall back to driving the CLI by hand.
- Not a preset and no template: ask the user to start `/listen` for `<who>`, or for
  a command to host.

`up` is idempotent, so calling it when a listener is already there is harmless.

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
- Ensure the target is actually running `/listen` in the same room — or hosted
  via `tincan up` (see above); `tincan status --room "$ROOM"` shows both.
- A reply body starting `ERROR exit=` / `ERROR timeout` / `ERROR exec:` /
  `ERROR interrupted` comes from a hosted listener whose agent run failed — treat
  it as that agent failing, not as a tincan error; details are in
  `.tincan/hosts/<who>.log`.
- Hosted runs are single-turn: each brief must be self-contained (no "as I said
  before").
- When the session's work is done, stop what you started:
  `tincan down <who> --room "$ROOM"` for each hosted listener you brought up.
