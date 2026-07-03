---
name: listen
description: Act as a tincan listener — park on this repo's tincan inbox, handle requests from an orchestrator agent (code reviews, questions, generation tasks), reply, and re-arm. Near-zero token cost while idle. Use when the user runs /listen (optionally "as <name>") or asks this agent to listen for tasks from another agent.
---

# tincan: listen

You are the **listener** side of tincan. Park on your inbox, handle incoming
requests from an orchestrator, reply, and immediately go back to listening. See
`PROTOCOL.md` in the tincan repo for the full model.

## Setup

1. Pick your **name**: from the invocation (`/listen as codex` → `codex`);
   otherwise ask the user, or default to a short lowercase agent name. Names are
   single path components (no slashes/dots).
2. Resolve the **room** once: `ROOM="$(git rev-parse --show-toplevel)"` (fall back
   to the absolute CWD outside a repo). tincan does NOT auto-climb to the repo
   root — always pass `--room "$ROOM"` explicitly. All parties must share it.

## Loop — this is YOUR loop, not a shell loop

Do not write `while true` in bash: a shell loop can't hand tasks back to your
reasoning context. Each iteration is one blocking command, then you act.

1. Run: `tincan recv --as <name> --room "$ROOM" --timeout 0`
   - Blocks at **no token cost** until a message arrives. `--timeout 0` means no
     deadline — recommended whenever you can run this in the background or as a
     long-running foreground call, since fsnotify already wakes instantly on a
     real message and a timeout only adds cost, never latency: every re-arm after
     a timeout is a fresh agent turn that re-reads full context.
   - Only fall back to a positive `--timeout` (sized under the cap) if your
     harness caps foreground command duration and you cannot background the call.
   - Leave `--format` at its default (`json`) — you need the full envelope, not
     just `body`, to see `reply_to` **and** `kind` (see step 3b). `--format body`
     drops both and will hide a `stop` control message as if it were normal work.
2. **Exit 3 (timeout, positive-timeout mode only):** run step 1 again. Don't stop
   to report status.
3. **On a message:** parse the JSON.
   - **3a. Normal message (`kind` absent or `""`):** `body` is the task,
     `reply_to` is the reply channel (`r-<hex>`). Do exactly what's asked:
     review the diff/PR, answer the question, generate the artifact into the
     repo. Continue to step 4.
   - **3b. Control message (`kind == "stop"`):** this is a wind-down request,
     not a task. `stop` is sent with `tincan stop` (not `ask`), so there is no
     `reply_to` channel to answer on — just ack briefly to the user/chat (e.g.
     "received stop, winding down"), then **exit the loop — do not go to step
     1, do not re-arm, skip step 4.** tincan only carries the typed message;
     honoring it is on you.
4. Reply: `tincan reply --room "$ROOM" --channel <reply_to> --from <name> --body-file <answer> [--artifact <path> ...]`
   Keep the body concise; return produced files as `--artifact` pointers, not inline.
   (Optionally add `--log` to your recv in step 1 to keep an audit copy of consumed
   messages in `.tincan/log/`.) This step applies to normal messages (3a) only.
5. Go to step 1 — unless step 3b just fired, in which case stop here.

Continue until the user tells you to stop, or a `stop` control message (step 3b)
ends the loop. Each real message costs one turn; idle time between messages costs
nothing.

## Unattended use

If approval prompts interrupt the loop, ask the user to allowlist
`Bash(tincan *)` in `.claude/settings.json` rather than switching to a
skip-permissions mode.
