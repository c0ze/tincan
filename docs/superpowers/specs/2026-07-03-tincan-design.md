# tincan — Design Spec

**Status:** Draft for review
**Date:** 2026-07-03
**Authors:** arda + Claude

---

## 1. Problem & motivation

Coordinating work between AI coding agents from different vendors (Claude Code, Codex, Gemini / Antigravity, Cursor) running locally on the same repo today means manual hand-offs: copy-paste, or opening PRs and typing "go review this," "handle the comments," and so on. Polling approaches (loops, scheduled jobs checking GitHub every N minutes) burn tokens and add latency.

**tincan** is a tiny local message-passing primitive that lets one agent (an *orchestrator*) hand tasks to one or more *listener* agents and collect their replies, with:

- **near-zero idle token cost** — listeners block on a receive, and a suspended tool call spends no tokens;
- **no polling**;
- **no persistent message store** — the agents' own chat transcripts are the record;
- **agent-agnostic transport** — any process that can run a shell command can participate;
- **cross-platform** identical behavior (Linux, macOS, Windows).

## 2. Goals / Non-goals

**Goals**

- One small, dependency-light CLI (`tincan`) providing send / receive / ask / reply over a local filesystem spool.
- Blocking receive with zero token cost while idle, and a safe re-arm.
- N participants (orchestrator + many listeners), star topology, addressed by name.
- Parallel fan-out from the orchestrator with race-free reply correlation.
- Two thin skill shims per agent (`/tell`, `/listen`) pointing at one canonical procedure.

**Non-goals (YAGNI)**

- No multi-machine / networked operation. Single host, single shared filesystem.
- No authentication or encryption (local trust boundary).
- No durable conversation history / archive. Messages are consumed on delivery; `--log` is opt-in debug only.
- No mesh coordination patterns. Listener↔listener transport is *possible* but unsupported; keep to a star.
- No delivery-ordering guarantees beyond per-inbox arrival order.
- No large payloads in messages — artifacts ride the repo/filesystem (see §9).

## 3. Core model: the directory spool

tincan is a **maildir-style spool**, not a socket and not a daemon. This choice buys queueing, crash-safety, harmless re-arm, and trivial cross-platform support in exchange for nothing we need.

- Each participant has an **inbox directory**. Sending = atomically dropping a message file into the recipient's inbox. Receiving = blocking until a file appears, claiming it, printing it, deleting it.
- **Queueing is free:** if no one is receiving, files simply accumulate; the next `recv` drains them oldest-first.
- **Re-arm is harmless:** a `recv` that times out and is re-issued loses nothing — queued files are still on disk. (Contrast a raw socket, where a message sent during the gap is dropped.)
- **Crash-safe:** the queue is on disk, not in a daemon's memory.
- **No daemon to babysit:** `send`/`recv` are short-lived CLI invocations; the filesystem is the shared state.

### Blocking mechanism

`recv` sets up an fsnotify watch on the inbox, then scans for already-queued files (watch-then-scan, with a rescan, to close the watch/scan race), claims the oldest via atomic rename, prints it, deletes it, and exits. While blocked, the CLI is asleep in the OS — the calling agent's tool call is suspended and costs no tokens.

## 4. Rooms, addressing, envelope

**Room** — a workspace scope. Defaults to the current working directory; override with `--room <path>` (e.g., the repo root). All state lives under `<room>/.tincan/`. Participants coordinate by sharing a room — normally, the same repo. `.tincan/` should be git-ignored.

**Names** — each participant picks an identity string (`orch`, `codex`, `gemini`, …). Inboxes are keyed by name: `<room>/.tincan/inbox/<name>/`. Going from 2 to N participants is just more inbox directories.

**Reply channels** — ephemeral per-request inboxes in the *same* namespace, named `r-<id>` (i.e. `<room>/.tincan/inbox/r-<id>/`). `ask` mints one, `reply` answers into it, and a late reply is collected with the ordinary `recv --as r-<id>` — no separate channel concept in the spool (see §8).

**Envelope** — one message = one JSON file:

```json
{
  "id":        "<uuid>",
  "corr_id":   "<channel-id>",
  "from":      "orch",
  "to":        "codex",
  "reply_to":  "<channel-id-or-empty>",
  "ts":        "2026-07-03T12:00:00Z",
  "body":      "review PR 56",
  "artifacts": ["path/in/repo.ext"]
}
```

