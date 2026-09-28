# tincan committees 2b-2: packets and reviewer jobs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
- A requester can capture a faithful, size-limited *packet* describing a change in a room.
- A member machine can accept a reviewer *job* for that packet: it builds a private workspace (a verified checkout, or the packet alone), runs the member preset once with a pinned executable, reports status and result, and handles cancellation, acknowledgement and cleanup.

**Architecture:**
- `internal/packet` builds packets with git plumbing: a private index snapshot, a tree-to-tree raw diff, an included tree built with `update-index`, a binary patch, and the post-change file contents.
- `internal/reviewjob` owns receiver jobs under `<state>/reviews/`. It keeps records and locks, materializes workspaces in two modes, and launches the member through `dispatch.Send` in the workspace room with a pinned preset. Creation runs in the background; a janitor resumes and cleans up.
- `tincan web` exposes the job API and runs the janitor.
- `internal/host` gains pinned-executable presets.
- Nothing requests reviews yet: that is 2b-3. This phase is exercised by tests and by `curl` against the API.

**Tech Stack:** Go 1.25 standard library, and the `git` CLI (any version shipped by current distros: `diff-tree -z`, `update-index --index-info`, `apply --index --binary`, `clone --shared`, `repack`, `mktree`).

**Spec:** `docs/superpowers/specs/2026-09-28-tincan-committees-design.md`: §6.5 (scopes, snapshot, omissions, limits), §6.6 (jobs, identity, repository identity, candidates, checkout and packet-only modes, path policy, pinned executables, execution, create/status/cancel/janitor), §6.7 (the four `review-jobs` routes), §6.8 (the workspace note), §9.

## Global Constraints

- Scopes are `none`, `uncommitted`, `branch`, `commit:<rev>` and `range:<a>..<b>`, with bases exactly as in spec §6.5:
  - `uncommitted`: `HEAD`, or the empty tree when `HEAD` is unborn.
  - `branch`: the merge-base of `HEAD` and `origin/HEAD`, else `main`, else `master`.
  - `commit:<rev>`: the first parent (empty for a root commit; a merge gets a note).
  - `range:<a>..<b>`: `a`, and `b`'s tree.
- The working-tree snapshot seeds a private index from the base, runs `git add -A` from the repository root, then `write-tree`. It is refused on an unmerged index or sparse checkout, and never touches the real index or working tree.
- Omitted changes, each with a reason in the manifest: secret-looking paths (`.env`, `.env.*`, `*.pem`, `*.key`, `*.p12`, `*.pfx`, `id_rsa*`, `id_ed25519*`, `.npmrc`, `.netrc`, `.pypirc`, `credentials*`, `*secret*`, matched case-insensitively on the base name); symlinks (mode `120000`); gitlinks (`160000`); target content over 1 MiB; path-policy failures. A rename is included only when both endpoints pass; otherwise it becomes a deletion plus an addition, each judged separately.
- Limits on raw bytes:
  - patch ≤ 3 MiB, else `complete: false` and no patch;
  - manifest ≤ 5 000 paths and ≤ 512 KiB (error);
  - question ≤ 64 KiB (error);
  - packet file contents ≤ 2 MiB total, text files only, in manifest order;
  - encoded job request ≤ 8 MiB; job status result ≤ 1 MiB.
- Path policy:
  - relative and clean, with no empty, `.` or `..` component;
  - no component that case-folds to `.git` or `.tincan`;
  - at most 1 024 bytes, with every component valid under `envelope.ValidComponent`;
  - no two included paths may case-fold to the same string.
  - Packet files are created with exclusive opens as 0600 regular files, in directories the materializer created itself.
- `repo_id`: the origin URL without credentials, with a lower-case host, the port kept (default 22/443 dropped), the path without `.git` or a trailing `/`, and scp-style, `ssh://` and `https://` forms unified.
- Candidates: registered rooms whose repository root has the same `repo_id`, deduplicated by root and ordered by `last_used` (newest first), then path.
  - Partial clones are skipped: `extensions.partialClone`, or any `remote.*.promisor` set to `true`.
  - With `GIT_NO_LAZY_FETCH=1`, a candidate qualifies if the base is empty or `git cat-file -e <base>^{commit}` succeeds. No fetch is ever attempted.
- Checkout mode:
  1. `git clone --no-checkout --shared`, then `repack -a -d -q`, remove `objects/info/alternates` and the `origin` remote.
  2. Check out the base with `checkout --detach`, or for an empty base point `HEAD` at an unborn branch and empty the index.
  3. Validate the patch's paths, then `git -c core.symlinks=false apply --index --binary`.
  4. `write-tree` must equal `included_tree`.
  - Any failure removes the workspace and falls back to packet-only mode.
- Job identity is (`job_id`, `packet_sha256`, `prompt_sha256`, `preset`, `expires_at`). An identical replay returns the job; a different identity answers 409. A create at or after `expires_at` answers 410, and so does a create after cancel-before-create (tombstone). `expires_at` must be at most 270 min after creation.
- Pinned execution:
  - the member preset's executable is resolved before the workspace exists; only bare or absolute executables are accepted;
  - the run uses that absolute path exactly, with no re-resolution, base-name fallback or `argv[0]` template expansion;
  - a vanished file fails the run;
  - the exec timeout is the time left until `expires_at`, and a launch with less than a minute left counts as expired;
  - runs are stateless, with request ID `job_id`.
- Workspaces live under `<state>/reviews/ws/<job_id>` and records under `<state>/reviews/jobs/`. Both are excluded from the room registry.
- Every read-modify-write of a job record holds `<state>/reviews/jobs/<job_id>.lock`. Status reads use a non-blocking try and return the stored record while creation holds the lock.
- Janitor, every minute:
  - resumes `creating` jobs whose lock is free;
  - imports terminal results;
  - after the result is acknowledged or the job is cancelled — or 1 h after `expires_at` for anything else — stops the listener and removes the workspace;
  - removes records 7 days after `expires_at`.
- No new Go dependencies. gofmt, `go vet ./...` and `GOOS=windows go vet ./...` must stay clean. Tests needing a Unix shell or symlinks skip on Windows. Never use `git stash`. Commit trailer: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A reviewed change adds an executable called like the member's binary** (a `./codex` script in the repo root). The member must still run the pinned absolute path, never the repository's file. Pinned in Task 1 and Task 8.
2. **A packet path that escapes or aliases**: `../x`, `.GIT/config`, `a/.tincan/x`, or two paths differing only in case. Nothing may be written outside the workspace, in either mode. Pinned in Tasks 2, 6 and 7.
3. **The requester's repository is a partial clone or lacks the base commit** on the member machine. The job must fall back to packet-only mode with the reason stated. It must never fetch or hang on a promisor. Pinned in Task 7.
4. **`tincan web` restarts while a job is `creating`.** The janitor resumes it, adopting a request that already ran instead of running it twice. Pinned in Task 9.
5. **A cancel arriving just after the member finished** keeps the result: the job stays `done` and the cancel is acknowledged. Pinned in Task 8.

---

### Task 1: Pinned-executable presets in `internal/host`

**Files:**
- Modify: `internal/host/preset.go` (`Preset.Pinned`; `Public` omits it)
- Modify: `internal/host/runner.go` (`RunSpec.Pinned`; strict path in `Run`)
- Modify: `internal/host/serve.go` (`handle`: no `argv[0]` rendering when pinned; `RunSpec.Pinned`)
- Modify: `internal/host/lifecycle.go` (`Up`: pinned presets are checked with `os.Stat`, not `ResolveExecutable`)
- Test: `internal/host/runner_test.go`, `internal/host/serve_test.go`

**Interfaces:**
- Produces:
  - the field `Preset.Pinned bool` (`json:"pinned,omitempty"`). It is not read from `agents.json` (`configPreset` has no such field).
  - the field `RunSpec.Pinned bool`.
  - `func PinnedExecutable(path string) error`, which returns nil when `path` is absolute and names an existing regular file with an execute bit (on Windows, any existing regular file).

- [ ] **Step 1: Write the failing tests.** Add to `internal/host/runner_test.go`:

```go
func TestPinnedRunUsesTheExactPath(t *testing.T) {
	t.Setenv("TINCAN_FAKE_AGENT", "1")
	exe, _ := os.Executable()
	res := Run(context.Background(), RunSpec{Argv: []string{exe, "echo", "pinned"}, Dir: t.TempDir(), Pinned: true})
	if res.Err != nil || string(res.Stdout) != "echo: pinned\n" {
		t.Fatalf("pinned run: %+v", res)
	}
	missing := filepath.Join(t.TempDir(), filepath.Base(exe))
	res = Run(context.Background(), RunSpec{Argv: []string{missing, "echo", "x"}, Dir: t.TempDir(), Pinned: true})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "pinned executable") {
		t.Fatalf("vanished pinned executable fell back: %+v", res)
	}
	if res := Run(context.Background(), RunSpec{Argv: []string{"relative/tool"}, Dir: t.TempDir(), Pinned: true}); res.Err == nil {
		t.Fatal("relative pinned executable accepted")
	}
}

func TestPinnedExecutable(t *testing.T) {
	exe, _ := os.Executable()
	if err := PinnedExecutable(exe); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "codex", "./codex", filepath.Join(t.TempDir(), "absent"), t.TempDir()} {
		if PinnedExecutable(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
```

Add to `internal/host/serve_test.go`:

```go
// A pinned preset's argv[0] is never template-expanded, and a file named like
// the executable inside the room is never what runs (committees §6.6).
func TestServePinnedPresetIgnoresRoomExecutables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script decoy")
	}
	exe, _ := os.Executable()
	p := Preset{Exec: []string{exe, "echo", "{body}"}, Pinned: true}
	room, sp, _, done, _ := startServe(t, p, "fake")
	decoy := filepath.Join(room, filepath.Base(exe))
	os.WriteFile(decoy, []byte("#!/bin/sh\necho decoy\n"), 0o755)
	if reply := askVia(t, sp, "hi"); reply.Body != "echo: hi" {
		t.Fatalf("reply = %q", reply.Body)
	}
	stopServe(t, sp, done)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/host -run 'TestPinned|TestServePinned'`
Expected: compile failure (`unknown field Pinned`).

- [ ] **Step 3: Implement.** In `preset.go`, add to `Preset` after `Provider`:

```go
	// Pinned marks Exec[0] as an absolute path resolved before the room
	// existed (reviewer jobs, committees §6.6): it is run exactly, never
	// re-resolved, never template-expanded. Not settable from agents.json.
	Pinned bool `json:"pinned,omitempty"`
```

Add to `runner.go`, at the end of the file:

```go
// PinnedExecutable checks a pinned executable: an absolute path to an
// existing regular file, executable on Unix.
func PinnedExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("pinned executable %q is not an absolute path", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("pinned executable %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return fmt.Errorf("pinned executable %s is not an executable file", path)
	}
	return nil
}
```

Add imports `os`, `path/filepath` and `runtime` to `runner.go` as needed. In `RunSpec` add `Pinned bool // Argv[0] is a pinned absolute path: no resolution or fallback`. In `Run`, replace `path, err := ResolveExecutable(spec.Argv[0], spec.Dir)` with:

```go
	var path string
	var err error
	if spec.Pinned {
		path, err = spec.Argv[0], PinnedExecutable(spec.Argv[0])
	} else {
		path, err = ResolveExecutable(spec.Argv[0], spec.Dir)
	}
```

In `serve.go` `handle`, replace `spec.Argv = Render(p.Exec, vars)` with:

```go
	spec.Argv = Render(p.Exec, vars)
	if p.Pinned {
		spec.Argv[0], spec.Pinned = p.Exec[0], true
	}
```

In `lifecycle.go` `Up`, replace the `ResolveExecutable` availability check with:

```go
	if o.Preset.Pinned {
		if err := PinnedExecutable(o.Preset.Exec[0]); err != nil {
			return UpResult{}, err
		}
	} else if _, err := ResolveExecutable(o.Preset.Exec[0], room); err != nil {
		return UpResult{}, fmt.Errorf("agent binary %q not found on PATH or in user bin directories (preset %s; fix the exec path in %s or install the CLI): %w", o.Preset.Exec[0], o.Label, ConfigPath(), err)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/host`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/host
git commit -m "Run pinned executables exactly, without resolution or fallback"
```

---

### Task 2: `internal/packet` — git runner, repository identity, path policy

**Files:**
- Create: `internal/packet/git.go`, `internal/packet/repoid.go`, `internal/packet/pathpolicy.go`
- Test: `internal/packet/repoid_test.go`, `internal/packet/pathpolicy_test.go`, `internal/packet/helpers_test.go`

**Interfaces:**
- Produces the git runner:
  - `type Git struct { Dir string; Env []string }`
  - `func (g Git) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error)`, which runs `git` in `Dir` with `GIT_TERMINAL_PROMPT=0`, `GIT_NO_LAZY_FETCH=1`, `GIT_OPTIONAL_LOCKS=0`, `LC_ALL=C`, plus `Env`. Its error includes stderr.
  - `func (g Git) Out(ctx context.Context, args ...string) (string, error)` returns the trimmed stdout.
- Produces `func RepoID(remote string) string` (`""` for an empty or unparseable remote) and `func RepoIDOf(ctx context.Context, root string) string`.
- Produces `func ValidPath(p string) error` and `func FoldCollisions(paths []string) map[string]string`. The latter maps each later-colliding path to the earlier path it collides with.
- Test helper (`helpers_test.go`): `func testRepo(t *testing.T) (dir string, git func(args ...string) string)`. It creates a repository with an identity configured; `git(...)` runs in it and fails the test on error.

- [ ] **Step 1: Write the failing tests.** Create `internal/packet/helpers_test.go`:

```go
package packet

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testRepo creates a git repository in a canonical temporary directory.
func testRepo(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	return dir, run
}

// runQuiet runs git in dir ignoring failure (e.g. a merge that conflicts).
func runQuiet(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	cmd.Run()
}
```

Create `internal/packet/repoid_test.go`:

```go
package packet

