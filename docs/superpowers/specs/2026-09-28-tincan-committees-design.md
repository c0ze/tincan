# tincan committees — Design Spec (phase 2)

Status: design discussed with the owner on 2026-09-28. Codex (gpt-6-astra,
xhigh) reviewed a draft (16 findings) and this spec (16 findings); both rounds
are resolved below (§13). Round 3 checked the spec against the phase 1 code
that landed (`96d7d76`); phase 2a shipped (`af82dc4`) and round 3's phase 2b
findings are resolved in §6 and §10 (§13), pending Codex round 4. The owner approved the key decisions (read-only
reviewer presets and independent clones with the residual risk stated;
committee executions count against the thread budget; quota skipping off by
default; phases 2a/2b/2c). Awaits the owner's review of this document.

Builds on phase 1, `docs/superpowers/specs/2026-09-28-tincan-web-chat-design.md`
including its §15 amendments (two-way link, quota panels, dispatcher lock at
`.tincan/dispatcher.lock`, registry exclusion, atomic journal transactions).

## 1. Problem

The owner routinely asks one agent to have its work reviewed by others. Today
that means hand-building each request, picking accounts, and repeating it per
reviewer and machine. The owner wants named groups of reviewers — *committees*
— whose members are specific agents on specific accounts on specific machines
(Codex on the business account on cachyos, Claude on the personal account on
the Mac, MiMo on cachyos, Grok on the Mac), and to say "get this reviewed by
committee X" from anywhere.

## 2. Goals / Non-goals

Goals
- Account profiles: one CLI, several accounts, selected per preset.
- Committees: named, owner-defined groups of `preset@machine` members.
- One durable review primitive used by MCP, the CLI and web threads.
- Members on another machine review a faithful packet of the change, in a
  reconstructed checkout when their machine has the repository.
- Every member's review is returned; the requester synthesizes.
- Reviews survive client exits, coordinator restarts and peer outages without
  running any member twice.
- Existing CLI/MCP contracts unchanged; new tools are additive.

Non-goals
- OS-level sandboxing of reviewers. Isolation is by independent clone plus
  read-only presets, with the residual risk documented (§9).
- Fetching from remotes, pushing, or transferring code beyond the packet.
- Quota-based skipping by default (opt-in only, §8).
- Chained peers; more machines than the configured peers.
- Chair/summarizer members.
- Budget propagation through arbitrary nested `tincan_send` delegation; only
  reviews carry a thread origin (§6.10).

## 3. Decisions

| Question | Decision |
|---|---|
| Code access across machines | Packet; reconstructed independent clone when the member's machine has the repository and every needed object. |
| Entry points | MCP, CLI and `@committee` in web threads. |
| Results | All member reviews; the requester synthesizes. |
| Definitions | Edited in the web UI, stored on the hub, cached by the peer. |
| Transport | The `tincan web` machine link (owner identity + CSRF rules of phase 1 §9). |
| Reviewer isolation | Independent dissociated clone, read-only presets, executables pinned outside reviewed content; residual risk documented. |
| Budget | Each member execution and each synthesis turn counts against the thread chain budget. |
| Quota skipping | Off by default; opt-in with strict, machine-local rules. |
| Phasing | 2a account profiles → 2b durable reviews → 2c synthesis delivery. |

## 4. Phasing

- **Phase 1** (separate spec/plan): web chat, two-way link, quota panels.
- **Phase 2a — account profiles** (§5).
- **Phase 2b — committees and durable reviews** (§6–§9).
- **Phase 2c — synthesis delivery** (§10).

Each phase ships with its own plan and tests and leaves the system working.

## 5. Phase 2a — account profiles

### 5.1 Preset fields

Presets in `~/.config/tincan/agents.json` gain:

- `env` — environment for the agent process. Keys match
  `^[A-Z_][A-Z0-9_]{0,63}$`; rejected: `PATH`, any key in the parent-session
  scrub list (`internal/host/env.go`), and the reserved prefix `TINCAN_`.
  Values are strings; a leading `~/` expands to the home directory at launch.
- `env_unset` — inherited variable names to remove (same key rules), e.g.
  `ANTHROPIC_API_KEY` so a profile's own login is used.
- `provider` — optional explicit session provider (`claude`, `grok`, `agy`,
  `kimi`), see §5.3.

Environment for an agent run, in order: the scrubbed tincan environment
(`agentEnv`), minus `env_unset`, plus `env`, **then** tincan's protocol
variables (§6.10), which presets cannot override.

Example:

```json
"codex-gmail": {"exec": ["codex","exec","-s","read-only","--skip-git-repo-check","-o","{out}","-"],
                "stdin": "body", "reply": "file", "env": {"CODEX_HOME": "~/.codex-gmail"}},
"claude-personal": {"exec": ["claude","-p","{body}","--permission-mode","plan","--setting-sources","user"],
                    "env": {"CLAUDE_CONFIG_DIR": "~/.claude-personal"}, "env_unset": ["ANTHROPIC_API_KEY"]}
```

### 5.2 Keeping configuration off the command line

`host.Up` currently passes the resolved preset as `--resolved-preset <json>`,
which would expose `env` values in process listings. Instead:

- `Up` writes the resolved preset to
  `<room>/.tincan/hosts/config/<name>/<owner>.json`, where `<owner>` is the
  host lifetime identity `Up` already generates (one directory per listener,
  because listener names may contain dots). The directory is 0700, the file
  0600, written atomically. `Up` passes `--resolved-preset-file <path>`
  (`--resolved-preset <json>` is removed).
- The file is only the hand-off from `Up` to the daemon it spawns: `serve`
  reads it at startup and removes it. `Up` removes it itself if the daemon
  never became ready. Holding the launch lock with the lifetime lock free, it
  first empties that listener's directory: any file there is stale.
- Foreground `tincan serve` resolves the preset in-process and needs no file.
- The durable record of the running configuration stays where phase 1 put it:
  the listener's 0600 state file (`State.Config`), written by `serve` in both
  modes. "Explicit settings must match the running configuration"
  (`existingResult`) keeps comparing against it, never against redacted views.
