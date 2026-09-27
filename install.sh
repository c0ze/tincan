#!/usr/bin/env bash
set -euo pipefail

# tincan installer — installs the `tincan` binary and the /tell + /listen skills.
#
# Usage:
#   ./install.sh                 install binary + shared and Claude skills
#   ./install.sh --skills-only   only (re)install the skills
#   ./install.sh --bin-only      only install the binary
#   ./install.sh --from-release  download a prebuilt binary even if Go is present
#   ./install.sh --mcp           also register `tincan mcp` with Claude Code/Codex
#
# Works from a checkout or piped from curl. Without Go, the binary is downloaded
# from GitHub releases and verified against checksums.txt.
#
# Env overrides:
#   AGENTS_SKILLS_DIR   default: ~/.agents/skills
#   CLAUDE_SKILLS_DIR   default: ~/.claude/skills
#   GEMINI_COMMANDS_DIR default: ~/.gemini/commands (if ~/.gemini exists)
#   TINCAN_BIN_DIR      release binary destination, default: ~/.local/bin
#   TINCAN_VERSION      release tag to install, default: latest

REPO_URL="https://github.com/c0ze/tincan"
RELEASE_BASE="${TINCAN_RELEASE_BASE:-$REPO_URL/releases}"
RAW_BASE="${TINCAN_RAW_BASE:-https://raw.githubusercontent.com/c0ze/tincan}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
AGENTS_SKILLS_DIR="${AGENTS_SKILLS_DIR:-$HOME/.agents/skills}"
CLAUDE_SKILLS_DIR="${CLAUDE_SKILLS_DIR:-$HOME/.claude/skills}"
TINCAN_BIN_DIR="${TINCAN_BIN_DIR:-$HOME/.local/bin}"
STAGED=""
trap 'if [ -n "$STAGED" ]; then rm -f "$STAGED"; fi' EXIT
DO_BIN=1
DO_SKILLS=1
DO_MCP=0
FROM_RELEASE=0
INSTALLED_BIN=""

for arg in "$@"; do
  case "$arg" in
    --skills-only)  DO_BIN=0 ;;
    --bin-only)     DO_SKILLS=0 ;;
    --from-release) FROM_RELEASE=1 ;;
    --mcp)          DO_MCP=1 ;;
    -h|--help)      sed -n '4,21p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

if [ "$DO_BIN" = 0 ] && [ "$DO_SKILLS" = 0 ]; then
  echo "!! --skills-only and --bin-only cannot be combined" >&2
  exit 2
fi

fetch() { # fetch <url> <dest>
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    echo "!! need curl or wget to download $1" >&2
    return 1
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

latest_tag() {
  if [ -n "${TINCAN_VERSION:-}" ]; then
    echo "$TINCAN_VERSION"
    return
  fi
  local url
  command -v curl >/dev/null 2>&1 || { echo "!! need curl to find the latest release; set TINCAN_VERSION=vX.Y.Z" >&2; return 1; }
  url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$RELEASE_BASE/latest")" || {
    echo "!! could not resolve the latest release; set TINCAN_VERSION=vX.Y.Z" >&2
    return 1
  }
  echo "${url##*/}"
}

install_release() {
  local os arch tag ext name tmp want got exe
  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    MINGW*|MSYS*|CYGWIN*) os=windows ;;
    *) echo "!! unsupported OS $(uname -s); build with Go instead" >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "!! unsupported CPU $(uname -m); build with Go instead" >&2; exit 1 ;;
  esac
  tag="$(latest_tag)"
  ext=tar.gz; exe=tincan
  [ "$os" = windows ] && { ext=zip; exe=tincan.exe; }
  name="tincan_${tag#v}_${os}_${arch}.${ext}"
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/tincan-release.XXXXXX")"
  echo ">> downloading $name ($tag)"
  fetch "$RELEASE_BASE/download/$tag/$name" "$tmp/$name"
  fetch "$RELEASE_BASE/download/$tag/checksums.txt" "$tmp/checksums.txt"
  want="$(awk -v n="$name" '$2 == n { print $1 }' "$tmp/checksums.txt")"
  got="$(sha256_of "$tmp/$name")"
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    rm -rf "$tmp"
    echo "!! checksum mismatch for $name" >&2
    exit 1
  fi
  if [ "$ext" = zip ]; then
    unzip -q "$tmp/$name" -d "$tmp/x"
  else
    mkdir -p "$tmp/x" && tar -xzf "$tmp/$name" -C "$tmp/x"
  fi
  mkdir -p "$TINCAN_BIN_DIR"
  # Stage under a unique name beside the target, then rename: a running host
  # keeps its original executable and no existing path is written through.
  STAGED="$(mktemp "$TINCAN_BIN_DIR/.$exe.XXXXXX")"
  cp "$tmp/x/$exe" "$STAGED"
  chmod 755 "$STAGED"
  mv -f "$STAGED" "$TINCAN_BIN_DIR/$exe"
  STAGED=""
  rm -rf "$tmp"
  INSTALLED_BIN="$TINCAN_BIN_DIR/$exe"
  echo ">> installed to $INSTALLED_BIN"
}

