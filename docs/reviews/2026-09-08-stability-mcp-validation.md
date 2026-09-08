# Stability and MCP validation — 2026-09-08

This change addresses the September audit and adds native MCP orchestration.
The provider adapters still run the installed provider executable for each turn;
explicit provider conversation IDs preserve context. The MCP connection and
detached tincan listener can remain open between tasks. Provider SDK/ACP backends
are not implemented.

## Audit resolutions

- Host shutdown uses an authenticated local control endpoint instead of signaling
  a PID loaded from disk. Lifetime and launch locks exclude duplicate hosts;
  ownership checks protect state cleanup and readiness works while busy.
- Claims remain on disk until a durable result is recorded and delivery is
  acknowledged. Recovery records an uncertain interrupted outcome without
  repeating potentially completed side effects. Explicit request IDs deduplicate
  identical submissions and reject conflicting work.
- Owned process groups are cleaned on completion, timeout, and cancellation;
  Windows execution uses Job Objects. Output and logs are bounded.
- Invalid JSON, oversized envelopes, symlinks, and special files are rejected or
  quarantined without blocking the listener. Runtime paths are private on POSIX.
- CLI output errors propagate and return the claimed envelope to the queue.
- Version metadata identifies source builds; installation instructions distinguish
  the old published release from this checkout. Shared skills and the Gemini
  control-message shim are updated, with installer regression coverage.
- Retry guidance collects the original request instead of submitting new work.
  Explicit garbage collection removes old terminal results and delivery logs,
  retaining queued/running/unacknowledged work and stable lock files.
- Release publishing depends on tests of the exact tagged commit. CI covers
  minimum and stable Go on Linux, macOS, and Windows.

## MCP behavior

Eight stdio tools expose preset discovery, launch, send, wait, status, cancel,
reset, and stop. Each server is bound to one room. Launch accepts configured
presets, and status never exposes control credentials or command arguments.
Requests and results survive client reconnects; progress uses bounded events
and cursors. Native hosted requests do not create unused reply inboxes.

Claude, Grok, Agy, and Kimi have persistent session adapters. Session IDs are
validated, stored per room/listener, and checked against the provider and preset.
Reset clears the local pointer; it does not delete the provider's transcript.
Incomplete session initialization requires explicit recovery rather than silently
starting a different conversation. Structured provider failures remain visible.

Legacy interactive listeners are supported through durable reply collection.
Pending interactive work retains its route while the listener is busy. These
receivers cannot reliably honor targeted cancellation, so their request records
explicitly report that cancellation is unavailable.

An independent Codex review found three additional defects: interactive retries
could republish consumed work, macOS temporary-directory aliases could prevent
file replies, and atomic journal replacements could spuriously fail route scans.
The follow-up fixes persist interactive publication intent/confirmation, resolve
temporary-directory aliases before creating reply files, and read routing metadata
through bounded snapshots that tolerate atomic replacement. Uncertain interactive
publication is reported explicitly and never automatically republished.
The follow-up review also checked a retry racing with reply collection. Validated
interactive replies can resolve publication uncertainty, so recording uncertainty
cannot discard an actual answer. Other terminal results remain immutable.

## Local verification

Automated coverage includes real stdio MCP discovery/calls, reconnect and cached
results, canceled work, abandoned claims, conflicting IDs, concurrent starts,
stale PID ownership, broken stdout, poisoned spool entries, process cleanup,
session parsing, output bounds, and installation into aliased skill directories.

The suite was run on Linux with Go 1.25.14 and Go 1.27.1, including race detection
on the latter. Vet, build, and installer checks passed. Windows/amd64 and
macOS/arm64 binaries cross-compiled locally; their runtime tests require CI.

Live tests used the stdio MCP tools with isolated rooms and the installed
providers. They included follow-up conversation, MCP disconnect/reconnect,
listener stop/restart, independently checked file reads/writes, and reset.

| Provider | Version | Conversation / reconnect | Exact file contents | Restart |
|---|---|---|---|---|
| Claude Code | 2.1.259 | Correct remembered color | Passed | Same session resumed; see below |
| Grok | 1.0.13 | Passed | Passed | Passed |
| Agy | 1.1.27 | Passed | Passed | Passed |
| Kimi | 0.41.0 | Passed | Passed | Passed |

Claude's default model rejected one repeated color-recall prompt with a provider
`reasoning_extraction` error. Tincan returned that failure and its reason.
The next file task completed in the same restarted session and included the
remembered color in its answer. Reset then returned no previous context. The
strict response-text assertions also flagged capitalization and extra prose;
the separate file-content comparisons passed for all four providers. These are
provider response differences, not evidence of lost session state.

The final live run stopped all four listeners and left zero queued messages.
These checks establish functional behavior and targeted failure recovery, not
long-duration endurance or production availability guarantees.

## Follow-up validation and v2 release preparation

The results above describe the initial local audit run. Subsequent live MCP
checks on a Linux ThinkPad and an Apple Silicon Mac mini passed for Claude,
Codex, Grok, Agy and Kimi. Claude, Grok, Agy and Kimi retained conversation context
across reconnects; Codex used stateless runs. These are checks of installed local
provider executables, not tests of desktop UI automation or cloud harnesses.

On macOS, Claude authentication required unlocking the login Keychain in the
same SSH session that launched the verifier. A Claude SDK no-tools diagnostic
printed alongside structured JSON exposed a parser issue; commit `f1f8612`
accepts that exact diagnostic without accepting arbitrary malformed output.
The final Claude retest passed readiness, reconnect, same-host identity,
conversation memory and shutdown.

Native CI subsequently passed all six Linux/macOS/Windows and Go 1.25/stable
jobs on [`676c6ae`](https://github.com/c0ze/tincan/actions/runs/34200099252),
including Windows snapshot replacement and exited-process liveness regressions.
This supersedes the earlier cross-compilation-only platform evidence. Windows
detached launch remains unsupported.

The v2.0.0 release changes the Go module path to `github.com/c0ze/tincan/v2`.
Current installation and compatibility guidance lives in the
[release notes](../releases/v2.0.0.md) and [setup guide](../setup.md).
