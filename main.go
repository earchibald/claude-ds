// claude-ds — DeepSeek wrapper for Claude Code (Go rewrite).
//
// This file is the Phase-1 scaffolding skeleton from CDS-10. It defines
// the full flag surface and dispatch stubs; the bodies of each subcommand
// are filled in by downstream phases (CDS-12 through CDS-23). The
// acceptance criteria for CDS-10 are:
//
//  1. `go build` produces a static binary
//  2. `claude-ds --version` prints VERSION (and `claude --version` if
//     claude is on PATH; gracefully skips if not)
//  3. `claude-ds --help` prints help text and forwards `claude --help`
//     through $PAGER if set, else stdout
//  4. `claude-ds -- <args>` forwards args to claude unchanged
//  5. Release workflow publishes 4 platform binaries + checksums.txt
//
// Static-binary build (documented in Makefile):
//
//	CGO_ENABLED=0 go build -ldflags="-s -w"
//
// Subcommand-body ownership map (each TODO is parked behind a comment so
// `git grep "TODO: " main.go` enumerates the open work):
//
//	--doctor          → CDS-20
//	--setup           → CDS-13 (first-run onboarding)
//	--rotate-key      → CDS-13
//	--reset-password  → CDS-13 (alias of --rotate-key)
//	--proxy-off       → CDS-15 / CDS-23 (proxy lifecycle)
//	--proxy-on[=spec] → CDS-15 / CDS-23
//	--no-update-check → CDS-18 (update-check goroutine gate)
//	upgrade / update  → CDS-18 (self-updater)
//	default launch    → CDS-23 (provider lifecycle, proxy goroutine,
//	                    branding, claude exec)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultConfigPath returns the canonical config file location:
// $XDG_CONFIG_HOME/claude-ds/config (default $HOME/.config/claude-ds/config).
// Centralised here so subcommands (--doctor, --setup, --rotate-key, the
// default launch path) all converge on the same resolution.
func defaultConfigPath() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "claude-ds", "config")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Last-ditch: a path the loader will treat as "missing" and
		// synthesise defaults from. Better than crashing.
		return filepath.Join(".config", "claude-ds", "config")
	}
	return filepath.Join(home, ".config", "claude-ds", "config")
}

// main is intentionally thin: parse args, dispatch, exit. All real work
// lives in per-subcommand functions so downstream phases can fill them
// in without touching arg parsing.
func main() {
	os.Exit(run(os.Args[1:]))
}