A reply swaps `from`/`to`, preserves `corr_id`, leaves `reply_to` empty, and puts the answer in `body` (+ optional `artifacts`). Filenames are `<ts-nano>-<id>.json` for oldest-first ordering. Delivery is atomic: write to a temp file, then `rename` into the inbox.

## 5. CLI

Two primitives (`send`, `recv`) and two conveniences (`ask`, `reply`).

**`tincan recv`** — block for one message on my inbox.

```
tincan recv --as <name> [--room <path>] [--timeout <sec=570>] [--format json|body] [--log]
```

Prints one message (JSON by default), deletes it, exits 0. On timeout, exits **3** (distinct from usage/other errors) with no output — the caller re-arms. `--log` moves consumed messages to `<room>/.tincan/log/` instead of deleting.

**`tincan send`** — deliver a message to a named inbox (queues if unread), then exit.

```
tincan send --to <name> --from <name> [--room <path>] [--corr <id>]
            [--reply-to <channel>] [--body <str> | --body-file <path>]
            [--artifact <path> ...]
```

**`tincan ask`** — orchestrator/driver one-liner: mint a reply channel, send, block for the reply, print it, clean up.

```
tincan ask --to <name> --from <name> [--room <path>] [--timeout <sec=570>]
           (--body <str> | --body-file <path>) [--artifact <path> ...]
```

Internally: mint channel `r-<id>`, `send --reply-to r-<id>`, then `recv --as r-<id>`. On timeout it leaves the channel in place and prints `pending channel=r-<id>`, so a late reply can be collected later with `tincan recv --as r-<id>`.

**`tincan reply`** — a listener's response to a received message.

```
tincan reply --channel <reply_to> [--room <path>]
             (--body <str> | --body-file <path>) [--artifact <path> ...]
```

