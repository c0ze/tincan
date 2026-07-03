# tincan Phase 1 Go Implementation Review

## Strengths

- The core spool shape matches the design: `Send` writes into `.tincan/tmp/` and then renames into the recipient inbox, so receivers never observe half-written JSON files (`internal/spool/spool.go:53`, `internal/spool/spool.go:65`, `internal/spool/spool.go:68`).
- The receive path uses the right watch-then-scan ordering: create/arm the inbox watcher, scan immediately, then block on fsnotify or timeout. That closes the usual "message arrived between scan and watch" race (`internal/spool/spool.go:92`, `internal/spool/spool.go:103`, `internal/spool/spool.go:120`).
- Exactly-once claiming is implemented with a rename out of the inbox before reading/parsing; concurrent receivers that lose the rename race do not process the same file (`internal/spool/spool.go:155`, `internal/spool/spool.go:156`).
- `recv` timeout behavior is clean for agent loops: `spool.ErrTimeout` maps to exit code 3 with no stdout/stderr, and tests cover that contract (`internal/cli/cli.go:173`, `internal/cli/cli.go:174`, `internal/cli/cli_test.go:58`).
- `ask` uses distinct `r-<id>` reply channels, keeps timed-out channels collectable, and treats post-reply cleanup as non-fatal, which is the right failure mode for preserving the answer (`internal/cli/cli.go:215`, `internal/cli/cli.go:224`, `internal/cli/cli.go:225`, `internal/cli/cli.go:236`).
- The envelope and main entrypoint are intentionally small. `cmd/tincan/main.go` is just an `os.Exit(cli.Run(...))` wrapper, and the envelope package has no hidden transport behavior (`cmd/tincan/main.go:11`, `internal/envelope/envelope.go:13`).

## Issues By Severity

### Critical

None found.

### Important

1. Raw inbox names can escape the spool root.

   `InboxDir` joins the participant/channel name directly into a filesystem path (`internal/spool/spool.go:34`), and CLI inputs feed that path through `--to`, `--as`, and `--channel` without validation (`internal/cli/cli.go:108`, `internal/cli/cli.go:151`, `internal/cli/cli.go:248`). A name such as `../outside` or `../../some-dir` can place inboxes outside `.tincan/inbox`; `recv` can then claim and remove `.json` files from an unintended directory, and `RemoveInbox` would be dangerous if ever called with non-minted input (`internal/spool/spool.go:187`). This breaks the room boundary and can cause accidental data loss. Add one shared validator for participant names and reply channels, reject path separators, absolute paths, empty path components, and `.`/`..`, then call it from the CLI and/or `spool`.

2. Claim-by-rename hides real filesystem errors as ordinary contention.

   `claimOldest` treats every `os.Rename(inboxFile, claimed)` failure as "another receiver claimed it first" (`internal/spool/spool.go:156`, `internal/spool/spool.go:157`). That is correct for `ENOENT` after a race, but incorrect for permission errors, read-only directories, destination problems, Windows sharing errors, or other filesystem failures. In those cases `recv` may silently skip queued messages and eventually return timeout, which violates the error/timeout distinction that agents branch on. Only ignore `os.IsNotExist(err)` or the precise race-class errors you intend to tolerate; return other rename errors.

### Minor

1. `ask --format` is not validated.

   `recv` rejects non-`json|body` formats with usage exit 2 (`internal/cli/cli.go:164`), but `ask` defines the same flag and never checks it (`internal/cli/cli.go:191`, `internal/cli/cli.go:224`, `internal/cli/cli.go:235`). Because `printEnvelope` treats any non-`body` format as JSON (`internal/cli/cli.go:90`), `tincan ask --format yaml ...` succeeds once a reply arrives. This is small, but the CLI contract is intentionally machine-consumed; invalid format values should fail before sending the request.

2. A few edge-case behaviors are important but untested.

   Current tests cover atomic queueing, oldest-first drain, timeout, re-arm, concurrent exact-once claim, log mode, and ask/reply lifecycle (`internal/spool/spool_test.go:27`, `internal/spool/spool_test.go:115`, `internal/spool/spool_test.go:159`, `internal/spool/spool_test.go:171`, `internal/spool/spool_test.go:189`, `internal/cli/cli_test.go:110`). Missing high-value tests: corrupt JSON is quarantined in tmp and does not loop forever (`internal/spool/spool.go:163`), non-race rename errors are surfaced once fixed (`internal/spool/spool.go:156`), path traversal names are rejected once fixed (`internal/spool/spool.go:34`), invalid `ask --format` exits 2 (`internal/cli/cli.go:191`), and the kqueue ENOENT arm/error tolerance remains non-fatal (`internal/spool/spool.go:96`, `internal/spool/spool.go:123`).

## Verification

- `mise x -- go test ./... -race -count=1` passes.
- CodeRabbit CLI is installed (`0.6.4`) but signed out, so this review was performed locally by source inspection plus the requested test command.

## Verdict

Phase 1 is structurally sound for the intended local listener workflow: send/recv/ask/reply are small, test-covered, and the main concurrency model is the right one. I would fix the name/path validation issue before wider use, because it is the one place the filesystem spool currently fails to enforce its own room boundary. After that, tightening rename-error handling and adding the missing edge tests should be enough for a solid Phase 1 baseline.
