// claude-ds — DeepSeek wrapper for Claude Code (Go rewrite).
//
// CDS-23 — Integration wiring. This file owns the full launcher
// orchestration: flag parsing, OTLP provider lifecycle, config load /
// repair / migrate, secretref resolution, proxy goroutine, tmux
// branding, signal forwarding, and exec'ing claude with the spoofed
// model env vars + the `--append-system-prompt` pseudo-skill.
//
// The flow (default-launch path) is:
//
//	parseFlags → dispatch
//	  ├─ --version, --help, --print-schema, --rotate-key, --setup, --doctor,
//	  │    upgrade/update → run handler, exit
//	  └─ default →
//	     ensureConfig (first-run if missing)
//	     LoadConfig → applyFlagOverrides → resolve api_key
//	     buildOTLPProviders + defer shutdown(1s)
//	     export claude env vars (incl. auto-mode unlock if cfg.UnlockAutoMode)
//	     NewProxy + Start (if needed)  → ANTHROPIC_BASE_URL=loopback
//	     warnFlashDowngrade if proxy disabled & UnlockAutoMode
//	     ApplyBranding (tmux); defer Restore
//	     start startupUpdateCheck goroutine
//	     exec.Command claude --append-system-prompt ... <args>
//	     signal.Notify SIGINT/SIGTERM → forward to claude
//	     wait → forward exit code
package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// defaultConfigPath returns the canonical config file location:
// $XDG_CONFIG_HOME/claude-ds/config (default $HOME/.config/claude-ds/config).
func defaultConfigPath() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "claude-ds", "config")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "claude-ds", "config")
	}
	return filepath.Join(home, ".config", "claude-ds", "config")
}

// main is intentionally thin: parse args, dispatch, exit.
func main() {
	os.Exit(run(os.Args[1:]))
}

// parsedArgs is the result of arg parsing. We carry the parsed pieces
// past the dispatch switch so the launch path can apply
// `--proxy-on/off` overrides on the loaded config.
type parsedArgs struct {
	cmd            subcommand
	passthrough    []string
	proxyOnSpec    string
	proxyOnSet     bool
	proxyOffSet    bool
	noUpdateCheck  bool
	sawDoubleDash  bool
}

// run returns the exit code so it can be tested without calling os.Exit.
func run(args []string) int {
	p := parseArgs(args)

	switch p.cmd {
	case subcommandVersion:
		return runVersion(os.Stdout, os.Stderr)
	case subcommandPrintSchema:
		fmt.Fprintln(os.Stdout, CurrentSchema)
		return 0
	case subcommandHelp:
		return runHelp(os.Stdout, os.Stderr)
	case subcommandDoctor:
		return runDoctorCmd()
	case subcommandSetup:
		return runSetupCmd()
	case subcommandRotateKey:
		return runRotateKeyCmd()
	case subcommandUpgrade:
		return runUpgrade()
	default:
		return runLaunch(p)
	}
}

// parseArgs walks argv once and produces a parsedArgs. `--` is a hard
// terminator: anything after it forwards to claude unchanged.
func parseArgs(args []string) parsedArgs {
	p := parsedArgs{cmd: subcommandLaunch}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if p.sawDoubleDash {
			p.passthrough = append(p.passthrough, a)
			continue
		}
		switch {
		case a == "--":
			p.sawDoubleDash = true
		case a == "--version" || a == "-V":
			p.cmd = subcommandVersion
		case a == "--print-schema":
			p.cmd = subcommandPrintSchema
		case a == "--help" || a == "-h":
			p.cmd = subcommandHelp
		case a == "--doctor":
			p.cmd = subcommandDoctor
		case a == "--setup":
			p.cmd = subcommandSetup
		case a == "--rotate-key" || a == "--reset-password":
			p.cmd = subcommandRotateKey
		case a == "--proxy-off":
			p.proxyOffSet = true
		case a == "--proxy-on":
			p.proxyOnSet = true
			p.proxyOnSpec = ""
		case strings.HasPrefix(a, "--proxy-on="):
			p.proxyOnSet = true
			p.proxyOnSpec = strings.TrimPrefix(a, "--proxy-on=")
		case a == "--no-update-check":
			p.noUpdateCheck = true
		case a == "upgrade" || a == "update":
			p.cmd = subcommandUpgrade
		default:
			p.passthrough = append(p.passthrough, a)
		}
	}
	return p
}

