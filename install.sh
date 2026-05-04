#!/usr/bin/env bash
# claude-ds installer — single curl | bash
#
#   curl -fsSL https://raw.githubusercontent.com/earchibald/claude-ds/main/install.sh | bash
#
# Downloads claude-ds and claude-ds-proxy.py, asks where to install,
# handles sudo when the target dir isn't user-writable, gracefully
# handles existing installs, then runs `claude-ds --setup` for
# first-time onboarding (config creation, secret ref, proxy opt-in,
# auto-mode unlock opt-in)
# without launching a claude session.

set -euo pipefail

if [[ -z "${REPO_BASE:-}" ]] && git rev-parse --git-dir >/dev/null 2>&1; then
  branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "main")
  REPO_BASE="https://raw.githubusercontent.com/earchibald/claude-ds/${branch}"
fi
: "${REPO_BASE:=https://raw.githubusercontent.com/earchibald/claude-ds/main}"
CLAUDE_DS_URL="$REPO_BASE/claude-ds"
PROXY_URL="$REPO_BASE/claude-ds-proxy.py"

# ---- helpers ----------------------------------------------------------------
die()   { echo "install: $*" >&2; exit 1; }
warn()  { echo "install: ⚠ $*" >&2; }
info()  { echo "install: $*" >&2; }

# Read a line from /dev/tty (works under curl|bash where stdin is the pipe).
prompt() {
  local answer
  printf '%s' "$1 " > /dev/tty
  IFS= read -r answer < /dev/tty
  printf '%s' "$answer"
}

# Check whether sudo is cached (no prompt) or will prompt.
_sudo_status() {
  if sudo -n true 2>/dev/null; then
    printf 'cached'
  else
    printf 'prompt'
  fi
}

# Install a file to a destination path, using sudo if the destination
# directory is not user-writable.
_install_file() {
  local src="$1" dest="$2" mode="$3"
  local destdir
  destdir=$(dirname "$dest")
  if [[ -w "$destdir" ]]; then
    cp "$src" "$dest"
  else
    case "$(_sudo_status)" in
      cached) info "using sudo to write to $destdir (cached) ..." ;;
      prompt) info "need sudo to write to $destdir — you will be prompted for your password." ;;
    esac
    sudo cp "$src" "$dest"
  fi
  chmod "$mode" "$dest" 2>/dev/null || sudo chmod "$mode" "$dest"
}

# Walk up from dir until we find a writable ancestor (or hit /).
# Echoes the first writable ancestor, or "" if none found.
_writable_ancestor() {
  local dir="$1"
  while [[ "$dir" != "/" ]]; do
    if [[ -w "$dir" ]]; then
      printf '%s' "$dir"
      return 0
    fi
    dir=$(dirname "$dir")
  done
  [[ -w "/" ]] && { printf '/'; return 0; }
  return 1
}

# ---- intro ------------------------------------------------------------------
echo
echo "claude-ds installer"
echo "━━━━━━━━━━━━━━━━━━━"
echo
echo "This will download claude-ds and claude-ds-proxy.py from"
echo "  $REPO_BASE/"
echo "and install them to a directory of your choice."
echo

# ---- pick install path -------------------------------------------------------
echo "Where should claude-ds be installed?"
echo "  [1] ~/.local/bin       (recommended — user-local, no sudo)"
echo "  [2] /usr/local/bin     (system-wide — may need sudo)"
echo "  [3] custom path"
echo
choice=$(prompt "Choice [1/2/3, default 1]:")

case "${choice:-1}" in
  1) install_dir="$HOME/.local/bin" ;;
  2) install_dir="/usr/local/bin" ;;
  3)
    custom=$(prompt "Custom install path:")
    install_dir="${custom/#\~/$HOME}"
    ;;
  *)
    # Treat free-form input as a custom path.
    install_dir="${choice/#\~/$HOME}"
    ;;
esac

[[ -n "$install_dir" ]] || die "empty install path."

# ---- create install dir if needed -------------------------------------------
if [[ ! -d "$install_dir" ]]; then
  writable=$(_writable_ancestor "$install_dir") || true
  want_sudo=0
  if [[ -z "$writable" ]]; then
    want_sudo=1
  fi
  info "creating $install_dir ..."
  if [[ "$want_sudo" -eq 1 ]]; then
    case "$(_sudo_status)" in
      cached) info "using sudo to create $install_dir (cached) ..." ;;
      prompt) info "need sudo to create $install_dir — you will be prompted for your password." ;;
    esac
    sudo mkdir -p "$install_dir"
  else
    mkdir -p "$install_dir"
  fi
fi

# ---- detect existing install ------------------------------------------------
dest_bin="$install_dir/claude-ds"
dest_proxy="$install_dir/claude-ds-proxy.py"
overwrite_bin=1