- Public views — `tincan presets`, `tincan_presets`, `status`, web — show
  `env`/`env_unset` keys only, never values. `State.Config` is never
  serialized to a public view.

### 5.3 Session provider identity

- Provider = `provider` if set, else the base name of `exec[0]` (without
  `.exe`) when it is a supported adapter, else none. The preset label no longer
  decides it, so `claude-personal` keeps persistent sessions.
- Explicit `session: "stateless"` wins; wrappers stay stateless unless they set
  `provider`.
- The session fingerprint includes provider, executable and a hash of the
  sorted `env`/`env_unset` configuration, so changing a listener's account
  never resumes another account's conversation. Session records gain
  `"version": 2`.
- Legacy records (no version; fingerprint `sha256(Exec, Stdin, Reply)`,
  `Provider` = label) are migrated in place, not invalidated, when the legacy
  fingerprint still matches, the preset has no `env`/`env_unset`, and the new
  provider equals the recorded one. Anything else keeps phase 1's error
  ("stop and reset the listener explicitly").
- All label-keyed session behaviour — `SupportsSessions`, `WithSession`,
  resume/new-session argv, session-ID validation, output parsing and Kimi's
  reply cleanup — switches to the provider through one helper.
- `tincan_presets` reports `session_supported` from the provider.
- Codex remains stateless.

### 5.4 Quota mapping

Phase 1's `quotas.json` maps entries to presets and may declare `blocking`
(`weekly`, `short` or `either`), used only by §8.

## 6. Phase 2b — committees and reviews

### 6.1 Committee definitions

```json
{"name": "reviewers", "version": 7,
 "members": ["codex-gmail@cachyos", "claude-personal@macmini", "mimo@cachyos", "grok@macmini"],
 "deadline_minutes": 30, "instructions": "Focus on correctness and security; cite file:line.",
 "skip_exhausted": false}
```

- `name`: `[A-Za-z0-9][A-Za-z0-9_-]{0,63}` (no `.`, which is reserved for
  thread listeners); not `you`, and not a preset name on any known machine.
- `members`: 1–8 unique `preset@machine`; `machine` is this machine or a
  configured peer. Each must resolve on its machine at save time, checked
  against that machine's `api/presets` catalogue (§6.7). The preset's
  executable must be a bare name or an absolute path (§6.6). Members whose argv
  contains a known permission bypass (`--dangerously-skip-permissions`,
  `--always-approve`, `-s danger-full-access`, `--yolo`) are accepted with a
  visible warning.
- `deadline_minutes`: 1–240, default 30. `instructions`: ≤ 8 KiB.
- `version` increments on every save.

Storage and sync:
- The hub stores `committees.json` in the shared state directory
  (`$TINCAN_STATE_DIR` > `$XDG_STATE_HOME/tincan` > `%LocalAppData%\tincan` >
  `~/.local/state/tincan`), file lock + atomic replace.
- A peer runs `tincan web --committees-from <peer>`: fetch at start, every
  60 s, and on demand (§6.7); keep the last valid copy with `fetched_at`; the
  UI shows cache age.
- Reviews start from the local copy (hub: authoritative; peer: cache). Members
  on unreachable machines become `unreachable` (§6.4) instead of blocking.
  Edits go to the hub and fail clearly when it is down.
- Every review stores a snapshot of the definition it used.
- Amends phase 1's non-goal "a central store" for committee definitions only.

### 6.2 Reviews in the room

```
<room>/.tincan/reviews/
  staging/<review_id>/       being assembled; READY written last
  <review_id>/
    input.json               immutable: committee snapshot, question, scope, origin, created, deadline
    packet/                  immutable: manifest.json, diff.patch, files/…, question.md
    prompts/<n>.txt          immutable: coordinator prompt for member n
    review.json              mutable state, rewritten atomically under ./lock
    results/<n>.md           member results
    bundle.md                written once at close
    lock
```

- **`review_id` is derived, not stored under a key.** With a request ID it is
  `rv-` + the first 16 hex digits of `sha256(canonical room + "\0" +
  request_id)`; a thread's `@committee` message uses its deterministic request
  ID `<tid>-<mid>`; without a request ID it is random. Publication is then
  idempotent by construction (§6.3) and needs no key records, closing the gap
  between publishing a directory and recording its key.
- `input.json` holds the **identity** compared on replay: committee name and
  version, question hash, scope, `included_tree`, origin.
- `review.json`: `status` (`running|closed|cancelled`), `settled` (bool),
  `cancel_requested`, `closed_at`, `delivered` (phase 2c), and `members[]`:
  `member`, `index`, `job_id` (`<review_id>-<index>`), `expires_at`, `state`
  (`planned|submitting|submitted|running|done|error|skipped|unreachable|cancelled|expired`),
  `late`, `posted` (result appended to the origin thread), `cancel_state`
  (`none|requested|acked|moot`), `ack_state` (`none|acked|moot`), `started`,
  `finished`, `note`.

### 6.3 Publication

Every entry point publishes the same way; `READY` and the rename are the only
commit points.

1. **Coordinator present.** `tincan web` writes `<state>/web.json`
   (`pid`, `started`, `updated`) every tick. MCP and CLI entry points refuse
   with "tincan web is not running on this machine; start it (see
   docs/web.md)" unless `updated` is under 30 s old. They then register the
   room (`rooms.Touch`) and create `.tincan/`, so the next tick coordinates it
   (phase 1 coordinates registered rooms that contain `.tincan/`). Nothing
   probes or creates `dispatcher.lock` from outside the coordinator.
2. **Replay.** If `reviews/<review_id>` exists, compare identities: equal →
   return it; different → conflict error.
3. **Stage.** Build `staging/<review_id>/` (removing a leftover one first):
   `input.json`, the packet (§6.5), every member prompt (§6.8), the initial
   `review.json` (`running`; members `planned`, or `skipped` per §8); fsync
   each file, then write and fsync `READY`.
4. **Origin commit** (thread origins only, §6.10): one thread transaction.
5. **Publish.** Rename `staging/<review_id>` → `<review_id>`. If the target
   exists, fall back to step 2's comparison (another process published the
   same staging first).