// subcommand is the dispatch enum.
type subcommand int

const (
	subcommandLaunch subcommand = iota
	subcommandVersion
	subcommandHelp
	subcommandDoctor
	subcommandSetup
	subcommandRotateKey
	subcommandUpgrade
	subcommandPrintSchema
)

// runVersion prints VERSION and `claude --version` (best-effort).
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

// runHelp prints help + forwards `claude --help` through $PAGER.
func runHelp(stdout, stderr io.Writer) int {
	fmt.Fprint(stdout, helpText)
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(stderr, "\n(claude not on PATH — skipping `claude --help` forward)")
		return 0
	}
	pager := os.Getenv("PAGER")
	if pager == "" {
		c := exec.Command(claudePath, "--help")
		c.Stdout = stdout
		c.Stderr = stderr
		_ = c.Run()
		return 0
	}
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

// runDoctorCmd dispatches --doctor. Sets CLAUDE_DS_DIAGNOSTIC_MODE so
// any spans/metrics emitted (currently none, but future hooks honour
// the gate) carry deployment.environment=doctor. Builds OTLP providers
// up-front so the doctor's OTLP-reachability check sees a real (or
// no-op) global TracerProvider.
func runDoctorCmd() int {
	_ = os.Setenv("CLAUDE_DS_DIAGNOSTIC_MODE", "1")

	cfg, cfgErr := LoadConfig(defaultConfigPath())
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "claude-ds doctor: config load failed: %v\n", cfgErr)
		cfg = &Config{Path: defaultConfigPath()}
		applyDefaults(cfg)
	}

	shutdown, err := buildOTLPProviders(cfg, map[string]string{
		"deployment.environment": "doctor",
	})
	if err != nil {
		// Provider construction is best-effort here. Fall back to no-op
		// and let the doctor's OTLP-reachability check report the
		// underlying problem.
		fmt.Fprintf(os.Stderr, "claude-ds doctor: OTLP provider init: %v\n", err)
		shutdown = noopShutdown
	}
	defer shutdownOTLPWithGrace(shutdown)

	return runDoctor(cfg)
}

// shutdownOTLPWithGrace wraps the shutdown call in a 1-second context
// per the observability doc §10. Best-effort: errors are logged at
// debug level only when proxy_debug is on (we don't have cfg here, so
// we just swallow).
func shutdownOTLPWithGrace(fn OTLPShutdownFunc) {
	if fn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = fn(ctx)
}

// ensureConfig is the head-of-launch hook installed by CDS-21. If the
// config file doesn't exist, run first-run onboarding and write the
// resulting config to disk.
func ensureConfig() int {
	path, err := configPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: %v\n", err)
		return 1
	}
	if _, err := os.Stat(path); err == nil {
		return 0
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "claude-ds: stat config: %v\n", err)
		return 1
	}
	cfg, err := runFirstRun()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: onboarding failed: %v\n", err)
		return 1
	}
	if cfg == nil {
		return 1
	}
	cfg.Path = path
	if err := WriteConfig(path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: write config: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "claude-ds: wrote config to %s\n", path)
	return 0
}

// runSetupCmd dispatches the --setup flag.
func runSetupCmd() int {
	_ = os.Setenv("CLAUDE_DS_DIAGNOSTIC_MODE", "1")
	cfg, err := runSetup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: --setup: %v\n", err)
		return 1
	}
	if cfg == nil {
		return 0
	}
	if err := WriteConfig(cfg.Path, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: write config: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "claude-ds: wrote config to %s\n", cfg.Path)
	return 0
}

