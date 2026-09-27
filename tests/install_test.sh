#!/usr/bin/env bash
# Installer regression checks; all destinations and fixtures are temporary.
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/tincan-install.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
new_case() {
  case_dir="$test_root/$1"
  mkdir -p "$case_dir/repo"
  cp "$repo_dir/install.sh" "$case_dir/repo/"
  cp -R "$repo_dir/skills" "$case_dir/repo/"
}
install_skills() {
  AGENTS_SKILLS_DIR="$case_dir/agents" \
  CLAUDE_SKILLS_DIR="$case_dir/claude" \
  GEMINI_COMMANDS_DIR="$case_dir/gemini" \
    bash "$case_dir/repo/install.sh" --skills-only >"$case_dir/output"
}
check_file() {
  cmp -s "$1" "$2" || fail "contents differ: $1 and $2"
}
no_backups() {
  local files=("$1".backup.*)
  [ ! -e "${files[0]}" ] && [ ! -L "${files[0]}" ] || fail "unexpected backup for $1"
}

new_case independent
install_skills
for root in agents claude; do
  for skill in tell listen; do
    check_file "$case_dir/repo/skills/$skill/SKILL.md" "$case_dir/$root/$skill/SKILL.md"
  done
done
check_file "$case_dir/repo/skills/gemini/listen.toml" "$case_dir/gemini/listen.toml"
install_skills
no_backups "$case_dir/agents/tell/SKILL.md"
no_backups "$case_dir/claude/listen/SKILL.md"
printf '%s\n' 'local customization' >"$case_dir/agents/tell/SKILL.md"
printf '%s\n' 'keep this extra file' >"$case_dir/agents/tell/notes.md"
install_skills
backups=("$case_dir/agents/tell/SKILL.md".backup.*)
[ "${#backups[@]}" -eq 1 ] || fail 'expected one backup'
[ "$(cat "${backups[0]}")" = 'local customization' ] || fail 'customization was not backed up'
[ "$(cat "$case_dir/agents/tell/notes.md")" = 'keep this extra file' ] || fail 'unmanaged file was changed'

new_case aliased_roots
mkdir -p "$case_dir/agents/tell"
printf '%s\n' 'shared customization' >"$case_dir/agents/tell/SKILL.md"
ln -s agents "$case_dir/claude"
install_skills
install_skills
backups=("$case_dir/agents/tell/SKILL.md".backup.*)
[ "${#backups[@]}" -eq 1 ] || fail 'aliased roots backed up twice'
[ "$(cat "${backups[0]}")" = 'shared customization' ] || fail 'shared backup was overwritten'
check_file "$case_dir/repo/skills/tell/SKILL.md" "$case_dir/claude/tell/SKILL.md"

new_case checkout_alias
ln -s repo/skills "$case_dir/agents"
ln -s agents "$case_dir/claude"
install_skills
no_backups "$case_dir/repo/skills/listen/SKILL.md"
check_file "$repo_dir/skills/tell/SKILL.md" "$case_dir/repo/skills/tell/SKILL.md"

new_case file_links
mkdir -p "$case_dir/agents/tell" "$case_dir/agents/listen"
printf '%s\n' 'external contents' >"$case_dir/agents/tell/external.md"
ln -s external.md "$case_dir/agents/tell/SKILL.md"
ln "$case_dir/repo/skills/listen/SKILL.md" "$case_dir/agents/listen/SKILL.md"
install_skills
[ ! -L "$case_dir/agents/tell/SKILL.md" ] || fail 'installer wrote through destination symlink'
[ "$(cat "$case_dir/agents/tell/external.md")" = 'external contents' ] || fail 'external symlink target was modified'
backups=("$case_dir/agents/tell/SKILL.md".backup.*)
[ -L "${backups[0]}" ] || fail 'symlink itself was not preserved'
[ "$(cat "${backups[0]}")" = 'external contents' ] || fail 'relative backup symlink changed meaning'
no_backups "$case_dir/agents/listen/SKILL.md"

new_case broken_link
mkdir -p "$case_dir/agents/tell"
ln -s absent.md "$case_dir/agents/tell/SKILL.md"
install_skills
backups=("$case_dir/agents/tell/SKILL.md".backup.*)
[ -L "${backups[0]}" ] || fail 'broken symlink was not preserved'
check_file "$case_dir/repo/skills/tell/SKILL.md" "$case_dir/agents/tell/SKILL.md"

