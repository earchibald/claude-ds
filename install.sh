#!/usr/bin/env bash
# install.sh — single-command installer for claude-ds.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/earchibald/claude-ds/main/install.sh | bash
#
# To install from a non-default branch / tag (development, testing):
#   curl -fsSL https://raw.githubusercontent.com/earchibald/claude-ds/<ref>/install.sh \
#     | CDS_INSTALL_REF=<ref> bash
#
# After downloading and installing the binary, runs `claude-ds --setup` to
# walk through first-time configuration interactively.  No `claude` session
# is started.
#
# Self-contained: requires only bash + curl, which are already present in a
# `curl | bash` pipeline.

set -euo pipefail

# Branch / tag to fetch claude-ds + claude-ds-proxy.py from. Override with
# CDS_INSTALL_REF=<branch-or-tag> in the environment to install from a
# development branch — useful for testing the installer itself end-to-end.
CDS_INSTALL_REF="${CDS_INSTALL_REF:-main}"
REPO_RAW="https://raw.githubusercontent.com/earchibald/claude-ds/${CDS_INSTALL_REF}"
BINARY_NAME="claude-ds"
PROXY_NAME="claude-ds-proxy.py"

# ---- helpers ----------------------------------------------------------------

_bold()  { printf '\033[1m%s\033[0m' "$*"; }
_green() { printf '\033[32m%s\033[0m' "$*"; }
_yellow(){ printf '\033[33m%s\033[0m' "$*"; }
_red()   { printf '\033[31m%s\033[0m' "$*"; }

_die() { echo "$(_red "error:") $*" >&2; exit 1; }

_say() { echo "  $*"; }

_confirm() {
  # _confirm "Prompt text" [default_y]  →  returns 0 for yes, 1 for no.
  local prompt="$1" default="${2:-n}"
  local yn_hint
  if [[ "$default" == "y" ]]; then yn_hint="[Y/n]"; else yn_hint="[y/N]"; fi
  printf "  %s %s: " "$prompt" "$yn_hint" >&2
  local ans; IFS= read -r ans
  ans="${ans:-$default}"
  case "${ans,,}" in y|yes) return 0 ;; *) return 1 ;; esac
}

# ---- banner -----------------------------------------------------------------

echo
echo "$(_bold "claude-ds installer")"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Downloads claude-ds and claude-ds-proxy.py from GitHub,"
echo "  installs them to your chosen directory, then runs first-time"
echo "  configuration (API key, proxy opt-in)."
if [[ "$CDS_INSTALL_REF" != "main" ]]; then
  echo
  echo "  $(_yellow "Source ref:") $CDS_INSTALL_REF (override via CDS_INSTALL_REF)"
fi
echo

# ---- path selection ---------------------------------------------------------

DEFAULT_LOCAL="$HOME/.local/bin"

echo "$(_bold "Where would you like to install claude-ds?")"
echo
echo "  [1] $DEFAULT_LOCAL  (default — user-only, no sudo needed)"
echo "  [2] /usr/local/bin  (system-wide, may need sudo)"
echo "  [3] Custom path"
echo
printf "  Choice [1/2/3, default 1]: "
read -r path_choice
path_choice="${path_choice:-1}"

case "$path_choice" in
  1|"")
    INSTALL_DIR="$DEFAULT_LOCAL"
    ;;
  2)
    INSTALL_DIR="/usr/local/bin"
    ;;
  3)
    printf "  Custom directory: "
    read -r INSTALL_DIR
    INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
    [[ -n "$INSTALL_DIR" ]] || _die "empty path, aborting."
    ;;
  *)
    _die "invalid choice '$path_choice'"
    ;;
esac

echo
_say "Install directory: $(_bold "$INSTALL_DIR")"

# ---- check / create directory -----------------------------------------------

if [[ ! -d "$INSTALL_DIR" ]]; then
  if _confirm "Directory does not exist. Create it?" y; then
    if [[ -w "$(dirname "$INSTALL_DIR")" ]]; then
      mkdir -p "$INSTALL_DIR"
    else
      sudo mkdir -p "$INSTALL_DIR"
    fi
    _say "Created $INSTALL_DIR"
  else
    _die "install directory not created, aborting."
  fi
fi

# Determine whether we need sudo to write to the target dir.
USE_SUDO=0
if [[ ! -w "$INSTALL_DIR" ]]; then
  _say "$(_yellow "warning:") $INSTALL_DIR is not writable by your user — will use sudo for file operations."
  USE_SUDO=1
fi