// run returns the exit code so it can be tested without calling
// os.Exit. The Phase-1 skeleton only handles --version, --help, and the
// `--` arg-terminator end-to-end; everything else prints a TODO line to
// stderr and exits 0 so downstream phases land cleanly.
func run(args []string) int {
	// Walk args once. We honour `--` as a hard terminator: anything after
	// it is forwarded to claude unchanged, even if it looks like a
	// claude-ds flag. This is the contract from acceptance criterion 5.
	var (
		passthrough   []string
		cmd           = subcommandLaunch // default action
		proxyOnSpec   string
		sawDoubleDash bool
		noUpdateCheck bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]

		if sawDoubleDash {
			passthrough = append(passthrough, a)
			continue
		}

		switch {
		case a == "--":
			sawDoubleDash = true
			continue

		case a == "--version" || a == "-V":
			cmd = subcommandVersion

		case a == "--print-schema":
			cmd = subcommandPrintSchema

		case a == "--help" || a == "-h":
			cmd = subcommandHelp

		case a == "--doctor":
			cmd = subcommandDoctor

		case a == "--setup":
			cmd = subcommandSetup

		case a == "--rotate-key" || a == "--reset-password":
			cmd = subcommandRotateKey

		case a == "--proxy-off":
			cmd = subcommandProxyOff

		case a == "--proxy-on":
			cmd = subcommandProxyOn
			proxyOnSpec = ""

		case strings.HasPrefix(a, "--proxy-on="):
			cmd = subcommandProxyOn
			proxyOnSpec = strings.TrimPrefix(a, "--proxy-on=")

		case a == "--no-update-check":
			// Doesn't change the dispatched command — it's a modifier on
			// the default-launch path. Honoured below in runLaunch.
			noUpdateCheck = true

		case a == "upgrade" || a == "update":
			// Subcommand only valid as the first non-flag arg; bail out
			// of arg parsing and dispatch.
			cmd = subcommandUpgrade

		default:
			// Unknown args are forwarded to claude unchanged. This
			// matches the Bash launcher's behavior — claude-ds is a
			// transparent wrapper for any flag it doesn't claim.
			passthrough = append(passthrough, a)
		}
	}

	switch cmd {
	case subcommandVersion:
		return runVersion(os.Stdout, os.Stderr)
	case subcommandPrintSchema:
		fmt.Fprintln(os.Stdout, CurrentSchema)
		return 0
	case subcommandHelp:
		return runHelp(os.Stdout, os.Stderr)
	case subcommandDoctor:
		// Best-effort config load. LoadConfig synthesises a defaults-only
		// Config when the file is missing — the doctor's own check 2
		// will surface that as ✗ with an actionable next step. Hard
		// errors (e.g. config exists but is unreadable) get reported
		// here and we still hand a usable Config to runDoctor so the
		// remaining checks run.
		cfg, cfgErr := LoadConfig(defaultConfigPath())
		if cfgErr != nil {
			fmt.Fprintf(os.Stderr, "claude-ds doctor: config load failed: %v\n", cfgErr)
			cfg = &Config{Path: defaultConfigPath()}
		}
		return runDoctor(cfg)
	case subcommandSetup:
		return runTODO("--setup", "CDS-13")
	case subcommandRotateKey:
		return runTODO("--rotate-key / --reset-password", "CDS-13")
	case subcommandProxyOff:
		return runTODO("--proxy-off", "CDS-15 / CDS-23")
	case subcommandProxyOn:
		_ = proxyOnSpec // CDS-15 / CDS-23 will read this
		return runTODO("--proxy-on", "CDS-15 / CDS-23")
	case subcommandUpgrade:
		return runUpgrade()
	default:
		// Default-launch path. Fire the startup update check (no-op if
		// gated out) before handing off to claude.
		if !noUpdateCheck {
			cfg, _ := LoadConfig(userConfigPath())
			startupUpdateCheck(cfg)
		}
		return runLaunch(passthrough)
	}
}

// subcommand is the dispatch enum for run().
type subcommand int

const (
	subcommandLaunch subcommand = iota
	subcommandVersion
	subcommandHelp
	subcommandDoctor
	subcommandSetup
	subcommandRotateKey
	subcommandProxyOff
	subcommandProxyOn
	subcommandUpgrade
	subcommandPrintSchema
)

// runVersion satisfies acceptance criterion 2: print VERSION, then try
// to print `claude --version`. Failure to find claude on PATH must be
// graceful — print a hint to stderr but exit 0.
func runVersion(stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "claude-ds %s\n", VERSION)

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(stderr, "claude: not found on PATH (install: https://docs.anthropic.com/claude-code)")
		return 0
	}

	out, err := exec.Command(claudePath, "--version").CombinedOutput()
	if err != nil {
		fmt.Fprintf(stderr, "claude --version failed: %v\n", err)
		return 0
	}
	stdout.Write(out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Fprintln(stdout)
	}
	return 0
}

// runHelp satisfies acceptance criterion 3: print our help text, then
// forward `claude --help` through $PAGER if set, else stdout. The help
// body is the source of truth for the CLI surface — keep it in sync
// with docs/superpowers/specs/2026-05-05-claude-ds-go-rewrite-design.md
// (CLI interface section).
func runHelp(stdout, stderr io.Writer) int {
	fmt.Fprint(stdout, helpText)

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(stderr, "\n(claude not on PATH — skipping `claude --help` forward)")
		return 0
	}

	pager := os.Getenv("PAGER")
	if pager == "" {
		// No pager → just print claude's help to our stdout.
		c := exec.Command(claudePath, "--help")
		c.Stdout = stdout
		c.Stderr = stderr
		_ = c.Run() // ignore exit code; help is best-effort
		return 0
	}

	// PAGER set → pipe `claude --help` through the pager.
	pagerCmd := exec.Command("sh", "-c", pager)
	pagerCmd.Stdout = stdout
	pagerCmd.Stderr = stderr

	pipeR, pipeW, pipeErr := os.Pipe()
	if pipeErr != nil {
		fmt.Fprintf(stderr, "pipe: %v\n", pipeErr)
		return 0
	}

	helpCmd := exec.Command(claudePath, "--help")
	helpCmd.Stdout = pipeW
	helpCmd.Stderr = stderr

	pagerCmd.Stdin = pipeR

	if err := pagerCmd.Start(); err != nil {
		fmt.Fprintf(stderr, "$PAGER (%s): %v\n", pager, err)
		pipeR.Close()
		pipeW.Close()
		return 0
	}
	if err := helpCmd.Start(); err != nil {
		fmt.Fprintf(stderr, "claude --help: %v\n", err)
		pipeW.Close()
		_ = pagerCmd.Wait()
		return 0
	}

	_ = helpCmd.Wait()
	pipeW.Close()
	_ = pagerCmd.Wait()
	pipeR.Close()
	return 0
}

