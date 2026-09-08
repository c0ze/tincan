# Contributing

Use Go 1.25 or newer. Clone the repository and run these checks from its root:

```sh
go mod download
go vet ./...
go build ./...
go test ./... -race -count=1
bash tests/install_test.sh
```

The installer checks require Bash and run in temporary directories. Provider
process tests use controlled helpers; they do not require provider logins.
Live-provider validation is separate and needs authenticated executables in
an isolated room. Never commit `.tincan/` state, host logs or credentials.

`go install ./cmd/tincan` installs your checkout. Inspect it with
`tincan version --format json`; an untagged source build reports development
or embedded VCS metadata. Official archives set version, commit and build date
through the GoReleaser configuration.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/tincan`, `internal/cli` | Command entry point and CLI behavior |
| `internal/mcpserver` | Room-bound stdio MCP tools |
| `internal/host` | Hosted lifecycle, provider execution and session adapters |
| `internal/request` | Durable requests, progress and interactive collection |
| `internal/spool`, `internal/envelope`, `internal/fsutil` | Delivery and filesystem safety |
| `internal/buildinfo` | Shared CLI/MCP version identity |
| `skills`, `install.sh`, `tests/install_test.sh` | Client instructions and installation |

## Releases

Go module imports use `github.com/c0ze/tincan/v2`. Keep the module declaration,
imports, installer target and GoReleaser metadata paths in sync. Use `v2.x.y`
tags for this module line.

Before tagging, update the current guides and add `docs/releases/vX.Y.Z.md`.
Keep older design plans and validation records historical and link current
guidance from the [documentation index](README.md).

With GoReleaser v2 installed, validate configuration and build local artifacts:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

Snapshot builds do not publish. Inspect archive contents, checksums and binary
version metadata. After merging the tested change to `main`, push an annotated
release tag. The release workflow tests the exact tagged commit on all six
OS/Go combinations, verifies the tag still names that commit, then builds and
publishes archives. Its release body comes from the matching committed notes.
Check the completed workflow, published assets and a versioned Go install before
announcing the release. Never move an already published tag to change its code.
