---
name: tell
description: Delegate a task to a hosted or running interactive agent via tincan and act on its reply. Use for "/tell <agent> <task>" (e.g. "/tell codex review PR 56", "/tell gemini generate an image from prompt.md") or when asked to consult/hand work to another agent. Supports parallel fan-out to several agents at once.
---

# tincan: tell

You are the **orchestrator** side of tincan. Send a task to a named agent, then
act on its reply. Tincan can host the agent or reach an existing `/listen`
receiver. See `PROTOCOL.md` in the tincan repo for
the full model. Your name defaults to `orch`.

## Native MCP workflow

When tincan MCP tools are available for the intended room, use them and skip the
CLI workflow below. Tool names may have a client-specific namespace prefix.

1. Call `tincan_status` to confirm its fixed room matches the task's workspace.
   Use `tincan_presets` when you need to check available providers.
2. Call `tincan_send` with `agent`, `body`, `from: "orch"`, and a new unique
   `request_id` that you retain for this task. Send starts a configured listener
   automatically. For an alias or explicit session mode, first call
   `tincan_launch` with `name`, `preset`, and optionally `session_mode`.
3. Call `tincan_wait` with that same `request_id` and `timeout_seconds: 30`.
   Retain `next_cursor` and pass it as `cursor` on subsequent waits. Continue
   until `terminal` is true; a wait timeout does not resubmit or cancel work.
4. Act on the final `result`. Inspect failed/interrupted results and the
   workspace before deciding whether new work is needed. Reuse an existing ID
   only for identical work, never to retry the side effects of a failed task.

For several agents, submit their tasks independently, then collect each request
by its own ID while continuing useful local work. Keep listeners running between
follow-ups: supported providers retain conversation context across calls and MCP
reconnects. `tincan_cancel` targets one request when `cancellable` is true;
`tincan_stop` ends a listener, and `tincan_reset` clears its saved conversation.
Stopping/resetting preserves queued work. Stop listeners you started when the
session's work is done, unless the user asked to keep them available.

## CLI setup (when MCP is unavailable)

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
  It prints `up name=<who> pid=… preset=…` and returns once the hosted listener is
  authenticated and ready, even if it has already begun queued work.
  If it exits non-zero, stop and report its stderr (it names the host log,
  `.tincan/hosts/<who>.log`) — do not fall back to driving the CLI by hand.
- Not a preset and no template: ask the user to start `/listen` for `<who>`, or for
  a command to host.

`up` reconnects to an existing listener. Explicit settings must match its running
configuration; stop it before changing preset or session mode.

## One target — blocking consult

```
tincan ask --to <who> --from orch --room "$ROOM" --body-file task.md [--timeout <sec>]
```
Blocks (no token cost) until the reply arrives; then act on it. `--format body`
prints just the answer text; default `json` gives the full envelope. If the reply
lists artifacts, read them from the repo. Size `--timeout` to the task (default
570s; reviews and generation can need more). Use `--timeout 0` for an indefinite
wait when your harness supports long-running or background commands.

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

- Exit 3 with `pending channel=r-<id>` = no reply in time; the original request
  may be queued or already running. Collect its reply with
  `tincan recv --as r-<id> --room "$ROOM" --timeout 0`. Do not issue another
  `ask` for the same task: that submits duplicate work and may repeat edits or
  other side effects. A timeout only stops waiting; it does not cancel work.
- Exit 1 = real error (read stderr); exit 2 = your flags were wrong.
- Ensure the target is actually running `/listen` in the same room — or hosted
  via `tincan up` (see above); `tincan status --room "$ROOM"` shows both.
- A hosted `ERROR ` reply marks failed or interrupted work. `ERROR session:`
  includes structured provider errors and session/parsing failures. Inspect the
  reply and `.tincan/hosts/<who>.log` to distinguish provider, process and storage
  problems before deciding whether to send new work.
- If a run was interrupted by a crash, inspect its status, log and workspace
  before deciding whether to submit new work. An interrupted run may have made
  changes; tincan does not automatically replay it.
- Claude/Grok/Agy/Kimi native executable presets keep a conversation per room and
  listener by default. Codex/Gemini/custom commands default to stateless runs and
  need self-contained briefs. Select `--session persistent|stateless` on launch;
  MCP `tincan_reset` stops a listener and explicitly starts its next conversation
  fresh. Stopping alone preserves the saved conversation.
- When the session's work is done, stop what you started unless the user asked
  to keep the listeners available:
  `tincan down <who> --room "$ROOM"` for each hosted listener you brought up.
