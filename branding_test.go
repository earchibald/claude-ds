package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplyBranding_GateClosed_NotInTmux verifies the gate skips when $TMUX
// is empty — no shell-out, no state.
func TestApplyBranding_GateClosed_NotInTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")

	// A bogus PATH ensures any accidental shell-out would fail loudly.
	t.Setenv("PATH", t.TempDir())

	state, err := ApplyBranding("deepseek-v4-pro", "auto")
	if err != nil {
		t.Fatalf("expected nil err when not in tmux, got %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state when not in tmux, got %+v", state)
	}
}

// TestApplyBranding_GateClosed_NoBranding verifies the explicit opt-out.
func TestApplyBranding_GateClosed_NoBranding(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "1")
	t.Setenv("PATH", t.TempDir())

	state, err := ApplyBranding("deepseek-v4-pro", "auto")
	if err != nil {
		t.Fatalf("expected nil err with NO_BRANDING=1, got %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state with NO_BRANDING=1, got %+v", state)
	}
}

// TestApplyBranding_GateClosed_NoTmuxPane verifies that an empty $TMUX_PANE
// (defensive — should never happen when $TMUX is set, but worth covering)
// also closes the gate.
func TestApplyBranding_GateClosed_NoTmuxPane(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")
	t.Setenv("PATH", t.TempDir())

	state, err := ApplyBranding("deepseek-v4-pro", "auto")
	if err != nil {
		t.Fatalf("expected nil err when TMUX_PANE empty, got %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state when TMUX_PANE empty, got %+v", state)
	}
}

// TestRestore_NilReceiver verifies the documented no-op behavior.
func TestRestore_NilReceiver(t *testing.T) {
	var s *BrandingState
	if err := s.Restore(); err != nil {
		t.Fatalf("Restore on nil receiver should not error, got %v", err)
	}
}

// TestApplyBranding_TmuxBinaryMissing verifies that a missing tmux binary
// (gate open but $PATH empty) yields a clean (nil, nil) — branding never
// fails the launch.
func TestApplyBranding_TmuxBinaryMissing(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")
	t.Setenv("PATH", t.TempDir())

	state, err := ApplyBranding("deepseek-v4-pro", "auto")
	if err != nil {
		t.Fatalf("expected nil err when tmux missing, got %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state when tmux missing, got %+v", state)
	}
}

// installFakeTmux drops a shell script named `tmux` in a temp dir and
// prepends that dir to $PATH. The script appends every invocation (with
// args, one per line, joined by tab) to a log file so tests can assert
// against the captured command sequence. It also serves canned responses
// for `display-message -p` and `show-window-options -v` so ApplyBranding's
// state-capture step gets the values the test scenario requires.
func installFakeTmux(t *testing.T, responses map[string]string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "tmux.log")

	// Build a case statement for the canned responses. Keys are
	// space-joined args; the script matches against $* (with $IFS=" ").
	var cases strings.Builder
	for argSig, response := range responses {
		cases.WriteString(`    "`)
		cases.WriteString(argSig)
		cases.WriteString(`") printf '%s\n' `)
		cases.WriteString(shellQuote(response))
		cases.WriteString(" ;;\n")
	}

	script := `#!/bin/sh
# Fake tmux for branding_test.go — log invocations and serve canned responses.
log='` + logPath + `'
# Match against full args BEFORE consuming any of them.
sig="$*"
# Tab-separated record of "args" so tests can split on tab cleanly.
{
  printf '%s' "$1"
  shift
  for a in "$@"; do
    printf '\t%s' "$a"
  done
  printf '\n'
} >> "$log"
case "$sig" in
` + cases.String() + `    *) ;;
esac
exit 0
`
	tmuxPath := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes
// the POSIX way ('"'"').
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// readTmuxLog returns the recorded invocations as one entry per call, with
// args split on tab. Used to assert the orchestration sequence.
func readTmuxLog(t *testing.T, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read tmux log: %v", err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		calls = append(calls, strings.Split(line, "\t"))
	}
	return calls
}

