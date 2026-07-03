# tincan

Local, cross-platform, agent-agnostic message passing between AI coding agents
(Claude Code, Codex, Gemini/Antigravity, Cursor) — scoped to a repo, over a
filesystem spool. No daemon, no message store, near-zero tokens while idle.

- **`/tell <agent> <task>`** — hand work to another running agent and act on its reply.
- **`/listen`** — park on the repo's inbox and answer requests, at ~no token cost while idle.

Design: [`docs/superpowers/specs/2026-07-03-tincan-design.md`](docs/superpowers/specs/2026-07-03-tincan-design.md).
Operating guide: [`PROTOCOL.md`](PROTOCOL.md).

> **Status:** Phase 1 (core engine) implemented — `send`/`recv`/`ask`/`reply` over the
> filesystem spool, with tests. The `go install` path below goes live once this repo
> is pushed to GitHub. Fan-out courier docs and other-agent shims: see spec phases 2–3.

## Install

### 1. The binary

```sh
go install github.com/c0ze/tincan/cmd/tincan@latest   # released tag
# or bleeding edge:
go install github.com/c0ze/tincan/cmd/tincan@main
```

Requires Go 1.23+. (Note: `go get <tool>` no longer installs executables — use
`go install …@version`.) Make sure Go's bin dir is on your `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"   # add to your shell profile
```

Prefer not to use Go? Grab a prebuilt binary from Releases (added with Phase 1) and
drop it on your `PATH`.

### 2. The skills

From a clone of this repo:

```sh
./install.sh                 # binary (if Go present) + Claude /tell and /listen skills
./install.sh --skills-only   # just the skills
```

This copies the skills to `~/.claude/skills/{tell,listen}/` (override with
`CLAUDE_SKILLS_DIR`). Restart your Claude Code session to pick them up, then use
`/tell` and `/listen`.

Per-agent shims installed by the same script:
- **Codex**: `~/.codex/prompts/listen.md` → `/listen` inside Codex.
- **Gemini CLI**: `~/.gemini/commands/listen.toml` → `/listen` inside Gemini CLI.
- **Antigravity**: reads workspace skills from `.agents/skills/`; this repo ships
  a `.agents/skills → skills` symlink, so `/listen` works out of the box here. For
  other repos, create the same symlink (Antigravity consumes the SKILL.md format
  directly).

*(A self-installing `tincan skills install` subcommand — binary-embedded, no clone
needed — lands with Phase 2. Cursor rule shim lands with Phase 3.)*

## Quick start

In repo `~/work/app`, two agents:

```sh
# Agent A (e.g. Codex), acting as a listener:
/listen as codex

# Agent B (e.g. Claude), orchestrating:
/tell codex review the diff on the current branch
```

B blocks with ~no token cost until A replies, then acts on the review. Fan out to
several agents at once and B collects replies as they finish.
