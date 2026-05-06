package main

// Tmux branding — visual decoration of the tmux pane/window when claude-ds is
// launched inside a tmux session. Mirrors the Bash launcher's behavior in the
// tmux block (see claude-ds lines 1822–1932) so the user can tell at a glance
// that this window is a DeepSeek-backed claude even though the model ids
// advertised to claude pretend to be Anthropic.
//
// Brand color is DeepSeek's #4D6BFE (electric indigo); the whale glyph is
// their wordmark icon. The decoration is window-scoped (tmux's pane border
// and active-border styles are *window* options, not pane options) so we
// resolve $TMUX_PANE → window-id once at apply-time and target everything by
// window-id thereafter.
//
// Apply is gated on three env vars (see ApplyBranding). When the gate is
// closed (no tmux, branding disabled, or missing tmux binary) the function
// returns (nil, nil) so callers can use a uniform `defer s.Restore()` —
// Restore is a no-op when the receiver is nil.

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// brandColor is DeepSeek's electric indigo. Applied to pane border + active
// border foreground; the badge in the format string flips it to background.
const brandColor = "#4D6BFE"

// brandWhale is the prefix for the renamed window and the badge.
const brandWhale = "🐋"

// brandBadgeText renders inside the tmux #[…] inline-style segment.
const brandBadgeText = brandWhale + " DEEPSEEK"

// BrandingState captures the original tmux window state captured before
// ApplyBranding mutates it. Restore() reverses those mutations.
//
// A nil *BrandingState is a valid no-op receiver, mirroring how the Bash
// version simply skips the cleanup block when ds_brand_window is empty.
type BrandingState struct {
	// windowID is the tmux window id (e.g. "@7") resolved from $TMUX_PANE
	// at apply-time. Stable even if the user focuses elsewhere mid-launch.
	windowID string

	// originalName is the window's name before we prefixed the whale glyph.
	originalName string

	// originalAutoRename is the literal value of `automatic-rename` for the
	// window before we forced it off. Empty string means the option was
	// unset (inherits global) and Restore should `set -wu` to unset.
	originalAutoRename string

	// initialPaneCount is the number of panes in the window at apply-time.
	// Used purely for diagnostics; Restore re-checks pane count at exit.
	initialPaneCount int
}

// ApplyBranding decorates the current tmux window with DeepSeek branding,
// returning a BrandingState that the caller MUST Restore via defer. The
// function is a no-op (returns nil, nil) when:
//   - $TMUX is empty (not running inside tmux)
//   - $TMUX_PANE is empty (defensive — should always be set when $TMUX is)
//   - $CLAUDE_DS_NO_BRANDING is non-empty (explicit opt-out)
//
// model is the resolved DeepSeek model id (e.g. "deepseek-v4-pro") and
// autoModeStatus is a free-form suffix (e.g. "wire id: claude-opus-4-7
// (spoofed for auto-mode)") appended after the model in the pane border
// format. Pass an empty string for autoModeStatus to omit the suffix.
//
// All tmux subcommand failures are tolerated — branding is purely cosmetic
// and must never fail the launch. Errors from `tmux display-message` while
// resolving the window id DO short-circuit (we return nil because there's
// nothing to restore), but errors from individual `set-window-option`
// calls are logged at debug level (see brandingDebug) and ignored.
func ApplyBranding(model, autoModeStatus string) (*BrandingState, error) {
	if !brandingEnabled() {
		brandingDebug("branding skipped: gate closed (TMUX=%q TMUX_PANE=%q NO_BRANDING=%q)",
			os.Getenv("TMUX"), os.Getenv("TMUX_PANE"), os.Getenv("CLAUDE_DS_NO_BRANDING"))
		return nil, nil
	}

	pane := os.Getenv("TMUX_PANE")

	// Resolve pane → window-id. If tmux isn't on $PATH or the lookup fails,
	// there's no stable target to mutate, so bail out cleanly.
	windowID, err := tmuxDisplay(pane, "#{window_id}")
	if err != nil || windowID == "" {
		brandingDebug("branding skipped: could not resolve window id (err=%v)", err)
		// Distinguish "tmux missing" (no error path for caller) from
		// other failures — both fall back to no-op.
		if errors.Is(err, exec.ErrNotFound) {
			return nil, nil
		}
		return nil, nil
	}

	state := &BrandingState{windowID: windowID}

	// Capture original window name and automatic-rename setting so Restore
	// can put them back exactly as they were.
	if name, e := tmuxDisplay(pane, "#{window_name}"); e == nil {
		state.originalName = name
	}
	if autoRename, e := tmuxShowWindowOption(windowID, "automatic-rename"); e == nil {
		state.originalAutoRename = autoRename
	}
	if pcStr, e := tmuxDisplay(pane, "#{window_panes}"); e == nil {
		if pc, perr := strconv.Atoi(strings.TrimSpace(pcStr)); perr == nil {
			state.initialPaneCount = pc
		}
	}

	// Strip an existing whale prefix so we don't double up (e.g. when the
	// window was created with `tmux new-window -n "🐋 …"`). Mirrors the
	// Bash variant at line 1879–1880.
	cleanName := strings.TrimPrefix(state.originalName, brandWhale+" ")
	cleanName = strings.TrimPrefix(cleanName, brandWhale)
	if cleanName == "" {
		cleanName = "claude-ds"
	}

	// Build the pane-border-format string. tmux's `#[…]` segments are
	// inline-style markers — flip background/fg for the badge, then
	// `default` resets to the border's own style for the rest.
	detail := "model: " + model
	if autoModeStatus != "" {
		detail += " · " + autoModeStatus
	}
	format := "#[bg=" + brandColor + ",fg=#FFFFFF,bold] " + brandBadgeText + " #[default] " + detail + " "

	// Apply mutations. Each call is independently best-effort — we log and
	// continue on failure so a partially-applied decoration is still
	// preferable to a hard exit.
	tmuxSetWindowOption(windowID, "automatic-rename", "off")
	tmuxRenameWindow(windowID, brandWhale+" "+cleanName)
	tmuxSetWindowOption(windowID, "pane-border-status", "top")
	tmuxSetWindowOption(windowID, "pane-border-lines", "heavy")
	tmuxSetWindowOption(windowID, "pane-border-style", "fg="+brandColor+",bg=default,bold")
	tmuxSetWindowOption(windowID, "pane-active-border-style", "fg="+brandColor+",bg=default,bold")
	tmuxSetWindowOption(windowID, "pane-border-format", format)

	return state, nil
}

