# tincan

![tincan — two agents, one string](assets/banner.png)

Local, cross-platform, agent-agnostic message passing between AI coding agents
(Claude Code, Codex, Gemini/Antigravity, Cursor) — scoped to a repo, over a
filesystem spool. No shared server or background coordinator; messages, hosted
logs and request state stay in the room. Near-zero tokens while idle.

- **`/tell <agent> <task>`** — hand work to another running agent and act on its reply.
- **`listen` / `/listen`** — park on the repo's inbox and answer requests, at ~no token cost while idle.
- **`tincan up <agent>`** — or skip the terminal: tincan hosts a headless agent CLI
  (codex, gemini, claude, …) as a listener itself, detached, and `down` stops it.
- **`tincan mcp --room /absolute/repo`** — expose structured orchestration tools
  to an MCP client over stdio, with durable request handles and bounded waits.

Design: [`docs/superpowers/specs/2026-07-03-tincan-design.md`](docs/superpowers/specs/2026-07-03-tincan-design.md).
Operating guide: [`PROTOCOL.md`](PROTOCOL.md).

> **Development checkout:** This README describes the current source tree.
> The published `v0.1.0` tag predates the hosted listener and MCP interfaces;
> `@latest` does not provide all features documented here. Build this checkout
> until a release containing these changes passes the release checks. CI tests
> Go 1.25 and stable Go on Linux/macOS/Windows; publishing requires the same
> checks on the exact tagged commit. Detached listeners support Linux/macOS;
> Windows can run `tincan serve` in a terminal.

## Install

### 1. The binary

```sh
# From this checkout, including changes not yet on the published main branch:
go install ./cmd/tincan

# To install the newest published main commit:
go install github.com/c0ze/tincan/cmd/tincan@main
tincan version --format json
```

Requires Go 1.25+. (Note: `go get <tool>` no longer installs executables — use
`go install …@version`.) Make sure Go's bin dir is on your `PATH`:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"   # add to your shell profile
```

Prebuilt binaries are published on
[Releases](https://github.com/c0ze/tincan/releases) for Linux/macOS/Windows,
amd64+arm64. Check the release notes and `tincan version`: older artifacts lack
the interfaces in this README. Until a matching release exists, build the source.

**Already have tincan installed?** An older binary predates `status`/`ping`/
`stop`/`up`/`down`/`presets` — reinstall to get them: `go install ./cmd/tincan`
from a clone (or `go install github.com/c0ze/tincan/cmd/tincan@main`), then check
`tincan --help` and `tincan version --format json`. `./install.sh` does the same
local build. `@main` follows the published main branch and excludes unpublished
branch changes.

Add `.tincan/` to your projects' `.gitignore` (or your global excludes): it holds
the spool and, with hosted listeners, per-agent logs and durable request state.
New runtime directories use `0700` and files use `0600` on POSIX. Keep rooms
private: prompts, replies and logs may contain repository content or credentials.

### 2. The skills

From a clone of this repo:

```sh
./install.sh                 # binary + shared and Claude tell/listen skills
./install.sh --skills-only   # just the skills
```

This copies the skills to both `~/.agents/skills/{tell,listen}/` and
`~/.claude/skills/{tell,listen}/` (override with `AGENTS_SKILLS_DIR` and
`CLAUDE_SKILLS_DIR`). It recognizes directories and files that alias each other,
skips identical files, and preserves changed destination files or symlinks as
adjacent `*.backup.*` files before replacement. Other files are left in place.
Restart your agent session to discover the installed skills.

Per-agent shims installed by the same script:

- **Codex**: uses the native skills installed in `~/.agents/skills/`, so they are
  available outside this checkout. Invoke with `listen as codex` or a request to
  `tell <agent> <task>`. No legacy prompt shim is installed.
- **Claude Code**: uses `~/.claude/skills/`; invoke `/tell` or `/listen`.
- **Gemini CLI**: when `~/.gemini` exists, installs
  `~/.gemini/commands/listen.toml` for `/listen`. Set `GEMINI_COMMANDS_DIR` to
  explicitly select a destination, even before Gemini has created its directory.
- **Antigravity**: reads workspace skills from `.agents/skills/`; this repo ships
  a `.agents/skills → skills` symlink, so `/listen` works out of the box here. For
  other repos, make the installed skills available through that workspace path
  if the client does not discover user-level skills.

The skill installer requires a checkout. MCP clients can use the binary directly.

### 3. MCP clients

Build this checkout, then configure a stdio MCP server with an absolute room path.
For clients using an `mcpServers` JSON configuration:

```json
{
  "mcpServers": {
    "tincan": {
      "command": "/absolute/path/to/tincan",
      "args": ["mcp", "--room", "/absolute/path/to/repo"]
    }
  }
}
```

Replace both paths before adding the configuration to your client. Using the
absolute binary path avoids differences between GUI and shell `PATH` values.
Each server is bound to one room; tool calls cannot switch rooms. Standard output
is reserved for MCP messages and diagnostics go to standard error.

The tools are `tincan_launch`, `tincan_send`, `tincan_wait`, `tincan_status`,
`tincan_cancel`, `tincan_reset`, `tincan_stop`, and `tincan_presets`. Launch uses
configured presets; the MCP API does not accept an arbitrary command to execute.
Send returns a durable `request_id`. Save it and use wait/status to collect the
result across client reconnects. A wait timing out does not resubmit or cancel
the task. Hosts continue running after the MCP connection closes.

For example, call `tincan_send` with
`{"agent":"claude","body":"Review the current diff","request_id":"review-1"}`,
then `tincan_wait` with `{"request_id":"review-1","timeout_seconds":30}`. Keep
waiting on that ID until `terminal` is true. Reusing an explicit ID with identical
work is idempotent while its request record is retained.

Claude, Grok, Agy and Kimi use persistent conversations by default with their
native executable presets. Their saved conversation survives calls, reconnects
and host restarts. Codex, Gemini and custom executables default to stateless
runs. Select `session_mode: "stateless"` on launch for independent requests;
`tincan_reset` stops a listener and clears its conversation pointer. See
[the MCP protocol](PROTOCOL.md#mcp-stdio) for tool arguments and lifecycle details.

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
completed run gets a reply, including agent failures (`ERROR exit=…`) and timeouts.
Interrupted work is recorded for inspection and is never automatically replayed;
it may already have changed the workspace.
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
