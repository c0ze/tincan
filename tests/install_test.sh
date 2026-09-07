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

echo 'Installer regression tests passed.'