// runTODO is the placeholder body for subcommands owned by other CDS-NN
// issues. Prints a one-line stub to stderr and exits 0 so downstream
// callers (tests, orchestrator smoke checks) don't trip on a
// not-yet-implemented flag.
func runTODO(name, owner string) int {
	fmt.Fprintf(os.Stderr, "TODO: %s (%s)\n", name, owner)
	return 0
}

// runLaunch is the default-launch entrypoint. CDS-23 owns the full
// implementation (provider lifecycle, proxy goroutine, branding, claude
// exec). For now, if claude isn't on PATH we emit a TODO; if it is, we
// invoke it with the passthrough args so `claude-ds -- <args>` already
// satisfies acceptance criterion 5 today.
func runLaunch(passthrough []string) int {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TODO: default launch (CDS-23) — claude not on PATH; install: https://docs.anthropic.com/claude-code")
		return 0
	}
	c := exec.CommandContext(context.Background(), claudePath, passthrough...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		// Forward claude's exit code if available.
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "claude: %v\n", err)
		return 1
	}
	return c.ProcessState.ExitCode()
}

// helpText is the static help body printed by `claude-ds --help`. It
// describes the claude-ds wrapper itself; `claude --help` is forwarded
// after this block so users see both surfaces in one stream.
const helpText = `claude-ds — DeepSeek wrapper for Claude Code

USAGE:
    claude-ds [OPTIONS] [-- <claude-args>...]
    claude-ds upgrade
    claude-ds update                 (alias for upgrade)

OPTIONS:
    --version, -V             Print version (claude-ds + claude --version)
    --print-schema            Print the compiled-in config schema number, then exit
                              (used by the self-updater to detect schema bumps)
    --help, -h                Print this help (forwards claude --help via $PAGER)
    --doctor                  Run 6-step diagnostics, then exit
    --setup                   Run first-run onboarding, then exit
    --rotate-key              Interactively rotate the configured API key
    --reset-password          Alias for --rotate-key
    --proxy-off               Disable the reasoning-effort proxy this session
    --proxy-on[=<spec>]       Enable the proxy with optional spec for this session
    --no-update-check         Skip the automatic update check on startup
    --                        Stop parsing claude-ds flags; forward all
                              remaining args to claude unchanged

SUBCOMMANDS:
    upgrade, update           Self-update from GitHub releases, then exit

ENVIRONMENT:
    CLAUDE_DS_PROXY_EFFORT       Override proxy_effort for this invocation
    CLAUDE_DS_PROXY_DEBUG        Enable proxy debug logging to stderr
    CLAUDE_DS_NO_BRANDING        Suppress tmux pane/window branding
    CLAUDE_DS_NO_UPDATE_CHECK    Same as --no-update-check
    GITHUB_TOKEN                 Auth for GitHub API (5,000/hr vs 60)
    XDG_CONFIG_HOME              Config directory; default ~/.config
    PAGER                        Pager used for --help

CONFIG:
    File: $XDG_CONFIG_HOME/claude-ds/config (default ~/.config/claude-ds/config)
    Mode: 0600  Format: key=value, # comments, blank lines ignored

See the design spec for the full feature set:
    docs/superpowers/specs/2026-05-05-claude-ds-go-rewrite-design.md

`