// runRotateKeyCmd dispatches the --rotate-key / --reset-password flag.
func runRotateKeyCmd() int {
	if err := runRotateKey(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: --rotate-key: %v\n", err)
		return 1
	}
	return 0
}

// runLaunch is the default-launch entrypoint. Owns the full integration
// flow described at the top of this file.
func runLaunch(p parsedArgs) int {
	// 1. Ensure config exists (first-run if missing). Writes the file.
	if rc := ensureConfig(); rc != 0 {
		return rc
	}

	// 2. Load config from disk. Validate / repair / migrate happens
	//    inside LoadConfig; env-var overrides + secretref resolution
	//    on OTLP keys also happen there.
	cfgPath := userConfigPath()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: load config: %v\n", err)
		return 1
	}

	// 3. Apply CLI-flag overrides (--proxy-on / --proxy-off win over
	//    config + env per spec).
	applyFlagOverrides(cfg, p)

	// 4. Apply CLAUDE_DS_PROXY_EFFORT env override (last word).
	if v := strings.TrimSpace(os.Getenv("CLAUDE_DS_PROXY_EFFORT")); v != "" {
		cfg.ProxyEffort = v
	}
	if v := os.Getenv("CLAUDE_DS_PROXY_DEBUG"); parseBool(v) {
		cfg.ProxyDebug = true
	}

	// 5. Resolve api_key_ref → real token.
	token := ""
	if cfg.APIKeyRef != "" {
		token, err = callResolve(cfg.APIKeyRef)
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude-ds: failed to resolve API key from %s: %v\n", cfg.APIKeyRef, err)
			return 1
		}
		if token == "" {
			fmt.Fprintf(os.Stderr, "claude-ds: resolved API key is empty (%s)\n", cfg.APIKeyRef)
			return 1
		}
	}

	// 6. Build OTLP providers BEFORE the proxy goroutine launches.
	//    Defer shutdown with 1-second grace per observability doc §10.
	shutdown, err := buildOTLPProviders(cfg, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: ⚠ OTLP provider init failed: %v (continuing with no-op providers)\n", err)
		shutdown = noopShutdown
	}
	defer shutdownOTLPWithGrace(shutdown)

	// 7. Build the env-var slate that exec(claude) will inherit. This
	//    sets CLAUDE_DS=1, ANTHROPIC_BASE_URL (DeepSeek for now; will
	//    be overwritten if proxy starts), ANTHROPIC_AUTH_TOKEN, and
	//    the spoofed claude-* model ids (per cfg.UnlockAutoMode).
	env := buildClaudeEnv(cfg, token)

	// 8. Start the proxy if needed; on success, repoint
	//    ANTHROPIC_BASE_URL at the loopback. On failure, warn the user
	//    if UnlockAutoMode is on (silent flash-downgrade hazard).
	var proxy *Proxy
	proxy, err = NewProxy(cfg, ProxyOpts{Debug: cfg.ProxyDebug})
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: ⚠ proxy init failed: %v — running with proxy DISABLED for this session.\n", err)
		warnFlashDowngrade(cfg)
	} else if proxy != nil {
		if startErr := proxy.Start(context.Background()); startErr != nil {
			fmt.Fprintf(os.Stderr, "claude-ds: ⚠ proxy start failed: %v — running with proxy DISABLED for this session.\n", startErr)
			warnFlashDowngrade(cfg)
			proxy = nil
		} else {
			// Repoint claude at the proxy.
			env["ANTHROPIC_BASE_URL"] = "http://" + proxy.Addr()
			// Allow Claude Code to use the Files API (the proxy
			// intercepts /v1/files and strips experimental-beta
			// headers). When proxy is off, leave the disable flag in
			// place — see buildClaudeEnv.
			delete(env, "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS")
			fmt.Fprintf(os.Stderr,
				"claude-ds: reasoning-effort proxy on %s → %s (default=%s)\n",
				proxy.Addr(), cfg.BaseURL, effortOrOff(cfg.ProxyEffort))
		}
	}
	// Defer proxy shutdown — graceful in-flight drain.
	if proxy != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = proxy.Shutdown(ctx)
		}()
	}

	// 9. Branding (tmux pane decoration). No-op outside tmux or when
	//    CLAUDE_DS_NO_BRANDING is set.
	autoModeStatus := ""
	if cfg.UnlockAutoMode {
		autoModeStatus = "wire id: " + env["ANTHROPIC_MODEL"] + " (spoofed for auto-mode)"
	}
	branding, _ := ApplyBranding(cfg.Model, autoModeStatus)
	if branding != nil {
		defer func() { _ = branding.Restore() }()
	}

	// 10. Startup update check (fire-and-forget goroutine; gated on
	//     --no-update-check + CLAUDE_DS_NO_UPDATE_CHECK).
	if !p.noUpdateCheck {
		startupUpdateCheck(cfg)
	}

	// 11. Build the system prompt and exec claude.
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-ds: claude not on PATH — install: https://docs.anthropic.com/claude-code")
		return 1
	}

	systemPrompt := buildSystemPrompt(cfg)
	args := append([]string{"--append-system-prompt", systemPrompt}, p.passthrough...)
	cmd := exec.Command(claudePath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = envSlice(env, os.Environ())

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-ds: claude exec failed: %v\n", err)
		return 1
	}

	// 12. Forward signals to the claude child. Per spec exactly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()

	// 13. Wait for claude. Forward exit code.
	waitErr := cmd.Wait()
	close(sigCh)
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "claude-ds: claude wait: %v\n", waitErr)
		return 1
	}
	return cmd.ProcessState.ExitCode()
}