import (
	"context"
	"testing"
)

func TestRepoIDNormalizes(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:c0ze/tincan.git":              "github.com/c0ze/tincan",
		"ssh://git@GitHub.com/c0ze/tincan.git":        "github.com/c0ze/tincan",
		"ssh://git@github.com:22/c0ze/tincan":         "github.com/c0ze/tincan",
		"https://user:tok@github.com/c0ze/tincan.git": "github.com/c0ze/tincan",
		"https://github.com:443/c0ze/tincan/":         "github.com/c0ze/tincan",
		"https://git.example.com:8443/team/repo.git":  "git.example.com:8443/team/repo",
		"ssh://git@git.example.com:2222/team/repo":    "git.example.com:2222/team/repo",
		"":            "",
		"not a url":   "",
		"/local/path": "",
	} {
		if got := RepoID(in); got != want {
			t.Errorf("RepoID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoIDOfReadsOrigin(t *testing.T) {
	dir, git := testRepo(t)
	if got := RepoIDOf(context.Background(), dir); got != "" {
		t.Fatalf("no origin: %q", got)
	}
	git("remote", "add", "origin", "git@github.com:c0ze/tincan.git")
	if got := RepoIDOf(context.Background(), dir); got != "github.com/c0ze/tincan" {
		t.Fatalf("origin: %q", got)
	}
}
```

Create `internal/packet/pathpolicy_test.go`:

```go
package packet

import (
	"strings"
	"testing"
)

func TestValidPath(t *testing.T) {
	for _, ok := range []string{"a.go", "dir/sub/file.txt", "a/.github/workflows/ci.yml", ".gitignore", "x/.gitkeep"} {
		if err := ValidPath(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "../x", "a/../b", "a//b", "./a", "a/", ".git/config", "x/.GIT/hooks/pre-commit",
		".tincan/requests/x", "a/.TinCan/b", "con/a", "a:b", strings.Repeat("a", 1025), "a\\b", "a/b\x00"} {
		if err := ValidPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFoldCollisions(t *testing.T) {
	got := FoldCollisions([]string{"README.md", "src/a.go", "readme.MD", "SRC/A.go", "b"})
	if got["readme.MD"] != "README.md" || got["SRC/A.go"] != "src/a.go" || len(got) != 2 {
		t.Fatalf("collisions: %v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/packet`
Expected: build failure (`undefined: RepoID`).

- [ ] **Step 3: Implement.** Create `internal/packet/git.go`:

```go
// Package packet captures a change in a room as a size-limited packet for
// committee reviewers (committees spec §6.5), using git plumbing only: a
// private index, tree-to-tree diffs and blobs. It never touches the real
// index or working tree and never contacts a remote.
package packet

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Git runs git commands in Dir with a hermetic, non-interactive environment.
type Git struct {
	Dir string
	Env []string
}

func (g Git) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, g.Env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g Git) Out(ctx context.Context, args ...string) (string, error) {
	out, err := g.Run(ctx, nil, args...)
	return strings.TrimSpace(string(out)), err
}
```

Create `internal/packet/repoid.go`:

```go
package packet

import (
	"context"
	"net/url"
	"strings"
)

// RepoID normalizes a remote URL into a credential-free repository identity
// (committees §6.6): lower-case host, non-default port kept, path without
// ".git" and trailing "/"; scp-style, ssh:// and https:// forms agree.
func RepoID(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	var host, port, path string
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" {
		host, port, path = strings.ToLower(u.Hostname()), u.Port(), u.Path
		if (port == "22" && u.Scheme == "ssh") || (port == "443" && u.Scheme == "https") || (port == "80" && u.Scheme == "http") {
			port = ""
		}
	} else if at, colon := strings.Index(remote, "@"), strings.Index(remote, ":"); colon > 0 && !strings.Contains(remote[:colon], "/") && (at < 0 || at < colon) {
		// scp-style: [user@]host:path
		host, path = strings.ToLower(remote[at+1:colon]), remote[colon+1:]
	} else {
		return ""
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	path = strings.Trim(path, "/")
	if host == "" || path == "" || strings.ContainsAny(host, " /") {
		return ""
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + path
}

// RepoIDOf is RepoID of root's origin remote, or "" without one.
func RepoIDOf(ctx context.Context, root string) string {
	remote, err := Git{Dir: root}.Out(ctx, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return RepoID(remote)
}
```

Create `internal/packet/pathpolicy.go`:

```go
package packet

import (
	"errors"
	"fmt"
	"strings"

	"github.com/c0ze/tincan/v2/internal/envelope"
)

// ValidPath applies the committees §6.6 path policy to one
// repository-relative path, on both requester and receiver.
func ValidPath(p string) error {
	if p == "" || len(p) > 1024 {
		return errors.New("path is empty or longer than 1024 bytes")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return fmt.Errorf("path %q is not relative", p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("path %q is not clean", p)
		}
		if f := strings.ToLower(c); f == ".git" || f == ".tincan" {
			return fmt.Errorf("path %q enters %s", p, c)
		}
		if err := envelope.ValidComponent(c); err != nil {
			return fmt.Errorf("path %q: %w", p, err)
		}
	}
	return nil
}

// FoldCollisions returns, for every path that case-folds to the same string
// as an earlier one, that earlier path.
func FoldCollisions(paths []string) map[string]string {
	seen := map[string]string{}
	out := map[string]string{}
	for _, p := range paths {
		k := strings.ToLower(p)
		if first, ok := seen[k]; ok {
			out[p] = first
			continue
		}
		seen[k] = p
	}
	return out
}
```

`envelope.ValidComponent` rejects NUL through `utf8`/control checks and rejects `:` and Windows reserved names such as `con`, which covers `con/a` and `a:b`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/packet`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/packet
git commit -m "Add the packet git runner, repository identity and path policy"
```

---

### Task 3: Scopes and the working-tree snapshot

**Files:**
- Create: `internal/packet/scope.go`
- Test: `internal/packet/scope_test.go`

**Interfaces:**
- Consumes: `Git` (Task 2).
- Produces:

  ```go
  type Resolved struct {
      Root       string // repository root
      Scope      string
      BaseKind   string // "commit" | "empty" | "none"
      BaseCommit string // when BaseKind == "commit"
      BaseTree   string // tree ID of the base (the empty tree for "empty")
      TargetTree string
      Notes      []string
  }
  ```

- Produces `func Resolve(ctx context.Context, dir, scope string) (Resolved, error)`. `dir` is the room; for scope `none` no git is needed and `Root` is `""`. The scope `""` means `uncommitted` in a Git room and `none` elsewhere.
- Produces `func EmptyTree(ctx context.Context, g Git) (string, error)`.

- [ ] **Step 1: Write the failing test.** Create `internal/packet/scope_test.go`:

```go
package packet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lsTree(t *testing.T, git func(...string) string, tree string) string {
	return git("ls-tree", "-r", "--name-only", tree)
}

func TestResolveUncommittedSnapshot(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "keep.txt", "a\n")
	write(t, dir, "tracked.log", "t\n")
	write(t, dir, ".gitignore", "*.log\n")
	git("add", "-f", ".")
	git("commit", "-qm", "base")
	head := git("rev-parse", "HEAD")
	write(t, dir, "keep.txt", "a\nb\n")
	write(t, dir, "tracked.log", "t\nmore\n") // tracked but ignored: still captured
	write(t, dir, "new.txt", "n\n")          // untracked: captured
	write(t, dir, "junk.log", "j\n")         // untracked and ignored: not captured
	before := git("status", "--porcelain")
	r, err := Resolve(ctx, filepath.Join(dir), "uncommitted")
	if err != nil {
		t.Fatal(err)
	}
	if r.BaseKind != "commit" || r.BaseCommit != head || r.Root != dir {
		t.Fatalf("resolved: %+v", r)
	}
	files := lsTree(t, git, r.TargetTree)
	if !strings.Contains(files, "new.txt") || strings.Contains(files, "junk.log") || !strings.Contains(files, "tracked.log") {
		t.Fatalf("snapshot files:\n%s", files)
	}
	if got := git("show", r.TargetTree+":tracked.log"); got != "t\nmore" {
		t.Fatalf("tracked ignored file content: %q", got)
	}
	if after := git("status", "--porcelain"); after != before {
		t.Fatalf("snapshot touched the real index:\n%s\nvs\n%s", before, after)
	}
}

func TestResolveUnbornHeadAndSubdirectory(t *testing.T) {
	dir, _ := testRepo(t)
	write(t, dir, "sub/a.txt", "a\n")
	r, err := Resolve(context.Background(), filepath.Join(dir, "sub"), "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Scope != "uncommitted" || r.BaseKind != "empty" || r.Root != dir {
		t.Fatalf("unborn: %+v", r)
	}
}

func TestResolveCommitRangeBranchAndNone(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "a.txt", "1\n")
	git("add", ".")
	git("commit", "-qm", "root")
	root := git("rev-parse", "HEAD")
	write(t, dir, "a.txt", "2\n")
	git("commit", "-qam", "two")
	two := git("rev-parse", "HEAD")
	git("checkout", "-qb", "feature")
	write(t, dir, "b.txt", "b\n")
	git("add", ".")
	git("commit", "-qm", "feature")

	if r, err := Resolve(ctx, dir, "commit:"+root); err != nil || r.BaseKind != "empty" {
		t.Fatalf("root commit: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, dir, "commit:"+two); err != nil || r.BaseCommit != root || r.TargetTree != git("rev-parse", two+"^{tree}") {
		t.Fatalf("commit: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, dir, "range:"+root+".."+two); err != nil || r.BaseCommit != root {
		t.Fatalf("range: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, dir, "branch"); err != nil || r.BaseCommit != two {
		t.Fatalf("branch (merge-base with main): %+v %v", r, err)
	}
	git("checkout", "-q", "main")
	git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	if r, err := Resolve(ctx, dir, "commit:HEAD"); err != nil || r.BaseCommit != two || len(r.Notes) == 0 {
		t.Fatalf("merge commit: %+v %v", r, err)
	}
	if r, err := Resolve(ctx, t.TempDir(), ""); err != nil || r.Scope != "none" || r.BaseKind != "none" {
		t.Fatalf("non-git room: %+v %v", r, err)
	}
	for _, bad := range []string{"commit:nope", "range:a", "sideways", "branch:x"} {
		if _, err := Resolve(ctx, dir, bad); err == nil {
			t.Errorf("scope %q accepted", bad)
		}
	}
}

func TestResolveRefusesUnmergedAndSparse(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "a.txt", "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "other")
	write(t, dir, "a.txt", "other\n")
	git("commit", "-qam", "other")
	git("checkout", "-q", "main")
	write(t, dir, "a.txt", "main\n")
	git("commit", "-qam", "main")
	runQuiet(dir, "merge", "other") // conflicts: exits non-zero
	if _, err := Resolve(ctx, dir, "uncommitted"); err == nil || !strings.Contains(err.Error(), "unmerged") {
		t.Fatalf("unmerged index: %v", err)
	}
	runQuiet(dir, "merge", "--abort")
	git("config", "core.sparseCheckout", "true")
	if _, err := Resolve(ctx, dir, "uncommitted"); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("sparse checkout: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/packet -run TestResolve`
Expected: build failure (`undefined: Resolve`).

- [ ] **Step 3: Implement.** Create `internal/packet/scope.go`:

```go
package packet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Resolved struct {
	Root       string
	Scope      string
	BaseKind   string // commit | empty | none
	BaseCommit string
	BaseTree   string
	TargetTree string
	Notes      []string
}

// EmptyTree returns the empty tree's ID in the repository's object format.
func EmptyTree(ctx context.Context, g Git) (string, error) {
	out, err := g.Run(ctx, []byte{}, "mktree")
	return strings.TrimSpace(string(out)), err
}

// Resolve maps a scope to its base and target trees (committees §6.5). ""
// means "uncommitted" in a Git room and "none" elsewhere.
func Resolve(ctx context.Context, dir, scope string) (Resolved, error) {
	root, err := Git{Dir: dir}.Out(ctx, "rev-parse", "--show-toplevel")
	inGit := err == nil
	if scope == "" {
		scope = "none"
		if inGit {
			scope = "uncommitted"
		}
	}
	if scope == "none" {
		return Resolved{Scope: "none", BaseKind: "none"}, nil
	}
	if !inGit {
		return Resolved{}, fmt.Errorf("scope %s needs a Git repository", scope)
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	g := Git{Dir: root}
	r := Resolved{Root: root, Scope: scope}
	empty, err := EmptyTree(ctx, g)
	if err != nil {
		return r, err
	}
	commit := func(rev string) (string, error) {
		return g.Out(ctx, "rev-parse", "--verify", "-q", rev+"^{commit}")
	}
	setBase := func(c string) error {
		if c == "" {
			r.BaseKind, r.BaseTree = "empty", empty
			return nil
		}
		r.BaseKind, r.BaseCommit = "commit", c
		t, err := g.Out(ctx, "rev-parse", c+"^{tree}")
		r.BaseTree = t
		return err
	}
	switch {
	case scope == "uncommitted":
		head, _ := commit("HEAD")
		if err := setBase(head); err != nil {
			return r, err
		}
		r.TargetTree, err = snapshot(ctx, g, r.BaseTree)
		return r, err
	case scope == "branch":
		head, err := commit("HEAD")
		if err != nil {
			return r, errors.New("scope branch needs a commit on HEAD")
		}
		def := ""
		if ref, err := g.Out(ctx, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
			def = ref
		} else if _, err := commit("refs/heads/main"); err == nil {
			def = "refs/heads/main"
		} else if _, err := commit("refs/heads/master"); err == nil {
			def = "refs/heads/master"
		} else {
			return r, errors.New("scope branch: no origin/HEAD, main or master to compare with")
		}
		mb, err := g.Out(ctx, "merge-base", head, def)
		if err != nil {
			return r, fmt.Errorf("scope branch: no merge-base with %s", def)
		}
		if err := setBase(mb); err != nil {
			return r, err
		}
		r.TargetTree, err = snapshot(ctx, g, r.BaseTree)
		return r, err
	case strings.HasPrefix(scope, "commit:"):
		c, err := commit(strings.TrimPrefix(scope, "commit:"))
		if err != nil || c == "" {
			return r, fmt.Errorf("scope %s: no such commit", scope)
		}
		parents, err := g.Out(ctx, "rev-list", "--parents", "-n", "1", c)
		if err != nil {
			return r, err
		}
		fields := strings.Fields(parents)[1:]
		first := ""
		if len(fields) > 0 {
			first = fields[0]
		}
		if len(fields) > 1 {
			r.Notes = append(r.Notes, "merge commit: diffed against its first parent")
		}
		if err := setBase(first); err != nil {
			return r, err
		}
		r.TargetTree, err = g.Out(ctx, "rev-parse", c+"^{tree}")
		return r, err
	case strings.HasPrefix(scope, "range:"):
		a, b, ok := strings.Cut(strings.TrimPrefix(scope, "range:"), "..")
		if !ok || a == "" || b == "" || strings.HasPrefix(b, ".") {
			return r, fmt.Errorf("scope %s: want range:<a>..<b>", scope)
		}
		ca, err := commit(a)
		if err != nil || ca == "" {
			return r, fmt.Errorf("scope %s: no such commit %s", scope, a)
		}
		cb, err := commit(b)
		if err != nil || cb == "" {
			return r, fmt.Errorf("scope %s: no such commit %s", scope, b)
		}
		if err := setBase(ca); err != nil {
			return r, err
		}
		r.TargetTree, err = g.Out(ctx, "rev-parse", cb+"^{tree}")
		return r, err
	}
	return r, fmt.Errorf("unknown scope %q (none, uncommitted, branch, commit:<rev>, range:<a>..<b>)", scope)
}

// snapshot captures the working tree as a tree object through a private
// index seeded from base, so tracked files keep their tracked status even
// when they match ignore rules. It never touches the real index.
func snapshot(ctx context.Context, g Git, baseTree string) (string, error) {
	if u, err := g.Out(ctx, "ls-files", "-u"); err != nil || u != "" {
		return "", errors.New("the index has unmerged entries; resolve the merge first")
	}
	if sparse, _ := g.Out(ctx, "config", "--bool", "core.sparseCheckout"); sparse == "true" {
		return "", errors.New("sparse checkouts are not supported for working-tree scopes")
	}
	tmp, err := os.MkdirTemp("", "tincan-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	ig := Git{Dir: g.Dir, Env: append(append([]string(nil), g.Env...), "GIT_INDEX_FILE="+filepath.Join(tmp, "index"))}
	if _, err := ig.Run(ctx, nil, "read-tree", baseTree); err != nil {
		return "", err
	}
	if _, err := ig.Run(ctx, nil, "add", "-A"); err != nil {
		return "", err
	}
	return ig.Out(ctx, "write-tree")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/packet`
Expected: PASS. On Windows, `testRepo` works when git is on `PATH` and `EvalSymlinks` returns a usable path. If the snapshot's temp index path causes trouble on Windows CI, record a ruling and skip only the failing assertion with `runtime.GOOS == "windows"`.

- [ ] **Step 5: Commit**

```bash
git add internal/packet
git commit -m "Resolve review scopes and snapshot the working tree through a private index"
```

---

### Task 4: Packet builder — changes, omissions, included tree, patch, files, limits

**Files:**
- Create: `internal/packet/packet.go`, `internal/packet/build.go`
- Test: `internal/packet/build_test.go`

**Interfaces:**
- Consumes: `Resolve`, `Git`, `ValidPath`, `FoldCollisions`, `RepoIDOf`.
- Produces:

  ```go
  type Change struct {
      Status   string `json:"status"`             // A M D R T
      Path     string `json:"path"`               // destination (source for D)
      From     string `json:"from,omitempty"`     // rename source
      OldMode  string `json:"old_mode,omitempty"`
      NewMode  string `json:"new_mode,omitempty"`
      Included bool   `json:"included"`
      Reason   string `json:"reason,omitempty"`   // why it was omitted
  }
  type Manifest struct {
      RepoID       string    `json:"repo_id,omitempty"`
      Scope        string    `json:"scope"`
      BaseKind     string    `json:"base_kind"`
      BaseCommit   string    `json:"base_commit,omitempty"`
      TargetTree   string    `json:"target_tree,omitempty"`
      IncludedTree string    `json:"included_tree,omitempty"`
      Captured     time.Time `json:"captured"`
      Complete     bool      `json:"complete"`
      Notes        []string  `json:"notes,omitempty"`
      Changes      []Change  `json:"changes"`
      FilesOmitted []string  `json:"files_omitted,omitempty"` // text files over the 2 MiB content budget
  }
  type Packet struct {
      Manifest Manifest          `json:"manifest"`
      Patch    []byte            `json:"patch,omitempty"`
      Files    map[string][]byte `json:"files,omitempty"`
      Question string            `json:"question"`
  }
  ```

- Produces `func Build(ctx context.Context, dir, scope, question string) (*Packet, error)`, `func Hash(p *Packet) string` (hex SHA-256 of `json.Marshal(p)`) and `func (p *Packet) Validate() error` (the receiver-side limits and path policy).
- Constants: `MaxPatch = 3 << 20`, `MaxFileContent = 1 << 20`, `MaxFilesTotal = 2 << 20`, `MaxPaths = 5000`, `MaxManifest = 512 << 10`, `MaxQuestion = 64 << 10`, `MaxEncoded = 8 << 20`.

- [ ] **Step 1: Write the failing test.** Create `internal/packet/build_test.go`:

```go
package packet

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func change(p *Packet, path string) (Change, bool) {
	for _, c := range p.Manifest.Changes {
		if c.Path == path {
			return c, true
		}
	}
	return Change{}, false
}

func TestBuildOmitsAndReproducesIncludedTree(t *testing.T) {
	dir, git := testRepo(t)
	ctx := context.Background()
	write(t, dir, "keep.txt", "a\n")
	write(t, dir, "old.txt", "rename me\n")
	write(t, dir, "gone.txt", "bye\n")
	git("add", ".")
	git("commit", "-qm", "base")
	git("remote", "add", "origin", "https://github.com/c0ze/tincan.git")
	write(t, dir, "keep.txt", "a\nb\n")
	git("mv", "old.txt", "new.txt")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, dir, ".env", "TOKEN=1\n")
	write(t, dir, "certs/server.PEM", "x\n")
	write(t, dir, "big.bin", strings.Repeat("x", MaxFileContent+1))
	write(t, dir, "bin.dat", "a\x00b")
	if runtime.GOOS != "windows" {
		os.Symlink("keep.txt", filepath.Join(dir, "link"))
	}
	p, err := Build(ctx, dir, "uncommitted", "Is this safe?")
	if err != nil {
		t.Fatal(err)
	}
	m := p.Manifest
	if m.RepoID != "github.com/c0ze/tincan" || !m.Complete || m.BaseKind != "commit" || m.IncludedTree == "" {
		t.Fatalf("manifest: %+v", m)
	}
	for path, reason := range map[string]string{".env": "secret", "certs/server.PEM": "secret", "big.bin": "larger than"} {
		if c, ok := change(p, path); !ok || c.Included || !strings.Contains(c.Reason, reason) {
			t.Errorf("%s: %+v", path, c)
		}
	}
	if runtime.GOOS != "windows" {
		if c, _ := change(p, "link"); c.Included || !strings.Contains(c.Reason, "symlink") {
			t.Errorf("symlink: %+v", c)
		}
	}
	if c, _ := change(p, "new.txt"); !c.Included || c.Status != "R" || c.From != "old.txt" {
		t.Errorf("rename: %+v", c)
	}
	if c, _ := change(p, "gone.txt"); !c.Included || c.Status != "D" {
		t.Errorf("deletion: %+v", c)
	}
	if string(p.Files["keep.txt"]) != "a\nb\n" || p.Files["bin.dat"] != nil || p.Files[".env"] != nil {
		t.Errorf("files: %v", keys(p.Files))
	}
	if strings.Contains(string(p.Patch), "TOKEN=1") {
		t.Fatal("secret content in the patch")
	}
	// The patch reproduces included_tree from the base.
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	runIn(t, ws, "clone", "-q", "--no-checkout", dir, "c")
	c := filepath.Join(ws, "c")
	runIn(t, c, "checkout", "-q", "--detach", m.BaseCommit)
	os.WriteFile(filepath.Join(ws, "p.patch"), p.Patch, 0o600)
	runIn(t, c, "-c", "core.symlinks=false", "apply", "--index", "--binary", filepath.Join(ws, "p.patch"))
	if got := runIn(t, c, "write-tree"); got != m.IncludedTree {
		t.Fatalf("applied tree %s != included %s", got, m.IncludedTree)
	}
	if Hash(p) == "" || Hash(p) != Hash(p) {
		t.Fatal("hash")
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("own packet invalid: %v", err)
	}
}

func TestBuildIncompleteWhenPatchTooLarge(t *testing.T) {
	dir, git := testRepo(t)
	write(t, dir, "a.txt", "a\n")
	git("add", ".")
	git("commit", "-qm", "base")
	for i := 0; i < 4; i++ {
		write(t, dir, filepath.Join("gen", string(rune('a'+i))+".txt"), strings.Repeat("line of text\n", 80000)) // ~1 MiB each, 4 MiB patch
	}
	p, err := Build(context.Background(), dir, "uncommitted", "q")
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Complete || p.Patch != nil {
		t.Fatalf("oversized patch kept: complete=%v patch=%d", p.Manifest.Complete, len(p.Patch))
	}
	total := 0
	for _, b := range p.Files {
		total += len(b)
	}
	if total > MaxFilesTotal || len(p.Manifest.FilesOmitted) == 0 {
		t.Fatalf("file budget: total=%d omitted=%v", total, p.Manifest.FilesOmitted)
	}
}

func TestBuildEmptyChangeNoneScopeAndLimits(t *testing.T) {
	dir, git := testRepo(t)
	write(t, dir, "a.txt", "a\n")
	git("add", ".")
	git("commit", "-qm", "base")
	p, err := Build(context.Background(), dir, "uncommitted", "q")
	if err != nil || !p.Manifest.Complete || len(p.Patch) != 0 || len(p.Manifest.Changes) != 0 {
		t.Fatalf("empty change: %+v %v", p, err)
	}
	p, err = Build(context.Background(), t.TempDir(), "", "just a question")
	if err != nil || p.Manifest.Scope != "none" || p.Question != "just a question" {
		t.Fatalf("none scope: %+v %v", p, err)
	}
	if _, err := Build(context.Background(), dir, "", strings.Repeat("q", MaxQuestion+1)); err == nil {
		t.Fatal("oversized question accepted")
	}
}

func TestValidateRejectsHostilePackets(t *testing.T) {
	for name, p := range map[string]Packet{
		"traversal":  {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "A", Path: "../x", Included: true}}}},
		"git dir":    {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "A", Path: ".GIT/config", Included: true}}}},
		"file path":  {Manifest: Manifest{Scope: "none"}, Files: map[string][]byte{"a/../../b": []byte("x")}},
		"collision":  {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "A", Path: "A.txt", Included: true}, {Status: "A", Path: "a.txt", Included: true}}}},
		"rename src": {Manifest: Manifest{Scope: "none", Changes: []Change{{Status: "R", Path: "ok.txt", From: ".tincan/x", Included: true}}}},
	} {
		p := p
		if err := p.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func runIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/packet -run 'TestBuild|TestValidate'`
Expected: build failure (`undefined: Build`).

- [ ] **Step 3: Implement.** Create `internal/packet/packet.go`:

```go
package packet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	MaxPatch       = 3 << 20
	MaxFileContent = 1 << 20
	MaxFilesTotal  = 2 << 20
	MaxPaths       = 5000
	MaxManifest    = 512 << 10
	MaxQuestion    = 64 << 10
	MaxEncoded     = 8 << 20
)

type Change struct {
	Status   string `json:"status"`
	Path     string `json:"path"`
	From     string `json:"from,omitempty"`
	OldMode  string `json:"old_mode,omitempty"`
	NewMode  string `json:"new_mode,omitempty"`
	Included bool   `json:"included"`
	Reason   string `json:"reason,omitempty"`
}

type Manifest struct {
	RepoID       string    `json:"repo_id,omitempty"`
	Scope        string    `json:"scope"`
	BaseKind     string    `json:"base_kind"`
	BaseCommit   string    `json:"base_commit,omitempty"`
	TargetTree   string    `json:"target_tree,omitempty"`
	IncludedTree string    `json:"included_tree,omitempty"`
	Captured     time.Time `json:"captured"`
	Complete     bool      `json:"complete"`
	Notes        []string  `json:"notes,omitempty"`
	Changes      []Change  `json:"changes"`
	FilesOmitted []string  `json:"files_omitted,omitempty"`
}

// Packet is everything a reviewer receives about a change.
type Packet struct {
	Manifest Manifest          `json:"manifest"`
	Patch    []byte            `json:"patch,omitempty"`
	Files    map[string][]byte `json:"files,omitempty"`
	Question string            `json:"question"`
}

// Hash identifies a packet in job identity: SHA-256 of its JSON encoding
// (encoding/json sorts map keys, so equal packets hash equally).
func Hash(p *Packet) string {
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Validate applies the limits and path policy a receiver enforces before
// materializing anything.
func (p *Packet) Validate() error {
	if len(p.Question) > MaxQuestion {
		return fmt.Errorf("question exceeds %d bytes", MaxQuestion)
	}
	if len(p.Patch) > MaxPatch {
		return fmt.Errorf("patch exceeds %d bytes", MaxPatch)
	}
	if len(p.Manifest.Changes) > MaxPaths {
		return fmt.Errorf("more than %d changed paths", MaxPaths)
	}
	if data, _ := json.Marshal(p.Manifest); len(data) > MaxManifest {
		return fmt.Errorf("manifest exceeds %d bytes", MaxManifest)
	}
	var included []string
	for _, c := range p.Manifest.Changes {
		if !c.Included {
			continue
		}
		if err := ValidPath(c.Path); err != nil {
			return err
		}
		included = append(included, c.Path)
		if c.From != "" {
			if err := ValidPath(c.From); err != nil {
				return err
			}
		}
	}
	if col := FoldCollisions(included); len(col) > 0 {
		for a, b := range col {
			return fmt.Errorf("paths %q and %q collide when case is ignored", a, b)
		}
	}
	total := 0
	var files []string
	for path, b := range p.Files {
		if err := ValidPath(path); err != nil {
			return err
		}
		if len(b) > MaxFileContent {
			return fmt.Errorf("file %s exceeds %d bytes", path, MaxFileContent)
		}
		total += len(b)
		files = append(files, path)
	}
	if total > MaxFilesTotal {
		return fmt.Errorf("file contents exceed %d bytes", MaxFilesTotal)
	}
	if col := FoldCollisions(files); len(col) > 0 {
		return fmt.Errorf("packet files collide when case is ignored")
	}
	return nil
}
```

Create `internal/packet/build.go`:

```go
package packet

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var secretPatterns = []string{".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ed25519*", ".npmrc", ".netrc", ".pypirc", "credentials*", "*secret*"}

func secretPath(p string) bool {
	base := strings.ToLower(path.Base(p))
	for _, pat := range secretPatterns {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

// rawChange is one diff-tree -z record.
type rawChange struct {
	oldMode, newMode, oldSHA, newSHA, status, src, dst string
}

func parseRaw(out []byte) ([]rawChange, error) {
	fields := bytes.Split(out, []byte{0})
	var list []rawChange
	for i := 0; i+1 < len(fields); {
		meta := strings.Fields(strings.TrimPrefix(string(fields[i]), ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("unexpected diff-tree record %q", fields[i])
		}
		c := rawChange{oldMode: meta[0], newMode: meta[1], oldSHA: meta[2], newSHA: meta[3], status: meta[4][:1]}
		if c.status == "R" || c.status == "C" {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("truncated rename record")
			}
			c.src, c.dst = string(fields[i+1]), string(fields[i+2])
			i += 3
		} else {
			c.src, c.dst = string(fields[i+1]), string(fields[i+1])
			i += 2
		}
		list = append(list, c)
	}
	return list, nil
}

// blobSizes returns the sizes of the given blobs via one cat-file --batch-check.
func blobSizes(ctx context.Context, g Git, shas []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(shas) == 0 {
		return out, nil
	}
	res, err := g.Run(ctx, []byte(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch-check=%(objectname) %(objectsize)")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(res)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			out[f[0]] = n
		}
	}
	return out, nil
}

// Build captures scope's change in dir as a packet (committees §6.5).
func Build(ctx context.Context, dir, scope, question string) (*Packet, error) {
	if len(question) > MaxQuestion {
		return nil, fmt.Errorf("question exceeds %d bytes", MaxQuestion)
	}
	r, err := Resolve(ctx, dir, scope)
	if err != nil {
		return nil, err
	}
	p := &Packet{Question: question, Manifest: Manifest{Scope: r.Scope, BaseKind: r.BaseKind, BaseCommit: r.BaseCommit,
		TargetTree: r.TargetTree, Captured: time.Now().UTC(), Complete: true, Notes: r.Notes, Changes: []Change{}}}
	if r.BaseKind == "none" {
		return p, nil
	}
	g := Git{Dir: r.Root}
	p.Manifest.RepoID = RepoIDOf(ctx, r.Root)
	out, err := g.Run(ctx, nil, "diff-tree", "-r", "-z", "--find-renames", r.BaseTree, r.TargetTree)
	if err != nil {
		return nil, err
	}
	raws, err := parseRaw(out)
	if err != nil {
		return nil, err
	}
	if len(raws) > MaxPaths {
		return nil, fmt.Errorf("the change touches %d paths (limit %d); narrow the scope", len(raws), MaxPaths)
	}
	var shas []string
	for _, c := range raws {
		if c.status != "D" {
			shas = append(shas, c.newSHA)
		}
	}
	sizes, err := blobSizes(ctx, g, shas)
	if err != nil {
		return nil, err
	}
	// Judge each change; a rename with a bad endpoint becomes D + A.
	var judged []struct {
		Change
		raw rawChange
	}
	judge := func(c Change, raw rawChange) {
		reason := ""
		for _, pth := range []string{c.From, c.Path} {
			if pth == "" {
				continue
			}
			if secretPath(pth) {
				reason = "secret-looking path"
			} else if err := ValidPath(pth); err != nil && reason == "" {
				reason = "path policy: " + err.Error()
			}
		}
		switch {
		case reason != "":
		case raw.newMode == "120000" || raw.oldMode == "120000":
			reason = "symlink (listed, not included)"
		case raw.newMode == "160000" || raw.oldMode == "160000":
			reason = "submodule (listed, not included)"
		case c.Status != "D" && sizes[raw.newSHA] > MaxFileContent:
			reason = fmt.Sprintf("target content larger than %d bytes", MaxFileContent)
		}
		c.Included, c.Reason = reason == "", reason
		judged = append(judged, struct {
			Change
			raw rawChange
		}{c, raw})
	}
	for _, raw := range raws {
		c := Change{Status: raw.status, Path: raw.dst, OldMode: raw.oldMode, NewMode: raw.newMode}
		if raw.status == "C" {
			c.Status = "A" // a copy is an addition of the destination
		}
		if raw.status == "R" {
			c.From = raw.src
			if ValidPath(raw.src) != nil || ValidPath(raw.dst) != nil || secretPath(raw.src) != secretPath(raw.dst) {
				del := raw
				del.status, del.newMode, del.newSHA, del.dst = "D", "000000", strings.Repeat("0", len(raw.oldSHA)), raw.src
				judge(Change{Status: "D", Path: raw.src, OldMode: raw.oldMode}, del)
				add := raw
				add.status, add.oldMode, add.src = "A", "000000", raw.dst
				judge(Change{Status: "A", Path: raw.dst, NewMode: raw.newMode}, add)
				continue
			}
		}
		judge(c, raw)
	}
	var includedPaths []string
	for _, j := range judged {
		if j.Included {
			includedPaths = append(includedPaths, j.Path)
		}
	}
	collide := FoldCollisions(includedPaths)
	var info bytes.Buffer
	zero := strings.Repeat("0", len(r.BaseTree))
	for i := range judged {
		j := &judged[i]
		if j.Included {
			if first, ok := collide[j.Path]; ok {
				j.Included, j.Reason = false, "path collides with "+first+" when case is ignored"
			}
		}
		if j.Included {
			if j.Status == "D" || j.Status == "R" {
				src := j.Path
				if j.Status == "R" {
					src = j.From
				}
				fmt.Fprintf(&info, "0 %s\t%s\x00", zero, src)
			}
			if j.Status != "D" {
				fmt.Fprintf(&info, "%s %s\t%s\x00", j.raw.newMode, j.raw.newSHA, j.Path)
			}
		}
		p.Manifest.Changes = append(p.Manifest.Changes, j.Change)
	}
	tmp, err := os.MkdirTemp("", "tincan-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	ig := Git{Dir: r.Root, Env: []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}}
	if _, err := ig.Run(ctx, nil, "read-tree", r.BaseTree); err != nil {
		return nil, err
	}
	if info.Len() > 0 {
		if _, err := ig.Run(ctx, info.Bytes(), "update-index", "-z", "--index-info"); err != nil {
			return nil, err
		}
	}
	if p.Manifest.IncludedTree, err = ig.Out(ctx, "write-tree"); err != nil {
		return nil, err
	}
	patch, err := g.Run(ctx, nil, "diff", "--binary", "--full-index", "--find-renames", r.BaseTree, p.Manifest.IncludedTree)
	if err != nil {
		return nil, err
	}
	if len(patch) > MaxPatch {
		p.Manifest.Complete = false
		p.Manifest.Notes = append(p.Manifest.Notes, fmt.Sprintf("patch of %d bytes exceeds %d; post-change file contents only", len(patch), MaxPatch))
	} else if len(patch) > 0 {
		p.Patch = patch
	}
	if err := collectFiles(ctx, g, p, judged); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("packet: %w", err)
	}
	return p, nil
}

// collectFiles adds post-change contents of included text files, in manifest
// order, until the MaxFilesTotal budget is spent.
func collectFiles(ctx context.Context, g Git, p *Packet, judged []struct {
	Change
	raw rawChange
}) error {
	var want []struct{ path, sha string }
	for _, j := range judged {
		if j.Included && j.Status != "D" {
			want = append(want, struct{ path, sha string }{j.Path, j.raw.newSHA})
		}
	}
	if len(want) == 0 {
		return nil
	}
	var in bytes.Buffer
	for _, w := range want {
		in.WriteString(w.sha + "\n")
	}
	out, err := g.Run(ctx, in.Bytes(), "cat-file", "--batch")
	if err != nil {
		return err
	}
	br := bufio.NewReader(bytes.NewReader(out))
	total := 0
	p.Files = map[string][]byte{}
	for _, w := range want {
		header, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return fmt.Errorf("cat-file: unexpected header %q", header)
		}
		n, _ := strconv.Atoi(f[2])
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		br.ReadByte() // trailing newline
		if bytes.IndexByte(body, 0) >= 0 {
			continue // binary: the patch carries it
		}
		if total+n > MaxFilesTotal {
			p.Manifest.FilesOmitted = append(p.Manifest.FilesOmitted, w.path)
			continue
		}
		total += n
		p.Files[w.path] = body
	}
	if len(p.Files) == 0 {
		p.Files = nil
	}
	return nil
}
```

The anonymous struct type `struct{ Change; raw rawChange }` appears three times. Name it `judgedChange` in the implementation for readability; the plan shows it inline only to keep each block self-contained.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/packet`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/packet
git commit -m "Build review packets: omissions, included tree, patch and file contents"
```

---

### Task 5: `internal/reviewjob` — job records, identity, tombstones

**Files:**
- Create: `internal/reviewjob/job.go`
- Test: `internal/reviewjob/job_test.go`

**Interfaces:**
- Produces:

  ```go
  type Job struct {
      ID          string    `json:"job_id"`
      ReviewID    string    `json:"review_id"`
      Requester   string    `json:"requester"`
      Preset      string    `json:"preset"`
      PacketSHA   string    `json:"packet_sha256"`
      PromptSHA   string    `json:"prompt_sha256"`
      ExpiresAt   time.Time `json:"expires_at"`
      State       string    `json:"state"` // creating|running|done|error|cancelled
      Workspace   string    `json:"workspace,omitempty"`
      Mode        string    `json:"mode,omitempty"` // checkout|packet
      Note        string    `json:"note,omitempty"`
      Result      string    `json:"result,omitempty"`
      Tombstone   bool      `json:"tombstone,omitempty"`
      Acked       bool      `json:"acked,omitempty"`
      Cleaned     bool      `json:"cleaned,omitempty"`
      Created     time.Time `json:"created"`
      Updated     time.Time `json:"updated"`
  }
  ```

- Produces errors carrying HTTP status: `type Error struct { Status int; Msg string }`, plus the constructors `conflict(msg)` (409), `gone(msg)` (410), `notFound(msg)` (404) and `invalid(msg)` (422).
- Produces `type Store struct{ Dir string }`, which is `<state>/reviews`, with:
  - `func (s Store) Path(id string) string`
  - `func (s Store) Lock(ctx context.Context, id string) (*filelock.Lock, error)`
  - `func (s Store) TryLock(id string) (*filelock.Lock, error)`
  - `func (s Store) Get(id string) (Job, bool, error)`
  - `func (s Store) Save(j Job) error`
  - `func (s Store) List() ([]Job, error)`
  - `func (s Store) Remove(id string) error`
- Produces `func (j Job) Terminal() bool` and `func (j Job) SameIdentity(o Job) bool`.
- Constants: `MaxLifetime = 270 * time.Minute`, `TombstoneKeep = 7 * 24 * time.Hour`, `MaxResult = 1 << 20`.

- [ ] **Step 1: Write the failing test.** Create `internal/reviewjob/job_test.go`:

```go
package reviewjob

import (
	"path/filepath"
	"testing"
	"time"
)

func stateDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStoreRoundTripAndIdentity(t *testing.T) {
	s := Store{Dir: filepath.Join(stateDir(t), "reviews")}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	j := Job{ID: "rv-0123456789abcdef-0", ReviewID: "rv-0123456789abcdef", Preset: "codex", PacketSHA: "p", PromptSHA: "q", ExpiresAt: exp, State: "creating"}
	if err := s.Save(j); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(j.ID)
	if err != nil || !ok || !got.SameIdentity(j) || got.State != "creating" {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
	other := j
	other.PromptSHA = "different"
	if got.SameIdentity(other) {
		t.Fatal("different prompt counted as the same identity")
	}
	if _, ok, _ := s.Get("rv-missing-0"); ok {
		t.Fatal("found a missing job")
	}
	if _, _, err := s.Get("../escape"); err == nil {
		t.Fatal("invalid job id accepted")
	}
	list, _ := s.List()
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	l, err := s.TryLock(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TryLock(j.ID); err == nil {
		t.Fatal("second TryLock succeeded")
	}
	l.Close()
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/reviewjob`
Expected: build failure.

- [ ] **Step 3: Implement.** Create `internal/reviewjob/job.go`:

```go
// Package reviewjob runs committee member jobs on the member's machine
// (committees spec §6.6): it materializes a private workspace from a
// packet, runs the member preset once with a pinned executable, and reports
// status, acknowledgement, cancellation and cleanup.
package reviewjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/c0ze/tincan/v2/internal/envelope"
	"github.com/c0ze/tincan/v2/internal/filelock"
	"github.com/c0ze/tincan/v2/internal/fsutil"
)

const (
	MaxLifetime   = 270 * time.Minute
	TombstoneKeep = 7 * 24 * time.Hour
	MaxResult     = 1 << 20
)

type Job struct {
	ID        string    `json:"job_id"`
	ReviewID  string    `json:"review_id"`
	Requester string    `json:"requester"`
	Preset    string    `json:"preset"`
	PacketSHA string    `json:"packet_sha256"`
	PromptSHA string    `json:"prompt_sha256"`
	ExpiresAt time.Time `json:"expires_at"`
	State     string    `json:"state"`
	Workspace string    `json:"workspace,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Note      string    `json:"note,omitempty"`
	Result    string    `json:"result,omitempty"`
	Tombstone bool      `json:"tombstone,omitempty"`
	Acked     bool      `json:"acked,omitempty"`
	Cleaned   bool      `json:"cleaned,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

func (j Job) Terminal() bool {
	return j.State == "done" || j.State == "error" || j.State == "cancelled"
}

func (j Job) SameIdentity(o Job) bool {
	return j.ID == o.ID && j.PacketSHA == o.PacketSHA && j.PromptSHA == o.PromptSHA && j.Preset == o.Preset && j.ExpiresAt.Equal(o.ExpiresAt)
}

// Error is a job API failure with its HTTP status (committees §6.4 obligation 1).
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func conflict(msg string) error { return &Error{409, msg} }
func gone(msg string) error     { return &Error{410, msg} }
func notFound(msg string) error { return &Error{404, msg} }
func invalid(msg string) error  { return &Error{422, msg} }

// Store keeps job records under <state>/reviews/jobs.
type Store struct{ Dir string }

func (s Store) jobsDir() string { return filepath.Join(s.Dir, "jobs") }

func (s Store) Path(id string) string { return filepath.Join(s.jobsDir(), id+".json") }

func validID(id string) error {
	if err := envelope.ValidComponent(id); err != nil || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid job id %q", id)
	}
	return nil
}

func (s Store) Lock(ctx context.Context, id string) (*filelock.Lock, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if err := fsutil.MkdirPrivate(s.jobsDir()); err != nil {
		return nil, err
	}
	return filelock.Acquire(ctx, filepath.Join(s.jobsDir(), id+".lock"))
}

func (s Store) TryLock(id string) (*filelock.Lock, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if err := fsutil.MkdirPrivate(s.jobsDir()); err != nil {
		return nil, err
	}
	return filelock.Try(filepath.Join(s.jobsDir(), id+".lock"))
}

func (s Store) Get(id string) (Job, bool, error) {
	var j Job
	if err := validID(id); err != nil {
		return j, false, err
	}
	data, err := fsutil.ReadFile(s.Path(id), 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, false, fmt.Errorf("%s: %w", s.Path(id), err)
	}
	return j, true, nil
}

func (s Store) Save(j Job) error {
	if err := validID(j.ID); err != nil {
		return err
	}
	if err := fsutil.MkdirPrivate(s.jobsDir()); err != nil {
		return err
	}
	j.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.Path(j.ID), data)
}

func (s Store) List() ([]Job, error) {
	entries, err := os.ReadDir(s.jobsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || strings.HasPrefix(id, ".") {
			continue
		}
		if j, ok, err := s.Get(id); err == nil && ok {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (s Store) Remove(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	for _, p := range []string{s.Path(id), filepath.Join(s.jobsDir(), id+".lock")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/reviewjob`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reviewjob
git commit -m "Add reviewer job records with identity and locks"
```

---

### Task 6: Packet-only workspaces

**Files:**
- Create: `internal/reviewjob/materialize.go`
- Test: `internal/reviewjob/materialize_test.go`

**Interfaces:**
- Consumes: `packet.Packet`, `packet.ValidPath`.
- Produces:
  - `func writeFileExclusive(root, rel string, data []byte) error`, which creates directories itself and refuses symlinked or pre-existing components with an exclusive 0600 create.
  - `func materializePacket(ws string, p *packet.Packet, reason string) (note string, err error)`, which writes `packet/manifest.json`, `packet/diff.patch` (if any), `packet/question.md` and `packet/files/<path>`.

- [ ] **Step 1: Write the failing test.** Create `internal/reviewjob/materialize_test.go`:

```go
package reviewjob

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/packet"
)

func TestMaterializePacketWritesOnlyInside(t *testing.T) {
	ws := filepath.Join(stateDir(t), "ws")
	p := &packet.Packet{Question: "why?", Patch: []byte("diff --git a/a b/a\n"),
		Manifest: packet.Manifest{Scope: "uncommitted", BaseKind: "commit", Complete: true, Changes: []packet.Change{{Status: "M", Path: "src/a.go", Included: true}}},
		Files:    map[string][]byte{"src/a.go": []byte("package a\n")}}
	note, err := materializePacket(ws, p, "no local clone of github.com/x/y")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"packet/manifest.json", "packet/diff.patch", "packet/question.md", "packet/files/src/a.go"} {
		info, err := os.Lstat(filepath.Join(ws, f))
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s: %v", f, err)
		} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", f, info.Mode().Perm())
		}
	}
	if !strings.Contains(note, "not a checkout") || !strings.Contains(note, "no local clone") || len(note) > 4096 {
		t.Fatalf("note: %q", note)
	}
}

func TestMaterializePacketRejectsHostilePaths(t *testing.T) {
	base := stateDir(t)
	for name, files := range map[string]map[string][]byte{
		"traversal": {"../../etc/x": []byte("x")},
		"git":       {".git/hooks/post-checkout": []byte("x")},
		"collision": {"A.txt": []byte("1"), "a.txt": []byte("2")},
	} {
		ws := filepath.Join(base, name)
		_, err := materializePacket(ws, &packet.Packet{Manifest: packet.Manifest{Scope: "none"}, Files: files}, "")
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(base)); len(entries) > 0 {
		for _, e := range entries {
			if e.Name() == "etc" {
				t.Fatal("wrote outside the workspace")
			}
		}
	}
}

func TestWriteFileExclusiveRefusesSymlinkedDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := stateDir(t)
	outside := stateDir(t)
	os.Symlink(outside, filepath.Join(root, "evil"))
	if err := writeFileExclusive(root, "evil/x.txt", []byte("x")); err == nil {
		t.Fatal("followed a symlinked directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err == nil {
		t.Fatal("file written through the symlink")
	}
	os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old"), 0o600)
	if err := writeFileExclusive(root, "exists.txt", []byte("new")); err == nil {
		t.Fatal("overwrote an existing file")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/reviewjob -run 'TestMaterializePacket|TestWriteFileExclusive'`
Expected: build failure.

- [ ] **Step 3: Implement.** Create `internal/reviewjob/materialize.go`:

```go
package reviewjob

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/c0ze/tincan/v2/internal/packet"
)

// writeFileExclusive creates root/rel as a new 0600 regular file. Every
// directory on the way is created here or must be a real directory (never a
// symlink), and the file itself must not exist (committees §6.6).
func writeFileExclusive(root, rel string, data []byte) error {
	parts := strings.Split(rel, "/")
	dir := root
	for _, c := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, c)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case !info.IsDir() || info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is not a plain directory", dir)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, parts[len(parts)-1]), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// materializePacket builds a packet-only workspace and returns its note.
func materializePacket(ws string, p *packet.Packet, reason string) (string, error) {
	if err := p.Validate(); err != nil {
		return "", invalid(err.Error())
	}
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return "", err
	}
	manifest, _ := json.MarshalIndent(p.Manifest, "", "  ")
	files := map[string][]byte{"packet/manifest.json": manifest, "packet/question.md": []byte(p.Question)}
	if len(p.Patch) > 0 {
		files["packet/diff.patch"] = p.Patch
	}
	for path, b := range p.Files {
		files["packet/files/"+path] = b
	}
	for rel, b := range files {
		if err := packet.ValidPath(rel); err != nil {
			return "", invalid(err.Error())
		}
		if err := writeFileExclusive(ws, rel, b); err != nil {
			return "", err
		}
	}
	note := "Your working directory holds a packet describing the change; this is not a checkout"
	if reason != "" {
		note += " (" + reason + ")"
	}
	note += ". Read packet/manifest.json (changed paths, omissions), packet/diff.patch (the change, when present) and packet/files/ (post-change contents of changed text files)."
	if !p.Manifest.Complete {
		note += " The packet is partial: the full patch was too large."
	}
	if len(note) > 4096 {
		note = note[:4096]
	}
	return note, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/reviewjob`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reviewjob
git commit -m "Materialize packet-only review workspaces with exclusive, symlink-free writes"
```

---

### Task 7: Checkout workspaces from a local candidate

**Files:**
- Modify: `internal/reviewjob/materialize.go` (candidates, `materializeCheckout`)
- Test: `internal/reviewjob/checkout_test.go`

**Interfaces:**
- Consumes: `rooms.Registry.List`, `packet.Git`, `packet.RepoIDOf`, `packet.ValidPath`.
- Produces:
  - `func candidates(ctx context.Context, reg *rooms.Registry, repoID string) []string`: repository roots, newest first.
  - `func materializeCheckout(ctx context.Context, ws string, p *packet.Packet, reg *rooms.Registry) (note string, reason string, err error)`. On success it returns the note. On fallback it returns `reason` (why packet-only) with `err == nil` and no workspace left behind. `err` is only for unexpected local failures.

- [ ] **Step 1: Write the failing test.** Create `internal/reviewjob/checkout_test.go`:

```go
package reviewjob

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture: a requester repo with a change, and a member-side clone
// registered as a room.
func checkoutFixture(t *testing.T) (*packet.Packet, *rooms.Registry, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	req := stateDir(t)
	gitIn(t, req, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(req, "a.txt"), []byte("a\n"), 0o644)
	gitIn(t, req, "add", ".")
	gitIn(t, req, "commit", "-qm", "base")
	gitIn(t, req, "remote", "add", "origin", "git@github.com:c0ze/fixture.git")
	member := filepath.Join(stateDir(t), "member")
	gitIn(t, filepath.Dir(member), "clone", "-q", req, member)
	gitIn(t, member, "remote", "set-url", "origin", "https://github.com/c0ze/fixture")
	os.WriteFile(filepath.Join(req, "a.txt"), []byte("a\nb\n"), 0o644)
	os.WriteFile(filepath.Join(req, "new.txt"), []byte("n\n"), 0o644)
	p, err := packet.Build(context.Background(), req, "uncommitted", "q")
	if err != nil {
		t.Fatal(err)
	}
	reg := rooms.Open(filepath.Join(stateDir(t), "rooms.json"))
	if _, err := reg.Add(member); err != nil {
		t.Fatal(err)
	}
	return p, reg, member
}

func TestCheckoutReproducesIncludedTree(t *testing.T) {
	p, reg, member := checkoutFixture(t)
	ws := filepath.Join(stateDir(t), "ws")
	note, reason, err := materializeCheckout(context.Background(), ws, p, reg)
	if err != nil || reason != "" {
		t.Fatalf("checkout: %q %v", reason, err)
	}
	if got := gitIn(t, ws, "write-tree"); got != p.Manifest.IncludedTree {
		t.Fatalf("tree %s != %s", got, p.Manifest.IncludedTree)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("clone still borrows the candidate's objects")
	}
	if remotes := gitIn(t, ws, "remote"); remotes != "" {
		t.Fatalf("remotes left: %q", remotes)
	}
	if !strings.Contains(note, "git diff --cached") {
		t.Fatalf("note: %q", note)
	}
	// The clone survives aggressive maintenance in the candidate.
	gitIn(t, member, "gc", "-q", "--prune=now")
	gitIn(t, ws, "fsck", "-q")
}

func TestCheckoutFallsBack(t *testing.T) {
	p, reg, member := checkoutFixture(t)
	ctx := context.Background()
	cases := map[string]func(){
		"missing base": func() { p.Manifest.BaseCommit = strings.Repeat("1", len(p.Manifest.BaseCommit)) },
		"partial clone": func() {
			gitIn(t, member, "config", "remote.origin.promisor", "true")
		},
		"other repo":     func() { p.Manifest.RepoID = "github.com/else/where" },
		"incomplete":     func() { p.Manifest.Complete = false },
		"tree mismatch":  func() { p.Manifest.IncludedTree = strings.Repeat("2", len(p.Manifest.IncludedTree)) },
	}
	for name, mutate := range cases {
		orig := *p
		origBase := p.Manifest
		mutate()
		ws := filepath.Join(stateDir(t), "ws")
		_, reason, err := materializeCheckout(ctx, ws, p, reg)
		if err != nil || reason == "" {
			t.Errorf("%s: reason=%q err=%v", name, reason, err)
		}
		if _, err := os.Stat(ws); err == nil {
			t.Errorf("%s: workspace left behind", name)
		}
		*p = orig
		p.Manifest = origBase
		unset := exec.Command("git", "config", "--unset-all", "remote.origin.promisor")
		unset.Dir = member
		unset.Run() // absent key exits non-zero; best effort
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/reviewjob -run TestCheckout`
Expected: build failure (`undefined: materializeCheckout`).

- [ ] **Step 3: Implement.** Append to `internal/reviewjob/materialize.go` (imports `context`, `sort`, `internal/packet`, `internal/rooms`):

```go
// candidates returns the repository roots of registered rooms whose origin
// has repoID, deduplicated and ordered newest first (committees §6.6).
func candidates(ctx context.Context, reg *rooms.Registry, repoID string) []string {
	if repoID == "" || reg == nil {
		return nil
	}
	list, err := reg.List()
	if err != nil {
		return nil
	}
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].LastUsed.Equal(list[j].LastUsed) {
			return list[i].LastUsed.After(list[j].LastUsed)
		}
		return list[i].Path < list[j].Path
	})
	seen := map[string]bool{}
	var out []string
	for _, r := range list {
		if r.Missing {
			continue
		}
		root, err := packet.Git{Dir: r.Path}.Out(ctx, "rev-parse", "--show-toplevel")
		if err != nil || seen[root] {
			continue
		}
		seen[root] = true
		if packet.RepoIDOf(ctx, root) == repoID {
			out = append(out, root)
		}
	}
	return out
}

func partialClone(ctx context.Context, g packet.Git) bool {
	if v, _ := g.Out(ctx, "config", "--get", "extensions.partialClone"); v != "" {
		return true
	}
	out, _ := g.Out(ctx, "config", "--get-regexp", `^remote\..*\.promisor$`)
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == "true" {
			return true
		}
	}
	return false
}

// materializeCheckout builds a checkout workspace: a dissociated clone of the
// first qualifying candidate at the base, with the patch applied to the index
// and verified against included_tree. Any failure removes ws and returns the
// reason for falling back to packet-only mode.
func materializeCheckout(ctx context.Context, ws string, p *packet.Packet, reg *rooms.Registry) (string, string, error) {
	m := p.Manifest
	switch {
	case !m.Complete:
		return "", "the packet is partial (patch too large)", nil
	case m.BaseKind == "none":
		return "", "the review has no code scope", nil
	case m.RepoID == "":
		return "", "the requester's repository has no origin remote", nil
	}
	if err := p.Validate(); err != nil {
		return "", "", invalid(err.Error())
	}
	var chosen string
	for _, root := range candidates(ctx, reg, m.RepoID) {
		g := packet.Git{Dir: root}
		if partialClone(ctx, g) {
			continue
		}
		if m.BaseKind == "commit" {
			if _, err := g.Run(ctx, nil, "cat-file", "-e", m.BaseCommit+"^{commit}"); err != nil {
				continue
			}
		}
		chosen = root
		break
	}
	if chosen == "" {
		return "", "no local clone of " + m.RepoID + " has the base commit", nil
	}
	fail := func(step string, err error) (string, string, error) {
		os.RemoveAll(ws)
		return "", fmt.Sprintf("checkout failed at %s: %v", step, err), nil
	}
	parent := filepath.Dir(ws)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", "", err
	}
	if _, err := (packet.Git{Dir: parent}).Run(ctx, nil, "clone", "-q", "--no-checkout", "--shared", chosen, ws); err != nil {
		return fail("clone", err)
	}
	g := packet.Git{Dir: ws}
	steps := [][]string{{"repack", "-a", "-d", "-q"}, {"remote", "remove", "origin"}}
	for _, s := range steps {
		if _, err := g.Run(ctx, nil, s...); err != nil {
			return fail(s[0], err)
		}
	}
	if err := os.Remove(filepath.Join(ws, ".git", "objects", "info", "alternates")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("alternates", err)
	}
	if m.BaseKind == "commit" {
		if _, err := g.Run(ctx, nil, "checkout", "-q", "--detach", m.BaseCommit); err != nil {
			return fail("checkout", err)
		}
	} else {
		if _, err := g.Run(ctx, nil, "symbolic-ref", "HEAD", "refs/heads/tincan-review"); err != nil {
			return fail("empty base", err)
		}
		if _, err := g.Run(ctx, nil, "read-tree", "--empty"); err != nil {
			return fail("empty base", err)
		}
	}
	if len(p.Patch) > 0 {
		allowed := map[string]bool{}
		for _, c := range m.Changes {
			if c.Included {
				allowed[c.Path] = true
				if c.From != "" {
					allowed[c.From] = true
				}
			}
		}
		patchFile := filepath.Join(parent, filepath.Base(ws)+".patch")
		if err := os.WriteFile(patchFile, p.Patch, 0o600); err != nil {
			return fail("patch", err)
		}
		defer os.Remove(patchFile)
		numstat, err := g.Run(ctx, nil, "apply", "--numstat", "-z", patchFile)
		if err != nil {
			return fail("patch check", err)
		}
		for _, rec := range strings.Split(string(numstat), "\x00") {
			if f := strings.SplitN(rec, "\t", 3); len(f) == 3 && f[2] != "" {
				if err := packet.ValidPath(f[2]); err != nil || !allowed[f[2]] {
					return fail("patch check", fmt.Errorf("patch touches %q, which the manifest does not include", f[2]))
				}
			}
		}
		if _, err := g.Run(ctx, nil, "-c", "core.symlinks=false", "apply", "--index", "--binary", patchFile); err != nil {
			return fail("apply", err)
		}
	}
	tree, err := g.Out(ctx, "write-tree")
	if err != nil {
		return fail("verify", err)
	}
	if tree != m.IncludedTree {
		return fail("verify", fmt.Errorf("tree %s does not match the packet's %s", tree, m.IncludedTree))
	}
	base := m.BaseCommit
	if len(base) > 12 {
		base = base[:12]
	}
	if base == "" {
		base = "an empty base"
	}
	note := fmt.Sprintf("Your working directory is a private, disposable checkout of %s at %s with the change under review staged: `git diff --cached` shows it. Do not modify it.", m.RepoID, base)
	return note, "", nil
}
```

`git apply --numstat -z` reports destination paths only. Rename sources are covered by the manifest's `ValidPath` checks (`p.Validate`) and by git's own `verify_path` during `apply`. The final tree comparison catches any difference.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/reviewjob`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reviewjob
git commit -m "Materialize checkout workspaces from a verified local clone"
```

---

### Task 8: The job service — create, launch, status, ack, cancel

**Files:**
- Create: `internal/reviewjob/service.go`
- Test: `internal/reviewjob/service_test.go`, `internal/reviewjob/main_test.go`

**Interfaces:**
- Consumes: `Store`, `materializeCheckout`, `materializePacket`, `packet.Hash`, `dispatch.Send`, `host.PinnedExecutable`, `host.ResolveExecutable`, `host.WithSession`, `request.Get`/`Cancel`, `host.Cancel`/`Existing`.
- Produces:

  ```go
  type CreateRequest struct {
      JobID     string         `json:"job_id"`
      ReviewID  string         `json:"review_id"`
      Requester string         `json:"requester"`
      Preset    string         `json:"preset"`
      ExpiresAt time.Time      `json:"expires_at"`
      Packet    *packet.Packet `json:"packet"`
      Prompt    string         `json:"prompt"`
      PromptSHA string         `json:"prompt_sha256"`
    }
  type Service struct {
      StateDir string
      Registry *rooms.Registry
      Presets  func() (map[string]host.Preset, error)
      Now      func() time.Time // default time.Now
      Executable string // tincan binary for listeners (dispatch.Options.Executable); "" = os.Executable
      // inflight tracks creations running in this process
  }
  ```

- Produces methods on `*Service`:
  - `Create(ctx, CreateRequest) (Job, error)` records the job and starts creation in the background.
  - `Status(id) (Job, error)`
  - `Ack(ctx, id) (Job, error)`
  - `Cancel(ctx, id string, expiresAt time.Time) (Job, bool, error)`, which returns the job and whether the cancel is acknowledged.
  - `Wait()` waits for background creations (tests and shutdown).
- The `CreateRequest` JSON tags are those used by the `POST api/review-jobs` body (Task 10).

- [ ] **Step 1: Write the failing tests.** Tests need two binaries:
- A real `tincan`, so `dispatch.Send` → `host.Up` can start a detached `tincan serve`. The test builds it once from `./cmd/tincan`. Importing `internal/cli` from these tests is impossible: `cli` imports `web`, which imports `reviewjob` in Task 10.
- A fake reviewer. The test binary itself plays this role when its first argument is `fake-review`.

Create `internal/reviewjob/main_test.go`:

```go
package reviewjob

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tincanBin is a tincan binary built for these tests; detached hosts are
// started from it.
var tincanBin string

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fake-review" {
		os.Exit(fakeReview(os.Args[2:]))
	}
	dir, err := os.MkdirTemp("", "tincan-bin-")
	if err != nil {
		panic(err)
	}
	tincanBin = filepath.Join(dir, "tincan")
	if out, err := exec.Command("go", "build", "-o", tincanBin, "../../cmd/tincan").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building tincan: %v\n%s", err, out)
		tincanBin = ""
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeReview is the reviewer: mode echo (default) appends a line to
// runs.log in its working directory and prints the prompt; pwd prints the
// working directory; sleep sleeps 30 s.
func fakeReview(args []string) int {
	mode := ""
	if len(args) > 0 {
		mode, args = args[0], args[1:]
	}
	switch mode {
	case "sleep":
		time.Sleep(30 * time.Second)
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Print(wd)
	default:
		f, _ := os.OpenFile("runs.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintln(f, "run")
		f.Close()
		fmt.Printf("REVIEW: %s", strings.Join(args, " "))
	}
	return 0
}
```

Create `internal/reviewjob/service_test.go`:

```go
package reviewjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/rooms"
)

func sha(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func testService(t *testing.T, mode string) *Service {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts are not supported on Windows")
	}
	if tincanBin == "" {
		t.Skip("could not build tincan")
	}
	state := stateDir(t)
	t.Setenv("TINCAN_STATE_DIR", state)
	exe, _ := os.Executable()
	return &Service{StateDir: state, Registry: rooms.Open(filepath.Join(state, "rooms.json")), Executable: tincanBin,
		Presets: func() (map[string]host.Preset, error) {
			return map[string]host.Preset{"reviewer": {Exec: []string{exe, "fake-review", mode, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}}, nil
		}}
}

func request(id, prompt string, exp time.Time) CreateRequest {
	return CreateRequest{JobID: id, ReviewID: "rv-0000000000000000", Requester: "cachyos", Preset: "reviewer", ExpiresAt: exp,
		Packet: &packet.Packet{Question: "q", Manifest: packet.Manifest{Scope: "none", BaseKind: "none", Changes: []packet.Change{}}},
		Prompt: prompt, PromptSHA: sha(prompt)}
}

func waitTerminal(t *testing.T, s *Service, id string) Job {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		j, err := s.Status(id)
		if err == nil && j.Terminal() {
			return j
		}
		time.Sleep(100 * time.Millisecond)
	}
	j, _ := s.Status(id)
	t.Fatalf("job %s not terminal: %+v", id, j)
	return j
}

func TestCreateRunsOnceAndReplaysIdentically(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	req := request("rv-0000000000000000-0", "review this", exp)
	j, err := s.Create(ctx, req)
	if err != nil || j.State != "creating" {
		t.Fatalf("create: %+v %v", j, err)
	}
	s.Wait()
	done := waitTerminal(t, s, req.JobID)
	if done.State != "done" || !strings.Contains(done.Result, "REVIEW:") || !strings.Contains(done.Result, "review this") || done.Mode != "packet" {
		t.Fatalf("result: %+v", done)
	}
	again, err := s.Create(ctx, req)
	if err != nil || again.State != "done" {
		t.Fatalf("replay: %+v %v", again, err)
	}
	changed := req
	changed.Prompt, changed.PromptSHA = "other", sha("other")
	var je *Error
	if _, err := s.Create(ctx, changed); !errors.As(err, &je) || je.Status != 409 {
		t.Fatalf("different identity: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	now := time.Now().UTC()
	for name, c := range map[string]struct {
		req    CreateRequest
		status int
	}{
		"expired":      {request("rv-0000000000000000-1", "p", now.Add(-time.Minute)), 410},
		"too far":      {request("rv-0000000000000000-2", "p", now.Add(MaxLifetime+time.Hour)), 422},
		"bad hash":     {func() CreateRequest { r := request("rv-0000000000000000-3", "p", now.Add(time.Hour)); r.PromptSHA = "x"; return r }(), 422},
		"unknown":      {func() CreateRequest { r := request("rv-0000000000000000-4", "p", now.Add(time.Hour)); r.Preset = "nope"; return r }(), 404},
		"bad id":       {request("../x", "p", now.Add(time.Hour)), 422},
		"hostile file": {func() CreateRequest { r := request("rv-0000000000000000-5", "p", now.Add(time.Hour)); r.Packet.Files = map[string][]byte{"../x": nil}; return r }(), 422},
	} {
		var je *Error
		if _, err := s.Create(ctx, c.req); !errors.As(err, &je) || je.Status != c.status {
			t.Errorf("%s: %v (want %d)", name, err, c.status)
		}
	}
}

func TestPinnedExecutableIgnoresWorkspaceDecoys(t *testing.T) {
	s := testService(t, "pwd")
	ctx := context.Background()
	req := request("rv-0000000000000000-6", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	exe, _ := os.Executable()
	req.Packet.Files = map[string][]byte{filepath.Base(exe): []byte("#!/bin/sh\necho decoy\n")}
	if _, err := s.Create(ctx, req); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	j := waitTerminal(t, s, req.JobID)
	if j.State != "done" || strings.Contains(j.Result, "decoy") || !strings.HasPrefix(j.Result, j.Workspace) {
		t.Fatalf("pinned run: %+v", j)
	}
}

func TestCancelKeepsAFinishedResult(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	req := request("rv-0000000000000000-7", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	waitTerminal(t, s, req.JobID)
	j, acked, err := s.Cancel(ctx, req.JobID, time.Time{})
	if err != nil || !acked || j.State != "done" || j.Result == "" {
		t.Fatalf("cancel after finish: %+v %v %v", j, acked, err)
	}
}

func TestCancelRunningAndBeforeCreate(t *testing.T) {
	s := testService(t, "sleep")
	ctx := context.Background()
	req := request("rv-0000000000000000-8", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	deadline := time.Now().Add(20 * time.Second)
	acked := false
	var j Job
	for !acked && time.Now().Before(deadline) {
		var err error
		j, acked, err = s.Cancel(ctx, req.JobID, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !acked || j.State != "cancelled" {
		t.Fatalf("cancel running: %+v %v", j, acked)
	}
	// Cancel before create leaves a tombstone that refuses the create.
	early := request("rv-0000000000000000-9", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	if _, acked, err := s.Cancel(ctx, early.JobID, early.ExpiresAt); err != nil || !acked {
		t.Fatalf("cancel unknown: %v %v", acked, err)
	}
	var je *Error
	if _, err := s.Create(ctx, early); !errors.As(err, &je) || je.Status != 410 {
		t.Fatalf("create after tombstone: %v", err)
	}
}

func TestAck(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	var je *Error
	if _, err := s.Ack(ctx, "rv-0000000000000000-a"); !errors.As(err, &je) || je.Status != 404 {
		t.Fatalf("ack unknown: %v", err)
	}
	req := request("rv-0000000000000000-b", "p", time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	s.Create(ctx, req)
	s.Wait()
	waitTerminal(t, s, req.JobID)
	if j, err := s.Ack(ctx, req.JobID); err != nil || !j.Acked {
		t.Fatalf("ack: %+v %v", j, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/reviewjob -run 'TestCreate|TestPinned|TestCancel|TestAck'`
Expected: build failure (`undefined: Service`).

- [ ] **Step 3: Implement.** Create `internal/reviewjob/service.go`:

```go
package reviewjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/c0ze/tincan/v2/internal/dispatch"
	"github.com/c0ze/tincan/v2/internal/fsutil"
	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/request"
	"github.com/c0ze/tincan/v2/internal/rooms"
	"github.com/c0ze/tincan/v2/internal/spool"
)

type CreateRequest struct {
	JobID     string         `json:"job_id"`
	ReviewID  string         `json:"review_id"`
	Requester string         `json:"requester"`
	Preset    string         `json:"preset"`
	ExpiresAt time.Time      `json:"expires_at"`
	Packet    *packet.Packet `json:"packet"`
	Prompt    string         `json:"prompt"`
	PromptSHA string         `json:"prompt_sha256"`
}

type Service struct {
	StateDir   string
	Registry   *rooms.Registry
	Presets    func() (map[string]host.Preset, error)
	Now        func() time.Time
	Executable string

	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
}

func (s *Service) store() Store { return Store{Dir: filepath.Join(s.StateDir, "reviews")} }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) workspace(id string) string {
	return filepath.Join(s.StateDir, "reviews", "ws", id)
}

func (s *Service) packetPath(id string) string {
	return filepath.Join(s.StateDir, "reviews", "jobs", id+".input.json")
}

// Wait blocks until background creations started by this process finish.
func (s *Service) Wait() { s.wg.Wait() }

// Create validates and records a job, then materializes and launches it in
// the background (committees §6.6). An identical replay returns the job;
// a different identity is a conflict; expired or tombstoned jobs are gone.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Job, error) {
	if err := validID(req.JobID); err != nil {
		return Job{}, invalid(err.Error())
	}
	if req.Packet == nil {
		return Job{}, invalid("packet is required")
	}
	if err := req.Packet.Validate(); err != nil {
		return Job{}, invalid(err.Error())
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	if hex.EncodeToString(sum[:]) != req.PromptSHA {
		return Job{}, invalid("prompt_sha256 does not match the prompt")
	}
	if len(req.Prompt) > 128<<10 {
		return Job{}, invalid("prompt exceeds 128 KiB")
	}
	now := s.now()
	if !req.ExpiresAt.After(now) {
		return Job{}, gone("the job has expired")
	}
	if req.ExpiresAt.After(now.Add(MaxLifetime)) {
		return Job{}, invalid(fmt.Sprintf("expires_at is more than %v away", MaxLifetime))
	}
	presets, err := s.Presets()
	if err != nil {
		return Job{}, err
	}
	if _, ok := presets[req.Preset]; !ok {
		return Job{}, notFound(fmt.Sprintf("no preset %q on this machine", req.Preset))
	}
	if err := spool.ValidName(req.Preset); err != nil {
		return Job{}, invalid(err.Error())
	}
	want := Job{ID: req.JobID, ReviewID: req.ReviewID, Requester: req.Requester, Preset: req.Preset,
		PacketSHA: packet.Hash(req.Packet), PromptSHA: req.PromptSHA, ExpiresAt: req.ExpiresAt.UTC(), State: "creating", Created: now.UTC()}
	st := s.store()
	lock, err := st.Lock(ctx, req.JobID)
	if err != nil {
		return Job{}, err
	}
	defer lock.Close()
	cur, ok, err := st.Get(req.JobID)
	if err != nil {
		return Job{}, err
	}
	if ok {
		if cur.Tombstone && cur.PacketSHA == "" {
			return Job{}, gone("the job was cancelled before it was created")
		}
		if !cur.SameIdentity(want) {
			return Job{}, conflict("a different job already uses this job_id")
		}
		return cur, nil
	}
	if err := writeInput(s.packetPath(req.JobID), req); err != nil {
		return Job{}, err
	}
	if err := st.Save(want); err != nil {
		return Job{}, err
	}
	s.start(req.JobID)
	return want, nil
}

// start runs creation in the background unless it already runs here.
func (s *Service) start(id string) {
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]bool{}
	}
	if s.inflight[id] {
		s.mu.Unlock()
		return
	}
	s.inflight[id] = true
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, id)
			s.mu.Unlock()
		}()
		s.create(context.Background(), id)
	}()
}

// create materializes the workspace and launches the member, holding the job
// lock throughout. It resumes a job left in "creating" by a dead process:
// a request already saved in the recorded workspace is adopted, so a member
// that ran is never run again (committees §6.6, round 5 #1).
func (s *Service) create(ctx context.Context, id string) {
	st := s.store()
	lock, err := st.Lock(ctx, id)
	if err != nil {
		return
	}
	defer lock.Close()
	j, ok, err := st.Get(id)
	if err != nil || !ok || j.State != "creating" {
		return
	}
	fail := func(msg string) {
		j.State, j.Result = "error", "ERROR "+msg
		st.Save(j)
	}
	if j.Workspace != "" {
		if r, err := request.Get(j.Workspace, j.ID); err == nil {
			j.State = "running"
			importRequest(&j, r)
			st.Save(j)
			return
		}
		os.RemoveAll(j.Workspace)
	}
	req, err := readInput(s.packetPath(id))
	if err != nil {
		fail("job input lost: " + err.Error())
		return
	}
	presets, err := s.Presets()
	if err != nil {
		fail(err.Error())
		return
	}
	preset, ok := presets[j.Preset]
	if !ok {
		fail("preset " + j.Preset + " no longer exists")
		return
	}
	pinned, err := pin(preset)
	if err != nil {
		fail(err.Error())
		return
	}
	left := time.Until(j.ExpiresAt)
	if left < time.Minute {
		j.State, j.Result = "error", "ERROR expired before launch"
		st.Save(j)
		return
	}
	pinned.ExecTimeoutSec = int(left / time.Second)
	j.Workspace = s.workspace(id)
	if err := st.Save(j); err != nil { // the workspace is recorded before anything runs
		return
	}
	note, reason, err := materializeCheckout(ctx, j.Workspace, req.Packet, s.Registry)
	j.Mode = "checkout"
	if err == nil && reason != "" {
		j.Mode = "packet"
		note, err = materializePacket(j.Workspace, req.Packet, reason)
	}
	if err != nil {
		os.RemoveAll(j.Workspace)
		fail("workspace: " + err.Error())
		return
	}
	j.Note = note
	body := note + "\n\n" + req.Prompt
	o := dispatch.Options{Room: j.Workspace, Executable: s.Executable, Presets: map[string]host.Preset{j.Preset: pinned}}
	r, err := dispatch.Send(ctx, o, dispatch.SendSpec{Agent: j.Preset, Preset: j.Preset, From: "review", Body: body, RequestID: j.ID})
	if err != nil {
		// A launch failure after the request was saved must never run later.
		if saved, gerr := request.Get(j.Workspace, j.ID); gerr == nil && !saved.Terminal() {
			request.Cancel(ctx, j.Workspace, j.ID)
		}
		fail("launch: " + err.Error())
		return
	}
	j.State = "running"
	importRequest(&j, r)
	st.Save(j)
}

// pin resolves the preset's executable before any workspace exists and
// returns a stateless preset that runs that exact path (committees §6.6).
func pin(p host.Preset) (host.Preset, error) {
	name := p.Exec[0]
	if strings.ContainsAny(name, `/\`) && !filepath.IsAbs(name) {
		return host.Preset{}, fmt.Errorf("preset executable %q is relative; reviewers need a bare name or an absolute path", name)
	}
	path, err := host.ResolveExecutable(name, "")
	if err != nil {
		return host.Preset{}, err
	}
	if err := host.PinnedExecutable(path); err != nil {
		return host.Preset{}, err
	}
	p.Exec = append([]string{path}, p.Exec[1:]...)
	p.Pinned = true
	return host.WithSession(p, "reviewer", "stateless")
}

// importRequest copies a terminal request's outcome into the job.
func importRequest(j *Job, r request.Record) {
	if !r.Terminal() {
		return
	}
	res := r.Result
	if len(res) > MaxResult {
		res = res[:MaxResult] + "\n[truncated]"
	}
	switch {
	case r.Status == "completed" && !strings.HasPrefix(r.Result, "ERROR"):
		j.State = "done"
	case r.Status == "canceled":
		j.State = "cancelled"
	default:
		j.State = "error"
	}
	j.Result = res
}

// refresh imports the request outcome of a running job. The caller holds
// the job lock.
func (s *Service) refresh(j *Job) bool {
	if j.State != "running" || j.Workspace == "" {
		return false
	}
	r, err := request.Get(j.Workspace, j.ID)
	if err != nil || !r.Terminal() {
		return false
	}
	importRequest(j, r)
	return true
}

// Status returns the job, importing a finished result when the lock is free;
// while creation holds the lock it returns the stored record.
func (s *Service) Status(id string) (Job, error) {
	st := s.store()
	j, ok, err := st.Get(id)
	if err != nil {
		return j, invalid(err.Error())
	}
	if !ok {
		return j, notFound("no such job")
	}
	lock, err := st.TryLock(id)
	if err != nil {
		return j, nil
	}
	defer lock.Close()
	if j, _, err = st.Get(id); err == nil && s.refresh(&j) {
		st.Save(j)
	}
	return j, err
}

// Ack records that the requester has the result; cleanup may follow.
func (s *Service) Ack(ctx context.Context, id string) (Job, error) {
	st := s.store()
	lock, err := st.Lock(ctx, id)
	if err != nil {
		return Job{}, invalid(err.Error())
	}
	defer lock.Close()
	j, ok, err := st.Get(id)
	if err != nil {
		return j, err
	}
	if !ok {
		return j, notFound("no such job")
	}
	s.refresh(&j)
	if !j.Terminal() {
		return j, conflict("the job has not finished")
	}
	j.Acked = true
	return j, st.Save(j)
}

// Cancel stops a job. A job that already finished keeps its result and the
// cancel is acknowledged; an unknown job gets a tombstone that refuses a
// later create. acked reports whether the member can no longer run.
func (s *Service) Cancel(ctx context.Context, id string, expiresAt time.Time) (Job, bool, error) {
	st := s.store()
	lock, err := st.Lock(ctx, id)
	if err != nil {
		return Job{}, false, invalid(err.Error())
	}
	defer lock.Close()
	j, ok, err := st.Get(id)
	if err != nil {
		return j, false, err
	}
	if !ok {
		if expiresAt.IsZero() {
			expiresAt = s.now().Add(TombstoneKeep)
		}
		j = Job{ID: id, State: "cancelled", Tombstone: true, ExpiresAt: expiresAt.UTC(), Created: s.now().UTC()}
		return j, true, st.Save(j)
	}
	s.refresh(&j)
	if j.Terminal() {
		j.Tombstone = true
		return j, true, st.Save(j)
	}
	j.Tombstone = true
	if j.State == "creating" && j.Workspace == "" {
		j.State, j.Result = "cancelled", "ERROR canceled before launch"
		return j, true, st.Save(j)
	}
	if j.Workspace != "" {
		if r, err := request.Cancel(ctx, j.Workspace, j.ID); err == nil {
			importRequest(&j, r)
		}
		if !j.Terminal() {
			host.Cancel(ctx, j.Workspace, j.Preset, j.ID)
			if _, alive := host.Existing(ctx, j.Workspace, j.Preset); !alive {
				j.State, j.Result = "cancelled", "ERROR canceled: the reviewer's host is not running"
			}
		}
	}
	if err := st.Save(j); err != nil {
		return j, false, err
	}
	return j, j.Terminal(), nil
}

var errNoInput = errors.New("no job input")

// writeInput / readInput keep the create request (packet and prompt) beside
// the record, so a resumed creation uses unchanged inputs.
func writeInput(path string, req CreateRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, data)
}

func readInput(path string) (CreateRequest, error) {
	var req CreateRequest
	data, err := fsutil.ReadFile(path, packet.MaxEncoded+(1<<20))
	if errors.Is(err, os.ErrNotExist) {
		return req, errNoInput
	}
	if err != nil {
		return req, err
	}
	return req, json.Unmarshal(data, &req)
}

```

The janitor (Task 9) deletes `<id>.input.json` together with the record.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/reviewjob`
Expected: PASS. If a detached host fails to start, read its log at `<workspace>/.tincan/hosts/reviewer.log`.

- [ ] **Step 5: Commit**

```bash
git add internal/reviewjob
git commit -m "Create, run, report, acknowledge and cancel reviewer jobs"
```

---

### Task 9: The job janitor

**Files:**
- Modify: `internal/reviewjob/service.go` (`Janitor`; `Store.Remove` deletes the input file)
- Test: `internal/reviewjob/janitor_test.go`

**Interfaces:**
- Produces `func (s *Service) Janitor(ctx context.Context) error`: one pass, as described in Global Constraints.

- [ ] **Step 1: Write the failing test.** Create `internal/reviewjob/janitor_test.go`:

```go
package reviewjob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
)

func TestJanitorResumesAdoptsAndCleans(t *testing.T) {
	s := testService(t, "echo")
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	// A creation interrupted after the request ran: the janitor adopts it.
	req := request("rv-0000000000000000-c", "p", exp)
	s.Create(ctx, req)
	s.Wait()
	done := waitTerminal(t, s, req.JobID)
	st := s.store()
	j, _, _ := st.Get(req.JobID)
	j.State, j.Result = "creating", ""
	st.Save(j) // as if the process died before recording "running"
	runs := countRuns(t, done.Workspace, req.JobID)
	if err := s.Janitor(ctx); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	j, _ = s.Status(req.JobID)
	if j.State != "done" || countRuns(t, done.Workspace, req.JobID) != runs {
		t.Fatalf("resume did not adopt: %+v", j)
	}

	// Acknowledged jobs are cleaned: listener stopped, workspace removed.
	s.Ack(ctx, req.JobID)
	if err := s.Janitor(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(done.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace kept after ack: %v", err)
	}
	if _, alive := host.Existing(ctx, done.Workspace, "reviewer"); alive {
		t.Fatal("listener still running")
	}

	// Records go 7 days after expiry.
	s.Now = func() time.Time { return exp.Add(TombstoneKeep + time.Hour) }
	s.Janitor(ctx)
	if _, ok, _ := st.Get(req.JobID); ok {
		t.Fatal("old record kept")
	}
}

// countRuns counts the fake reviewer's executions in a workspace.
func countRuns(t *testing.T, ws, _ string) int {
	data, _ := os.ReadFile(filepath.Join(ws, "runs.log"))
	return strings.Count(string(data), "\n")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/reviewjob -run TestJanitor`
Expected: build failure (`s.Janitor undefined`).

- [ ] **Step 3: Implement.** Add to `service.go`:

```go
// Janitor is one maintenance pass (committees §6.6): resume creations whose
// creator died, import finished results, clean up acknowledged, cancelled
// or long-expired jobs, and forget records 7 days after expiry.
func (s *Service) Janitor(ctx context.Context) error {
	st := s.store()
	jobs, err := st.List()
	if err != nil {
		return err
	}
	now := s.now()
	for _, j := range jobs {
		if now.After(j.ExpiresAt.Add(TombstoneKeep)) {
			s.cleanup(ctx, j)
			st.Remove(j.ID)
			os.Remove(s.packetPath(j.ID))
			continue
		}
		lock, err := st.TryLock(j.ID)
		if err != nil {
			continue // creation or another operation holds it
		}
		cur, ok, err := st.Get(j.ID)
		if err != nil || !ok {
			lock.Close()
			continue
		}
		if cur.State == "creating" {
			lock.Close()
			s.start(cur.ID)
			continue
		}
		if s.refresh(&cur) {
			st.Save(cur)
		}
		due := cur.Acked || cur.State == "cancelled" || now.After(cur.ExpiresAt.Add(time.Hour))
		if due && !cur.Cleaned {
			s.cleanup(ctx, cur)
			cur.Cleaned = true
			st.Save(cur)
		}
		lock.Close()
	}
	return nil
}

// cleanup stops the job's listener and removes its workspace; the
// workspace must lie under <state>/reviews/ws.
func (s *Service) cleanup(ctx context.Context, j Job) {
	if j.Workspace == "" {
		return
	}
	root := filepath.Join(s.StateDir, "reviews", "ws") + string(filepath.Separator)
	if !strings.HasPrefix(j.Workspace, root) {
		return
	}
	if _, err := os.Stat(j.Workspace); err != nil {
		return
	}
	if err := host.Down(ctx, j.Workspace, j.Preset, 10*time.Second); err != nil {
		return // try again next pass
	}
	os.RemoveAll(j.Workspace)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/reviewjob`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reviewjob
git commit -m "Add the reviewer job janitor"
```

---

### Task 10: Job API in `tincan web`, the janitor loop, and docs

**Files:**
- Create: `internal/web/jobs.go`
- Modify: `internal/web/server.go` (`Server.jobs *reviewjob.Service` when `StateDir != ""`; `Run` starts the janitor loop and waits for creations on shutdown), `internal/web/api.go` (routes)
- Modify: `docs/web.md` ("Reviewer jobs" subsection under Committees)
- Test: `internal/web/jobs_test.go`

**Interfaces:**
- Consumes: `reviewjob.Service`, `(*peer).call`.
- Produces the routes:
  - `POST /api/review-jobs` (body ≤ 8 MiB → `reviewjob.CreateRequest`), answering 202 with the job;
  - `GET /api/review-jobs/{job_id}`;
  - `POST /api/review-jobs/{job_id}/ack` (≤ 1 KiB);
  - `POST /api/review-jobs/{job_id}/cancel` (≤ 1 KiB, body `{"expires_at"?}`), answering `{"job": …, "acked": bool}`.
- Produces `type jobView struct` (the job without `Workspace`): `job_id`, `state`, `mode`, `note`, `result`, `expires_at`, `created`, `updated`, `acked`.
- Errors: `*reviewjob.Error` maps to its `Status`. When `StateDir == ""` every route answers 503.

- [ ] **Step 1: Write the failing test.** Create `internal/web/jobs_test.go`:

```go
package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c0ze/tincan/v2/internal/host"
	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

func TestReviewJobsOverTheMachineLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("detached hosts")
	}
	hub, _, _, member := peerPair(t)
	state, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TINCAN_STATE_DIR", state)
	member.cfg.StateDir = state
	bin := filepath.Join(t.TempDir(), "reviewer")
	os.WriteFile(bin, []byte("#!/bin/sh\necho REVIEWED \"$1\"\n"), 0o755)
	member.cfg.Dispatch.Presets = map[string]host.Preset{"reviewer": {Exec: []string{bin, "{body}"}, Stdin: "none", Reply: "stdout", ExecTimeoutSec: 60}}
	member.initJobs() // Server builds its reviewjob.Service from cfg
	member.jobs.Executable = buildTincan(t)

	prompt := "look at this"
	sum := sha256.Sum256([]byte(prompt))
	req := reviewjob.CreateRequest{JobID: "rv-1111111111111111-0", ReviewID: "rv-1111111111111111", Requester: "cachyos", Preset: "reviewer",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second), Prompt: prompt, PromptSHA: hex.EncodeToString(sum[:]),
		Packet: &packet.Packet{Question: "q", Manifest: packet.Manifest{Scope: "none", BaseKind: "none", Changes: []packet.Change{}}}}
	p := hub.peers["macmini"]
	var created jobView
	if err := p.call(context.Background(), "POST", "review-jobs", req, 1<<20, &created); err != nil || created.State != "creating" {
		t.Fatalf("create: %+v %v", created, err)
	}
	var got jobView
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := p.call(context.Background(), "GET", "review-jobs/"+req.JobID, nil, 2<<20, &got); err == nil && got.State == "done" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.State != "done" || !strings.Contains(got.Result, "REVIEWED") {
		t.Fatalf("status: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), state) {
		t.Fatal("status exposes the workspace path")
	}
	if err := p.call(context.Background(), "POST", "review-jobs/"+req.JobID+"/ack", map[string]string{}, 1<<20, nil); err != nil {
		t.Fatalf("ack: %v", err)
	}
	var pe *PeerError
	changed := req
	changed.Prompt, changed.PromptSHA = "x", "y"
	if err := p.call(context.Background(), "POST", "review-jobs", changed, 1<<20, nil); !asPeerError(err, &pe) || pe.Status != 422 {
		t.Fatalf("bad hash: %v", err)
	}
	var cancel struct {
		Acked bool `json:"acked"`
	}
	if err := p.call(context.Background(), "POST", "review-jobs/rv-2222222222222222-0/cancel", map[string]string{}, 1<<20, &cancel); err != nil || !cancel.Acked {
		t.Fatalf("cancel unknown: %+v %v", cancel, err)
	}
	member.jobs.Wait()
}

func asPeerError(err error, target **PeerError) bool {
	pe, ok := err.(*PeerError)
	if ok {
		*target = pe
	}
	return ok
}

func TestReviewJobsNeedStateDir(t *testing.T) {
	s := testServer(t)
	if rec := do(t, s.Handler(), "GET", "/api/review-jobs/rv-1-0", "", ownerHdr()); rec.Code != 503 {
		t.Fatalf("no state dir: %d", rec.Code)
	}
}
```

Detached hosts need a real `tincan` binary. Importing `internal/cli` here would be an import cycle (`cli` imports `web`). Add this helper to `jobs_test.go` (imports `os/exec`, `sync`):

```go
var (
	tincanOnce sync.Once
	tincanPath string
)

// buildTincan builds ./cmd/tincan once per test binary.
func buildTincan(t *testing.T) string {
	t.Helper()
	tincanOnce.Do(func() {
		dir, err := os.MkdirTemp("", "tincan-bin-")
		if err != nil {
			return
		}
		p := filepath.Join(dir, "tincan")
		if exec.Command("go", "build", "-o", p, "../../cmd/tincan").Run() == nil {
			tincanPath = p
		}
	})
	if tincanPath == "" {
		t.Skip("could not build tincan")
	}
	return tincanPath
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/web -run TestReviewJobs`
Expected: build failure (`member.initJobs undefined`).

- [ ] **Step 3: Implement.** Create `internal/web/jobs.go`:

```go
package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/c0ze/tincan/v2/internal/packet"
	"github.com/c0ze/tincan/v2/internal/reviewjob"
)

type jobView struct {
	JobID     string    `json:"job_id"`
	State     string    `json:"state"`
	Mode      string    `json:"mode,omitempty"`
	Note      string    `json:"note,omitempty"`
	Result    string    `json:"result,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Acked     bool      `json:"acked,omitempty"`
}

func viewJob(j reviewjob.Job) jobView {
	return jobView{JobID: j.ID, State: j.State, Mode: j.Mode, Note: j.Note, Result: j.Result, ExpiresAt: j.ExpiresAt, Created: j.Created, Updated: j.Updated, Acked: j.Acked}
}

// initJobs builds the reviewer job service when a state directory is set.
func (s *Server) initJobs() {
	if s.cfg.StateDir == "" {
		return
	}
	s.jobs = &reviewjob.Service{StateDir: s.cfg.StateDir, Registry: s.cfg.Registry, Presets: s.cfg.Dispatch.PresetMap, Executable: s.cfg.Dispatch.Executable}
}

func (s *Server) jobsReady(w http.ResponseWriter) bool {
	if s.jobs == nil {
		s.fail(w, http.StatusServiceUnavailable, errNoState)
		return false
	}
	return true
}

func (s *Server) failJob(w http.ResponseWriter, err error) {
	var je *reviewjob.Error
	if errors.As(err, &je) {
		s.fail(w, je.Status, err)
		return
	}
	s.fail(w, http.StatusInternalServerError, err)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	var req reviewjob.CreateRequest
	if err := readJSON(w, r, packet.MaxEncoded, &req); err != nil {
		s.failBody(w, err)
		return
	}
	j, err := s.jobs.Create(r.Context(), req)
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, viewJob(j))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	j, err := s.jobs.Status(r.PathValue("job"))
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewJob(j))
}

func (s *Server) ackJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	var in struct{}
	if err := readJSON(w, r, 1<<10, &in); err != nil {
		s.failBody(w, err)
		return
	}
	j, err := s.jobs.Ack(r.Context(), r.PathValue("job"))
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, viewJob(j))
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if !s.jobsReady(w) {
		return
	}
	var in struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := readJSON(w, r, 1<<10, &in); err != nil {
		s.failBody(w, err)
		return
	}
	j, acked, err := s.jobs.Cancel(r.Context(), r.PathValue("job"), in.ExpiresAt)
	if err != nil {
		s.failJob(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"job": viewJob(j), "acked": acked})
}

// runJobJanitor runs the reviewer job janitor every minute until ctx ends,
// then waits for background creations.
func (s *Server) runJobJanitor(ctx context.Context) {
	if s.jobs == nil {
		return
	}
	defer s.jobs.Wait()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := s.jobs.Janitor(ctx); err != nil {
			s.logOnce("jobs.janitor", "tincan web: review jobs: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
```

`readJSON` with an empty body returns an `unexpected end of JSON input` error. For ack and cancel, treat an empty body as `{}`: skip the decode when `r.ContentLength == 0`, or make the peer client always send `{}` (it does when `in` is non-nil). Handle `ContentLength == 0` in both handlers.

In `server.go`: add `jobs *reviewjob.Service` to `Server`, call `s.initJobs()` in `New` after `s.routes()`, and in `Run` add `go s.runJobJanitor(ctx)`. In `api.go`'s `apiRoutes`:

```go
	s.mux.HandleFunc("POST /api/review-jobs", s.createJob)
	s.mux.HandleFunc("GET /api/review-jobs/{job}", s.getJob)
	s.mux.HandleFunc("POST /api/review-jobs/{job}/ack", s.ackJob)
	s.mux.HandleFunc("POST /api/review-jobs/{job}/cancel", s.cancelJob)
```

In `docs/web.md`, under Committees, add a "Reviewer jobs" subsection:
- another machine's coordinator creates jobs through `api/review-jobs`;
- each job gets a private workspace under `<state>/reviews/ws/`: a verified checkout when a registered clone of the same repository has the base commit (never fetched), otherwise the packet alone;
- the member preset runs once, statelessly, with its executable resolved before the workspace exists;
- results stay until acknowledged;
- the janitor cleans up every minute;
- the listed secret-looking files, symlinks and submodules are never sent;
- residual risk: a reviewer with a permission bypass, or a CLI that honours repository configuration, can act outside the workspace; use read-only presets (`claude --permission-mode plan --setting-sources user`, `codex exec -s read-only`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/web ./internal/reviewjob ./internal/packet ./internal/host`
Expected: PASS.

- [ ] **Step 5: Run the whole suite, vet and format**

Run: `gofmt -l . ; go vet ./... && GOOS=windows go vet ./... && go test ./...`
Expected: no gofmt output; every package passes.

- [ ] **Step 6: Commit**

```bash
git add internal/web internal/reviewjob docs/web.md
git commit -m "Serve reviewer jobs over the machine link and run their janitor"
```
