# tincan committees — Design Spec (phase 2)

Status: design discussed with the owner on 2026-09-28; a draft was reviewed by
Codex (gpt-6-astra, xhigh) and its 16 findings are resolved below (§13). The
owner approved the resulting decisions (read-only reviewer presets and
independent clones with the residual risk stated; committee executions count
against the thread budget; quota skipping off by default; phases 2a/2b/2c).
Awaits the owner's review of this document.

Builds on phase 1, `docs/superpowers/specs/2026-09-28-tincan-web-chat-design.md`
(the web chat, including its amendments for a two-way machine link and quota
panels, §12 of this document).

## 1. Problem

The owner routinely asks one agent to have its work reviewed by others ("have
it reviewed by codex"). Today that means hand-building each request, picking
accounts, and repeating it per reviewer and per machine. The owner wants named
groups of reviewers — *committees* — whose members are specific agents on
specific accounts on specific machines (e.g. Codex on the business account on
cachyos, Claude on the personal account on the Mac, MiMo on cachyos, Grok on
the Mac), and to say "get this reviewed by committee X" from anywhere.

## 2. Goals / Non-goals

Goals
- Account profiles: one CLI, several accounts, selected per preset.
- Committees: named, owner-defined groups of `preset@machine` members.
- One durable review primitive used by MCP, the CLI and web threads.
- Members on another machine than the code review a faithful packet of the
  change, in a reconstructed checkout when their machine has the repository.
- Every member's review is returned; the requester synthesizes.
- Reviews survive client exits, web restarts and peer outages without running
  any member twice.
- No change to existing CLI/MCP contracts; new tools are additive.

Non-goals
- OS-level sandboxing of reviewers (bubblewrap, sandbox-exec). Isolation is by
  independent clone plus read-only presets, with the residual risk documented
  (§9).
- Automatic fetching from remotes, automatic push, or code transfer beyond the
  review packet.
- Automatic quota-based skipping by default (opt-in only, §8).
- Committees spanning more than the configured peers; chained peers.
- Chair/summarizer members (the requester synthesizes).

## 3. Decisions

| Question | Decision |
|---|---|
| Code access across machines | Packet; reconstructed independent clone when the member's machine has the repository. |
| Entry points | MCP, CLI and `@committee` in web threads. |
| Results | All member reviews; the requester synthesizes. |
| Definitions | Edited in the web UI, stored on the hub, cached by the peer. |
| Transport | The `tincan web` machine link (owner identity + CSRF rules of phase 1 §9). |
| Reviewer isolation | Independent clone + read-only reviewer presets; residual risk documented. |
| Budget | Each member execution and each synthesis turn counts against the thread chain budget. |
| Quota skipping | `skip_exhausted` defaults to false; opt-in with strict rules. |
| Phasing | 2a account profiles → 2b durable reviews (MCP, CLI, `@committee`, reviews posted) → 2c automatic synthesis delivery in threads. |

## 4. Phasing

- **Phase 1** (separate spec/plan): web chat, two-way machine link, read-only
  quota panels.
- **Phase 2a — account profiles** (§5). Useful on its own for threads and MCP.
- **Phase 2b — committees and durable reviews** (§6, §7, §8): definitions,
  packets, member jobs, coordinator, MCP/CLI entry points, `@committee` in
  threads posting member reviews, UI.
- **Phase 2c — synthesis delivery** (§10): the review bundle is sent back to the
  thread agent that asked, as exactly one durable turn.

Each phase ships with its own plan and tests and leaves the system working.

## 5. Phase 2a — account profiles

### 5.1 Preset fields

`~/.config/tincan/agents.json` presets gain:

- `env` — map of environment variables for the agent process. Keys must match
  `^[A-Z_][A-Z0-9_]{0,63}$`; keys in the parent-session scrub list
  (`internal/host/env.go`) and `PATH` are rejected. Values are strings; a
  leading `~/` expands to the home directory at launch.
- `env_unset` — list of inherited variable names to remove (e.g.
  `ANTHROPIC_API_KEY`, so a profile's own login is used instead of an API key
  inherited from the shell).
- `provider` — optional explicit session provider (`claude`, `grok`, `agy`,
  `kimi`); see §5.3.

Example:

```json
"codex-gmail": {"exec": ["codex","exec","-s","read-only","--skip-git-repo-check","-o","{out}","-"],
                "stdin": "body", "reply": "file", "env": {"CODEX_HOME": "~/.codex-gmail"}},
"claude-personal": {"exec": ["claude","-p","{body}","--permission-mode","plan"],
                    "env": {"CLAUDE_CONFIG_DIR": "~/.claude-personal"}, "env_unset": ["ANTHROPIC_API_KEY"]}
```

Environment precedence for an agent run: the scrubbed tincan environment
(`agentEnv`), minus `env_unset`, plus `env`.

### 5.2 Keeping configuration off the command line

`host.Up` currently passes the whole resolved preset as `--resolved-preset
<json>`, which would expose `env` values in process listings. Instead `Up`
writes it to `<room>/.tincan/hosts/<name>.preset.json` (0600, atomic) and passes
`--resolved-preset-file <path>`; `serve` reads and deletes nothing (the file is
the host's configuration while it runs and is removed by `Down`). Public views —
`tincan presets`, `tincan_presets`, host state, `status` — show env **keys**
only, never values.

### 5.3 Session provider identity

- A preset's session provider is `provider` if set, else the base name of
  `exec[0]` (without `.exe`) when it is a supported adapter (`claude`, `grok`,
  `agy`, `kimi`), else none. The preset **label** no longer decides it, so
  `claude-personal` keeps persistent sessions.
- Explicit `session: "stateless"` still wins; wrappers whose executable is not
  a provider stay stateless unless they set `provider`.
- The session fingerprint includes the provider, the executable, and a hash of
  the sorted `env`/`env_unset` configuration, so switching a listener's account
  never resumes another account's conversation.
- `tincan_presets` reports `session_supported` from the provider, not the name.
- Codex remains stateless.

### 5.4 Quota mapping

Phase 1's `~/.config/tincan/quotas.json` maps quota entries to presets
(`"codex-gmail": {"label": "Codex gmail", "presets": ["codex-gmail"]}`). Phase 2
uses this mapping for the member picker and for §8.

## 6. Phase 2b — committees and reviews

### 6.1 Committee definitions

```json
{"name": "reviewers", "version": 7,
 "members": ["codex-gmail@cachyos", "claude-personal@macmini", "mimo@cachyos", "grok@macmini"],
 "deadline_minutes": 30, "instructions": "Focus on correctness and security; cite file:line.",
 "skip_exhausted": false}
```

- `name`: mention syntax (`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`); must not be
  `you`, a preset name on any known machine, or contain `.` (reserved for thread
  listeners).
- `members`: 1–8 unique `preset@machine`; `machine` is this machine's name or a
  configured peer. Each member must resolve on its machine at save time (same
  check as the thread agent picker, through the link for peers). A member whose
  preset lacks a read-only mode is accepted with a visible warning when its
  argv contains a known permission bypass (`--dangerously-skip-permissions`,
  `--always-approve`, `-s danger-full-access`, `--yolo`).
- `deadline_minutes`: 1–240, default 30. `instructions`: ≤ 8 KiB.
- `version` increments on every save.

Storage and sync:
- The hub keeps `committees.json` in the shared state directory
  (`$TINCAN_STATE_DIR` > `$XDG_STATE_HOME/tincan` > `%LocalAppData%\tincan` >
  `~/.local/state/tincan`), under a file lock with atomic replace.
- A peer runs `tincan web --committees-from <peer name>`. It fetches
  `api/committees` at start, every 60 s and on `committees` SSE notes, and keeps
  the last valid copy with its `fetched_at`. The UI shows the cache age.
- Starting a review uses the local copy (hub: authoritative; peer: cache).
  Members on an unreachable machine become `unreachable` (§6.7) rather than
  blocking the review. Edits go to the hub and fail clearly when it is down.
- Every review stores a snapshot of the definition it used, including
  `version`.
- This amends phase 1's non-goal "a central store": committee definitions
  (only) are stored centrally on the hub.

### 6.2 Reviews in the room

A review lives in the requesting room:

```
<room>/.tincan/reviews/<review_id>/
  review.json        state (below), rewritten atomically under ./lock
  packet/            manifest.json, diff.patch, files/…, question.md
  prompts/<n>.txt    exact prompt for member n
  results/<n>.md     full member result
  bundle.md          assembled when the review closes
  lock
```

- `review_id` = `rv-` + 12 hex. A request carrying an idempotency key
  (`request_id`, validated like tincan request IDs) is recorded in
  `<room>/.tincan/reviews/keys/<request_id>` → `review_id`; a repeat with the
  same key and the same (committee name, question, scope) returns the existing
  review; a different body is rejected as a conflict.
- `review.json`: `id`, `committee` (snapshot), `question_sha256`, `scope`,
  `origin` (see §6.10), `status` (`requested|running|closed|cancelled`),
  `cancel_requested`, `created`, `deadline`, `closed_at`, `packet_sha256`,
  `members[]` with `member`, `index`, `job_id` (`<review_id>-<index>`),
  `prompt_sha256`, `state`
  (`planned|submitted|running|done|error|skipped|unreachable|cancelled|expired`),
  `late`, `cancel_acked`, `result_acked`, `started`, `finished`, `note`.

### 6.3 One coordinator: `tincan web`

- Reviews are coordinated only by the process holding the room's dispatcher
  lock. Phase 1 moves that lock from `.tincan/threads/dispatcher.lock` to
  `.tincan/dispatcher.lock` (§12) so it covers threads and reviews.
- MCP and CLI entry points **write the review request into the room** (steps 1–2
  of §6.4 under the review lock) and return; they never contact another
  machine. Before writing they check that some process holds the dispatcher
  lock (a non-blocking try that succeeds means no coordinator); if none does,
  they fail with "tincan web is not running on this machine; start it (see
  docs/web.md)".
- The coordinator's reconcile loop (phase 1, every 2 s) advances every
  non-closed review; all steps are idempotent and safe to repeat after a crash.

### 6.4 Lifecycle

1. **Request**: validate committee (local copy), question (≤ 64 KiB), scope;
   create `review.json` with `status: requested` and the committee snapshot.
2. **Packet** (§6.5) written under `packet/`; `packet_sha256` recorded.
3. **Plan members**: for each member: quota rule (§8) may mark it `skipped`;
   budget reservation for thread-originated reviews (§6.10); write the exact
   prompt (§6.8) to `prompts/<n>.txt`, record `prompt_sha256`, state `planned`.
   All of this is persisted before any job is sent.
4. **Submit**: local members create a local job (§6.6) directly; remote members
   `POST <peer>/api/review-jobs` (§6.7). Success → `submitted`. Failures retry
   every 15 s until the deadline; at the deadline an unsent member becomes
   `unreachable`.
5. **Collect**: poll jobs (local records; remote `GET …/review-jobs/<job_id>`)
   every 5 s and on SSE notes; copy results to `results/<n>.md`, set the member
   state, then acknowledge (`…/ack`) so the member machine can clean up.
6. **Close**: when every member is terminal, or the deadline passes, set
   `status: closed`, `closed_at`, write `bundle.md` (§6.9). Members still
   running are marked `late: true`; their jobs keep running until execution
   expiry (§6.6) and their results are still recorded and shown, but **closing
   happens once**: late results never trigger another delivery (§10).
7. **Cancel** (any time before close): persist `cancel_requested: true` first;
   planned members become `cancelled` without being sent; submitted members get
   `POST …/cancel`, retried every 15 s until `cancel_acked`; the review becomes
   `cancelled` when every member is terminal or its cancel is acknowledged.
   The UI shows outstanding cancel acknowledgements.

### 6.5 Scopes and the packet

Repository root = `git rev-parse --show-toplevel` of the room; paths in the
packet are repository-relative. The room may be a subdirectory; the scope still
covers the whole repository.

| Scope | Base | Target |
|---|---|---|
| `none` | — | — (question only; no Git required) |
| `uncommitted` (default in a Git room) | `HEAD` (empty tree when HEAD is unborn) | working tree: tracked changes + untracked non-ignored files |
| `branch` | merge-base of `HEAD` and the default branch (`origin/HEAD`, else `main`, else `master`) | working tree, as above |
| `commit:<rev>` | first parent (empty tree for a root commit; merges use the first parent and say so) | `<rev>^{tree}` |
| `range:<a>..<b>` | `<a>` | `<b>` |

Working-tree targets are captured as one consistent snapshot: a temporary index
(`GIT_INDEX_FILE`) populated with `git add -A` from the repository root, then
`git write-tree`. This writes ordinary blob/tree objects to the repository's
object store (harmless, collected by `git gc`) and never touches the real index
or the working tree. Historical targets read only from commits. Non-Git rooms
allow only `none`.

The patch is `git diff --binary --find-renames <base> <target>` restricted to
included paths. The manifest lists every changed path with status
(added/modified/deleted/renamed/type-changed/submodule), and for omitted paths
the reason:

- **secret-looking** paths (`.env`, `.env.*`, `*.pem`, `*.key`, `*.p12`,
  `*.pfx`, `id_rsa*`, `id_ed25519*`, `.npmrc`, `.netrc`, `.pypirc`,
  `credentials*`, `*secret*`), always omitted;
- **submodule** changes (gitlink shown, contents not included);
- **size**: files whose post-change content exceeds 1 MiB.

Limits: patch ≤ 4 MiB, manifest ≤ 5 000 paths, question ≤ 64 KiB,
instructions ≤ 8 KiB. If the patch would exceed its limit, the packet is marked
`complete: false` with reason `patch too large`; the patch is **not** included
and the packet carries post-change contents of the first changed text files up
to 2 MiB total (listed in the manifest) — this is packet-only mode.
`complete: true` means the patch reproduces every included change. Omissions
are always listed and are stated in each member's prompt.

`repo_id` (§6.6) and the base/target commit IDs are included; remote URLs are
never included (credentials stripped anyway).

### 6.6 Member jobs (on the member's machine)

Job record: `<state>/reviews/jobs/<job_id>.json` (0600), `job_id` validated
with `envelope.ValidComponent`. Fields: `job_id`, `review_id`, `requester`
(machine name), `preset`, `packet_sha256`, `prompt_sha256`, `expires_at`,
`workspace`, `listener`, `request_id` (= `job_id`), `status`
(`accepted|running|done|error|cancelled|expired`), `result` (≤ 1 MiB),
`result_acked`, `tombstone` (true after ack/cancel), `created`, `updated`.

**Workspace** (`<state>/reviews/workspaces/<job_id>/`):
1. `repo_id` = credential-free normalized origin URL: lower-case host, path
   without `.git` and trailing `/`, user/password/port stripped, SSH and HTTPS
   forms unified (`git@github.com:c0ze/tincan.git` and
   `https://github.com/c0ze/tincan` → `github.com/c0ze/tincan`). No origin → no
   `repo_id`.
2. Candidates: registered rooms whose repository root has the same `repo_id`,
   deduplicated by repository root, ordered by registry `last_used` (newest
   first) then path. For each, verify `git cat-file -e <base>^{commit}`; the
   first that has it is used. No fetch is ever attempted.
3. Checkout mode (packet `complete` and a candidate found): `git clone --shared
   --no-checkout <candidate root> <workspace>`; `git -C <workspace> checkout
   --detach <base>`; `git -C <workspace> apply --binary packet/diff.patch`. The
   clone has its own `.git` (objects are read through alternates; nothing is
   written to the candidate). Any failure removes the directory entirely before
   falling back.
4. Packet-only mode (otherwise): a fresh directory with `packet/` materialized:
   `manifest.json`, `diff.patch` (if present), `question.md`, and `files/<path>`
   for included contents.
5. Materialization rules: every path must be relative, clean, without `..`,
   empty or `.` components, without `.git`/`.tincan` components, ≤ 1 024 bytes,
   and valid for the platform (`envelope.ValidComponent` per component).
   Files are created with exclusive, no-follow opens as regular files 0600 in
   directories the materializer created itself; nothing is ever written
   through an existing path.
6. Workspaces are excluded from the room registry: `rooms.Touch` ignores any
   path under `<state>/reviews/`.

**Execution**: the preset runs as a hosted listener named after the preset in
the workspace (as its room), stateless, with exec timeout until `expires_at`
(review deadline + 30 min). The executable is resolved against the workspace
before launch. The request ID is the `job_id`; the prompt must hash to
`prompt_sha256`.

**Idempotency and tombstones**: a job is identified by (`job_id`,
`packet_sha256`, `prompt_sha256`, `preset`). A repeat with the same identity
returns the current state; a different identity is a conflict (409). A job that
was cancelled or acknowledged keeps a tombstone record for 30 days and is never
re-run. A cancel for an unknown `job_id` creates a tombstone, so a late submit
after a cancel is refused.

**Cleanup** after `result_acked` or cancellation: stop the listener
(`host.Down`, waiting for exit), then remove the workspace, keeping the job
record as a tombstone (result removed after 7 days). Active jobs are never
swept by age; an unacknowledged terminal job is cleaned up 7 days after
`expires_at`.

### 6.7 Machine-link API (added to phase 1's `tincan web`)

All routes use phase 1's owner check and CSRF rules; request bodies have
endpoint-specific limits.

| Method | Path | Body limit | Purpose |
|---|---|---|---|
| GET | `api/committees` | — | Definitions + version (hub) or cache + age (peer). |
| PUT | `api/committees/{name}` | 16 KiB | Create/update (hub only). |
| DELETE | `api/committees/{name}` | — | Delete (hub only). |
| POST | `api/review-jobs` | 8 MiB | Create a job `{job_id, review_id, requester, preset, packet, prompt, expires_at}`; idempotent (§6.6). |
| GET | `api/review-jobs/{job_id}` | — | Status and result (result ≤ 1 MiB). |
| POST | `api/review-jobs/{job_id}/ack` | 1 KiB | Acknowledge the result. |
| POST | `api/review-jobs/{job_id}/cancel` | 1 KiB | Cancel (creates a tombstone if unknown). |
| GET | `api/rooms/{rid}/reviews` | — | Reviews in a room (UI). |
| GET | `api/rooms/{rid}/reviews/{review_id}` | — | One review with member states and results. |
| POST | `api/rooms/{rid}/reviews` | 80 KiB | Start a review from the UI. |
| POST | `api/rooms/{rid}/reviews/{review_id}/cancel` | 1 KiB | Cancel. |

The owner check authenticates the calling **device owner**, not a specific
daemon: any process on the owner's untagged devices can create jobs, exactly as
it can use the chat (phase 1 §9). The `requester` field is informational and
never used for authorization.

### 6.8 Member prompt

In order, ≤ 256 KiB total:
1. "You are reviewing a change for the owner as member `<member>` of committee
   `<name>`. Review only: do not modify, create or delete files, and do not run
   commands that change state. Cite file:line."
2. Workspace description: checkout mode ("a checkout of `<repo_id>` at `<base>`
   with the change applied; `git diff` shows it") or packet-only mode ("the
   change is described in ./packet; this is not a checkout"), plus the list of
   omitted paths with reasons and, when `complete` is false, that the review is
   partial.
3. Committee instructions.
4. The question.

### 6.9 Bundle

`bundle.md`: a header (committee, version, scope, base/target, closed/late
status), then one section per member —
`## codex-gmail@cachyos — done in 4m12s` / `— error` / `— skipped: <why>` /
`— unreachable` / `— late (still running)` — with the result, each capped at
200 KiB inline with a pointer to `results/<n>.md`. Total ≤ 1 MiB.

### 6.10 Entry points

**Origin.** Every review records `origin`:
- `{kind: "thread", thread, message, chain, listener}` for `@committee` in a
  thread message, or for an MCP call made by a thread agent (below);
- `{kind: "mcp"}` or `{kind: "cli"}` otherwise.

Hosted runs set `TINCAN_REQUEST_ID=<request id>` in the agent's environment
(phase 2b change to the runner). A `tincan mcp` server started by that agent
inherits it; `tincan_review` then looks the request up (`<tid>-<mid>` for thread
turns) and records a thread origin. Thread origins make the review subject to
the thread's budget, Stop and archive.

**MCP** (additive tools; existing tools unchanged):
- `tincan_review {committee, question, scope?, request_id?}` →
  `{review_id, members: [{member, state}]}`.
- `tincan_review_wait {review_id, timeout_seconds 0–30}` →
  `{status, closed, members: [{member, state, late}], bundle?}`; `bundle` is the
  inline bundle when ≤ 256 KiB, else its path. Room-scoped: only reviews in the
  server's room are visible.
- `tincan_review_cancel {review_id}`.

**CLI**:
- `tincan review --committee X (--question <text> | --question-file <f>)
  [--scope …] [--request-id id] [--wait]` prints the review ID, or with
  `--wait` blocks and prints the bundle (exit 3 when the deadline closed it with
  late or unreachable members).
- `tincan review --wait <review_id>` reattaches; `tincan review --cancel
  <review_id>` cancels.

**Web threads** (phase 2b):
- `@<committee>` is a mention target (resolution after presets, before hosted
  listeners; committee names cannot collide with presets).
- The review starts in the thread's room with scope `uncommitted` (a Git room)
  or `none`; the question is the mentioning message plus the bounded thread
  transcript (phase 1 §6.4 rules).
- **Budget**: each non-skipped member reserves one execution in the chain. If
  the chain cannot reserve all of them, the committee mention becomes a
  `suggested` message instead (whole committee, never a partial committee).
- Each member review is posted to the thread when it arrives, role `review`
  (author `<committee>/<member>`). Review messages never trigger mentions.
- Stop and archive cancel reviews originated from the thread (§6.4 step 7).
- Automatic synthesis is phase 2c (§10); in 2b the reviews stay in the thread.

**UI**: Committees page (list, create, edit, delete; member picker per machine
with quota and read-only warnings; cache age on the peer); a review card in
threads with per-member progress; reviews listed in Activity.

## 7. Failure handling

| Situation | Behaviour |
|---|---|
| No coordinator running | MCP/CLI fail clearly (§6.3); nothing is written. |
| Committee unknown / definitions unavailable | Error naming the committee and the cache age. |
| Peer unreachable | Remote submits retry until the deadline, then `unreachable`; cancel retries until acknowledged. |
| Repository not on the member machine / base missing | Packet-only mode, stated in the prompt. |
| Patch fails to apply | Clone removed; packet-only mode. |
| Member preset missing on its machine | Job rejected; member `error` with the reason. |
| Member exceeds the deadline | `late`; result recorded if it finishes before `expires_at`, else `expired`. |
| All members skipped/unreachable | Review closes with "no coverage" in the bundle. |
| Coordinator crash at any step | Next reconcile continues from `review.json`; job identity prevents double execution. |
| Conflicting retry of a request key or job | Rejected (409 / conflict error). |

## 8. Quotas and skipping

- Display: the member picker and review cards show each member's quota from
  phase 1's normalized entries.
- `skip_exhausted` (default **false**). When true, a member is skipped only if
  all hold: `quotas.json` explicitly maps its preset to an entry and names the
  blocking window (`"blocking": "weekly" | "short" | "either"`); the entry's
  last attempt succeeded; it was fetched within 15 minutes; the blocking
  window's used percentage is ≥ 100 and its reset time is known and in the
  future. Unknown, stale, error or ambiguous readings never skip. The skip note
  names the reset time.

## 9. Security

- Only the owner's devices reach any endpoint (phase 1 §9); review jobs arrive
  only through `tincan web`.
- Reviewers run with the owner's OS permissions. The independent clone keeps
  ordinary edits away from the owner's working tree and Git metadata, and
  read-only presets stop well-behaved CLIs from writing at all, but a reviewer
  running with a permission bypass **can** still write elsewhere. The docs say
  so and recommend read-only reviewer presets; the UI warns on bypass presets.
- Packets exclude secret-looking files, strip credentials from remote URLs and
  never include remote URLs; they still contain source code and travel over the
  tailnet (HTTPS through `tailscale serve`), are stored 0600 under `.tincan/`
  (self-ignored) and the state directory, and are sent by reviewers to their
  model providers like any prompt.
- Materialization never follows or writes through existing paths (§6.6).
- `env` values never appear in process arguments or public views (§5.2).

## 10. Phase 2c — synthesis delivery

For reviews with a thread origin whose requester is an agent (an agent's reply
mentioned the committee, or a thread agent called `tincan_review`):

- When the review closes, the coordinator creates exactly one synthesis intent
  for the originating listener in the originating thread and chain, and marks
  `delivered` in `review.json` in the same step (thread journal event +
  review state, both idempotent by review ID).
- It reserves one execution in the chain; if the chain is stopped, the thread
  is archived, or the budget is exhausted, it posts a `suggested` message
  instead ("Send committee results to claude.t7f2").
- The prompt says the committee's reviews are complete, points to
  `<room>/.tincan/reviews/<id>/bundle.md`, and includes the bundle inline up to
  128 KiB. The turn queues behind any other work of that listener.
- Late results after close are shown on the review card but never create a
  second synthesis turn.
- Owner-mentioned committees and non-thread origins get no automatic synthesis.

## 11. Testing

Phase 2a
- Env validation (bad keys, scrubbed names, `PATH`), `~` expansion, precedence
  with `env_unset`; values absent from process arguments (read
  `/proc/<pid>/cmdline` or `ps` for a live host) and from `presets`/`status`
  output.
- Provider identity: `claude-personal` persistent; explicit stateless wins;
  wrapper stays stateless unless `provider` is set; account change changes the
  session fingerprint.

Phase 2b
- Packet builder on fixture repositories: each scope; unborn HEAD; root and
  merge commits; renames, deletions, mode changes; untracked and ignored files;
  binary files; submodule; secret-looking paths omitted; oversize patch →
  packet-only; subdirectory room.
- Reconstruction: checkout mode reproduces the target tree exactly (compare
  `git write-tree` of the applied clone with the packet's target tree);
  failure cleans up; candidate selection order; no fetch attempted.
- Materialization: `..`, absolute, `.git`, symlink pre-existing at a target,
  platform-invalid names → rejected, nothing written outside the workspace.
- Idempotency: repeated submit (same identity) runs once; conflicting identity
  409; cancel-before-create tombstone refuses a later submit; ack then retry
  never re-runs.
- Coordinator crash at each step (request written, packet written, prompts
  written, submitted, result collected, closed) → exactly one execution per
  member (execution counter as in phase 1's fixture agent).
- Deadline: late member recorded after close without a second close; expiry
  kills execution.
- Two machines: an in-process peer (`servedLike` harness from phase 1) runs
  jobs; peer offline → retries → `unreachable`; cancel retries until acked.
- Budget: a committee larger than the remaining chain budget becomes one
  suggestion; members count toward the budget.
- MCP/CLI: coordinator absent → clear error; `TINCAN_REQUEST_ID` produces a
  thread origin and Stop cancels the review.
- Quota skip rules: each condition that must prevent a skip.

Phase 2c
- Exactly one synthesis turn per closed agent-originated review across crashes;
  gated by Stop, archive and budget; late results do not trigger a second turn.

## 12. Amendments to phase 1

1. **Two-way link**: each machine's `tincan web` may list the other as
   `--peer`; either page shows both machines. Peers are never chained
   (`api/peers/<a>/api/peers/<b>` is refused), and each instance lists only its
   own rooms plus its direct peers.
2. **Quota panels** (read-only; phase 1 spec gains a section): discovery of
   `~/.cache/<provider>-quota[-<profile>].json`, normalization of both cache
   schemas keeping last success and last attempt separately with nullable
   values, validated timestamps, freshness recomputed on every read and a
   `quota` note every minute so staleness changes without file changes reach
   the UI; `api/quotas`; the Limits panel; quota on chips and autocomplete.
3. **Dispatcher lock** moves to `.tincan/dispatcher.lock`.
4. **Registry** ignores paths under `<state>/reviews/`.

## 13. Review record (draft review by Codex, 2026-09-28)

| # | Finding | Resolution |
|---|---|---|
| 1 | Worktrees don't isolate; shared Git metadata | Independent `--shared` clone, read-only presets, residual risk documented (§6.6, §9). |
| 2 | Path validation incomplete; secrets | Materialization rules, secret-path omission, credential-free identity (§6.5, §6.6). |
| 3 | Packet can describe the wrong change | Base/target per scope, one snapshot via temporary index, completeness flag, omission manifest (§6.5). |
| 4 | Deterministic IDs insufficient | Request keys, snapshots, hashed prompts, job identity, tombstones, single coordinator (§6.2–6.6). |
| 5 | Deadline vs cancellation | Close once, `late` flag, execution expiry, cancel persisted first, cancel tombstones and retries (§6.4, §6.6). |
| 6 | Workspace lifecycle | Job records outside workspaces, ack → stop host → remove, no age sweep of active jobs, registry exclusion (§6.6, §12). |
| 7 | "One hop" contradicts the budget | Every member and synthesis counts; whole-committee suggestion; thread origin via `TINCAN_REQUEST_ID` (§6.10, §10). |
| 8 | Bundle delivery undefined | Phase 2c: one durable synthesis intent with persisted origin; review messages non-dispatching (§10). |
| 9 | Env values in argv | Preset file instead of argv, key validation, scrub list kept, redacted views (§5). |
| 10 | Alias sessions need provider identity | Provider field/basename, fingerprint includes account config, capability reporting (§5.3). |
| 11 | Size limits don't compose | Endpoint-specific limits and caps throughout (§6.5, §6.7–6.9). |
| 12 | CLI/MCP coordination and peer identity | `tincan web` as sole coordinator, room-written requests, clear error, device-owner trust stated, additive tools (§6.3, §6.7, §6.10). |
| 13 | Remote matching not deterministic | Normalized `repo_id`, ordered candidates, `base^{commit}` check, no fetch, workspace-relative executable resolution (§6.6). |
| 14 | Cached definitions semantics | Versions, snapshots, cache age, unreachable members, reserved names, central-store amendment (§6.1). |
| 15 | `skip_exhausted` unsound as default | Default false; strict opt-in rules (§8); phase 1 normalization keeps attempt/success separately (§12). |
| 16 | Split further | Phases 2a/2b/2c; quota panels in phase 1 (§4). |