// TestApplyBranding_OrchestrationSequence verifies the full apply flow:
// 4 capture calls (display-message ×3 + show-window-options ×1) followed
// by 7 mutation calls (set-window-option ×6 + rename-window ×1).
func TestApplyBranding_OrchestrationSequence(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")

	logPath := installFakeTmux(t, map[string]string{
		"display-message -p -t %0 #{window_id}":   "@7",
		"display-message -p -t %0 #{window_name}": "shell",
		"show-window-options -v -t @7 automatic-rename": "on",
		"display-message -p -t %0 #{window_panes}": "1",
	})

	state, err := ApplyBranding("deepseek-v4-pro", "wire id: claude-opus-4-7 (spoofed for auto-mode)")
	if err != nil {
		t.Fatalf("ApplyBranding: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state inside tmux")
	}
	if state.windowID != "@7" {
		t.Errorf("windowID = %q, want @7", state.windowID)
	}
	if state.originalName != "shell" {
		t.Errorf("originalName = %q, want shell", state.originalName)
	}
	if state.originalAutoRename != "on" {
		t.Errorf("originalAutoRename = %q, want on", state.originalAutoRename)
	}
	if state.initialPaneCount != 1 {
		t.Errorf("initialPaneCount = %d, want 1", state.initialPaneCount)
	}

	calls := readTmuxLog(t, logPath)
	if len(calls) < 11 {
		t.Fatalf("expected at least 11 tmux calls, got %d: %+v", len(calls), calls)
	}

	// Capture phase: order matters for clarity but not correctness — assert
	// each expected capture appears.
	wantCaptures := [][]string{
		{"display-message", "-p", "-t", "%0", "#{window_id}"},
		{"display-message", "-p", "-t", "%0", "#{window_name}"},
		{"show-window-options", "-v", "-t", "@7", "automatic-rename"},
		{"display-message", "-p", "-t", "%0", "#{window_panes}"},
	}
	for _, want := range wantCaptures {
		if !containsCall(calls, want) {
			t.Errorf("missing capture call %v in log %+v", want, calls)
		}
	}

	// Mutation phase: each must be present, in order *after* the captures.
	wantMutations := [][]string{
		{"set-window-option", "-t", "@7", "automatic-rename", "off"},
		{"rename-window", "-t", "@7", "🐋 shell"},
		{"set-window-option", "-t", "@7", "pane-border-status", "top"},
		{"set-window-option", "-t", "@7", "pane-border-lines", "heavy"},
		{"set-window-option", "-t", "@7", "pane-border-style", "fg=#4D6BFE,bg=default,bold"},
		{"set-window-option", "-t", "@7", "pane-active-border-style", "fg=#4D6BFE,bg=default,bold"},
	}
	for _, want := range wantMutations {
		if !containsCall(calls, want) {
			t.Errorf("missing mutation call %v in log %+v", want, calls)
		}
	}

	// pane-border-format has a long format string we want to assert
	// loosely — just verify the badge text and model + auto-mode detail
	// appear inside the value.
	found := false
	for _, c := range calls {
		if len(c) >= 5 && c[0] == "set-window-option" && c[3] == "pane-border-format" {
			v := c[4]
			if !strings.Contains(v, "🐋 DEEPSEEK") {
				t.Errorf("pane-border-format missing badge: %q", v)
			}
			if !strings.Contains(v, "model: deepseek-v4-pro") {
				t.Errorf("pane-border-format missing model detail: %q", v)
			}
			if !strings.Contains(v, "spoofed for auto-mode") {
				t.Errorf("pane-border-format missing auto-mode detail: %q", v)
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("pane-border-format set-window-option call not found")
	}
}

// TestApplyBranding_StripsExistingWhalePrefix covers the de-dup path: when
// the window already has a whale prefix in its name, ApplyBranding must
// strip it before re-prefixing so we don't end up with "🐋 🐋 foo".
func TestApplyBranding_StripsExistingWhalePrefix(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")

	logPath := installFakeTmux(t, map[string]string{
		"display-message -p -t %0 #{window_id}":          "@7",
		"display-message -p -t %0 #{window_name}":        "🐋 already-branded",
		"show-window-options -v -t @7 automatic-rename":  "off",
		"display-message -p -t %0 #{window_panes}":       "1",
	})

	if _, err := ApplyBranding("deepseek-v4-pro", ""); err != nil {
		t.Fatalf("ApplyBranding: %v", err)
	}
	calls := readTmuxLog(t, logPath)
	want := []string{"rename-window", "-t", "@7", "🐋 already-branded"}
	if !containsCall(calls, want) {
		t.Errorf("expected rename to %q (single whale), calls=%+v", want, calls)
	}
}

// TestRestore_SinglePane_LeavesBorderOptions matches the Bash launcher: when
// only one pane remains, restore the name and automatic-rename, but leave
// the pane-border-* options alone (tmux's pane-death cleanup handles them).
func TestRestore_SinglePane_LeavesBorderOptions(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")

	logPath := installFakeTmux(t, map[string]string{
		"display-message -p -t @7 #{window_panes}": "1",
	})

	state := &BrandingState{
		windowID:           "@7",
		originalName:       "shell",
		originalAutoRename: "on",
	}
	if err := state.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	calls := readTmuxLog(t, logPath)

	// Must rename back to original.
	if !containsCall(calls, []string{"rename-window", "-t", "@7", "shell"}) {
		t.Errorf("expected rename-window back to original, calls=%+v", calls)
	}
	// Must restore automatic-rename to original value (not -u unset).
	if !containsCall(calls, []string{"set-window-option", "-t", "@7", "automatic-rename", "on"}) {
		t.Errorf("expected automatic-rename restored to 'on', calls=%+v", calls)
	}
	// Must NOT call set-window-option -u for any pane-border-* option.
	for _, c := range calls {
		if len(c) >= 5 && c[0] == "set-window-option" && c[1] == "-u" {
			if strings.HasPrefix(c[4], "pane-border") || c[4] == "pane-active-border-style" {
				t.Errorf("single-pane restore should not unset %q, but got %v", c[4], c)
			}
		}
	}
}

// TestRestore_MultiPane_StripsBorderOptions matches the Bash launcher: when
// other panes remain, all pane-border-* options are unset so the surviving
// panes don't keep our decoration.
func TestRestore_MultiPane_StripsBorderOptions(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")

	logPath := installFakeTmux(t, map[string]string{
		"display-message -p -t @7 #{window_panes}": "3",
	})

	state := &BrandingState{
		windowID:           "@7",
		originalName:       "shell",
		originalAutoRename: "on",
	}
	if err := state.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	calls := readTmuxLog(t, logPath)
	wantUnsets := []string{
		"pane-border-status",
		"pane-border-lines",
		"pane-border-style",
		"pane-active-border-style",
		"pane-border-format",
	}
	for _, opt := range wantUnsets {
		want := []string{"set-window-option", "-u", "-t", "@7", opt}
		if !containsCall(calls, want) {
			t.Errorf("multi-pane restore should unset %q, calls=%+v", opt, calls)
		}
	}
}

// TestRestore_AutoRenameUnset uses `set -wu` (unset) when the original
// automatic-rename value was empty (i.e. the option was inherited from
// the global default, not explicitly set on the window).
func TestRestore_AutoRenameUnset(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	t.Setenv("TMUX_PANE", "%0")
	t.Setenv("CLAUDE_DS_NO_BRANDING", "")

	logPath := installFakeTmux(t, map[string]string{
		"display-message -p -t @7 #{window_panes}": "1",
	})

	state := &BrandingState{
		windowID:           "@7",
		originalName:       "shell",
		originalAutoRename: "", // inherited
	}
	if err := state.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	calls := readTmuxLog(t, logPath)
	want := []string{"set-window-option", "-u", "-t", "@7", "automatic-rename"}
	if !containsCall(calls, want) {
		t.Errorf("expected automatic-rename to be unset (-u) when originally inherited, calls=%+v", calls)
	}
}

// containsCall returns true if the recorded log includes a call whose args
// (positional) start with the want slice. We use prefix-match because the
// fake tmux records the program name's args verbatim and the test inputs
// already include every meaningful arg.
func containsCall(calls [][]string, want []string) bool {
	for _, c := range calls {
		if len(c) < len(want) {
			continue
		}
		match := true
		for i, w := range want {
			if c[i] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
