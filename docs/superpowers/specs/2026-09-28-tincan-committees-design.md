# tincan committees — Design Spec (phase 2)

Status: design discussed with the owner on 2026-09-28. Codex (gpt-6-astra,
xhigh) reviewed a draft (16 findings) and this spec (16 findings); both rounds
are resolved below (§13). The owner approved the key decisions (read-only
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
  `<room>/.tincan/hosts/config/<owner>.json`, where `<owner>` is the host
  lifetime identity `Up` already generates. The directory is 0700, the file
  0600, written atomically. `Up` passes `--resolved-preset-file <path>`.
- The file lives as long as that host lifetime; `Down` and a clean `serve`
  exit remove it; stale files whose owner is not alive are removed by `Up`.
- "Explicit settings must match the running configuration" compares against
  this private file, never against redacted views.
- Public views — `tincan presets`, `tincan_presets`, host state, `status` —
  show `env` keys only, never values.

### 5.3 Session provider identity

- Provider = `provider` if set, else the base name of `exec[0]` (without
  `.exe`) when it is a supported adapter, else none. The preset label no longer
  decides it, so `claude-personal` keeps persistent sessions.
- Explicit `session: "stateless"` wins; wrappers stay stateless unless they set
  `provider`.
- The session fingerprint includes provider, executable and a hash of the
  sorted `env`/`env_unset` configuration, so changing a listener's account
  never resumes another account's conversation.
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
  configured peer. Each must resolve on its machine at save time. The preset's
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
  60 s and on `committees` notes; keep the last valid copy with `fetched_at`;
  the UI shows cache age.
- Reviews start from the local copy (hub: authoritative; peer: cache). Members
  on unreachable machines become `unreachable` (§6.4) instead of blocking.
  Edits go to the hub and fail clearly when it is down.
- Every review stores a snapshot of the definition it used.
- Amends phase 1's non-goal "a central store" for committee definitions only.

### 6.2 Reviews in the room

```
<room>/.tincan/reviews/
  keys/<request_id>          → review_id (idempotency key record)
  keys/<request_id>.lock
  staging/<review_id>/       being assembled (never read by the coordinator)
  <review_id>/
    input.json               immutable: committee snapshot, question, scope, origin, created, deadline
    packet/                  immutable: manifest.json, diff.patch, files/…, question.md
    prompts/<n>.txt          immutable: coordinator prompt for member n
    review.json              mutable state, rewritten atomically under ./lock
    results/<n>.md           member results
    bundle.md                written once at close
    lock
```

- `review_id` = `rv-` + 12 hex.
- `review.json`: `status` (`running|closed|cancelled`), `settled` (bool),
  `cancel_requested`, `closed_at`, `delivered` (phase 2c), and `members[]`:
  `member`, `index`, `job_id` (`<review_id>-<index>`), `state`
  (`planned|submitting|submitted|running|done|error|skipped|unreachable|cancelled|expired`),
  `late`, `cancel_state` (`none|requested|acked`), `result_acked`, `started`,
  `finished`, `note`, `reservation` (budget key, §6.10).

### 6.3 Publication (entry points)

MCP and CLI entry points (and the web thread path through the coordinator) run
this, in the requesting room, before returning:

1. Check a coordinator exists: a non-blocking try of `.tincan/dispatcher.lock`
   that **succeeds** means none; release it and fail with "tincan web is not
   running on this machine; start it (see docs/web.md)". Nothing is written.
2. With a `request_id`: take `keys/<request_id>.lock` for the rest of the
   procedure. If `keys/<request_id>` exists, compare its review's `input.json`
   (committee name, question, scope): identical → return that review; different
   → conflict error.
3. Build everything immutable in `staging/<review_id>/`: `input.json`, the
   packet (§6.5), and every member prompt (§6.8), fsyncing files.
4. Write the initial `review.json` (`status: running`, every member `planned`
   or `skipped` per §8) in staging.
5. Publish: rename `staging/<review_id>` to `<review_id>` (atomic within the
   filesystem), then write `keys/<request_id>` if any, then release the key
   lock.

Recovery: the coordinator ignores `staging/`; staging directories older than
one hour are deleted. A key record whose review directory is missing is
deleted. Because the packet is built by the entry point from the room at
request time, the coordinator never rebuilds it.

### 6.4 Coordination and lifecycle

One coordinator per room: the process holding `.tincan/dispatcher.lock`
(`tincan web`). Its reconcile loop (every 2 s) processes every review that is
not `settled`. Every step below is idempotent.

1. **Budget** (thread origins only, §6.10): reserve one execution per
   non-skipped member before any submission, using deterministic reservation
   keys; a review whose members cannot all be reserved is cancelled with a
   note (the thread path checks this before publishing, §6.10).
2. **Submit**: for each `planned` member: set `submitting` (persisted), then
   create the job locally or `POST <peer>/api/review-jobs`. A success response
   → `submitted`. A failure or lost response leaves `submitting`; retries
   repeat the same job identity (§6.6), every 15 s, until the deadline, after
   which an unsent member is `unreachable` only if a final `GET` confirms the
   peer has no such job.
3. **Collect**: for `submitted`/`running` members, read the job (local record
   or `GET …/review-jobs/<job_id>`) every 5 s and on notes; on a terminal job
   write `results/<n>.md`, set the member's state, then `POST …/ack`; set
   `result_acked` when the ack succeeds.
4. **Close** once: when every member is terminal or the deadline passes, set
   `status: closed`, `closed_at`, and mark running members `late: true`. The
   bundle (§6.9) is written from the state at close, before or in the same
   step as setting `closed` (a crash in between rewrites the same bundle).
   Late members keep running until their job's expiry; their results are
   recorded and shown but the bundle and any delivery are not redone.
5. **Cancel** (allowed until settled, including after close): persist
   `cancel_requested` first; `planned` members become `cancelled` unsent;
   every member in `submitting`, `submitted` or `running` gets
   `POST …/cancel` (a job that does not exist gets a tombstone, §6.6), retried
   every 15 s until acknowledged (`cancel_state: acked`). The review is
   `cancelled` (if not already closed) when no member is still running.
6. **Settle**: a review is `settled` when every member is terminal, every
   terminal remote result is acknowledged or its job is gone, every requested
   cancel is acknowledged, and (phase 2c) delivery is done or not applicable.
   Settled reviews are no longer reconciled.

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
absolute paths), and the resolved absolute path is used for the run.
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