Recovery rules, applied by the coordinator:
- A staging directory with `READY` whose review is referenced by a thread
  committee message (§6.10) is published by the coordinator (step 5).
- Any other staging directory older than one hour is removed.
- The coordinator never builds a packet except for a thread's `@committee`
  mention (§6.10), and then only in step 3 of this procedure.

### 6.4 Coordination and lifecycle

**Placement.** Reviews reconcile inside phase 1's room pass, on the
`thread.Dispatcher` that already owns the room's `dispatcher.lock` — the same
per-room serialization and cross-room concurrency as threads. Each pass visits
every review that is not `settled`, after the room's threads. Lock order is
always thread lock → review lock → job lock.

**Per-member obligations.** Each member carries up to four independent
obligations; each step is idempotent and has a bound derived from the
member's `expires_at` (deadline + 30 min, §6.6), so no review is reconciled
forever.

1. **Submit** (`planned` → `submitting` → `submitted`): persist `submitting`,
   then create the job — in-process for a local member, `POST
   api/review-jobs` through the peer client (§6.7) for a remote one. Responses:
   - 2xx → `submitted` (or the job's reported state).
   - 409 identity conflict, 400/404/422 (unknown preset, relative
     executable, invalid request) → `error` with the response text:
     permanent, never retried.
   - 410 → `expired`.
   - 5xx, timeout, connection failure → stay `submitting`, retry every 15 s.
   At the deadline a `submitting` member gets one `GET`: a job that exists is
   adopted (its state becomes the member's); 404 → send a tombstoning cancel
   (so a delayed create is refused) and mark `unreachable` once it is acked;
   still no answer → `unreachable` with note "outcome unknown: peer not
   reachable". After `expires_at` nothing further is attempted for submission:
   a job can neither be created nor launched past its expiry (§6.6).
2. **Collect** (`submitted`/`running` → terminal): read the job every 5 s and
   on notes; on a terminal job write `results/<n>.md`, then the member state.
   An unreachable peer leaves the member as is; past `expires_at` + 5 min the
   member becomes `expired` with note "result not retrievable".
3. **Acknowledge** (remote members with a recorded terminal result):
   `POST …/ack` until 2xx, 404 or 410 → `ack_state: acked`; past `expires_at`
   + 7 days (when the peer drops unacknowledged jobs anyway) → `moot`.
   Acknowledgement never blocks close.
4. **Cancel** (only after `cancel_requested`): `planned` members become
   `cancelled` unsent; `submitting`, `submitted` and `running` members get
   `POST …/cancel` (tombstone if unknown, §6.6) every 15 s until acked; past
   `expires_at` the job cannot be running, so `cancel_state` becomes `moot`.
   A member whose cancel is acked and that has no result is `cancelled`.

**Close** once: when every member is terminal or the deadline passes, set
`status: closed`, `closed_at`, and mark non-terminal members `late: true`.
The bundle (§6.9) is written from the state at close before `closed` is
persisted (a crash in between rewrites the same bundle). Late results are
recorded and shown; the bundle and any delivery are not redone.

**Cancel** (allowed until settled, including after close): persist
`cancel_requested`, then run obligation 4. The review becomes `cancelled` if
it had not closed.

**Settle**: every member terminal; every remote terminal result acked or
moot; every requested cancel acked or moot; (2c) delivery done or not
applicable; (thread origins) every result posted or the thread no longer
open. Settled reviews are not reconciled again.

### 6.5 Scopes and the packet

Built by the entry point in the requesting room. Repository root =
`git rev-parse --show-toplevel`; packet paths are repository-relative; the
scope covers the whole repository even for a subdirectory room.

| Scope | Base | Target |
|---|---|---|
| `none` | — | — (question only; no Git required) |
| `uncommitted` (default in a Git room) | `HEAD` commit, or the empty tree when HEAD is unborn | working-tree snapshot |
| `branch` | merge-base of `HEAD` and the default branch (`origin/HEAD`, else `main`, else `master`) | working-tree snapshot |
| `commit:<rev>` | first parent commit (empty tree for a root commit; merges use the first parent and say so) | `<rev>^{tree}` |
| `range:<a>..<b>` | `<a>` | `<b>^{tree}` |

**Working-tree snapshot.** Refused with a clear error when the index has
unmerged entries or the repository uses sparse checkout. Otherwise, with a
private temporary index file (`GIT_INDEX_FILE`):
1. seed it from the base: `git read-tree <base>` (or `git read-tree --empty`),
   so tracked files — including tracked files matching ignore rules — keep
   their tracked status;
2. `git add -A` from the repository root (adds untracked non-ignored files,
   records modifications and deletions);
3. `git write-tree` → the target tree.
This writes ordinary objects to the repository's object store (collected by
`git gc`) and never touches the real index or working tree. It is not an
atomic filesystem snapshot: files edited during capture may be captured
mid-edit; the manifest records the capture time.

**Included changes.** `git diff --binary --find-renames --raw` between base
and target lists changes. Omitted, with a reason in the manifest:
- secret-looking paths (`.env`, `.env.*`, `*.pem`, `*.key`, `*.p12`, `*.pfx`,
  `id_rsa*`, `id_ed25519*`, `.npmrc`, `.netrc`, `.pypirc`, `credentials*`,
  `*secret*`);
- symlinks (mode 120000) and submodules (gitlinks), listed but not included;
- files whose target content exceeds 1 MiB;
- paths failing the path policy (§6.6), listed.
A rename is included only if both endpoints pass; otherwise it is represented
as a deletion plus an addition, each judged separately.

The **included target tree** is computed on the requester: base plus the
included changes (applied to a second temporary index), `git write-tree`. The
patch is `git diff --binary --find-renames <base> <included target>`. The
manifest records: `repo_id` (§6.6), `base_kind` (`commit|empty`),
`base_commit`, `target_tree`, `included_tree`, capture time, every changed path
with status, and omissions.

**Limits**, applied to raw bytes before JSON encoding: patch ≤ 3 MiB; manifest
≤ 5 000 paths and ≤ 512 KiB; question ≤ 64 KiB; instructions ≤ 8 KiB;
packet-only file contents ≤ 2 MiB total. The encoded job request (§6.7) is
≤ 8 MiB. If the patch exceeds its limit, the packet is `complete: false`: no
patch, and post-change contents of changed text files in manifest order up to
the file-content limit (listed). `complete: true` means the patch reproduces
`included_tree` from the base. An empty patch is valid ("no changes");
members still run on the question.

### 6.6 Member jobs (on the member's machine)

Job record `<state>/reviews/jobs/<job_id>.json` (0600), `job_id` validated with
`envelope.ValidComponent`; all create/cancel/ack/cleanup operations on a job
hold `<state>/reviews/jobs/<job_id>.lock`.

**Identity**: (`job_id`, `packet_sha256`, `prompt_sha256`, `preset`,
`expires_at`). A repeat with the same identity returns the current state; a
different identity → 409. `expires_at` is absolute (review deadline + 30 min,
at most 270 min after creation); a create or replay at or after `expires_at` is
rejected (410). Cancelled and acknowledged jobs keep a tombstone for 30 days;
since every job expires within hours, any replay after the tombstone is gone is
rejected as expired, so a job can never run twice. A cancel for an unknown
`job_id` creates a tombstone with the supplied `expires_at` (or 30 days), and a
later create with that ID is refused.

**Repository identity** (`repo_id`): the origin URL without credentials,
normalized — lower-case host, port kept (non-default ports are part of the
identity; default ports 22/443 dropped), path without `.git` and trailing `/`,
SSH scp-style and `ssh://`/`https://` forms unified. No origin → no `repo_id`.

**Candidate selection**: registered rooms whose repository root has the same
`repo_id`, deduplicated by repository root, ordered by registry `last_used`
(newest first) then path. Partial clones (`extensions.partialClone` or a
promisor remote) are skipped. With lazy fetching disabled
(`GIT_NO_LAZY_FETCH=1`), a candidate qualifies if `base_kind` is `empty` or
`git cat-file -e <base_commit>^{commit}` succeeds. No fetch is attempted.

**Checkout mode** (packet `complete` and a candidate qualifies):
1. `git clone --no-checkout --shared <candidate root> <ws>`, then
   `git -C <ws> repack -a -d -q` and remove `.git/objects/info/alternates`, so
   the clone owns its objects and survives maintenance in the candidate.
2. Base: `git -C <ws> checkout --detach <base_commit>`, or for an empty base
   `git -C <ws> checkout --orphan tincan-review` with an empty index and tree.
3. Validate the patch before applying (below), then
   `git -C <ws> -c core.symlinks=false apply --index --binary packet/diff.patch`.
4. Verify `git -C <ws> write-tree` equals `included_tree`.
Any failure removes `<ws>` entirely before falling back to packet-only mode.
The reviewer is told that `git diff --cached` (or `git diff HEAD`) shows the
change. Git hooks never run in the clone (fresh `.git/hooks`), and the
candidate's configuration is not copied.

**Packet-only mode**: a fresh directory containing `packet/manifest.json`,
`packet/diff.patch` (if any), `packet/question.md` and `packet/files/<path>`.

**Path policy** (both modes): every path — including both rename endpoints and
every path in the patch header — is relative, clean, without empty, `.` or
`..` components, without a component that case-folds to `.git` or `.tincan`,
≤ 1 024 bytes, each component valid under `envelope.ValidComponent`; no two
included paths may case-fold to the same string; symlink and gitlink modes are
never applied. Packet files are created with exclusive, no-follow opens as
regular 0600 files in directories the materializer created itself.

**Executables are pinned outside reviewed content.** The member preset's
executable is resolved on the member machine **before** the workspace exists,
never relative to the workspace (committee members may only use bare names or
absolute paths), and the resolved absolute path is used for the run through a
strict execution path: no re-resolution, no base-name fallback (phase 1's
`ResolveExecutable` falls back when an absolute path is missing), no template
expansion of `argv[0]`; if the pinned file has disappeared the job fails.
Repository-controlled agent configuration inside the workspace (e.g. a
reviewed repo's `.claude/settings.json` hooks) can still influence a reviewer;
reviewer presets should disable project configuration where the CLI allows it
(`claude --setting-sources user`), and the docs list this as residual risk.

**Execution**: the preset runs as a hosted listener named after the preset,
with the workspace as its room, stateless, exec timeout = time remaining until
`expires_at` at launch (a launch with less than one minute left is rejected as
expired). Request ID = `job_id`. The prompt is the receiver's workspace note
(§6.8) followed by the coordinator prompt, whose hash must equal
`prompt_sha256`. Workspaces are never registered as rooms (phase 1 §15.3).

**Create**: under the job lock: record the job (identity, `created`), resolve
and pin the executable, materialize the workspace, then submit through
`dispatch.Send` with request ID `job_id`. A `Send` error after it saved the
request (a launch failure) cancels that saved request before the job is marked
`error`, so a later listener launch cannot run it; the error is permanent and
a replay with the same identity returns it without running anything.

**Cancel acknowledgement**: under the job lock, record the tombstone (launch is
now impossible), cancel the request and ask the host to stop it, and
acknowledge only once the request is terminal or the host has exited.

**Cleanup** after the result is acknowledged or an acknowledged cancel: stop the listener
(`host.Down`, wait for exit), remove the workspace, keep the tombstone (result
removed after 7 days). Active jobs are never swept by age; an unacknowledged
terminal job is cleaned up 7 days after `expires_at`.

### 6.7 Machine-link API (added to phase 1's `tincan web`)

All routes use phase 1's owner check and CSRF rules, with endpoint-specific
body limits.

| Method | Path | Limit | Purpose |
|---|---|---|---|
| GET | `api/presets` | — | Machine-level redacted catalogue: name, available, provider, `session_supported`, env keys, bypass warning, quota entry. |
| GET | `api/committees` | — | Definitions + version (hub) or cache + age (peer). |
| PUT | `api/committees/{name}` | 16 KiB | Create/update (hub only). |
| DELETE | `api/committees/{name}` | — | Delete (hub only). |
| POST | `api/review-jobs` | 8 MiB | Create a job `{job_id, review_id, requester, preset, expires_at, packet, prompt}`; idempotent (§6.6). |
| GET | `api/review-jobs/{job_id}` | — | Status and result (≤ 1 MiB). |
| POST | `api/review-jobs/{job_id}/ack` | 1 KiB | Acknowledge the result. |
| POST | `api/review-jobs/{job_id}/cancel` | 1 KiB | Cancel `{expires_at?}`; tombstone if unknown. |
| GET | `api/presets/{preset}/quota-status` | — | Machine-local skip decision (§8). |
| GET | `api/rooms/{rid}/reviews` | — | Reviews in a room. |
| GET | `api/rooms/{rid}/reviews/{review_id}` | — | One review with member states and results. |
| POST | `api/rooms/{rid}/reviews` | 80 KiB | Start a review from the UI. |
| POST | `api/rooms/{rid}/reviews/{review_id}/cancel` | 1 KiB | Cancel. |

The owner check authenticates the calling **device owner**, not a specific
daemon: any process on the owner's untagged devices can create jobs, as it can
use the chat (phase 1 §9). `requester` is informational only.

**Outbound peer client.** Coordinator-to-peer calls do not go through phase
1's browser proxy. A shared client sends each request to the peer's configured
public URL (`https://<node>.<tailnet>.ts.net/<path>/api/…`), so `tailscale
serve` on the peer attaches this device owner's identity and the peer's Host
allowlist, forwarded-Host, owner and Funnel checks all pass unchanged. The
client sets `X-Tincan-Request: 1`, never sends `Origin`, identity or forwarded
headers, uses phase 1's peer timeouts (5 s dial, 30 s response headers) and a
per-call deadline of 60 s (job creation) or 10 s (everything else), and caps
response bodies at the route's limit. The browser proxy stays as it is.

**Change notes.** The scanner adds note kinds `committees` (fingerprint of
`<state>/committees.json`) and `reviews` (per room, `.tincan/reviews` and each
unsettled `review.json`). Daemons do not subscribe to each other's SSE: a
peer refreshes its committee cache every 60 s, and immediately when the local
UI requests `api/committees` with a cache older than 10 s; the browser already
receives peer notes through the proxied SSE stream.

### 6.8 Prompts

The coordinator prompt is **mode-neutral** and hashed (`prompt_sha256`), ≤
128 KiB: "You are reviewing a change for the owner as member `<member>` of
committee `<name>`. Review only: do not modify, create or delete files, and do
not run commands that change state. Cite file:line." + omitted paths with
reasons + "partial review" when `complete` is false + committee instructions +
the question.

The receiver prepends a **workspace note** (≤ 4 KiB, stored in the job
record): checkout mode ("a checkout of `<repo_id>` at `<base>` with the change
staged; `git diff --cached` shows it") or packet-only mode ("the change is
described in ./packet; this is not a checkout", with the reason).

### 6.9 Bundle

`bundle.md` (≤ 1 MiB): a header (committee, version, scope, base/included
tree, close time, late members), then one section per member —
`## codex-gmail@cachyos — done in 4m12s` / `— error` / `— skipped: <why>` /
`— unreachable` / `— late (still running)` — each result inline up to
`min(200 KiB, 896 KiB / members)`, truncated with a pointer to
`results/<n>.md`.

### 6.10 Entry points, origin and threads

**Protocol variables.** `serve` sets, for every agent execution, after preset
`env` (§5.1) and replacing any inherited value: `TINCAN_ROOM` (canonical room)
and `TINCAN_REQUEST_ID` (the request being executed). Both are derived afresh
per execution from the request itself; a variable with no value is removed,
never inherited from the host's own environment. There is no separate origin
variable and nothing new is persisted per request.

- `tincan mcp` without `--room` uses `TINCAN_ROOM` when set, before inferring
  from the working directory (a subdirectory room is found correctly).
- **Origin is the request's own identity.** A thread turn's request ID is
  phase 1's deterministic `<tid>-<mid>`. `tincan_review`/`tincan review`
  treat the call as thread-originated only if `TINCAN_REQUEST_ID` parses as
  `<tid>-<mid>` and, in `TINCAN_ROOM`'s thread `<tid>`, message `<mid>` is an
  agent turn with that request ID in state `running`. Anything else —
  including a later request of the same listener, or nested delegation through
  `tincan_send` — has origin `mcp`/`cli`: a request can never inherit another
  request's chain.
- Agent CLIs that start MCP servers with a filtered environment (Codex does
  unless configured) lose these variables; their reviews are `mcp`-origin:
  unbudgeted and unaffected by thread Stop. The docs say so.

**Thread journal additions.** A new message role `committee` (author = the
committee name) represents one review in a thread; its `Review` field names
the `review_id`, and it is *active* while `pending` or `running` (phase 1's
Stop barrier waits for active messages). Member results are messages with role
`review`, author `<committee>/<member>`, `Review` = `<review_id>/<n>`; they
never trigger mentions. Chain `reserve` events gain an optional `key`:
replaying a keyed reservation already in the journal is a no-op; unkeyed
(phase 1) reservations still count once each.

**`@committee` mentions** (owner messages and agent handoffs).
`@<committee>` is resolved after presets and before hosted listeners. In the
same transaction phase 1 uses to plan turns — the owner's post, or an agent's
handoffs marker — the dispatcher appends the committee message, its intent
(request ID `<tid>-<mid>`, hence the `review_id`) and `pending` state; for an
agent handoff it also appends one keyed reservation `review:<review_id>:<n>`
per member, or, if the chain cannot afford them all, one `suggested` message
for the whole committee and nothing else. On the next pass the dispatcher
builds the review (§6.3 steps 2, 3 and 5; the origin is already committed) — scope `uncommitted` in a Git room, else
`none`; the question is the mentioning message plus the transcript,
truncated oldest-first to fit 64 KiB — and marks the message `running`. Build
errors (refused scope, unknown member) mark it `error` with the reason.

**`tincan_review` from a thread turn.** The entry point stages the review
(§6.3 step 3), then in one thread transaction checks that the thread is open,
the chain is not stopped and the budget covers every member, and appends the
committee message (`Review` = `review_id`, `ReplyTo` the calling turn, in its
chain), its keyed reservations and `pending` — unless a committee message for
that review already exists. It then publishes (§6.3 step 5). A refused
transaction removes the staging directory and returns the reason ("chain
budget exhausted: 3 needed, 1 left"). If the entry point dies after the
transaction, the coordinator publishes the `READY` staging directory; a
committee message whose staging and review are both missing after ten
minutes becomes `error` ("review input was lost").

**Results in the thread.** When a member's result is recorded, the coordinator
appends the member's `review` message (skipped if one with that `Review`
already exists), then sets `posted`. When the review closes or is cancelled,
the committee message's state follows (`done`, `cancelled`, `error` when every
member failed). Nothing is appended once the thread is not open; `posted` is
then set without appending.

**Stop and archive.** `finishStop` treats an active committee message like an
agent turn: it persists `cancel_requested` in `review.json` (thread lock, then
review lock), marks the message `cancelled`, and does not wait for remote
acknowledgements — the review keeps cancelling on its own and its card shows
"cancelling n members". New committee messages and reservations are refused on
stopped chains and non-open threads. Archived threads are not reconciled, but
their reviews are, because reviews reconcile from `.tincan/reviews`.

**MCP** (additive):
- `tincan_review {committee, question, scope?, request_id?}` →
  `{review_id, members: [{member, state}]}`.
- `tincan_review_wait {review_id, timeout_seconds 0–30}` →
  `{status, closed, settled, members: [{member, state, late}], bundle?}`;
  `bundle` inline when ≤ 256 KiB, else its path. Room-scoped.
- `tincan_review_cancel {review_id}`.

**CLI**: `tincan review --committee X (--question <text> | --question-file
<f>) [--scope …] [--request-id id] [--wait]`; `tincan review --wait <id>`
reattaches; `tincan review --cancel <id>`. `--wait` exits 3 when the review
closed with late or unreachable members.

**UI**: Committees page (members per machine from each machine's
`api/presets`, with quota and warnings; cache age on the peer); review cards
in threads; reviews in Activity.

## 7. Failure handling

| Situation | Behaviour |
|---|---|
| No coordinator (stale `web.json`) | MCP/CLI fail clearly; nothing written. |
| Unknown committee / no definitions | Error naming the committee and cache age. |
| Conflicting request key or job identity | Conflict error / 409 (permanent member error). |
| Peer unreachable | Submit retries until the deadline, then `unreachable` ("outcome unknown"); collection, acks and cancels stop at their `expires_at`-derived bounds (§6.4). |
| Lost submit response | Member stays `submitting`; identical retries; a final `GET` adopts an existing job. |
| Job launch fails on the member machine | Saved request cancelled; member `error`, never retried. |
| Entry point dies after the thread transaction | Coordinator publishes the `READY` staging; otherwise the committee message errors after ten minutes. |
| Thread stopped or archived | Committee messages cancelled at once; the review keeps cancelling remote members on its own. |
| No candidate / missing objects / partial clone | Packet-only mode, stated in the workspace note. |
| Patch validation or apply fails, tree mismatch | Clone removed; packet-only mode. |
| Unmerged index or sparse checkout | Request refused with a clear error. |
| Member preset missing or relative executable | Job rejected; member `error`. |
| Member past the deadline | `late`; recorded if done before `expires_at`, else `expired`. |
| All members skipped/unreachable | Closes with "no coverage". |
| Coordinator crash at any step | Next reconcile continues; nothing runs twice. |

## 8. Quotas and skipping

- Display: the member picker and review cards show each member's quota.
- `skip_exhausted` defaults to **false**. When true, the requester asks the
  member's machine `GET api/presets/{preset}/quota-status` (local call for
  local members) at publication. That machine decides, and answers
  `{exhausted: true, reset_at, window}` only if: exactly one `quotas.json`
  entry explicitly maps the preset — counted over the whole configuration,
  including entries whose cache file is absent (phase 1's `Load` enumerates
  caches, so this needs a config-first count); a malformed `quotas.json` is
  non-fatal for display and always prevents skipping (none or several → not exhausted, with the
  reason); the entry declares `blocking`; its state is `ok` (phase 1 truth
  table: fresh, successful, valid); and the blocking window's percentage is
  ≥ 100 with a reset time in the future. `either` means the weekly or the
  short window qualifies, each judged separately. Anything else — including an
  unreachable machine — is not exhausted, and the member runs.
- The skip note names the window and reset time.

## 9. Security

- Only the owner's devices reach any endpoint; jobs arrive only through
  `tincan web`. Exception inherited from phase 1: where `tincan web` listens
  on loopback TCP (the macOS deployment), other local OS users can forge the
  identity header (`docs/web.md`); the same caveat covers the machine link.
- Reviewers run with the owner's OS permissions. The dissociated clone keeps
  ordinary edits away from the owner's tree and Git metadata; read-only
  presets stop well-behaved CLIs from writing; executables are pinned outside
  reviewed content. A reviewer with a permission bypass, or a CLI that honours
  repository-controlled configuration, **can** still act outside the workspace;
  the docs say so and recommend read-only presets with project configuration
  disabled, and the UI warns on bypass presets.
- Packets omit secret-looking files and never include remote URLs; they still
  contain source code, travel over the tailnet (HTTPS through
  `tailscale serve`), are stored 0600 under `.tincan/` and the state
  directory, and reach reviewers' model providers like any prompt.
- Path policy and exclusive no-follow creation for every materialized path.
- `env` values never appear in process arguments or public views.

## 10. Phase 2c — synthesis delivery

For reviews with a verified thread origin whose requester is an agent (an
agent's reply mentioned the committee, or a thread agent called
`tincan_review`):

- When the review closes, the coordinator (holding the thread lock, then the
  review lock) first writes the exact synthesis prompt with phase 1's
  `WritePrompt` under the turn's request ID, then appends, in one thread
  journal transaction: a `synthesis` reservation keyed `synthesis:<review_id>`,
  an agent message with the deterministic ID `m` + first 12 hex of
  SHA-256(`synthesis:<review_id>`) and `Review` = `<review_id>`, its intent,
  and its pending state — unless that message already exists. It then sets
  `delivered` in `review.json`. A crash between the two is repaired on the next
  pass because the message ID is deterministic; phase 1's `submit` sends the
  stored prompt unchanged.
- If the chain is stopped, the thread archived, or the budget exhausted, it
  appends a `suggested` message ("Send committee results to claude.t7f2")
  with the same deterministic ID instead.
- The prompt says the reviews are complete, points to
  `<room>/.tincan/reviews/<id>/bundle.md`, and includes the bundle inline up
  to 128 KiB. The turn queues behind the listener's other work.
- Late results never create a second synthesis turn. Owner-mentioned
  committees and non-thread origins get no automatic synthesis.
- **Retry and Send.** Retrying a failed synthesis turn reuses its stored
  prompt (copied to the new request ID) instead of phase 1's rebuild from the
  trigger; a never-submitted one is retried in place, and only a fresh retry
  gets the single-use `KindRetry` marker. Sending a synthesis suggestion plans
  the synthesis turn itself (bundle and requester from the review), not a new
  owner message with the suggestion's text.

## 11. Testing

Phase 2a
- Env key validation (bad keys, scrubbed names, `PATH`, `TINCAN_*`), `~`
  expansion, precedence (`env_unset`, `env`, protocol variables last).
- `env` values absent from process arguments of a live host and from
  `presets`/`status`; private config file path, permissions and removal;
  explicit-settings comparison uses the private file.
- Provider identity cases; account change changes the fingerprint.

Phase 2b
- Packet builder on fixture repositories: each scope; unborn HEAD; root and
  merge commits; renames (both endpoints valid / one invalid); deletions; mode
  changes; untracked, ignored, and tracked-but-ignored files; binaries;
  symlinks and submodules omitted; secret paths omitted; oversize patch →
  incomplete; empty patch; subdirectory room; unmerged and sparse refusals.
- Reconstruction: checkout mode's `write-tree` equals `included_tree`;
  empty-base checkout; partial-clone candidate skipped; missing objects with
  lazy fetch disabled → packet-only; clone survives `git gc --prune=now` in the
  candidate; failures leave no directory; candidate order.
- Path policy: `..`, absolute, `.git`/`.GIT`/`.tincan`, case-fold collisions,
  pre-existing symlink targets, platform-invalid names → rejected, nothing
  written outside the workspace.
- Executable pinning: a reviewed change adding `./reviewer` never runs it;
  relative-executable presets rejected as members.
- Publication: concurrent identical requests with one request ID → one
  review; differing → conflict; crash at each step (staged, `READY`, thread
  transaction, renamed) leaves nothing visible, a review the coordinator
  finishes publishing, or a complete review; stale staging removed; a stale
  `web.json` refuses without writing.
- Jobs: identical replay runs once; different identity 409; expired create 410;
  cancel-before-create tombstone refuses a later create; ack then replay never
  re-runs; cancel acknowledged only after termination.
- Coordinator crash at every step (reserved, submitting, submitted, collected,
  closed, bundle written, cancel requested) → exactly one execution per member
  (execution counter as in phase 1's fixture agent); closed-but-unsettled
  reviews keep collecting late results and acks.
- Two machines with the in-process peer harness: offline peer → retries →
  `unreachable`; lost response → `submitting` → final `GET` adopts the one
  job; permanent 4xx → `error` without retry; cancel retries until acked or
  moot; ack retries until acked or moot; every obligation stops at its bound.
- Outbound peer client passes the peer's Host, forwarded-Host, owner, Funnel
  and CSRF checks.
- Budget: keyed reservations are not double-charged on replay; a committee
  larger than the remaining budget becomes one suggestion.
- Origin: `TINCAN_ROOM` picks the subdirectory room; a request ID that is not
  a running thread turn in that room yields `mcp` origin; variables are not
  inherited by the next request; Stop cancels a thread-origin review without
  waiting for remote acks; archived threads' reviews keep reconciling.
- Quota skip: every condition that must prevent a skip; ambiguous mappings.

Phase 2c
- Exactly one synthesis turn per closed agent-originated review across crashes
  between journal append and `delivered`; gated by Stop, archive and budget;
  late results do not trigger a second turn.

## 12. Dependencies on phase 1

Phase 1 (spec §15) provides: the two-way link; quota entries with `explicit`
and `blocking`; `.tincan/dispatcher.lock` taken for every registered room with
a `.tincan/` directory; registry exclusion of `<state>/reviews/` at every
insertion point; one-line atomic journal transactions with journal-authoritative
meta; deterministic turn request IDs; `WritePrompt`/`submit`; the Stop/archive
barrier over active messages; per-room passes on the owning dispatcher.
Phase 2b adds: keyed reservations, the `committee` and `review` message roles
and the `Review` event field; review reconciliation in the room pass;
`<state>/web.json`; protocol variables in `serve`; the outbound peer client;
`api/presets`; `committees`/`reviews` notes. Quota skipping needs a
config-first count of `quotas.json` mappings (§8), which phase 1's `Load`
does not provide.

## 13. Review record

### Round 1 (draft, 16 findings)

| # | Finding | Resolution |
|---|---|---|
| 1 | Worktrees don't isolate | Dissociated independent clone, read-only presets, residual risk (§6.6, §9). |
| 2 | Path validation, secrets | Path policy, secret omission, credential-free identity (§6.5, §6.6). |
| 3 | Packet can describe the wrong change | Scope table, seeded snapshot, included tree, completeness (§6.5). |
| 4 | IDs insufficient for idempotency | Publication protocol, job identity, tombstones (§6.3, §6.6). |
| 5 | Deadline vs cancellation | Close once, late flag, expiry, cancel protocol (§6.4, §6.6). |
| 6 | Workspace lifecycle | Job records outside workspaces, ack → stop → remove (§6.6). |
| 7 | "One hop" vs budget | Every member and synthesis counts; keyed reservations (§6.10, §10). |
| 8 | Bundle delivery undefined | Phase 2c with deterministic delivery (§10). |
| 9 | Env values in argv | Private config file, key rules, redaction (§5). |
| 10 | Alias sessions | Provider identity and fingerprint (§5.3). |
| 11 | Size limits | Raw and encoded limits throughout (§6.5–6.9). |
| 12 | CLI/MCP coordination | Single coordinator, entry-point publication, clear errors (§6.3). |
| 13 | Remote matching | `repo_id` with ports, candidate order, object checks, no fetch (§6.6). |
| 14 | Cached definitions | Versions, snapshots, cache age, reserved names (§6.1). |
| 15 | `skip_exhausted` default | Default false; machine-local strict rules (§8). |
| 16 | Split further | Phases 2a/2b/2c; quotas in phase 1 (§4). |

### Round 2 (this spec, 16 findings)

| # | Finding | Resolution |
|---|---|---|
| 1 | Cancellation missed uncertain submissions and closed reviews | `submitting` state, cancel until settled, ack after termination (§6.4, §6.6). |
| 2 | No publication protocol | Key locks, staging + atomic rename, immutable inputs built by the entry point, job locks (§6.3, §6.6). |
| 3 | Closed reviews lost outstanding work | `settled` separate from `closed`; reconcile until settled (§6.2, §6.4). |
| 4 | Cross-store atomicity | Keyed reservations, deterministic delivery IDs, lock order, one-line journal transactions (§6.10, §10, phase 1 §15.3). |
| 5 | Origin propagation | `TINCAN_ROOM`/`TINCAN_ORIGIN` after preset env, verified against the journal, MCP room from `TINCAN_ROOM` (§6.10). |
| 6 | Snapshot could manufacture deletions | Index seeded from the base, unmerged/sparse refused, weaker consistency stated (§6.5). |
| 7 | Reviewed code could become the executable | Executables pinned outside the workspace; bare/absolute only; project-config risk documented (§6.6). |
| 8 | Reconstruction vs verification | `apply --index`, `included_tree`, empty base, empty patch (§6.5, §6.6). |
| 9 | Shared clone durability, partial clones, ports | Repack + drop alternates, skip partial clones, lazy fetch off, ports kept (§6.6). |
| 10 | Git-mode materialization | Unified path policy incl. rename endpoints, case folding, no symlinks/gitlinks (§6.5, §6.6). |
| 11 | Expiry and tombstones | Absolute expiry in identity, 410 on expired, remaining-time timeout, dedup guarantee argued (§6.6). |
| 12 | Payload and prompt contracts | Raw vs encoded limits, per-member bundle cap, mode-neutral hashed prompt + receiver note (§6.5, §6.8, §6.9). |
| 13 | Quota state undefined | Truth table, skew bound, equal-timestamp failure, short-window expiry (phase 1 §15.2). |
| 14 | Remote skipping lacked information | Machine-local `quota-status` decision; explicit single mapping required (§8). |
| 15 | Preset file collided with listener state | `hosts/config/<owner>.json`; private comparison (§5.2). |
| 16 | Phase 1 integration | Dispatcher lock for all `.tincan` rooms; exclusion at every insertion point (phase 1 §15.3). |

### Round 3 (against the phase 1 code at `96d7d76`, 11 findings)

Phase 2a findings (#6 partly, #11) were resolved in §5 and shipped. The phase
2b findings reopened round 2 #1–#5 and #7; this revision resolves them.

| # | Finding | Resolution |
|---|---|---|
| 1 | Crash between directory rename and `keys/<request_id>` duplicates a review | Key records removed: `review_id` is derived from the request ID, so publication is idempotent by construction; `READY` + rename are the only commit points (§6.2, §6.3). |
| 2 | Journal reservation, review publication and handoff marker are not one transaction | Committee message + intent + keyed reservations are appended in the same thread transaction as the post or handoffs marker; the review is then built/published idempotently like phase 1's `submit`; agent calls stage first, commit in the thread, then publish, and the coordinator finishes a `READY` staging (§6.10). |
| 3 | Origin lives only in runner variables | Origin is the executing request's own identity: `TINCAN_ROOM`/`TINCAN_REQUEST_ID` are derived per execution, and only a running `<tid>-<mid>` turn verified in the journal is a thread origin; nothing is inherited, nothing new persisted; filtered MCP environments degrade to `mcp` origin (§6.10). |
| 4 | Stop/archive have no review barrier | Committee messages are active messages; `finishStop` persists `cancel_requested` and cancels the message without waiting for remote acks; stopped chains refuse reservations; reviews reconcile from `.tincan/reviews`, so archive does not stop them (§6.10). |
| 5 | Unsettled states without transitions | Four per-member obligations (submit, collect, ack, cancel), each idempotent with a bound derived from `expires_at`; response classification (permanent 4xx, 410, transient); final `GET` adopts an existing job; job-side launch failure cancels the saved request (§6.4, §6.6). |
| 6 | Absolute executables are re-resolved with fallback | Strict pinned execution (§6.6). |
| 7 | Coordinator probe misreads first-use rooms; `filelock.Try` writes | `<state>/web.json` heartbeat; entry points register the room and create `.tincan/`; no external lock probing; reviews reconcile on the owning dispatcher in the room pass (§6.3, §6.4). |
| 8 | Machine link has no outbound RPC contract | Outbound peer client to the peer's public URL, `X-Tincan-Request`, no forwarded/identity headers, timeouts and body caps; loopback-TCP caveat in §9 (§6.7). |
| 9 | No machine-level preset catalogue; SSE doesn't cover committees/reviews | `api/presets` catalogue; `committees`/`reviews` notes; peer committee cache by polling plus on-demand refresh; no daemon-to-daemon SSE (§6.7). |
| 10 | Synthesis doesn't fit Send/Retry | Stored exact prompt before the intent; synthesis-specific Retry and Send; `KindRetry` only for fresh retries (§10). |
| 11 | Foreground serve, fingerprint migration, quota ambiguity count | Resolved in §5.2, §5.3, §8 and shipped. |
