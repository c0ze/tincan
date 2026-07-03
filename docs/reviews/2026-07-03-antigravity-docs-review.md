# tincan Developer Experience and Documentation Review

## What Works

- **Binary Execution & Help**: The compiled `tincan` binary executes reliably, and its built-in help commands (`--help`) are clear and outline the basic subcommand usage.
- **Protocol Conformance**: Testing `send`, `recv`, `ask`, and `reply` subcommands in a temporary room verified that the filesystem-based message-passing logic is robust. Message files are correctly queued under `.tincan/inbox/<name>/` and safely consumed.
- **Exit Codes**: The exit code behavior is correct (e.g., `tincan recv` exits with code 3 upon timeout with no messages), which is vital for enabling structured agent control loops.
- **Install Script Compilation**: `install.sh` builds the binary from local source directories when Go is present, putting it in the user's Go bin directory, and cleanly copying skills to the standard directory.

---

## Mismatches & Gaps

### 1. Undocumented CLI Flags
A number of flags supported by the actual CLI implementation are missing from `PROTOCOL.md` and the skill files:
- **`tincan recv --log`**: 
  - *Reference*: Missing from [PROTOCOL.md](file:///Users/arda/projects/tincan/PROTOCOL.md#L21) and [skills/listen/SKILL.md](file:///Users/arda/projects/tincan/skills/listen/SKILL.md#L20).
  - *Detail*: This flag persists consumed messages into `.tincan/log/`. It is useful for auditing and history inspection, but currently undocumented.
- **`tincan ask --format <json|body>`**: 
  - *Reference*: Missing from [PROTOCOL.md](file:///Users/arda/projects/tincan/PROTOCOL.md#L22) and [skills/tell/SKILL.md](file:///Users/arda/projects/tincan/skills/tell/SKILL.md#L20).
  - *Detail*: Allows orchestrators to receive replies formatted purely as the raw body.
- **`tincan ask --timeout <sec>`**: 
  - *Reference*: Missing from [PROTOCOL.md](file:///Users/arda/projects/tincan/PROTOCOL.md#L22) and [skills/tell/SKILL.md](file:///Users/arda/projects/tincan/skills/tell/SKILL.md#L20).
  - *Detail*: Controls how long the orchestrator blocks waiting for a reply (defaults to 570s).
- **`tincan send --corr <id>` and `--reply-to <channel>`**: 
  - *Reference*: Missing from [PROTOCOL.md](file:///Users/arda/projects/tincan/PROTOCOL.md#L24) and [skills/tell/SKILL.md](file:///Users/arda/projects/tincan/skills/tell/SKILL.md#L20).
  - *Detail*: These flags allow manually constructing request/reply metadata.
- **`tincan reply --from <name>`**: 
  - *Reference*: Missing from [PROTOCOL.md](file:///Users/arda/projects/tincan/PROTOCOL.md#L23) and [skills/listen/SKILL.md](file:///Users/arda/projects/tincan/skills/listen/SKILL.md#L25).
  - *Detail*: Allows reply messages to optionally record who sent the reply.

### 2. Ambiguity on Room Location (`--room` Default)
- *Reference*: [PROTOCOL.md](file:///Users/arda/projects/tincan/PROTOCOL.md#L26) and [README.md](file:///Users/arda/projects/tincan/README.md#L26).
- *Detail*: The documentation lists the default room as `CWD / repo root`. However, the binary does **not** auto-discover parent repo roots (e.g., walking up to find `.git` or `.tincan`). It strictly defaults to CWD (`.`). If an agent changes directory during execution to a subdirectory and runs `tincan` without `--room`, it will silently create and use a new `.tincan/` directory in that subdirectory instead of communicating via the project-root room. 

---

## Onboarding Friction

### 1. The "Agent-Level Loop" vs "Shell-Level Loop" Cognitive Dissonance
- *Reference*: [PROTOCOL.md](file:///Users/arda/projects/tincan/PROTOCOL.md#L30-L37) and [skills/listen/SKILL.md](file:///Users/arda/projects/tincan/skills/listen/SKILL.md#L18-L30).
- *Detail*: Documented loops tell the listener agent to:
  `Loop (do not stop between iterations)... Go back to step 1.`
  If an agent tries to implement this literally as a shell loop (e.g. `while true; do ... done`), it becomes unable to perform the actual tasks. A shell process runs independently and cannot delegate back to the agent's LLM context to write code, modify files, or use custom tools. 
  To work, the loop must run at the **agent level**: the agent runs `tincan recv` in the background, yields control (stops calling tools), and when the tool finishes and surfaces the task to the agent context, the agent performs the code modifications, sends a reply, and re-arms by running the next background `tincan recv`. This distinction is not clear in the docs.

### 2. Lack of Async/Background Execution Guidance
- *Detail*: Many agent harnesses (e.g., Claude Code, Gemini/Antigravity) cap synchronous command execution durations or timeout. A blocking call of `tincan recv` or `tincan ask` that waits for minutes will hit timeouts or hang the shell session. The documentation needs to explicitly guide agents to run these long-running blocks in the background (using backgrounding CLI commands or agent tools) rather than running them synchronously.

---

## Suggestions

1. **Update `PROTOCOL.md` and Skill Reference Commands**:
   - Document all missing flags (`--log`, `--format`, `--timeout`, `--corr`, `--reply-to`, and `--from`).
   - Clarify that `--room` defaults to the immediate current directory (`.`) and does not auto-climb directories. Advise listeners to always run using the absolute project root path or `--room $(git rev-parse --show-toplevel)`.

2. **Revise the Listener / Orchestration Prompting**:
   - Clarify the difference between a shell-level loop and an agent-level loop.
   - Instruct agents to execute the blocking `recv` / `ask` commands asynchronously and yield control to allow notifications to wake them back up.

3. **Enhance `install.sh`**:
   - Allow `install.sh` to download prebuilt binaries from GitHub Releases via `curl`/`wget` when the Go toolchain is missing, rather than failing immediately.
   - Add options or print instructions for installing skills to other agent platforms (e.g. Cursor rule configurations, Codex shims).
