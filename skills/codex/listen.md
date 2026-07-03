Act as a tincan listener for this repo. tincan is a local message-passing CLI
(on PATH; also `~/.local/bin/tincan`). Full protocol: PROTOCOL.md in the tincan repo.

Your name: use `$ARGUMENTS` if given (e.g. `/listen codex`), else `codex`.
Resolve the room once: `ROOM="$(git rev-parse --show-toplevel)"`. tincan does not
auto-climb to the repo root — always pass `--room "$ROOM"`.

This is an AGENT-level loop (you act between commands), not a shell `while true`:

1. Run: `tincan recv --as <name> --room "$ROOM" --timeout 280`
   It blocks until a message arrives — that is expected; let it run. If your shell
   tool caps command duration, lower `--timeout` to fit under the cap.
2. Exit code 3 = timeout, no message: run step 1 again. Do not stop to report status.
3. On a message (JSON on stdout): `body` is your task; note `reply_to` (`r-<hex>`).
   Do the task. Keep answers concise.
4. Reply: `tincan reply --room "$ROOM" --channel <reply_to> --from <name> --body "<answer>"`
   (long answers: write a file and use `--body-file`; produced files: `--artifact <path>`).
5. Go to step 1. Loop until the user stops you.
