# tincan — Hosted listeners (`up` / `serve` / `down`) — Design Spec

Status: approved by the owner on 2026-09-07 (design discussion in a Claude Code
session; approach "A — tincan hosts the listener itself" chosen over launching
interactive agent windows).

## 1. Problem

A tincan listener is today an *interactive* agent session running the `/listen`
loop, so a human must open one terminal per agent in the project directory
before an orchestrator can `/tell` it anything. In practice (2026-09-07 skrivist
audit) no listener was ever parked; the orchestrator fell back to driving each
CLI's headless mode by hand (`codex exec`, `grok -p`, `kimi -p`, `agy -p`) and
re-discovered every flag and quirk. Availability must be controllable by the
orchestrating agent, not by a human.

## 2. Goals / Non-goals

Goals
- One command makes a named agent available in a room, detached, with no
  terminal and no agent-side skill installation.
- The orchestrator can see whether a hosted listener is parked or busy, and can
  stop it.
- Built-in presets for the CLIs that are known to work headlessly; user
  overrides in a config file; any command usable via a template.
- Every request gets a reply, including failures and timeouts, so `ask` never
  hangs on a crashed agent.
- Zero new runtime dependencies (stdlib + existing fsnotify).

Non-goals (explicitly deferred)
- Sticky sessions (reusing one agent conversation across messages via
  `--continue` / `resume --last`).
- Launching interactive agents in tmux/Terminal windows.
- Multiple concurrent workers per name (one hosted process = one serial worker;
  running `up` twice with different names is the fan-out path).
- Streaming partial output; the reply is the agent's final output.
- `gc` of stale channels (still Phase 2 of the original spec).

## 3. Model

A **hosted listener** is a detached `tincan serve` process that:

1. parks on `<room>/.tincan/inbox/<name>/` with the normal `Recv` (so
   presence, `status` and `ping` work unchanged);
2. for each ordinary message, renders the configured agent command with the
   message body as the prompt, runs it with `cwd = room`, captures its output;
3. sends the output as the reply on the message's `reply_to` channel (if the
   message has none, the output is only logged);
4. loops; a `kind == "stop"` message or SIGTERM/SIGINT ends the loop.

Each message is a **fresh single-turn run** of the agent CLI: no memory between
messages. Briefs must be self-contained (this matches how the orchestrator
already writes them).

## 4. CLI

```
tincan up    <name> [--room R] [--preset P] [--exec '<template>'] [--stdin body|none]
                    [--reply stdout|file] [--exec-timeout <sec>] [--wait <sec=10>]
tincan serve <name> [same flags as up] [--daemon]
tincan down  <name> [--room R] [--wait <sec=15>]
tincan status …      (gains MODE and busy/parked)
```

`up`
- Idempotent: if a listener named `<name>` is present in the room (any kind),
  print `already up name=<name> pid=<n>` and exit 0.
- Resolves the preset: `--preset` if given, else `<name>` if it is a known
  preset, else requires `--exec`. Unknown preset and no `--exec` → exit 2.
- Checks the agent binary is on `PATH` (`exec.LookPath` on the first template
  token) → exit 1 with a clear message if missing.
- Starts `tincan serve <name> … --daemon` detached (own process group, stdio
  redirected to the host log), then polls presence until the listener is
  parked or `--wait` elapses (exit 1, log path printed). Prints
  `up name=<name> pid=<n> preset=<p>`.

`serve`
- Foreground loop (also useful for debugging). `--daemon` is what `up` passes:
  it means "stdio is already redirected; write the state file; don't print
  usage banners".
- Per message: write state `busy` (+ message id), run command, write reply,
  write state `parked`.
- `kind == "stop"` → remove state file, exit 0. SIGTERM/SIGINT → kill the
  in-flight agent process (whole process group), remove state file, exit 0.
- The in-flight agent's stdout/stderr are captured to memory (reply) and
  appended to the host log.

`down`
- Sends `stop` (from `orch`), then polls until presence disappears or `--wait`
  elapses; then sends SIGTERM to the recorded pid; after a further 5 s SIGKILL.
  Prints `down name=<name>`; exit 0 if the process is gone, 1 otherwise.

`status`
- New columns: `MODE` = `hosted:<preset>` (from the state file) or `agent`
  (presence without a state file); `STATE` = `parked` | `busy` | `—`.
- `--format json` adds `mode`, `preset`, `busy`, `current_id`.

## 5. Presets and config

Built-in presets (the invocations verified on 2026-09-07):

| preset | exec template | stdin | reply |
|---|---|---|---|
| `codex`  | `codex exec -s workspace-write --skip-git-repo-check -o {out} -` | body | file |
| `grok`   | `grok -p {body} --always-approve` | none | stdout |
| `kimi`   | `kimi -p {body}` | none | stdout |
| `agy`    | `agy -p {body} --dangerously-skip-permissions --model gemini-3.8-flash-high --print-timeout 150m` | none | stdout |
| `gemini` | `gemini -p {body}` | none | stdout |
| `claude` | `claude -p {body} --dangerously-skip-permissions` | none | stdout |