(Sugar over `send` into the reply channel's inbox.)

## 6. Zero-token idle & re-arm

While `recv`/`ask` block, the agent's shell tool call is suspended → **no tokens** are spent. Each host caps a foreground tool call (Claude Code: ~10 min), so `--timeout` defaults to 570s to stay under it. On timeout the skill simply re-issues `recv` — one cheap turn, and nothing is lost because the spool persists. Net idle cost: one trivial turn per timeout window, plus one turn to handle each real message. No hooks required — the skill's loop instructions carry the re-arm.

## 7. The two skills

The substance lives once in `PROTOCOL.md`; each agent gets a thin trigger in its own format (Claude `SKILL.md`, Codex `AGENTS.md`, Cursor `.cursor/rules`).

**`/listen --as <name>`** (a consultant / worker):

1. `tincan recv --as <name> --timeout 570`.
2. If a message arrives: do the requested work (review a diff, answer a question, produce an artifact).
3. `tincan reply --channel <reply_to>` with the answer and/or artifact pointers.
4. Re-arm: go to 1. (On timeout/empty, also go to 1.) Continue until the user stops the session.

Costs zero tokens while blocked; the loop is prompt-driven — no hook.

**`/tell <who> <task>`** (the orchestrator; generalizes a 2-way `/pair`):

- **Single target:** `tincan ask --to <who> --from orch --body-file <task>` in the foreground → block → act on the reply.
- **Multiple targets / fire-and-continue:** dispatch each `ask` as a background courier (see §8), keep working, act on each reply as it arrives.
- If a reply is a pointer, read the artifact from the repo.

## 8. Fan-out & background couriers

Parallel fan-out uses the one interrupt-style wakeup a Claude Code orchestrator has: **background task completion.** (This layer is Claude-orchestrator-specific; see the portability note.)

- The orchestrator fires one **background** `tincan ask` per target (`run_in_background` Bash). Each detaches, blocks on its own reply channel (zero tokens, no model context), and on exit the harness re-invokes the orchestrator with that reply.
- The orchestrator stays free to do its own work between completions; it collects replies as an interrupt stream rather than a blocking loop.
- **Race-free by construction:** each `ask` blocks on a *distinct* reply channel, so concurrent couriers never steal each other's replies. Correlation is structural (one channel per request), not filtering a shared inbox.

```
orchestrator turn:
  bg: tincan ask --to codex  --from orch --body "review PR 56"      (blocks on reply/c1)
  bg: tincan ask --to gemini --from orch --body-file prompt.md      (blocks on reply/c2)
  → keep working…
  codex done  → reply/c1 filled → courier exits → re-invoke → integrate review
  gemini done → reply/c2 filled → courier exits → re-invoke → read out/img.png
```

**Courier grades:**

- **Background Bash (default):** cheapest — a detached shell command, no extra model context. Use when the reply is small enough to hand back raw.
- **Background subagent (escalation):** only when a reply is expected to be large (a full review, a big diff). The subagent absorbs it in its own context and returns a compact summary + artifact pointer, protecting the orchestrator's context. Its value is context isolation, not parallelism.

**Portability note:** background couriers are a Claude Code feature. The universal fallback for any orchestrator is the **sequential collect-loop** (foreground `ask` per target, one at a time) — slower, but works on any agent. Transport and listeners remain fully agent-agnostic.

## 9. Artifacts

Some tasks return files, not text (e.g., image generation). tincan carries **coordination + pointers only**: the reply body says "done, wrote `out/img.png`" and lists it under `artifacts`; the file itself rides the repo/filesystem where the orchestrator can read it. This keeps messages small and avoids reintroducing storage.

## 10. Error handling & edge cases

- **Target not listening:** the message queues in its inbox. If the target never comes up, the orchestrator's `ask` times out and reports `pending`; the orchestrator decides to retry or skip. No message is lost — it stays queued, and the reply channel persists for late collection.
- **Concurrent `recv` on one inbox:** the atomic-rename claim ensures exactly one receiver processes a given file.
- **Half-written files:** never observed by receivers — delivery is temp-write-then-rename (atomic on one filesystem).
- **Stale reply channels:** channels left by timed-out `ask`s are swept by age (a `tincan gc` / startup sweep removes `inbox/r-*` dirs older than a configurable TTL).
- **Crash of any party:** no shared daemon to corrupt; queued files simply await the next receiver.
- **Large instructions:** pass via `--body-file`; keep bodies to instructions/answers and artifacts as pointers.

## 11. Cross-platform & build

- Pure Go, no cgo. Single third-party dependency: `fsnotify` (pure Go; inotify / kqueue / ReadDirectoryChangesW). One static binary per OS/arch.
- The spool model uses only directories and atomic `rename` — no AF_UNIX — so Windows is a first-class target with no special-casing.
- Distribution: `go install`, or prebuilt binaries per platform. Skills assume `tincan` is on `PATH`.

## 12. Testing

- **Unit:** envelope encode/decode; atomic delivery; oldest-first claim; timeout status; reply-channel lifecycle.
- **Integration (temp room):** send-before-recv queues and drains; a timed-out-then-re-armed recv loses nothing; two concurrent couriers on distinct channels never cross; a message to an absent name is picked up once it starts listening.
- **Cross-platform:** `go test` matrix on Linux / macOS / Windows.
- **End-to-end (documented manual procedure):** two agent sessions in one repo — one `/listen`, one `/tell` — exchanging a real request/reply, including a fan-out to two listeners.

## 13. Repo layout

```
tincan/
  cmd/tincan/main.go          # CLI entry / flag parsing
  internal/envelope/          # message struct + JSON + filenames
  internal/spool/             # inbox paths, atomic send, blocking recv, claim, gc
  PROTOCOL.md                 # canonical, agent-agnostic procedure
  skills/
    tell/SKILL.md             # Claude orchestrator shim
    listen/SKILL.md           # Claude listener shim
    codex/AGENTS.md           # Codex shim snippet
    cursor/rules.md           # Cursor rule shim
  docs/superpowers/specs/2026-07-03-tincan-design.md
  README.md
  go.mod
```

## 14. Phasing

- **Phase 1 (core):** `send`/`recv`/`ask`/`reply`, spool, envelope, blocking + timeout, atomic delivery; `PROTOCOL.md`; Claude `/tell` + `/listen` skills; unit + integration tests. Enables 2-way pairing and sequential N.
- **Phase 2 (fan-out polish):** background-courier guidance in `/tell`, subagent escalation, `gc` for stale channels, `--log`.
- **Phase 3 (other agents):** Codex `AGENTS.md` + Cursor rule shims; cross-platform CI; optional `tincan who` presence.