// applyFlagOverrides honours `--proxy-on[=spec]` and `--proxy-off`.
// Per spec: CLI flags take precedence over config + env.
func applyFlagOverrides(cfg *Config, p parsedArgs) {
	if cfg == nil {
		return
	}
	if p.proxyOffSet {
		cfg.ProxyEffort = "off"
		cfg.ProxyEffortOpus = "off"
		cfg.ProxyEffortSonnet = "off"
		cfg.ProxyEffortHaiku = "off"
		cfg.ProxyEffortSmallFast = "off"
		return
	}
	if p.proxyOnSet {
		spec := p.proxyOnSpec
		if spec == "" {
			spec = "auto"
		}
		cfg.ProxyEffort = spec
	}
}

// effortOrOff is a tiny stringifier for the friendly proxy boot summary.
func effortOrOff(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "off"
	}
	return s
}

// buildClaudeEnv builds the env-var slate exec(claude) inherits. Mirrors
// the Bash launcher's "exec claude" preamble byte-for-byte:
//
//   - CLAUDE_DS=1 (marker)
//   - ANTHROPIC_BASE_URL (DeepSeek by default; runLaunch overwrites
//     when the proxy starts)
//   - ANTHROPIC_AUTH_TOKEN (resolved api_key_ref)
//   - ANTHROPIC_MODEL (cfg.Model unless UnlockAutoMode → claude-opus-4-7)
//   - ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL + ANTHROPIC_SMALL_FAST_MODEL
//     with per-tier overrides winning over spoofed values
//   - ANTHROPIC_DEFAULT_*_MODEL_NAME / _DESCRIPTION (DeepSeek-branded
//     picker labels) when UnlockAutoMode
//   - ANTHROPIC_DEFAULT_*_MODEL_SUPPORTED_CAPABILITIES when cfg.Capabilities
//   - CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1 (unset later if proxy starts)
//   - CLAUDE_DISABLE_NONSTREAMING_FALLBACK=1
func buildClaudeEnv(cfg *Config, token string) map[string]string {
	env := map[string]string{
		"CLAUDE_DS":                            "1",
		"ANTHROPIC_BASE_URL":                   cfg.BaseURL,
		"ANTHROPIC_AUTH_TOKEN":                 token,
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
		"CLAUDE_DISABLE_NONSTREAMING_FALLBACK": "1",
	}

	resolved := cfg.Model
	if resolved == "" {
		resolved = defaultModel
	}

	var spoofMain, spoofOpus, spoofSonnet, spoofHaiku, spoofSmall string
	if cfg.UnlockAutoMode {
		spoofMain = "claude-opus-4-7"
		spoofOpus = "claude-opus-4-7"
		spoofSonnet = "claude-sonnet-4-6"
		spoofHaiku = "claude-haiku-4-5"
		spoofSmall = "claude-haiku-4-5"
	} else {
		spoofMain = resolved
		spoofOpus = resolved
		spoofSonnet = resolved
		spoofHaiku = resolved
		spoofSmall = resolved
	}

	env["ANTHROPIC_MODEL"] = spoofMain
	env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = firstNonEmpty(cfg.ModelOpus, spoofOpus)
	env["ANTHROPIC_DEFAULT_SONNET_MODEL"] = firstNonEmpty(cfg.ModelSonnet, spoofSonnet)
	env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = firstNonEmpty(cfg.ModelHaiku, spoofHaiku)
	env["ANTHROPIC_SMALL_FAST_MODEL"] = firstNonEmpty(cfg.ModelSmallFast, spoofSmall)

	if cfg.UnlockAutoMode {
		env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"] = "DeepSeek (opus tier · " + resolved + ")"
		env["ANTHROPIC_DEFAULT_SONNET_MODEL_NAME"] = "DeepSeek (sonnet tier · " + resolved + ")"
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME"] = "DeepSeek (haiku tier · " + resolved + ")"
		desc := "Routes to " + resolved + " via " + cfg.BaseURL + " — claude-* id is a spoof to satisfy the auto-mode model gate."
		env["ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION"] = desc
		env["ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION"] = desc
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION"] = desc
	}
	if cfg.Capabilities != "" {
		env["ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES"] = cfg.Capabilities
		env["ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES"] = cfg.Capabilities
		env["ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES"] = cfg.Capabilities
	}
	return env
}