// Restore reverses the mutations applied by ApplyBranding. Safe to call on a
// nil receiver. Mirrors the Bash claude_ds_cleanup function (lines 1908–1931):
//
//   - Always restore the original window name (when we captured one).
//   - Restore automatic-rename to its captured value, or `set -wu` (unset)
//     when the option was originally inherited.
//   - Strip the pane-border-* options ONLY when the window currently has
//     more than one pane. When this is the only pane, leave them — the pane
//     is about to die and tmux's pane-death cleanup will tear them down on
//     its own. (If we strip them here, a sibling pane that *just* arrived
//     would briefly see them removed.)
//
// All errors are swallowed (logged at debug only) so cleanup can never abort
// process exit.
func (s *BrandingState) Restore() error {
	if s == nil || s.windowID == "" {
		return nil
	}

	// Re-check pane count at restore time — the window may have grown or
	// shrunk since ApplyBranding ran. The Bash variant defaults to 1 when
	// the lookup fails, which is the conservative choice (don't strip).
	paneCount := 1
	if pcStr, err := tmuxDisplayWindow(s.windowID, "#{window_panes}"); err == nil {
		if pc, perr := strconv.Atoi(strings.TrimSpace(pcStr)); perr == nil {
			paneCount = pc
		}
	}

	if s.originalName != "" {
		tmuxRenameWindow(s.windowID, s.originalName)
	}
	if s.originalAutoRename != "" {
		tmuxSetWindowOption(s.windowID, "automatic-rename", s.originalAutoRename)
	} else {
		tmuxUnsetWindowOption(s.windowID, "automatic-rename")
	}

	if paneCount > 1 {
		tmuxUnsetWindowOption(s.windowID, "pane-border-status")
		tmuxUnsetWindowOption(s.windowID, "pane-border-lines")
		tmuxUnsetWindowOption(s.windowID, "pane-border-style")
		tmuxUnsetWindowOption(s.windowID, "pane-active-border-style")
		tmuxUnsetWindowOption(s.windowID, "pane-border-format")
	}

	return nil
}

// brandingEnabled checks the three environment gates: $TMUX must be set,
// $TMUX_PANE must be set, and $CLAUDE_DS_NO_BRANDING must be unset/empty.
func brandingEnabled() bool {
	return os.Getenv("TMUX") != "" &&
		os.Getenv("TMUX_PANE") != "" &&
		os.Getenv("CLAUDE_DS_NO_BRANDING") == ""
}

// tmuxBin is the binary name; overridable in tests via $PATH manipulation
// (the test harness installs a fake `tmux` script earlier in $PATH).
const tmuxBin = "tmux"

// tmuxDisplay runs `tmux display-message -p -t <pane> <fmt>` and returns
// the captured stdout (trimmed of the trailing newline tmux always adds).
func tmuxDisplay(pane, format string) (string, error) {
	out, err := exec.Command(tmuxBin, "display-message", "-p", "-t", pane, format).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// tmuxDisplayWindow is the window-targeted variant — used at restore time
// when we have a windowID rather than a pane id.
func tmuxDisplayWindow(window, format string) (string, error) {
	out, err := exec.Command(tmuxBin, "display-message", "-p", "-t", window, format).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// tmuxShowWindowOption returns the value of a window option, or empty string
// if unset. tmux prints `<option> <value>` for `show-window-options`; we
// invoke with `-v` to get just the value.
func tmuxShowWindowOption(window, option string) (string, error) {
	out, err := exec.Command(tmuxBin, "show-window-options", "-v", "-t", window, option).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// tmuxSetWindowOption applies a window option. Failures are logged but
// otherwise ignored — branding is non-fatal.
func tmuxSetWindowOption(window, option, value string) {
	if err := exec.Command(tmuxBin, "set-window-option", "-t", window, option, value).Run(); err != nil {
		brandingDebug("set-window-option %s=%q failed: %v", option, value, err)
	}
}

// tmuxUnsetWindowOption clears a previously-set window option (the `-u`
// flag is tmux's "unset" — restores inheritance from global default).
func tmuxUnsetWindowOption(window, option string) {
	if err := exec.Command(tmuxBin, "set-window-option", "-u", "-t", window, option).Run(); err != nil {
		brandingDebug("unset-window-option %s failed: %v", option, err)
	}
}

// tmuxRenameWindow renames the target window. Errors are logged.
func tmuxRenameWindow(window, name string) {
	if err := exec.Command(tmuxBin, "rename-window", "-t", window, name).Run(); err != nil {
		brandingDebug("rename-window %q failed: %v", name, err)
	}
}

// brandingDebug logs to stderr only when $CLAUDE_DS_PROXY_DEBUG is truthy.
// Branding noise is otherwise silent — it's purely cosmetic and we don't
// want to pollute stderr on every launch.
func brandingDebug(format string, args ...any) {
	if os.Getenv("CLAUDE_DS_PROXY_DEBUG") == "" {
		return
	}
	log.Printf("[branding] "+format, args...)
}