new_case missing_source
rm "$case_dir/repo/skills/listen/SKILL.md"
if install_skills 2>"$case_dir/error"; then fail 'missing skill source accepted'; fi
[ ! -e "$case_dir/agents" ] || fail 'changed installation before validating all skills'

new_case invalid_args
if bash "$case_dir/repo/install.sh" --skills-only --bin-only >"$case_dir/output" 2>&1; then
  fail 'conflicting install modes accepted'
fi

# Record Go invocations without downloading modules or installing real binaries.
fake_go() {
  mkdir -p "$case_dir/bin"
  cat >"$case_dir/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'env GOBIN') printf '%s\n' "$TINCAN_TEST_GOBIN" ;;
  'install '*)
    printf '%s\n' "$*" >"$TINCAN_TEST_RECORD"
    pwd -P >"$TINCAN_TEST_RECORD.cwd"
    ;;
  *) exit 1 ;;
esac
GO
  chmod +x "$case_dir/bin/go"
}
install_binary() {
  PATH="$case_dir/bin:$PATH" \
  TINCAN_TEST_GOBIN="$case_dir/bin" \
  TINCAN_TEST_RECORD="$case_dir/go-args" \
    bash "$case_dir/repo/install.sh" --bin-only >"$case_dir/output"
}

new_case released_binary
fake_go
install_binary
[ "$(cat "$case_dir/go-args")" = 'install github.com/c0ze/tincan/v2/cmd/tincan@latest' ] ||
  fail 'standalone installer did not select the stable v2 module'
[ ! -e "$case_dir/agents" ] || fail 'binary-only install changed skills'

new_case checkout_binary
mkdir -p "$case_dir/repo/cmd/tincan"
fake_go
install_binary
[ "$(cat "$case_dir/go-args")" = 'install ./cmd/tincan' ] || fail 'checkout was not built locally'
[ "$(cat "$case_dir/go-args.cwd")" = "$(cd "$case_dir/repo" && pwd -P)" ] ||
  fail 'checkout build used the wrong working directory'

# A local release mirror stands in for GitHub; curl reads it via file://.
fake_release() {
  local os arch tag=v9.9.9 name
  case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) os=windows ;; esac
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; *) arch=arm64 ;; esac
  name="tincan_${tag#v}_${os}_${arch}.tar.gz"
  mkdir -p "$case_dir/release/download/$tag" "$case_dir/stage" "$case_dir/raw/$tag"
  printf '#!/bin/sh\necho fake tincan\n' >"$case_dir/stage/tincan"
  chmod +x "$case_dir/stage/tincan"
  tar -czf "$case_dir/release/download/$tag/$name" -C "$case_dir/stage" tincan
  ( cd "$case_dir/release/download/$tag" && { sha256sum "$name" 2>/dev/null || shasum -a 256 "$name"; } >checksums.txt )
  cp -R "$repo_dir/skills" "$case_dir/raw/$tag/"
  release_name="$name"
}
install_release() {
  PATH="${test_path:-$PATH}" \
  TINCAN_RELEASE_BASE="file://$case_dir/release" \
  TINCAN_RAW_BASE="file://$case_dir/raw" \
  TINCAN_VERSION=v9.9.9 \
  TINCAN_BIN_DIR="$case_dir/bin" \
  AGENTS_SKILLS_DIR="$case_dir/agents" \
  CLAUDE_SKILLS_DIR="$case_dir/claude" \
  GEMINI_COMMANDS_DIR="$case_dir/gemini" \
    bash "$case_dir/repo/install.sh" --from-release "$@" >"$case_dir/output" 2>&1
}

new_case release_download
fake_release
install_release --bin-only || fail "release install failed: $(cat "$case_dir/output")"
[ "$("$case_dir/bin/tincan")" = 'fake tincan' ] || fail 'release binary not installed'

new_case release_checksum
fake_release
printf 'tampered' >>"$case_dir/release/download/v9.9.9/$release_name"
if install_release --bin-only; then fail 'tampered release accepted'; fi
[ ! -e "$case_dir/bin/tincan" ] || fail 'tampered binary was installed'