**Cancel acknowledgement**: under the job lock, record the tombstone (launch is
now impossible), cancel the request and ask the host to stop it, and
acknowledge only once the request is terminal or the host has exited.

**Cleanup** after `result_acked` or an acknowledged cancel: stop the listener
(`host.Down`, wait for exit), remove the workspace, keep the tombstone (result
removed after 7 days). Active jobs are never swept by age; an unacknowledged
terminal job is cleaned up 7 days after `expires_at`.

### 6.7 Machine-link API (added to phase 1's `tincan web`)

All routes use phase 1's owner check and CSRF rules, with endpoint-specific
body limits.

| Method | Path | Limit | Purpose |
|---|---|---|---|
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

### 6.10 Entry points and origin

**Protocol variables.** Every hosted agent run gets, after preset `env` (§5.1):
`TINCAN_ROOM` (canonical room), `TINCAN_REQUEST_ID`, and `TINCAN_ORIGIN`
(`thread:<room id>:<tid>:<message id>:<chain>` for thread turns, otherwise the
value inherited from the request that caused this run, or absent).

- `tincan mcp` without `--room` uses `TINCAN_ROOM` when set, before inferring
  from the working directory (a subdirectory room is found correctly).
- `tincan_review` and `tincan review` read `TINCAN_ORIGIN`; a thread origin is
  accepted only after verifying that the thread journal in `TINCAN_ROOM` holds
  an intent with that request ID (`TINCAN_REQUEST_ID`) for that message and
  chain; otherwise the origin is `mcp`/`cli`.
- A thread origin makes the review subject to the thread's budget, Stop and
  archive.

**Budget reservation** (thread origins): the chain journal records
`reserve` events keyed `review:<review_id>:<n>` (and `synthesis:<review_id>`
in 2c). Appending a keyed reservation that already exists is a no-op, so
replays never double-charge. Lock order is always thread lock → review lock.
For `@committee` in a thread, the thread path reserves every non-skipped
member in the same journal transaction that records the review request; if
the chain cannot reserve them all, it posts one `suggested` message for the
whole committee instead and publishes nothing.

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

