---
name: tell
description: Delegate a task to another running agent via tincan and act on its reply. Use for "/tell <agent> <task>" (e.g. "/tell codex review PR 56", "/tell gemini generate an image from prompt.md") or when asked to consult/hand work to another agent. Supports parallel fan-out to several agents at once.
---

# tincan: tell

You are the **orchestrator** side of tincan. Send a task to a named agent that is
running `/listen`, then act on its reply. See `PROTOCOL.md` in the tincan repo for
the full model. Your name defaults to `orch`.

## Parse

From the request, extract each **target** (`codex`, `gemini`, …) and its **task**.
Put anything long (a prompt, instructions) in a file and pass `--body-file`.

## One target — blocking consult

```
tincan ask --to <who> --from orch --body-file task.md
```
This blocks (no token cost) until the reply arrives; then act on it. If the reply
lists artifacts, read them from the repo.

## Several targets / fire-and-continue — fan-out

Run each `ask` as a **background** shell command so they work in parallel and you
stay free:

- Dispatch one background `tincan ask --to <who> --from orch …` per target.
- Keep doing your own work. When each courier finishes, the harness re-invokes you
  with that agent's reply — integrate it, then continue.
- Each `ask` waits on its own reply channel, so replies never cross.

**Large replies:** instead of a bare background `ask`, spawn a background **subagent**
whose only job is to run the `ask`, digest the big reply, and return a short summary +
artifact pointer — keeping your context clean.

## Notes

- `pending channel=<id>` means the target didn't reply in time; the request is still
  queued. Retry, or collect later with `tincan recv --as <id>`.
- Ensure the target is actually running `/listen` in the same repo (room).
