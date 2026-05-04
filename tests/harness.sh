#!/usr/bin/env bash
# tests/harness.sh — tmux-driven end-to-end tests for install.sh + claude-ds --setup.
# See tests/protocol.md for design.

set -uo pipefail

# ---- args ------------------------------------------------------------------

REF="main"
INCLUDE_SUDO=0
SCENARIOS=()

usage() {
  cat <<EOF
Usage: $0 [--ref <branch>] [--include-sudo] [SCENARIO_ID...]

  --ref <branch>     Install from this branch/tag (default: main)
  --include-sudo     Run sudo-required scenarios (skipped by default)
  SCENARIO_ID...     One or more of T01..T13. Default: all (minus sudo)
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ref) REF="$2"; shift 2 ;;
    --include-sudo) INCLUDE_SUDO=1; shift ;;
    -h|--help) usage; exit 0 ;;
    T*) SCENARIOS+=("$1"); shift ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

# ---- prerequisites ---------------------------------------------------------

command -v tmux >/dev/null 2>&1 || { echo "tmux not installed; aborting." >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl not installed; aborting." >&2; exit 1; }

# ---- harness state ---------------------------------------------------------

TESTROOT_BASE="${TMPDIR:-/tmp}/cds-test-$$"
mkdir -p "$TESTROOT_BASE"
PASSES=()
FAILS=()
SKIPS=()
CURRENT_SCENARIO=""
CURRENT_TESTROOT=""
CURRENT_SESSION=""

cleanup() {
  if [[ -n "$CURRENT_SESSION" ]]; then
    tmux kill-session -t "$CURRENT_SESSION" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# ---- color helpers ---------------------------------------------------------

c_red()    { printf '\033[31m%s\033[0m' "$*"; }
c_green()  { printf '\033[32m%s\033[0m' "$*"; }
c_yellow() { printf '\033[33m%s\033[0m' "$*"; }
c_bold()   { printf '\033[1m%s\033[0m' "$*"; }

log_step() { printf '  %s\n' "$*"; }
log_pass() { printf '  %s %s\n' "$(c_green '✓')" "$*"; }
log_fail() { printf '  %s %s\n' "$(c_red '✗')" "$*"; }

# ---- scenario lifecycle ----------------------------------------------------

# scenario_setup <ID>
# Allocates a fresh sandbox dir + tmux session for this scenario.
scenario_setup() {
  CURRENT_SCENARIO="$1"
  CURRENT_TESTROOT="$TESTROOT_BASE/$CURRENT_SCENARIO"
  CURRENT_SESSION="cds-test-$CURRENT_SCENARIO-$$"
  mkdir -p "$CURRENT_TESTROOT/home"

  # Start a tmux session with sandbox env. The shell starts in $TESTROOT
  # with $HOME pointing into the sandbox. We unset XDG_CONFIG_HOME so the
  # wrapper falls through to $HOME/.config.
  tmux new-session -d -s "$CURRENT_SESSION" -x 200 -y 50 \
    -e "HOME=$CURRENT_TESTROOT/home" \
    -e "XDG_CONFIG_HOME=" \
    -e "PATH=$CURRENT_TESTROOT/home/.local/bin:/usr/local/bin:/usr/bin:/bin" \
    -e "CDS_INSTALL_REF=$REF" \
    -e "PS1=$ " \
    "bash --noprofile --norc"

  # Drain the initial prompt prep.
  sleep 0.3
}

scenario_teardown() {
  if [[ -n "$CURRENT_SESSION" ]]; then
    # Capture full pane history before killing.
    capture_pane > "$CURRENT_TESTROOT/final.pane" 2>/dev/null || true
    tmux kill-session -t "$CURRENT_SESSION" 2>/dev/null || true
  fi
  CURRENT_SESSION=""
}

# ---- tmux helpers ----------------------------------------------------------

# send_line "<text>" — types text + Enter.
send_line() {
  tmux send-keys -t "$CURRENT_SESSION" "$1" Enter
}

# send_keys "<text>" — types text without Enter.
send_keys_nl() {
  tmux send-keys -t "$CURRENT_SESSION" "$1"
}

# send_enter — bare Enter.
send_enter() {
  tmux send-keys -t "$CURRENT_SESSION" Enter
}

# capture_pane — full pane history (scrollback included).
capture_pane() {
  tmux capture-pane -t "$CURRENT_SESSION" -p -S -3000 2>/dev/null
}

# wait_for "<substring>" [timeout_seconds]
# Polls capture_pane until the substring appears or timeout. Returns 0 on
# match, 1 on timeout.
wait_for() {
  local needle="$1" timeout="${2:-15}" elapsed=0
  while (( elapsed < timeout * 10 )); do
    if capture_pane | grep -Fq -- "$needle"; then
      return 0
    fi
    sleep 0.1
    ((elapsed++)) || true
  done
  return 1
}

# wait_for_idle [seconds]
# Wait for the pane to stop changing (poor-man's "ready for input").
wait_for_idle() {
  local quiet="${1:-1}"
  local prev cur
  prev=$(capture_pane | md5)
  sleep "$quiet"
  cur=$(capture_pane | md5)
  while [[ "$prev" != "$cur" ]]; do
    prev="$cur"
    sleep "$quiet"
    cur=$(capture_pane | md5)
  done
}

# ---- assertions ------------------------------------------------------------

assert_pane_contains() {
  local needle="$1"
  if capture_pane | grep -Fq -- "$needle"; then
    log_pass "pane contains: $needle"
    return 0
  else
    log_fail "pane MISSING: $needle"
    return 1
  fi
}

assert_pane_lacks() {
  local needle="$1"
  if capture_pane | grep -Fq -- "$needle"; then
    log_fail "pane unexpectedly contains: $needle"
    return 1
  else
    log_pass "pane lacks: $needle"
    return 0
  fi
}

assert_file_exists() {
  local path="$1"
  if [[ -f "$path" ]]; then
    log_pass "file exists: $path"
    return 0
  else
    log_fail "file MISSING: $path"
    return 1
  fi
}

assert_file_lacks() {
  local path="$1"
  if [[ -e "$path" ]]; then
    log_fail "file unexpectedly exists: $path"
    return 1
  else
    log_pass "file absent: $path"
    return 0
  fi
}

assert_file_mode() {
  local path="$1" expected="$2"
  local actual
  actual=$(stat -f '%A' "$path" 2>/dev/null || stat -c '%a' "$path" 2>/dev/null)
  if [[ "$actual" == "$expected" ]]; then
    log_pass "$path mode is $expected"
    return 0
  else
    log_fail "$path mode is $actual, expected $expected"
    return 1
  fi
}

# ---- result tracking -------------------------------------------------------

SCENARIO_FAILS=0

mark_fail() { SCENARIO_FAILS=$((SCENARIO_FAILS + 1)); }

# Wrapper: run an assertion, count failures.
ck() {
  if "$@"; then
    return 0
  else
    mark_fail
    return 1
  fi
}

# ---- shared install helpers ------------------------------------------------

# Run install.sh from the configured ref, with the answers we want piped via
# tmux send_line. Caller is responsible for sending the actual answers
# after this returns (it just kicks off the curl|bash).
start_installer() {
  send_line "curl -fsSL https://raw.githubusercontent.com/earchibald/claude-ds/${REF}/install.sh | bash"
  if ! wait_for "Where would you like to install" 30; then
    log_fail "installer never printed path-selection prompt"
    mark_fail
    return 1
  fi
}

# Walk through the secret-ref / proxy prompts with a fake key.
# Assumes the prompt has reached "Configure DeepSeek API key for claude-ds."
walk_onboarding_with_fake_key() {
  if ! wait_for "Configure DeepSeek API key" 30; then
    log_fail "onboarding never asked for API key"
    mark_fail
    return 1
  fi
  wait_for_idle 1
  send_line "test-key-123"
  if ! wait_for "Try a different reference" 30; then
    log_fail "liveness check did not produce retry prompt"
    mark_fail
    return 1
  fi
  wait_for_idle 1
  send_line "n"
  if ! wait_for "Reasoning-effort proxy" 30; then
    log_fail "proxy opt-in never appeared"
    mark_fail
    return 1
  fi
  wait_for_idle 1
  send_line "s"
  wait_for_idle 2
}

# ---- scenarios -------------------------------------------------------------

T01() {
  echo "$(c_bold "[T01] Fresh install, default path (~/.local/bin)")"
  scenario_setup T01

  start_installer || return
  send_line "1"                            # default path

  # Mkdir prompt only fires when the dir doesn't exist. Sandbox is empty,
  # so it WILL fire.
  wait_for "Directory does not exist" 10
  send_line "y"

  # No existing binary, no existing config → straight to download.
  wait_for "Downloading" 20
  wait_for "Installing" 30
  wait_for "First-time setup" 30 \
    || { log_fail "first-time setup never started"; mark_fail; return; }

  walk_onboarding_with_fake_key

  # Assert.
  ck assert_file_exists "$CURRENT_TESTROOT/home/.local/bin/claude-ds"
  ck assert_file_exists "$CURRENT_TESTROOT/home/.local/bin/claude-ds-proxy.py"
  ck assert_file_mode   "$CURRENT_TESTROOT/home/.local/bin/claude-ds" 755
  ck assert_file_exists "$CURRENT_TESTROOT/home/.config/claude-ds/config"
  ck assert_file_mode   "$CURRENT_TESTROOT/home/.config/claude-ds/config" 600
  ck assert_pane_contains "claude-ds installer"
}

T02() {
  echo "$(c_bold "[T02] Fresh install, custom path")"
  scenario_setup T02
  local custom="$CURRENT_TESTROOT/custom-bin"

  start_installer || return
  send_line "3"
  wait_for "Custom directory" 5
  send_line "$custom"

  wait_for "Directory does not exist" 10
  send_line "y"

  wait_for "Downloading" 20
  wait_for "Installing" 30

  walk_onboarding_with_fake_key

  ck assert_file_exists "$custom/claude-ds"
  ck assert_file_exists "$custom/claude-ds-proxy.py"
}

T03() {
  echo "$(c_bold "[T03] Custom path with ~ expansion")"
  scenario_setup T03

  start_installer || return
  send_line "3"
  wait_for "Custom directory" 5
  send_line "~/cds-tilde-test/bin"

  wait_for "Directory does not exist" 10
  send_line "y"

  wait_for "Downloading" 20
  wait_for "Installing" 30

  walk_onboarding_with_fake_key

  ck assert_file_exists "$CURRENT_TESTROOT/home/cds-tilde-test/bin/claude-ds"
  ck assert_pane_lacks  "Custom directory: ~/cds-tilde-test/bin"  # i.e. wasn't shown literally as ~
}

T04() {
  echo "$(c_bold "[T04] Existing binary, decline overwrite")"
  scenario_setup T04

  # Pre-create a binary stub.
  mkdir -p "$CURRENT_TESTROOT/home/.local/bin"
  echo "#!/bin/bash" > "$CURRENT_TESTROOT/home/.local/bin/claude-ds"
  echo "echo SENTINEL_OLD" >> "$CURRENT_TESTROOT/home/.local/bin/claude-ds"
  chmod +x "$CURRENT_TESTROOT/home/.local/bin/claude-ds"

  start_installer || return
  send_line "1"

  wait_for "Existing installation found" 10
  send_line "n"
  wait_for "Skipping binary install" 5

  # Verify the old stub is preserved.
  ck assert_pane_contains "Skipping binary install"
  if grep -q SENTINEL_OLD "$CURRENT_TESTROOT/home/.local/bin/claude-ds"; then
    log_pass "old binary preserved (SENTINEL_OLD intact)"
  else
    log_fail "old binary was overwritten despite decline"
    mark_fail
  fi
}

T05() {
  echo "$(c_bold "[T05] Existing binary, accept overwrite")"
  scenario_setup T05

  mkdir -p "$CURRENT_TESTROOT/home/.local/bin"
  echo "#!/bin/bash" > "$CURRENT_TESTROOT/home/.local/bin/claude-ds"
  echo "echo SENTINEL_OLD" >> "$CURRENT_TESTROOT/home/.local/bin/claude-ds"
  chmod +x "$CURRENT_TESTROOT/home/.local/bin/claude-ds"

  start_installer || return
  send_line "1"

  wait_for "Existing installation found" 10
  send_line "y"

  wait_for "Downloading" 20
  wait_for "Installing" 30

  walk_onboarding_with_fake_key

  if grep -q SENTINEL_OLD "$CURRENT_TESTROOT/home/.local/bin/claude-ds"; then
    log_fail "old binary not replaced (SENTINEL_OLD still present)"
    mark_fail
  else
    log_pass "binary was replaced with downloaded version"
  fi
}

T06() {
  echo "$(c_bold "[T06] Existing config, keep")"
  scenario_setup T06

  mkdir -p "$CURRENT_TESTROOT/home/.config/claude-ds"
  cat > "$CURRENT_TESTROOT/home/.config/claude-ds/config" <<EOF
_schema=1
api_key_ref=keep-this-marker
base_url=https://api.deepseek.com/anthropic
model=deepseek-v4-pro
proxy_effort=off
EOF
  chmod 600 "$CURRENT_TESTROOT/home/.config/claude-ds/config"

  start_installer || return
  send_line "1"
  wait_for "Directory does not exist" 10
  send_line "y"

  wait_for "Existing config found" 10
  send_line "k"

  wait_for "Setup complete" 30 \
    || { log_fail "Setup-complete banner never appeared"; mark_fail; return; }

  if grep -q "keep-this-marker" "$CURRENT_TESTROOT/home/.config/claude-ds/config"; then
    log_pass "existing config preserved"
  else
    log_fail "existing config lost"
    mark_fail
  fi
}

T07() {
  echo "$(c_bold "[T07] Existing config, backup + re-run")"
  scenario_setup T07

  mkdir -p "$CURRENT_TESTROOT/home/.config/claude-ds"
  echo "api_key_ref=marker-to-backup" > "$CURRENT_TESTROOT/home/.config/claude-ds/config"
  chmod 600 "$CURRENT_TESTROOT/home/.config/claude-ds/config"

  start_installer || return
  send_line "1"
  wait_for "Directory does not exist" 10
  send_line "y"

  wait_for "Existing config found" 10
  send_line "b"
  wait_for "Backed up config" 10

  walk_onboarding_with_fake_key

  # Verify a backup file with the original marker exists.
  local found=0
  for f in "$CURRENT_TESTROOT/home/.config/claude-ds/"config.install.*.bak; do
    [[ -e "$f" ]] || continue
    if grep -q "marker-to-backup" "$f"; then found=1; fi
  done
  if [[ "$found" -eq 1 ]]; then
    log_pass "backup file contains old marker"
  else
    log_fail "no backup file with old marker found"
    mark_fail
  fi
  # New config should have the test-key, not the marker.
  if grep -q "test-key-123" "$CURRENT_TESTROOT/home/.config/claude-ds/config" 2>/dev/null; then
    log_pass "new config written with fresh key"
  else
    log_fail "new config missing fresh key"
    mark_fail
  fi
}

T08() {
  echo "$(c_bold "[T08] Existing config, overwrite without backup")"
  scenario_setup T08

  mkdir -p "$CURRENT_TESTROOT/home/.config/claude-ds"
  echo "api_key_ref=marker-doomed" > "$CURRENT_TESTROOT/home/.config/claude-ds/config"
  chmod 600 "$CURRENT_TESTROOT/home/.config/claude-ds/config"

  start_installer || return
  send_line "1"
  wait_for "Directory does not exist" 10
  send_line "y"

  wait_for "Existing config found" 10
  send_line "o"
  wait_for "Removed existing config" 5

  walk_onboarding_with_fake_key

  # No backup files should exist.
  local backups=0
  for f in "$CURRENT_TESTROOT/home/.config/claude-ds/"config.install.*.bak; do
    [[ -e "$f" ]] && backups=$((backups+1))
  done
  if [[ "$backups" -eq 0 ]]; then
    log_pass "no backup created (overwrite mode)"
  else
    log_fail "unexpected backup file(s) created in overwrite mode"
    mark_fail
  fi
}

T09() {
  echo "$(c_bold "[T09] --setup with config already present")"
  scenario_setup T09

  # Install first.
  start_installer || return
  send_line "1"
  wait_for "Directory does not exist" 10; send_line "y"
  wait_for "Downloading" 20
  walk_onboarding_with_fake_key

  # Now run claude-ds --setup with the config still in place.
  send_line "$CURRENT_TESTROOT/home/.local/bin/claude-ds --setup"
  wait_for "already configured" 10 \
    || { log_fail "--setup did not detect existing config"; mark_fail; return; }
  wait_for_idle 1

  ck assert_pane_contains "already configured"
  ck assert_pane_contains "--rotate-key"
}

T10() {
  echo "$(c_bold "[T10] PATH-not-on-\$PATH warning")"
  scenario_setup T10

  start_installer || return
  send_line "3"
  wait_for "Custom directory" 5
  send_line "$CURRENT_TESTROOT/notpath/bin"

  wait_for "Directory does not exist" 10; send_line "y"
  wait_for "Downloading" 20
  wait_for "Installing" 30

  # PATH warning shows BEFORE onboarding kicks in.
  ck assert_pane_contains "is not on your \$PATH"
  walk_onboarding_with_fake_key
}

T11() {
  echo "$(c_bold "[T11] Damaged config recovery")"
  scenario_setup T11

  # Install first to get a real claude-ds in place.
  start_installer || return
  send_line "1"
  wait_for "Directory does not exist" 10; send_line "y"
  wait_for "Downloading" 20
  walk_onboarding_with_fake_key

  # Corrupt the config: inject malformed lines.
  cat >> "$CURRENT_TESTROOT/home/.config/claude-ds/config" <<EOF
this-line-has-no-equals-sign
=value-with-no-key
unknown_key=zzz
EOF

  # Run --doctor (less destructive than running claude itself, and
  # exercises the same validate+repair pipeline).
  send_line "$CURRENT_TESTROOT/home/.local/bin/claude-ds --doctor"
  wait_for "doctor done" 30 \
    || { log_fail "doctor never finished"; mark_fail; return; }

  ck assert_pane_contains "damaged config detected"
  ck assert_pane_contains "repaired config"
  # A backup should exist.
  local backups=0
  for f in "$CURRENT_TESTROOT/home/.config/claude-ds/"config.broken.*.bak; do
    [[ -e "$f" ]] && backups=$((backups+1))
  done
  if [[ "$backups" -ge 1 ]]; then
    log_pass "broken-config backup created"
  else
    log_fail "no broken-config backup found"
    mark_fail
  fi
}

T12() {
  echo "$(c_bold "[T12] Branch-aware install (CDS_INSTALL_REF)")"
  scenario_setup T12

  start_installer || return

  # Banner should display the ref override (when ref != main).
  if [[ "$REF" != "main" ]]; then
    ck assert_pane_contains "Source ref: $REF"
  else
    log_step "REF=main, skipping banner-override check"
  fi

  send_line "1"
  wait_for "Directory does not exist" 10; send_line "y"
  wait_for "Downloading" 20
  wait_for "Installing" 30
  walk_onboarding_with_fake_key

  # Verify the installed binary has the version we expect for this branch.
  send_line "$CURRENT_TESTROOT/home/.local/bin/claude-ds --version | head -1"
  wait_for "claude-ds " 10
  if [[ "$REF" == "worktree-cds-2-installer" || "$REF" == "main" ]]; then
    # Both should ship 0.7.0 once merged; for the branch, definitely 0.7.0.
    if [[ "$REF" == "worktree-cds-2-installer" ]]; then
      ck assert_pane_contains "claude-ds 0.7.0"
    fi
  fi
}

T13() {
  echo "$(c_bold "[T13] Install to /usr/local/bin via sudo (sudo-required)")"
  if [[ "$INCLUDE_SUDO" -ne 1 ]]; then
    log_step "skipping (--include-sudo not passed)"
    SKIPS+=("T13")
    return
  fi
  scenario_setup T13

  start_installer || return
  send_line "2"   # /usr/local/bin

  # Either it asks "Directory does not exist" (no) or it goes straight
  # to overwrite-prompt-or-download. Either way, sudo prompts may appear
  # — we just wait for "Installing".
  wait_for "Installing" 60 \
    || { log_fail "/usr/local/bin install never reached Installing step"; mark_fail; return; }
  wait_for_idle 2

  if [[ -f /usr/local/bin/claude-ds ]]; then
    log_pass "/usr/local/bin/claude-ds present"
  else
    log_fail "/usr/local/bin/claude-ds missing"
    mark_fail
  fi
}

# ---- runner ----------------------------------------------------------------

ALL=(T01 T02 T03 T04 T05 T06 T07 T08 T09 T10 T11 T12 T13)

if [[ ${#SCENARIOS[@]} -eq 0 ]]; then
  SCENARIOS=("${ALL[@]}")
fi

echo
echo "$(c_bold "claude-ds installer test harness")"
echo "  ref:           $REF"
echo "  testroot:      $TESTROOT_BASE"
echo "  include-sudo:  $INCLUDE_SUDO"
echo "  scenarios:     ${SCENARIOS[*]}"
echo

for s in "${SCENARIOS[@]}"; do
  SCENARIO_FAILS=0
  if ! declare -F "$s" >/dev/null; then
    echo "$(c_red "[$s] no such scenario")"
    FAILS+=("$s")
    continue
  fi
  "$s"
  scenario_teardown
  if [[ "$SCENARIO_FAILS" -eq 0 ]]; then
    PASSES+=("$s")
    echo "  $(c_green "[$s] PASS")"
  else
    FAILS+=("$s")
    echo "  $(c_red "[$s] FAIL ($SCENARIO_FAILS assertion(s))")"
    echo "  pane saved: $CURRENT_TESTROOT/final.pane"
  fi
  echo
done

echo "$(c_bold "Summary")"
echo "  passes: ${#PASSES[@]}  -- ${PASSES[*]:-}"
echo "  fails:  ${#FAILS[@]}   -- ${FAILS[*]:-}"
echo "  skips:  ${#SKIPS[@]}   -- ${SKIPS[*]:-}"
echo "  testroot: $TESTROOT_BASE  (left in place for forensics)"

[[ ${#FAILS[@]} -eq 0 ]] || exit 1