**Web threads** (2b):
- `@<committee>` is a mention target, resolved after presets and before hosted
  listeners.
- Scope `uncommitted` in a Git room, else `none`. Question: the mentioning
  message plus the thread transcript, truncated oldest-first to fit 64 KiB.
- Member reviews are posted as they arrive, role `review` (author
  `<committee>/<member>`); review messages never trigger mentions.
- Stop and archive cancel reviews originated from the thread (§6.4 step 5).
- In 2b, reviews stay in the thread; synthesis is 2c.

**UI**: Committees page (members per machine with quota and warnings; cache
age on the peer); review cards in threads; reviews in Activity.

## 7. Failure handling

| Situation | Behaviour |
|---|---|
| No coordinator | MCP/CLI fail clearly; nothing written. |
| Unknown committee / no definitions | Error naming the committee and cache age. |
| Conflicting request key or job identity | Conflict error / 409. |
| Peer unreachable | Submit retries; `unreachable` after the deadline when the peer confirms no job; cancel retries until acknowledged. |
| Lost submit response | Member stays `submitting`; identical retries; cancel covers it. |
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
  entry explicitly maps the preset (none or several → not exhausted, with the
  reason); the entry declares `blocking`; its state is `ok` (phase 1 truth
  table: fresh, successful, valid); and the blocking window's percentage is
  ≥ 100 with a reset time in the future. `either` means the weekly or the
  short window qualifies, each judged separately. Anything else — including an
  unreachable machine — is not exhausted, and the member runs.
- The skip note names the window and reset time.

## 9. Security

- Only the owner's devices reach any endpoint; jobs arrive only through
  `tincan web`.
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
  review lock) appends, in one thread journal transaction: a `synthesis`
  reservation keyed `synthesis:<review_id>`, an agent message with the
  deterministic ID `m` + first 12 hex of SHA-256(`synthesis:<review_id>`), its
  intent, and its pending state — unless that message already exists. It then
  sets `delivered` in `review.json`. A crash between the two is repaired on the
  next pass because the message ID is deterministic.
- If the chain is stopped, the thread archived, or the budget exhausted, it
  appends a `suggested` message ("Send committee results to claude.t7f2")
  with the same deterministic ID instead.
- The prompt says the reviews are complete, points to
  `<room>/.tincan/reviews/<id>/bundle.md`, and includes the bundle inline up
  to 128 KiB. The turn queues behind the listener's other work.
- Late results never create a second synthesis turn. Owner-mentioned
  committees and non-thread origins get no automatic synthesis.

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
- Publication: concurrent identical requests with one key → one review;
  differing → conflict; crash at each publication step leaves either nothing
  visible or a complete review; stale staging removed.
- Jobs: identical replay runs once; different identity 409; expired create 410;
  cancel-before-create tombstone refuses a later create; ack then replay never
  re-runs; cancel acknowledged only after termination.
- Coordinator crash at every step (reserved, submitting, submitted, collected,
  closed, bundle written, cancel requested) → exactly one execution per member
  (execution counter as in phase 1's fixture agent); closed-but-unsettled
  reviews keep collecting late results and acks.
- Two machines with the in-process peer harness: offline peer → retries →
  `unreachable`; lost response → `submitting` → one job; cancel retries until
  acknowledged.
- Budget: keyed reservations are not double-charged on replay; a committee
  larger than the remaining budget becomes one suggestion.
- Origin: `TINCAN_ROOM` picks the subdirectory room; a forged
  `TINCAN_ORIGIN` without a matching journal intent is ignored; Stop cancels a
  thread-origin review.
- Quota skip: every condition that must prevent a skip; ambiguous mappings.

Phase 2c
- Exactly one synthesis turn per closed agent-originated review across crashes
  between journal append and `delivered`; gated by Stop, archive and budget;
  late results do not trigger a second turn.

## 12. Dependencies on phase 1

Phase 1 (spec §15) provides: the two-way link; quota entries with `explicit`
and `blocking`; `.tincan/dispatcher.lock` taken for every registered room with
a `.tincan/` directory; registry exclusion of `<state>/reviews/` at every
insertion point; one-line atomic journal transactions. Phase 2 adds keyed
reservation events and the `review` message role to the thread journal, and
protocol variables to the runner.

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