Template placeholders: `{body}` (the message body, passed as a single argv
element — never through a shell), `{out}` (a per-message temp file whose
contents become the reply when `reply = file`), `{room}`, `{name}`, `{id}`.
`stdin = body` pipes the body to the process' stdin instead of an argument.

User config `~/.config/tincan/agents.json` (JSON, no new deps) overrides or adds
presets:

```json
{
  "agy":   { "exec": ["agy", "-p", "{body}", "--dangerously-skip-permissions", "--model", "gemini-3.8-flash-high", "--print-timeout", "150m"], "stdin": "none", "reply": "stdout" },
  "myllm": { "exec": ["my-llm", "--prompt-file", "-"], "stdin": "body", "reply": "stdout", "exec_timeout_sec": 1800 }
}
```

Merge order: built-in ← config file ← command-line flags. `tincan presets`
(small helper, `--format table|json`) lists the effective presets; useful for
skills and debugging.

Post-processing of replies is preset-level and minimal: trim trailing
whitespace; for `kimi` drop the trailing `To resume this session: …` line.
Nothing else is rewritten.

## 6. Files under the room

```
.tincan/hosts/<name>.json   # {"pid":…, "preset":…, "exec":[…], "started":ts, "state":"parked|busy", "current_id":"…"}
.tincan/hosts/<name>.log    # per message: "=== <id> from=<from> started=<ts>" … agent stdout+stderr … "=== exit=<code> duration=<s>s"
```

The state file is written atomically (tmp + rename) and removed on clean exit;
`status` treats a state file whose pid is dead as stale and reports `—`.
`.tincan/` is already gitignored in this repo; the README tells users to add
`.tincan/` to their projects' `.gitignore` (or global excludes).

## 7. Error handling

- Agent exits non-zero → reply body: `ERROR exit=<code>\n<last 4 KiB of
  stderr>` followed by any stdout. The asker always gets a reply.
- `--exec-timeout` elapsed (default 3600 s; 0 = none) → kill the process group,
  reply `ERROR timeout after <sec>s`.
- Agent binary missing at run time (removed after `up`) → reply
  `ERROR exec: <message>`; the listener keeps running.
- Malformed/unknown `kind` → ignored and logged, no reply.
- `reply` send failure (spool error) → logged; the loop continues.
- Room rules are unchanged: explicit `--room`, root-warning on non-root rooms.

## 8. Security

Same trust boundary as today, stated more loudly: anything that can write into
`<room>/.tincan/inbox/<name>/` drives the hosted agent, and the presets bake in
auto-approval flags (that is the point). Mitigations: presets use the narrowest
mode that works unattended (`codex -s workspace-write`, never
`danger-full-access`); `cwd` is the room; the body is passed as an argv
element or on stdin, never interpolated into a shell string; the README/PROTOCOL
carry a "hosted listeners execute whatever lands in the inbox" warning.

## 9. Skills and docs

- `skills/tell/SKILL.md`: before `ask`, run `tincan ping --to <who>`; if absent
  and `<who>` is a known preset (or `--exec` is configured), run
  `tincan up <who> --room "$ROOM"` and proceed. Mention `down` for cleanup at
  the end of a session.
- `PROTOCOL.md`: new section "Hosted listeners" (model, CLI, files, errors,
  security caveat). `README.md`: quick start gains the one-liner
  `tincan up codex --room "$(git rev-parse --show-toplevel)"`.
- `install.sh` unchanged in behaviour, but the README notes that an older
  installed binary must be reinstalled (`go install ./cmd/tincan`) to get
  `status/ping/up/down`.

## 10. Testing

- Unit: template rendering (all placeholders, argv boundaries, body with
  spaces/newlines/quotes), config merge precedence, reply post-processing,
  state-file read/write/stale detection, `status` rendering with hosted rows.
- Serve loop with a fake agent script (`sh`/Go test helper) — cases: echo
  reply; non-zero exit → `ERROR exit=`; sleep past `--exec-timeout` → `ERROR
  timeout`; `stop` message ends the loop; SIGTERM mid-run kills the child;
  message without `reply_to` is logged only.
- Integration (temp room): `up` (fake preset via config file) → `ask` → reply
  arrives → `status` shows hosted/parked → `down` → presence gone, pid dead.
- All tests must pass under `go test -race ./...` on the existing CI matrix
  (Linux/macOS/Windows). Windows: process-group kill uses `taskkill /T`; the
  detach uses `CREATE_NEW_PROCESS_GROUP`; if `up` cannot be made reliable on
  Windows in this iteration, it must fail loudly there with a clear message
  rather than half-work.

## 11. Implementation notes

- New package `internal/host` (presets, config, template, runner, state file);
  CLI wiring in `internal/cli` (`cmdUp`, `cmdServe`, `cmdDown`, `cmdPresets`,
  `status` extension). Keep `spool` unchanged except a small helper to read the
  presence pid for a name if not already exposed.
- Detach: re-exec `os.Args[0]` with `serve … --daemon`, `Setsid`/new process
  group, stdio → log file, `cmd.Process.Release()`.
- Reply transport reuses `spool.Send` with `To = reply_to`, `From = <name>`.
