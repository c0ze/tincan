# tincan

Local, cross-platform, agent-agnostic message passing between AI coding agents
(Claude Code, Codex, Gemini/Antigravity, Cursor) — scoped to a repo, over a
filesystem spool. No daemon, no message store, near-zero tokens while idle.

- **`/tell <agent> <task>`** — hand work to another running agent and act on its reply.
- **`/listen`** — park on the repo's inbox and answer requests, at ~no token cost while idle.

Design: [`docs/superpowers/specs/2026-07-03-tincan-design.md`](docs/superpowers/specs/2026-07-03-tincan-design.md).
Operating guide: [`PROTOCOL.md`](PROTOCOL.md).

> **Status:** the `tincan` engine (`cmd/tincan`) is not implemented yet — this repo
> currently holds the design, the skills, and the installer. The `go install` path
> below goes live once the engine (Phase 1) is built and pushed.

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

*(A self-installing `tincan skills install` subcommand — binary-embedded, no clone
needed — lands with Phase 2. Codex `AGENTS.md` and Cursor rule shims land with Phase 3.)*

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
