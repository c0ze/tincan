#!/usr/bin/env bash
set -euo pipefail

# tincan installer — installs the `tincan` binary and the /tell + /listen skills.
#
# Usage:
#   ./install.sh                 install binary (if Go present) + Claude skills
#   ./install.sh --skills-only   only (re)install the skills
#   ./install.sh --bin-only      only install the binary
#
# Env overrides:
#   CLAUDE_SKILLS_DIR   default: ~/.claude/skills

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAUDE_SKILLS_DIR="${CLAUDE_SKILLS_DIR:-$HOME/.claude/skills}"
DO_BIN=1
DO_SKILLS=1

for arg in "$@"; do
  case "$arg" in
    --skills-only) DO_BIN=0 ;;
    --bin-only)    DO_SKILLS=0 ;;
    -h|--help)     sed -n '4,13p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

install_bin() {
  if ! command -v go >/dev/null 2>&1; then
    echo "!! Go toolchain not found. Install Go 1.23+ or grab a prebuilt binary." >&2
    exit 1
  fi
  if [ -d "$REPO_DIR/cmd/tincan" ]; then
    echo ">> building tincan from source ($REPO_DIR)"
    ( cd "$REPO_DIR" && go install ./cmd/tincan )
  else
    echo ">> installing tincan from GitHub"
    go install github.com/c0ze/tincan/cmd/tincan@latest
  fi
  local gobin; gobin="$(go env GOBIN)"; [ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
  echo ">> installed to $gobin/tincan"
  case ":$PATH:" in
    *":$gobin:"*) : ;;
    *) echo "   NOTE: add $gobin to your PATH." ;;
  esac
}

install_skills() {
  for s in tell listen; do
    local src="$REPO_DIR/skills/$s" dst="$CLAUDE_SKILLS_DIR/$s"
    [ -d "$src" ] || { echo "!! missing $src" >&2; exit 1; }
    mkdir -p "$dst"
    cp "$src"/*.md "$dst"/
    echo ">> installed Claude skill: $dst"
  done
  echo ">> Claude: restart your session, then use /tell and /listen."

  # Codex: custom prompts become /slash commands.
  if [ -d "$HOME/.codex" ]; then
    mkdir -p "$HOME/.codex/prompts"
    cp "$REPO_DIR/skills/codex/listen.md" "$HOME/.codex/prompts/listen.md"
    echo ">> installed Codex prompt: ~/.codex/prompts/listen.md (use /listen in Codex)"
  else
    echo "-- Codex not detected (~/.codex missing); skipped its /listen prompt."
  fi

  # Gemini CLI (and Antigravity if it reads gemini-style commands).
  if [ -d "$HOME/.gemini" ]; then
    mkdir -p "$HOME/.gemini/commands"
    cp "$REPO_DIR/skills/gemini/listen.toml" "$HOME/.gemini/commands/listen.toml"
    echo ">> installed Gemini command: ~/.gemini/commands/listen.toml (use /listen in Gemini CLI)"
  else
    echo "-- Gemini CLI not detected (~/.gemini missing); skipped its /listen command."
  fi
}

[ "$DO_BIN" = 1 ] && install_bin
[ "$DO_SKILLS" = 1 ] && install_skills
echo "done."
