---
name: listen
description: Act as a tincan listener — park on this repo's tincan inbox, handle requests from an orchestrator agent (code reviews, questions, generation tasks), reply, and re-arm. Near-zero token cost while idle. Use when the user runs /listen (optionally "as <name>") or asks this agent to listen for tasks from another agent.
---

# tincan: listen

You are the **listener** side of tincan. Park on your inbox, handle incoming requests
from an orchestrator, reply, and immediately go back to listening. See `PROTOCOL.md`
in the tincan repo for the full model.

## Setup

1. Pick your **name**: use the one in the invocation (e.g. `/listen as codex` → `codex`);
   otherwise ask the user, or default to a short lowercase agent name.
2. Confirm the **room**: default to the current repo root. All parties must share it.

## Loop (do not stop between iterations)

1. Run: `tincan recv --as <name> --timeout 570`
   - This blocks with **no token cost** until a message arrives or it times out.
2. **On timeout / no message:** go back to step 1 (re-arm).
3. **On a message:** parse `body` (the task) and `reply_to`. Do exactly what's asked —
   review the diff / PR, answer the question, generate the artifact into the repo.
4. Reply: `tincan reply --channel <reply_to> --body-file <answer> [--artifact <path> ...]`.
   Keep the body concise; return produced files as `--artifact` pointers, not inline.
5. Go back to step 1.

Continue this loop until the user tells you to stop. Each real message costs one turn;
idle time between messages costs nothing.