// envSlice merges `over` into the inherited env (`base`), with `over`
// winning. Returns a slice in `KEY=VALUE` format suitable for
// exec.Cmd.Env.
func envSlice(over map[string]string, base []string) []string {
	out := make([]string, 0, len(base)+len(over))
	seen := map[string]struct{}{}
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		k := kv[:i]
		if _, ok := over[k]; ok {
			continue
		}
		out = append(out, kv)
		seen[k] = struct{}{}
	}
	for k, v := range over {
		out = append(out, k+"="+v)
	}
	return out
}

// warnFlashDowngrade prints the 3-line stderr warning + 2-second sleep
// when UnlockAutoMode is set and the proxy disabled. Mirrors the Bash
// launcher's `_cds_warn_flash_downgrade` byte-for-byte.
func warnFlashDowngrade(cfg *Config) {
	if cfg == nil || !cfg.UnlockAutoMode {
		return
	}
	fmt.Fprintln(os.Stderr, "claude-ds: ⚠   ⚠ UNLOCK_AUTO_MODE IS ON — spoofed claude-* model IDs will NOT be rewritten.")
	fmt.Fprintln(os.Stderr, "claude-ds: ⚠   ⚠ DeepSeek will silently alias them to deepseek-flash (cheapest model).")
	fmt.Fprintln(os.Stderr, "claude-ds: ⚠   ⚠ Fix: ensure python3 is installed and claude-ds-proxy.py is reachable.")
	time.Sleep(2 * time.Second)
}

