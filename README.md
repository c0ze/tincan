# tincan

![tincan — two agents, one string](assets/banner.png)

Local, cross-platform, agent-agnostic message passing between AI coding agents
(Claude Code, Codex, Gemini/Antigravity, Cursor) — scoped to a repo, over a
filesystem spool. No daemon, no message store, near-zero tokens while idle.

- **`/tell <agent> <task>`** — hand work to another running agent and act on its reply.
- **`listen` / `/listen`** — park on the repo's inbox and answer requests, at ~no token cost while idle.
- **`tincan up <agent>`** — or skip the terminal: tincan hosts a headless agent CLI
  (codex, gemini, claude, …) as a listener itself, detached, and `down` stops it.

Design: [`docs/superpowers/specs/2026-07-03-tincan-design.md`](docs/superpowers/specs/2026-07-03-tincan-design.md).
Operating guide: [`PROTOCOL.md`](PROTOCOL.md).

> **Status:** v0.1 — the core engine (`send`/`recv`/`ask`/`reply` over the filesystem
> spool), observability (`status`/`ping`/`stop`) and hosted listeners
> (`up`/`serve`/`down`) are implemented, tested (race-detector CI on
> Linux/macOS/Windows), and in daily use coordinating Claude Code, Codex, and
> Antigravity. Young but working. Roadmap (gc, more agent shims): spec phases 2–3
> in `docs/`. Hosted-listener detach is Linux/macOS only for now (Windows fails
> loudly; `tincan serve` works there).

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

Prefer not to use Go? Grab a prebuilt binary for your platform from
[Releases](https://github.com/c0ze/tincan/releases) (linux/macOS/windows,
amd64+arm64) and drop it on your `PATH`.

**Already have tincan installed?** An older binary predates `status`/`ping`/
`stop`/`up`/`down`/`presets` — reinstall to get them: `go install ./cmd/tincan`
from a clone (or `go install github.com/c0ze/tincan/cmd/tincan@main`), then check
`tincan --help`. `./install.sh` does the same build.

Add `.tincan/` to your projects' `.gitignore` (or your global excludes): it holds
the spool and, with hosted listeners, per-agent logs.

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
- **Codex**: no shim is installed. Codex CLI 0.142.5 discovers this repo's
  `skills/listen/SKILL.md` natively through the workspace skill registry
  (`skills/` and this repo's `.agents/skills -> skills` symlink are visible in
  `codex debug prompt-input`). Invoke it with plain `listen` or `listen as codex`,
  not `/listen`. For unattended listening, start with `codex --full-auto`
  (workspace-write sandbox, auto-approve); if the sandbox interferes with the
  loop, escalate to `--ask-for-approval never --sandbox danger-full-access` in a
  trusted repo only.
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
listen as codex

# Agent B (e.g. Claude), orchestrating:
/tell codex review the diff on the current branch
```

B blocks with ~no token cost until A replies, then acts on the review. Fan out to
several agents at once and B collects replies as they finish.

No terminal for agent A? Let tincan host it — one command, detached, no skill
install on the agent side:

```sh
tincan up codex --room "$(git rev-parse --show-toplevel)"    # up name=codex pid=… preset=codex
/tell codex review the diff on the current branch              # same as before
tincan status --room "$(git rev-parse --show-toplevel)"        # MODE hosted:codex, STATE parked|busy
tincan down codex --room "$(git rev-parse --show-toplevel)"    # when you are done
```

Built-in presets: `codex`, `grok`, `kimi`, `agy`, `gemini`, `claude` (`tincan
presets` lists them; `~/.config/tincan/agents.json` adds or overrides). Every
request gets a reply, including agent failures (`ERROR exit=…`) and timeouts.
Mind the trust boundary: a hosted listener runs whatever lands in its inbox
with auto-approval — see PROTOCOL.md "Hosted listeners".

## Brand

The logo and banner were designed by the listening agents themselves, briefed and
delivered over tincan in a head-to-head contest — Codex (GPT-image) vs Antigravity
(Nano Banana Pro). Codex's entry won and became the official brand; both entries
live in [`assets/`](assets/):

| Codex — winner | Antigravity — alternate |
|---|---|
| ![codex logo](assets/codex/logo.png) | ![antigravity logo](assets/antigravity/logo.png) |
| ![codex banner](assets/codex/banner.png) | ![antigravity banner](assets/antigravity/banner.png) |

## License

[MIT](LICENSE)
