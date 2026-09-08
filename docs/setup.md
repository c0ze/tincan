# Client and provider setup

tincan has two roles: an MCP server used by your current agent client, and a
host that runs other installed agent executables in a room. You can launch and
talk to workers using MCP tools without opening a terminal for each worker.

The host stays available between requests. For supported providers, a saved
conversation ID preserves context even though the provider executable runs
again for each turn. There is no provider SDK/ACP backend or desktop window
automation in v2.

## Install and authenticate workers

Install [tincan v2](../README.md#install) and whichever provider executables you
want to use on the same machine. Authenticate those providers under the OS
account that runs tincan. A desktop application's login does not necessarily
authenticate its corresponding command-line provider.

| Preset | Executable | Default conversation mode |
|---|---|---|
| `claude` | `claude` | Persistent |
| `codex` | `codex` | Stateless |
| `grok` | `grok` | Persistent |
| `agy` | `agy` | Persistent |
| `kimi` | `kimi` | Persistent |
| `gemini` | `gemini` | Stateless |

`tincan_presets` reports executable availability and adapter support. Finding an
executable is not an authentication test; send a small task to verify the whole
path. Provider models, credentials, network access and billing remain governed
by the installed provider. Grok cloud execution is a separate environment and
is not configured by tincan's local `grok` preset.

Custom commands and absolute executable paths belong in
`~/.config/tincan/agents.json`. Each override replaces the preset entry, so include
its full command and input/output settings. Custom wrappers default to stateless
execution unless they explicitly select a supported session adapter. See
[preset configuration](../PROTOCOL.md#presets-and-configtincanagentsjson).

## Connect an MCP client

Choose an existing project directory and keep its absolute path fixed as the
room. Add a stdio server to any CLI or desktop client that supports local MCP
servers. For clients accepting an `mcpServers` JSON object:

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

For Codex's TOML configuration, the equivalent server entry is:

```toml
[mcp_servers.tincan]
command = "/absolute/path/to/tincan"
args = ["mcp", "--room", "/absolute/path/to/repo"]
```

Use the configuration location supported by your client, replace both paths,
and restart or reconnect it. Desktop clients may have a different `PATH` from
your terminal; an absolute tincan path resolves only the server executable.
Workers must also be discoverable in that environment, or use absolute paths
in their preset entries.

There is no separate desktop preset: a Claude, Codex or other desktop client
uses the tools if it supports local stdio MCP. Worker names still refer to the
executables in the table above. Clients running in the cloud cannot reach a
local room merely because this configuration exists on your computer.

Each computer has its own rooms and hosts. Installing on a second computer does
not bridge messages between machines. On Windows, start a foreground
`tincan serve <name> --room <path>` before sending through MCP; automatic detached
launch is supported on Linux and macOS.

## Start a session

Paste this into a client after connecting tincan:

> Use tincan's MCP tools for this workspace. First inspect tincan_status and
> tincan_presets, and confirm the room matches this project. Use tincan_send to
> delegate tasks, retain each request_id, and collect results with tincan_wait
> using timeout_seconds 30 and the returned progress cursor. Keep waiting on the
> same ID when a wait times out. Keep the listeners available for follow-up work
> until I ask you to stop them.

For a first worker check, ask:

> Through tincan, ask claude to reply TINCAN_OK. Retain the request ID and collect
> its final result. Then ask the same claude listener what it just replied, and
> report whether it remembered. Keep the listener running.

Replace `claude` with `grok`, `agy` or `kimi` to check those persistent adapters.
For `codex` or `gemini`, test a self-contained task; their default mode does not
promise memory of a previous request. A connection may close while work is
running: reconnect to the same room and collect the saved request ID.

If MCP is unavailable, install the [tell/listen skills](../README.md#2-the-skills).
An interactive session can receive tasks with `listen as <name>`. Use a distinct
name from any hosted worker already in the room. That loop needs the client to
stay open; it is different from a detached hosted listener.

## Stop, reset and recover

- `tincan_stop` stops a host but keeps its conversation pointer.
- `tincan_reset` stops it and clears the pointer so the next launch starts fresh.
- Both preserve queued requests. Cancel unwanted queued work explicitly before
  restarting; cancellation does not undo edits already made.
- Retain request IDs across reconnects. Reusing the same ID for identical work
  is idempotent while its record exists; a new ID is new work.
- Inspect interrupted requests and workspace changes before submitting another
  task. tincan does not automatically replay uncertain work.

## Troubleshooting

- **Tools missing:** check the absolute server path and room, restart the client,
  and inspect its MCP diagnostics. `tincan version --format json` should identify
  the v2 binary used by that configuration.
- **Preset unavailable:** the host cannot find the provider executable. Fix its
  `PATH` or configure an absolute executable path in `agents.json`.
- **Tools work, provider fails:** read the returned error and the room's
  `.tincan/hosts/<name>.log`. Authenticate the provider in the same account and
  execution context. MCP discovery alone does not check provider credentials.
- **Claude on macOS over SSH:** a locked login Keychain can prevent credential
  access even when a desktop session is logged in. In that SSH session, run
  `security unlock-keychain "$HOME/Library/Keychains/login.keychain-db"`, enter
  the password at the prompt, and launch the test from that session. Do not put
  passwords in commands, preset files or prompts.
- **Saved session cannot resume:** inspect the provider error. tincan preserves
  the pointer instead of silently starting a new conversation. Use
  `tincan_reset` when you intend to discard that conversation's local pointer.

Treat the room as private workspace data and exclude `.tincan/` from Git.
Anyone able to write its inbox can drive workers with their configured
permissions. See the [operating protocol](../PROTOCOL.md) for the full lifecycle
and trust boundary.