// buildSystemPrompt builds the pseudo-skill markdown injected via
// `claude --append-system-prompt`. Mirrors the Bash launcher's
// CLAUDE_DS_SYSTEM_PROMPT block. Only the model name and auto-mode
// status differ across runs.
func buildSystemPrompt(cfg *Config) string {
	resolved := cfg.Model
	if resolved == "" {
		resolved = defaultModel
	}
	autoMode := "off"
	if cfg.UnlockAutoMode {
		autoMode = "on (spoofs claude-* model IDs)"
	}
	upstreamHost := cfg.BaseURL
	if u, err := url.Parse(cfg.BaseURL); err == nil && u.Host != "" {
		upstreamHost = u.Host
	}

	var b strings.Builder
	b.WriteString("# claude-ds\n")
	b.WriteString("You are running via **claude-ds** — a wrapper that routes `claude` through DeepSeek's Anthropic-compatible API.\n")
	fmt.Fprintf(&b, "Your underlying model is `%s` (via %s). Auto-mode is %s.\n\n", resolved, upstreamHost, autoMode)
	b.WriteString("**Image support**: You CAN process image attachments. The proxy transparently intercepts Claude Code's Files API uploads and rewrites `file_id` references to inline base64, then routes image-containing requests to a vision-capable model. You will receive and can describe images pasted in the chat — do not claim otherwise.\n\n")
	b.WriteString("## Config: ~/.config/claude-ds/config\n")
	b.WriteString("`key=value` file (mode 0600). Stanzas and `#` comments preserved.\n\n")
	b.WriteString("**Secret ref** (`api_key_ref`): `op://VAULT/ITEM/FIELD` (1Password), `system://<acct>` (keychain), `infisical://PROJ/ENV/PATH#KEY`, or a bare key. Run `--rotate-key` to change.\n\n")
	fmt.Fprintf(&b, "**Model** (`model`): Default `%s`. Per-tier overrides: `model_{opus,sonnet,haiku,small_fast}`.\n\n", defaultModel)
	b.WriteString("**Auto mode** (`unlock_auto_mode=1`): Spoofs `claude-opus-4-7`/etc. model ids so Claude Code's permission classifier activates. `/model` picker shows real DeepSeek labels. Does NOT change which model actually serves requests.\n\n")
	b.WriteString("**Reasoning proxy** (`proxy_effort`, off by default): Translates Claude's `/effort` levels into DeepSeek `reasoning_effort`. With `proxy_effort=auto`:\n")
	b.WriteString("  low/medium/high → DeepSeek `high` (default reasoning)\n")
	b.WriteString("  xhigh/max       → DeepSeek `max`  (maximum reasoning)\n")
	b.WriteString("  no thinking     → `none` (no reasoning tokens)\n")
	b.WriteString("Override the mapping via matrix syntax: `none=<v>|high=<v>|max=<v>`.\n")
	b.WriteString("Per-tier: `proxy_effort_{opus,sonnet,haiku,small_fast}`.\n")
	b.WriteString("One-shot disable: `CLAUDE_DS_PROXY_EFFORT=off`. Debug: `CLAUDE_DS_PROXY_DEBUG=1`.\n\n")
	b.WriteString("**Self-healing**: Schema auto-migrated on version bumps (`<config>.v<N>.bak` backed up). Damaged configs detected, backed up (`<config>.broken.<ts>.bak`), and repaired — no data lost.\n\n")
	b.WriteString("## Diagnostics\n")
	b.WriteString("- `--doctor`: 7-step health check (PATH, config, secret resolution, API liveness, proxy, tier collisions, OTLP reachability).\n")
	b.WriteString("- `CLAUDE_DS_NO_BRANDING=1`: Suppress tmux pane branding.\n")
	b.WriteString("- `CLAUDE_DS_PROXY_DEBUG=1`: Log proxy regime injections to stderr.")
	return b.String()
}

// helpText is the static help body printed by `claude-ds --help`.
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
    --doctor                  Run 7-step diagnostics, then exit
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
    CLAUDE_DS_DIAGNOSTIC_MODE    Force OTLP deployment.environment=doctor
                                 (auto-set by --doctor and --setup)
    GITHUB_TOKEN                 Auth for GitHub API (5,000/hr vs 60)
    XDG_CONFIG_HOME              Config directory; default ~/.config
    PAGER                        Pager used for --help

CONFIG:
    File: $XDG_CONFIG_HOME/claude-ds/config (default ~/.config/claude-ds/config)
    Mode: 0600  Format: key=value, # comments, blank lines ignored

See the design spec for the full feature set:
    docs/superpowers/specs/2026-05-05-claude-ds-go-rewrite-design.md

`