new_case remote_skills
fake_release
rm -rf "$case_dir/repo/skills"
install_release --skills-only || fail "remote skill install failed: $(cat "$case_dir/output")"
check_file "$repo_dir/skills/tell/SKILL.md" "$case_dir/claude/tell/SKILL.md"
check_file "$repo_dir/skills/gemini/listen.toml" "$case_dir/gemini/listen.toml"

# Fake client CLIs record their arguments; a separate fake tincan earlier on
# PATH must trigger the shadow warning.
fake_clients() {
  mkdir -p "$case_dir/clients"
  for c in claude codex; do
    printf '#!/bin/sh\necho "$*" >>"%s/%s.args"\n' "$case_dir" "$c" >"$case_dir/clients/$c"
    chmod +x "$case_dir/clients/$c"
  done
  printf '#!/bin/sh\necho old tincan\n' >"$case_dir/clients/tincan"
  chmod +x "$case_dir/clients/tincan"
}

new_case mcp_registration
fake_release
fake_clients
test_path="$case_dir/clients:$PATH" install_release --bin-only --mcp ||
  fail "--mcp install failed: $(cat "$case_dir/output")"
grep -qx "mcp add -s user tincan -- $case_dir/bin/tincan mcp" "$case_dir/claude.args" ||
  fail "claude registration: $(cat "$case_dir/claude.args")"
grep -qx "mcp add tincan -- $case_dir/bin/tincan mcp" "$case_dir/codex.args" ||
  fail "codex registration: $(cat "$case_dir/codex.args")"
if grep -q -- '--room' "$case_dir/claude.args" "$case_dir/codex.args"; then
  fail 'registration pinned a room'
fi
grep -q "WARNING: 'tincan' on PATH is $case_dir/clients/tincan" "$case_dir/output" ||
  fail 'shadowing tincan was not reported'

new_case no_shadow_warning
fake_release
test_path="$case_dir/bin:$PATH" install_release --bin-only || fail "install failed: $(cat "$case_dir/output")"
if grep -q WARNING "$case_dir/output"; then fail "unexpected warning: $(cat "$case_dir/output")"; fi

# Without Go on PATH the installer falls back to the release on its own.
new_case no_go_fallback
fake_release
mkdir -p "$case_dir/nogo"
for tool in bash sh env uname mktemp curl awk sha256sum shasum perl tar gzip \
    cp chmod mv rm mkdir dirname cut sed cmp cat basename; do
  src="$(command -v "$tool" || true)"
  [ -n "$src" ] && ln -s "$src" "$case_dir/nogo/$tool"
done
PATH="$case_dir/nogo" \
TINCAN_RELEASE_BASE="file://$case_dir/release" \
TINCAN_VERSION=v9.9.9 \
TINCAN_BIN_DIR="$case_dir/bin" \
  "$case_dir/nogo/bash" "$case_dir/repo/install.sh" --bin-only >"$case_dir/output" 2>&1 ||
  fail "no-Go install failed: $(cat "$case_dir/output")"
grep -q 'Go not found' "$case_dir/output" || fail 'did not report the release fallback'
[ "$("$case_dir/bin/tincan")" = 'fake tincan' ] || fail 'no-Go install missing binary'

# curl | bash: the script arrives on stdin and runs outside any checkout.
new_case piped
fake_release
mkdir -p "$case_dir/elsewhere"
( cd "$case_dir/elsewhere" &&
  TINCAN_RAW_BASE="file://$case_dir/raw" \
  TINCAN_VERSION=v9.9.9 \
  AGENTS_SKILLS_DIR="$case_dir/agents" \
  CLAUDE_SKILLS_DIR="$case_dir/claude" \
  GEMINI_COMMANDS_DIR="$case_dir/gemini" \
    bash -s -- --skills-only <"$case_dir/repo/install.sh" >"$case_dir/output" 2>&1 ) ||
  fail "piped install failed: $(cat "$case_dir/output")"
check_file "$repo_dir/skills/listen/SKILL.md" "$case_dir/agents/listen/SKILL.md"
[ -z "$(ls -A "$case_dir/elsewhere")" ] || fail 'piped install wrote into the working directory'

echo 'Installer regression tests passed.'