if [[ -f "$dest_bin" ]]; then
  echo
  info "claude-ds already exists at $dest_bin"
  answer=$(prompt "Overwrite? [Y/n, default y]:")
  case "${answer:-y}" in
    y|Y|yes|YES) overwrite_bin=1 ;;
    *)           overwrite_bin=0
                 info "keeping existing binary — install skipped for $dest_bin"
                 ;;
  esac
fi

# ---- detect existing config --------------------------------------------------
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/claude-ds"
CONFIG_FILE="$CONFIG_DIR/config"
run_setup=1

if [[ -f "$CONFIG_FILE" ]]; then
  echo
  info "existing claude-ds config found at $CONFIG_FILE"
  echo "  [k] keep          — skip onboarding, keep existing config"
  echo "  [b] backup+rerun  — back up old config, run fresh onboarding"
  echo "  [o] overwrite     — overwrite without backup"
  echo
  choice=$(prompt "Choice [k/b/o, default k]:")
  case "${choice:-k}" in
    k|K)
      run_setup=0
      info "keeping existing config — onboarding skipped."
      ;;
    b|B)
      stamp=$(date +%Y%m%d%H%M%S)
      backup="${CONFIG_FILE}.install.${stamp}.bak"
      cp -p "$CONFIG_FILE" "$backup"
      rm -f "$CONFIG_FILE"
      info "backed up config to $backup — running fresh onboarding."
      ;;
    o|O)
      rm -f "$CONFIG_FILE"
      info "overwriting existing config — running fresh onboarding."
      ;;
    *)
      info "unrecognised choice '${choice}' — keeping existing config."
      run_setup=0
      ;;
  esac
fi

# ---- download ----------------------------------------------------------------
if [[ "$overwrite_bin" -eq 0 ]] && [[ -f "$dest_proxy" ]]; then
  info "both binaries already present — skipping download."
else
  echo
  info "downloading claude-ds and claude-ds-proxy.py ..."

  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  if ! curl -fsSL --max-time 30 "$CLAUDE_DS_URL" -o "$tmpdir/claude-ds"; then
    die "failed to download $CLAUDE_DS_URL"
  fi
  if ! curl -fsSL --max-time 30 "$PROXY_URL" -o "$tmpdir/claude-ds-proxy.py"; then
    die "failed to download $PROXY_URL"
  fi

  # Sanity-check the downloaded binary has a bash shebang.
  read -r shebang < "$tmpdir/claude-ds"
  case "$shebang" in
    '#!/'*bash*) ;;
    '#!/'*env' bash') ;;
    *) die "downloaded claude-ds does not look like a bash script (shebang: ${shebang:0:60})" ;;
  esac

  # Install files.
  if [[ "$overwrite_bin" -eq 1 ]]; then
    _install_file "$tmpdir/claude-ds"          "$dest_bin"  755
    _install_file "$tmpdir/claude-ds-proxy.py" "$dest_proxy" 755
    info "installed to $install_dir/"
  fi
fi

# ---- stale system-level proxy sync ------------------------------------------
# If a claude-ds-proxy.py exists in a system path that differs from where we
# just installed (e.g. a root-owned /usr/local/bin/claude-ds-proxy.py from a
# previous install), offer to update it so it doesn't shadow or diverge.
for _sys_proxy in /usr/local/bin/claude-ds-proxy.py /opt/homebrew/bin/claude-ds-proxy.py; do
  if [[ "$_sys_proxy" != "$dest_proxy" && -f "$_sys_proxy" ]]; then
    if ! diff -q "$dest_proxy" "$_sys_proxy" >/dev/null 2>&1; then
      echo
      warn "stale proxy found at $_sys_proxy (different from the one just installed)."
      warn "This can cause existing sessions to use the old version."
      _stale_choice=$(prompt "Sync $_sys_proxy to match the new version? [Y/n, default y]:" </dev/tty)
      case "${_stale_choice:-y}" in
        [Yy]*)
          case "$(_sudo_status)" in
            cached) info "using cached sudo to update $_sys_proxy ..." ;;
            prompt) info "sudo required to update $_sys_proxy — you will be prompted for your password." ;;
          esac
          sudo cp "$dest_proxy" "$_sys_proxy" && info "updated $_sys_proxy" || warn "could not update $_sys_proxy — run: sudo cp $dest_proxy $_sys_proxy"
          ;;
        *) warn "skipped — run manually: sudo cp $dest_proxy $_sys_proxy" ;;
      esac
    fi
  fi
done

# ---- PATH check --------------------------------------------------------------
if ! echo "${PATH:-}" | tr ':' '\n' | grep -q -F "$install_dir"; then
  echo
  warn "$install_dir is not on your \$PATH"
  warn "add it to your shell rc to run claude-ds directly:"
  warn "  export PATH=\"$install_dir:\$PATH\""
fi

# ---- onboarding --------------------------------------------------------------
if [[ "$run_setup" -eq 1 ]]; then
  echo
  info "running first-time onboarding (claude-ds --setup) ..."
  echo
  exec "$dest_bin" --setup < /dev/tty
else
  echo
  info "skipping onboarding — run 'claude-ds' to start a session."
fi