_install_file() {
  local src="$1" dst="$2"
  if [[ "$USE_SUDO" -eq 1 ]]; then
    sudo cp "$src" "$dst"
    sudo chmod 755 "$dst"
  else
    cp "$src" "$dst"
    chmod 755 "$dst"
  fi
}

# ---- handle existing installation -------------------------------------------

BINARY_DST="$INSTALL_DIR/$BINARY_NAME"

if [[ -f "$BINARY_DST" ]]; then
  echo
  _say "$(_yellow "Existing installation found:") $BINARY_DST"
  if ! _confirm "Overwrite?" y; then
    echo
    echo "  Skipping binary install. Exiting."
    exit 0
  fi
fi

# ---- handle existing config -------------------------------------------------

CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/claude-ds"
CONFIG_FILE="$CONFIG_DIR/config"
BACKUP_CONFIG=0

if [[ -f "$CONFIG_FILE" ]]; then
  echo
  _say "$(_yellow "Existing config found:") $CONFIG_FILE"
  echo "  Options:"
  echo "    [k] Keep existing config (skip onboarding)"
  echo "    [b] Back it up and re-run onboarding"
  echo "    [o] Overwrite without backup"
  printf "  Choice [k/b/o, default k]: "
  read -r cfg_choice
  cfg_choice="${cfg_choice:-k}"
  case "${cfg_choice,,}" in
    k|keep)
      echo
      _say "Keeping existing config. Will not re-run onboarding."
      SKIP_ONBOARDING=1
      ;;
    b|back*)
      stamp=$(date +%Y%m%d%H%M%S)
      bak="$CONFIG_FILE.install.$stamp.bak"
      cp "$CONFIG_FILE" "$bak"
      rm "$CONFIG_FILE"
      _say "Backed up config to $bak"
      SKIP_ONBOARDING=0
      BACKUP_CONFIG=1
      ;;
    o|over*)
      rm "$CONFIG_FILE"
      _say "Removed existing config."
      SKIP_ONBOARDING=0
      ;;
    *)
      _die "invalid choice '$cfg_choice'"
      ;;
  esac
else
  SKIP_ONBOARDING=0
fi

# ---- download ---------------------------------------------------------------

echo
echo "$(_bold "Downloading...")"

TMPDIR_WORK=$(mktemp -d)
trap 'rm -rf "$TMPDIR_WORK"' EXIT

_download() {
  local url="$1" dest="$2" label="$3"
  printf "  %-30s" "$label"
  if curl -fsSL --max-time 30 "$url" -o "$dest" 2>/dev/null; then
    echo " $(_green "✓")"
  else
    echo " $(_red "✗")"
    _die "failed to download $url"
  fi
}

_download "$REPO_RAW/$BINARY_NAME"  "$TMPDIR_WORK/$BINARY_NAME"  "$BINARY_NAME"
_download "$REPO_RAW/$PROXY_NAME"   "$TMPDIR_WORK/$PROXY_NAME"   "$PROXY_NAME"

# Sanity check: downloaded binary must look like a bash script.
if ! head -1 "$TMPDIR_WORK/$BINARY_NAME" | grep -q 'bash'; then
  _die "downloaded $BINARY_NAME does not look like a bash script (bad URL or network error?)"
fi

# ---- install ----------------------------------------------------------------

echo
echo "$(_bold "Installing...")"

_install_file "$TMPDIR_WORK/$BINARY_NAME" "$BINARY_DST"
_say "$(_green "✓") installed $BINARY_DST"

PROXY_DST="$INSTALL_DIR/$PROXY_NAME"
_install_file "$TMPDIR_WORK/$PROXY_NAME" "$PROXY_DST"
_say "$(_green "✓") installed $PROXY_DST"

# ---- PATH check -------------------------------------------------------------

echo
if ! echo ":${PATH}:" | grep -q ":${INSTALL_DIR}:"; then
  echo "$(_yellow "Note:") $INSTALL_DIR is not on your \$PATH."
  echo "  Add this to your shell rc file (~/.bashrc, ~/.zshrc, etc.):"
  echo
  echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
  echo
fi

# ---- onboarding -------------------------------------------------------------

if [[ "${SKIP_ONBOARDING:-0}" -eq 1 ]]; then
  echo "$(_bold "Setup complete.")"
  echo "  Existing config kept. Run $(_bold "claude-ds --doctor") to verify everything is working."
  echo
  exit 0
fi

echo "$(_bold "First-time setup")"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo

exec "$BINARY_DST" --setup
