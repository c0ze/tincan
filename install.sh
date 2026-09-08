#!/usr/bin/env bash
set -euo pipefail

# tincan installer — installs the `tincan` binary and the /tell + /listen skills.
#
# Usage:
#   ./install.sh                 install binary + shared and Claude skills
#   ./install.sh --skills-only   only (re)install the skills
#   ./install.sh --bin-only      only install the binary
#
# Env overrides:
#   AGENTS_SKILLS_DIR   default: ~/.agents/skills
#   CLAUDE_SKILLS_DIR   default: ~/.claude/skills
#   GEMINI_COMMANDS_DIR default: ~/.gemini/commands (if ~/.gemini exists)

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENTS_SKILLS_DIR="${AGENTS_SKILLS_DIR:-$HOME/.agents/skills}"
CLAUDE_SKILLS_DIR="${CLAUDE_SKILLS_DIR:-$HOME/.claude/skills}"
DO_BIN=1
DO_SKILLS=1

for arg in "$@"; do
  case "$arg" in
    --skills-only) DO_BIN=0 ;;
    --bin-only)    DO_SKILLS=0 ;;
    -h|--help)     sed -n '4,15p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

if [ "$DO_BIN" = 0 ] && [ "$DO_SKILLS" = 0 ]; then
  echo "!! --skills-only and --bin-only cannot be combined" >&2
  exit 2
fi

install_bin() {
  if ! command -v go >/dev/null 2>&1; then
    echo "!! Go toolchain not found. Install Go 1.25+ or grab a prebuilt binary." >&2
    exit 1
  fi
  if [ -d "$REPO_DIR/cmd/tincan" ]; then
    echo ">> building tincan from source ($REPO_DIR)"
    ( cd "$REPO_DIR" && go install ./cmd/tincan )
  else
    echo ">> installing tincan from GitHub"
    go install github.com/c0ze/tincan/v2/cmd/tincan@latest
  fi
  local gobin; gobin="$(go env GOBIN)"; [ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
  echo ">> installed to $gobin/tincan"
  case ":$PATH:" in
    *":$gobin:"*) : ;;
    *) echo "   NOTE: add $gobin to your PATH." ;;
  esac
}

# Replace a managed file without writing through a destination symlink. Shared
# skill roots often alias each other, or point straight back to this checkout.
install_file() {
  local src="$1" dst="$2" staged backup
  if [ "$src" -ef "$dst" ] || cmp -s "$src" "$dst"; then
    echo ">> already current: $dst"
    return
  fi
  if [ -d "$dst" ]; then
    echo "!! expected a file, found a directory: $dst" >&2
    return 1
  fi
  mkdir -p "$(dirname "$dst")"
  staged="$(mktemp "${dst}.tmp.XXXXXX")"
  if ! cp "$src" "$staged"; then
    rm -f "$staged"
    return 1
  fi
  if [ -e "$dst" ] || [ -L "$dst" ]; then
    backup="$(mktemp "${dst}.backup.XXXXXX")"
    if ! mv -f "$dst" "$backup"; then
      rm -f "$staged" "$backup"
      return 1
    fi
    echo ">> preserved previous file: $backup"
  fi
  mv -f "$staged" "$dst"
  echo ">> installed: $dst"
}

install_skills() {
  local s root src
  # Validate before changing any installation.
  for s in tell listen; do
    [ -f "$REPO_DIR/skills/$s/SKILL.md" ] || { echo "!! missing skills/$s/SKILL.md" >&2; exit 1; }
  done
  for root in "$AGENTS_SKILLS_DIR" "$CLAUDE_SKILLS_DIR"; do
    for s in tell listen; do
      for src in "$REPO_DIR/skills/$s"/*.md; do
        install_file "$src" "$root/$s/$(basename "$src")"
      done
    done
  done
  echo ">> Restart your agent session to discover tell/listen skills."
  echo ">> Codex: use 'listen as codex' or 'tell codex <task>'; Claude: /listen or /tell."

  # Gemini CLI (and Antigravity if it reads gemini-style commands).
  if [ -n "${GEMINI_COMMANDS_DIR:-}" ] || [ -d "$HOME/.gemini" ]; then
    install_file "$REPO_DIR/skills/gemini/listen.toml" "${GEMINI_COMMANDS_DIR:-$HOME/.gemini/commands}/listen.toml"
    echo ">> Gemini CLI: use /listen."
  else
    echo "-- Gemini CLI not detected (~/.gemini missing); skipped its /listen command."
  fi
}

[ "$DO_BIN" = 1 ] && install_bin
[ "$DO_SKILLS" = 1 ] && install_skills
echo "done."