install_go() {
  if [ -d "$REPO_DIR/cmd/tincan" ]; then
    echo ">> building tincan from source ($REPO_DIR)"
    ( cd "$REPO_DIR" && go install ./cmd/tincan )
  else
    echo ">> installing tincan from GitHub"
    go install github.com/c0ze/tincan/v2/cmd/tincan@latest
  fi
  local gobin; gobin="$(go env GOBIN)"; [ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
  INSTALLED_BIN="$gobin/tincan"
  echo ">> installed to $INSTALLED_BIN"
}

install_bin() {
  if [ "$FROM_RELEASE" = 0 ] && command -v go >/dev/null 2>&1; then
    install_go
  else
    [ "$FROM_RELEASE" = 1 ] || echo ">> Go not found; using a prebuilt release"
    install_release
  fi
  local dir; dir="$(dirname "$INSTALLED_BIN")"
  case ":$PATH:" in
    *":$dir:"*) : ;;
    *) echo "   NOTE: add $dir to your PATH." ;;
  esac
  # Another tincan earlier on PATH would keep answering `tincan` commands.
  local found
  found="$(command -v tincan 2>/dev/null || true)"
  if [ -n "$found" ] && [ ! "$found" -ef "$INSTALLED_BIN" ]; then
    echo "   WARNING: 'tincan' on PATH is $found, not $INSTALLED_BIN." >&2
    echo "            Remove or relink it, or MCP clients may run an old build." >&2
  fi
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

# Without a checkout (curl | bash), fetch the skill sources for the release.
remote_skills() {
  local tag tmp f
  tag="$(latest_tag)"
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/tincan-skills.XXXXXX")"
  for f in tell/SKILL.md listen/SKILL.md gemini/listen.toml; do
    mkdir -p "$tmp/skills/$(dirname "$f")"
    fetch "$RAW_BASE/$tag/skills/$f" "$tmp/skills/$f"
  done
  REPO_DIR="$tmp"
}

install_skills() {
  local s root src
  # A checkout ships install.sh beside skills/; anything else is curl | bash.
  { [ -f "$REPO_DIR/install.sh" ] && [ -d "$REPO_DIR/skills" ]; } || remote_skills
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

# Register one user-level server with no --room: Claude Code and Codex start
# stdio servers in the session's project, and `tincan mcp` uses that project's
# git work tree as the room. No PATH is frozen into the registration; tincan
# searches PATH and common per-user bin directories when it launches agents.
register_mcp() {
  local bin="${INSTALLED_BIN:-$(command -v tincan || true)}"
  if [ -z "$bin" ]; then
    echo "!! tincan binary not found; install it before --mcp" >&2
    exit 1
  fi
  local registered=0
  if command -v claude >/dev/null 2>&1; then
    claude mcp remove tincan -s user >/dev/null 2>&1 || true
    claude mcp add -s user tincan -- "$bin" mcp
    echo ">> Claude Code: registered tincan (user scope; room = session project)"
    registered=1
  fi
  if command -v codex >/dev/null 2>&1; then
    codex mcp remove tincan >/dev/null 2>&1 || true
    codex mcp add tincan -- "$bin" mcp
    echo ">> Codex: registered tincan (room = session project)"
    registered=1
  fi
  [ "$registered" = 1 ] || echo "-- no claude or codex CLI found; add the server manually."
  cat <<EOF
>> Other stdio MCP clients: command "$bin", args ["mcp"].
   Clients that start servers outside a project (e.g. Claude Desktop) need
   args ["mcp", "--room", "/absolute/project"]. Restart clients to reconnect.
EOF
}

[ "$DO_BIN" = 1 ] && install_bin
[ "$DO_SKILLS" = 1 ] && install_skills
[ "$DO_MCP" = 1 ] && register_mcp
echo "done."
