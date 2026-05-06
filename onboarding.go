// onboarding.go — first-run, --setup, and --rotate-key flows for the
// claude-ds Go rewrite (CDS-21).
//
// Mirrors the Bash launcher's `prompt_for_key`, `_cds_validate_or_warn`,
// `_cds_validate_api_key`, `_cds_prompt_proxy_choice`, and
// `_cds_prompt_auto_mode` functions, plus the per-flag dispatch for
// `--setup`, `--rotate-key`, and `--reset-password`. The translated flow:
//
//  1. Print a welcome banner with VERSION.
//  2. Prompt for a secret reference scheme (op / system / infisical /
//     bare). Bare entries are stored in the OS keychain so they roundtrip
//     through Resolve via a `system://…` ref.
//  3. Resolve the ref, then liveness-check the resolved key against
//     DeepSeek with a single 5-second POST. Re-ask up to 3 times on
//     401/403; advisory non-2xx codes are logged and accepted.
//  4. Prompt for proxy_effort (a/m/c/s with spec-language reference).
//  5. Prompt for unlock_auto_mode (Y/n, default Y).
//  6. Build a *Config; the caller writes it via WriteConfig.
//
// Function indirection (`promptFn`, `passwordFn`, `livenessFn`) makes the
// flow unit-testable without a real TTY or upstream API. Mutex-guarded
// like `resolveFn` in config.go so parallel tests don't race the globals.
//
// Phase 5 child of CDS-9. Parallel with CDS-20 and CDS-22.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// livenessResult is the outcome of a single API-key liveness probe.
// Mirrors the Bash launcher's `_cds_validate_api_key` echoes
// (ok / unauth / skipped / network / advisory:NNN), translated to a Go
// enum so callers can switch on it without parsing strings.
type livenessResult int

const (
	livenessOK       livenessResult = iota // 2xx — key works
	livenessUnauth                         // 401 / 403 — key rejected
	livenessSkipped                        // local probe disabled (kept for parity; rarely used)
	livenessNetwork                        // timeout / DNS / connection refused
	livenessAdvisory                       // some other non-2xx — log and accept
)

// promptFn / passwordFn are the test-injection seams for interactive
// input. Production reads from /dev/tty via the helpers in secretref.go;
// tests overwrite these to scripted readers. Like resolveFn, they are
// guarded by a single mutex so concurrent parallel tests don't race.
var (
	promptMu   sync.RWMutex
	promptFn   = PromptInteractive
	passwordFn = PromptPassword
	livenessFn = checkLiveness
)

func callPrompt(prompt string) (string, error) {
	promptMu.RLock()
	fn := promptFn
	promptMu.RUnlock()
	return fn(prompt)
}

func callPassword(prompt string) (string, error) {
	promptMu.RLock()
	fn := passwordFn
	promptMu.RUnlock()
	return fn(prompt)
}

func callLiveness(token, baseURL, model string) livenessResult {
	promptMu.RLock()
	fn := livenessFn
	promptMu.RUnlock()
	return fn(token, baseURL, model)
}

// configPath returns the path to the user's claude-ds config file,
// honoring XDG_CONFIG_HOME → defaulting to ~/.config. The directory is
// created on demand by WriteConfig (mode 0700).
func configPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "claude-ds", "config"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "claude-ds", "config"), nil
}

// runFirstRun is the no-config-on-disk entrypoint. Walks the user
// through the full onboarding flow and returns a populated *Config; the
// caller writes it with WriteConfig.
func runFirstRun() (*Config, error) {
	printWelcome("First-run setup")
	return runOnboardingFlow(nil)
}

// runSetup is the --setup entrypoint. If a config already exists, ask
// whether to re-run onboarding (Bash semantics: a one-line "Re-run
// onboarding to configure new options? [Y/n]" prompt). On "no", returns
// (nil, nil) so the caller can exit cleanly. On "yes", reuses the
// existing api_key_ref and runs the proxy + auto-mode prompts.
func runSetup() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// No existing config: behave exactly like first-run.
		printWelcome("First-run setup")
		cfg, err := runOnboardingFlow(nil)
		if err != nil {
			return nil, err
		}
		if cfg != nil {
			cfg.Path = path
		}
		return cfg, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat config %s: %w", path, err)
	}

	// Existing config: load it, then ask whether to re-run.
	existing, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}

	fmt.Fprintf(os.Stderr, "claude-ds: existing config at %s\n", path)
	rerun, err := callPrompt("Re-run onboarding to configure new options? [Y/n]: ")
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(rerun)) {
	case "n", "no":
		fmt.Fprintln(os.Stderr, "claude-ds: keeping existing config unchanged.")
		return nil, nil
	}

	printWelcome("Re-run onboarding")
	cfg, err := runOnboardingFlow(existing)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		cfg.Path = path
	}
	return cfg, nil
}

// runRotateKey is the --rotate-key / --reset-password entrypoint. Loads
// the existing config (if any), preserves all fields except api_key_ref
// (and the resolved key), prompts for a new ref, liveness-checks it,
// writes the file. On a missing config, falls through to the first-run
// flow.
func runRotateKey() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "claude-ds: no existing config at %s — running first-run prompt.\n", path)
		cfg, err := runFirstRun()
		if err != nil {
			return err
		}
		if cfg == nil {
			return nil
		}
		cfg.Path = path
		return WriteConfig(path, cfg)
	} else if err != nil {
		return fmt.Errorf("stat config %s: %w", path, err)
	}

	existing, err := LoadConfig(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "claude-ds: --rotate-key — rotating stored API key reference.\n")

	ref, err := promptForRefWithLiveness(existing.BaseURL, existing.Model)
	if err != nil {
		return err
	}
	existing.APIKeyRef = ref
	existing.Path = path
	if err := WriteConfig(path, existing); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "claude-ds: wrote config %s (proxy_effort preserved: %s).\n",
		path, existing.ProxyEffort)
	return nil
}

// runOnboardingFlow runs the secret-ref + proxy + auto-mode prompts and
// returns a populated *Config. When existing is non-nil (re-run setup),
// it's used as the starting point so unrelated fields (Unknown, OTLP*,
// per-tier models) survive the round-trip. existing.APIKeyRef is always
// overwritten by the new ref captured here.
func runOnboardingFlow(existing *Config) (*Config, error) {
	cfg := existing
	if cfg == nil {
		cfg = &Config{Unknown: map[string]string{}}
	}
	if cfg.Unknown == nil {
		cfg.Unknown = map[string]string{}
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}

	ref, err := promptForRefWithLiveness(baseURL, model)
	if err != nil {
		return nil, err
	}

	proxy, err := promptProxyChoice()
	if err != nil {
		return nil, err
	}
	autoMode, err := promptAutoMode()
	if err != nil {
		return nil, err
	}

	cfg.Schema = CurrentSchema
	cfg.APIKeyRef = ref
	cfg.BaseURL = baseURL
	cfg.Model = model
	cfg.ProxyEffort = proxy
	cfg.UnlockAutoMode = autoMode

	return cfg, nil
}

// promptForRefWithLiveness drives the scheme picker → resolve →
// liveness-check loop. Re-asks up to 3 times on a 401/403 response.
// After 3 unauth attempts, the most-recently-entered ref is returned
// anyway (matches Bash semantics — "saving anyway after 3 attempts").
func promptForRefWithLiveness(baseURL, model string) (string, error) {
	const maxAttempts = 3
	var lastRef string
	for attempt := 0; attempt < maxAttempts; attempt++ {
		ref, err := promptSecretRef("DeepSeek API key for claude-ds")
		if err != nil {
			return "", err
		}
		lastRef = ref

		// Resolve, then liveness-check.
		token, resErr := callResolve(ref)
		if resErr != nil || token == "" {
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠ could not resolve %q yet — saving anyway, rerun --rotate-key to fix.\n",
				ref)
			return ref, nil
		}

		result := callLiveness(token, baseURL, model)
		switch result {
		case livenessOK:
			fmt.Fprintf(os.Stderr, "claude-ds: ✓ API key validated against %s.\n", baseURL)
			return ref, nil
		case livenessSkipped:
			fmt.Fprintf(os.Stderr, "claude-ds: (liveness check skipped — saving anyway.)\n")
			return ref, nil
		case livenessNetwork:
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠ couldn't reach %s for liveness check (timeout / DNS). Saving anyway.\n",
				baseURL)
			return ref, nil
		case livenessAdvisory:
			fmt.Fprintf(os.Stderr,
				"claude-ds: %s returned a non-2xx during liveness check — saving anyway (this may be a model-id mismatch, not a key problem).\n",
				baseURL)
			return ref, nil
		case livenessUnauth:
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠ %s rejected the supplied key (HTTP 401/403).\n",
				baseURL)
			if attempt+1 >= maxAttempts {
				fmt.Fprintln(os.Stderr,
					"claude-ds: ⚠ saving anyway after 3 attempts — rerun --rotate-key once you have a working key.")
				return ref, nil
			}
			again, err := callPrompt("Try a different reference? [Y/n]: ")
			if err != nil {
				return "", err
			}
			switch strings.ToLower(strings.TrimSpace(again)) {
			case "n", "no":
				return ref, nil
			}
		}
	}
	return lastRef, nil
}

// promptSecretRef walks the user through scheme selection. Mirrors the
// Bash launcher's `secretref_prompt` flow with a few simplifications:
// instead of accepting a free-form `op://…` ref entry, the user picks a
// scheme tag (op / system / infisical / bare) and we then prompt for
// scheme-specific parameters. Bare keys are stored in the OS keychain
// (account="claude-ds") so the resulting config holds a stable
// `system://claude-ds` ref instead of a plaintext key on disk.
func promptSecretRef(label string) (string, error) {
	tty, _ := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if tty != nil {
		fmt.Fprintln(tty)
		fmt.Fprintf(tty, "Configure %s.\n", label)
		fmt.Fprintln(tty)
		fmt.Fprintln(tty, "Choose a secret-reference scheme:")
		fmt.Fprintln(tty, "  [op]        1Password CLI       — op://VAULT/ITEM/FIELD")
		fmt.Fprintln(tty, "  [system]    OS keychain         — system://<account>  (interactive picker)")
		fmt.Fprintln(tty, "  [infisical] Infisical CLI       — infisical://PROJECT/ENV/PATH#KEY")
		fmt.Fprintln(tty, "  [bare]      paste the key       — stored in keychain (account=claude-ds)")
		fmt.Fprintln(tty)
		tty.Close()
	}

	for {
		choice, err := callPrompt("Scheme [op/system/infisical/bare, default system]: ")
		if err != nil {
			return "", err
		}
		choice = strings.ToLower(strings.TrimSpace(choice))
		if choice == "" {
			choice = "system"
		}
		switch choice {
		case "op":
			ref, err := callPrompt("op://VAULT/ITEM/FIELD: ")
			if err != nil {
				return "", err
			}
			ref = strings.TrimSpace(ref)
			if !strings.HasPrefix(ref, "op://") {
				fmt.Fprintln(os.Stderr, "claude-ds: ⚠ op:// prefix required, please re-enter.")
				continue
			}
			return ref, nil

		case "system":
			account, err := InteractiveSystemSelector()
			if err != nil {
				return "", err
			}
			// If no entry exists for this account, prompt for the secret
			// and store it. Otherwise reuse the existing entry (matches
			// `secretref_prompt`).
			if _, lookupErr := KeychainLookup(account); lookupErr != nil {
				fmt.Fprintf(os.Stderr,
					"claude-ds: no existing keychain entry for service=%q account=%q.\n",
					keychainService, account)
				secret, err := callPassword(fmt.Sprintf("Paste %s (will be stored in OS keychain): ", label))
				if err != nil {
					return "", err
				}
				if secret == "" {
					fmt.Fprintln(os.Stderr, "claude-ds: ⚠ empty key, please re-enter.")
					continue
				}
				if err := KeychainStore(account, secret); err != nil {
					return "", err
				}
				fmt.Fprintf(os.Stderr,
					"claude-ds: stored secret — service=%q account=%q.\n",
					keychainService, account)
			} else {
				fmt.Fprintf(os.Stderr,
					"claude-ds: found existing keychain entry — service=%q account=%q. Reusing it.\n",
					keychainService, account)
			}
			return "system://" + account, nil

		case "infisical":
			return InteractiveInfisicalBuilder()

		case "bare":
			secret, err := callPassword(fmt.Sprintf("Paste %s (will be stored in OS keychain): ", label))
			if err != nil {
				return "", err
			}
			if secret == "" {
				fmt.Fprintln(os.Stderr, "claude-ds: ⚠ empty key, please re-enter.")
				continue
			}
			account := "claude-ds"
			if err := KeychainStore(account, secret); err != nil {
				return "", err
			}
			fmt.Fprintf(os.Stderr,
				"claude-ds: stored secret — service=%q account=%q.\n",
				keychainService, account)
			return "system://" + account, nil

		default:
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠ unknown scheme %q (expected: op, system, infisical, bare).\n",
				choice)
		}
	}
}

// promptProxyChoice mirrors `_cds_prompt_proxy_choice` from the Bash
// launcher. Returns the proxy_effort spec string. The `c` (custom)
// branch validates the user's free-form input through ParseSpec; an
// invalid spec re-prompts.
func promptProxyChoice() (string, error) {
	tty, _ := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if tty != nil {
		fmt.Fprintln(tty)
		fmt.Fprintln(tty, "Reasoning-effort proxy (optional)")
		fmt.Fprintln(tty, "─────────────────────────────────")
		fmt.Fprintln(tty, "DeepSeek auto-detects \"high\" reasoning when claude sends a")
		fmt.Fprintln(tty, "thinking block (which it does by default). Claude's /effort levels")
		fmt.Fprintln(tty, "(low/medium/high/xhigh/max) are NOT understood by DeepSeek — it")
		fmt.Fprintln(tty, "ignores budget_tokens and maps everything to \"high\".")
		fmt.Fprintln(tty)
		fmt.Fprintln(tty, "The proxy translates claude's effort levels into DeepSeek's")
		fmt.Fprintln(tty, "reasoning_effort so /effort actually changes behavior:")
		fmt.Fprintln(tty, "  low/medium/high → DeepSeek high   (default reasoning)")
		fmt.Fprintln(tty, "  xhigh/max       → DeepSeek max    (maximum reasoning)")
		fmt.Fprintln(tty, "  no thinking     → DeepSeek none   (no reasoning tokens)")
		fmt.Fprintln(tty)
		fmt.Fprintln(tty, "  • [a] auto:high — translate claude's effort to DeepSeek (default)")
		fmt.Fprintln(tty, "  • [m] auto:max  — always force maximum reasoning")
		fmt.Fprintln(tty, "  • [c] custom    — write the spec yourself (per-tier knobs, etc.)")
		fmt.Fprintln(tty, "  • [s] skip      — leave the proxy off")
		fmt.Fprintln(tty)
		tty.Close()
	}

	for {
		choice, err := callPrompt("Choice [a/m/c/s, default a]: ")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "", "a":
			return "auto:high", nil
		case "m":
			return "auto:max", nil
		case "s", "off":
			return "off", nil
		case "c":
			custom, err := callPrompt("Custom proxy_effort: ")
			if err != nil {
				return "", err
			}
			custom = strings.TrimSpace(custom)
			if custom == "" {
				return "off", nil
			}
			if _, err := ParseSpec(custom); err != nil {
				fmt.Fprintf(os.Stderr,
					"claude-ds: ⚠ invalid proxy_effort spec: %v — please re-enter.\n", err)
				continue
			}
			return custom, nil
		default:
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠ unknown choice %q (expected a, m, c, or s).\n", choice)
		}
	}
}

// promptAutoMode mirrors `_cds_prompt_auto_mode`. Default is "yes".
func promptAutoMode() (bool, error) {
	tty, _ := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if tty != nil {
		fmt.Fprintln(tty)
		fmt.Fprintln(tty, "Auto-mode unlock (recommended)")
		fmt.Fprintln(tty, "─────────────────────────────────")
		fmt.Fprintln(tty, "Claude Code's 'auto mode' — the permission classifier that auto-approves")
		fmt.Fprintln(tty, "routine tool calls — is gated on the model ID matching a known Claude")
		fmt.Fprintln(tty, "model name (e.g. claude-opus-4-7). Since claude-ds runs against DeepSeek,")
		fmt.Fprintln(tty, "the gate blocks auto mode by default.")
		fmt.Fprintln(tty)
		fmt.Fprintln(tty, "Setting unlock_auto_mode=1 spoofs the wire-level model ID so the gate")
		fmt.Fprintln(tty, "passes. The /model picker still shows real DeepSeek labels. This does")
		fmt.Fprintln(tty, "NOT change which model serves requests — it only satisfies the classifier.")
		fmt.Fprintln(tty)
		fmt.Fprintln(tty, "Enable this for the closest experience to native Claude Code — auto mode")
		fmt.Fprintln(tty, "is the recommended choice for most users.")
		fmt.Fprintln(tty)
		tty.Close()
	}

	choice, err := callPrompt("Enable auto-mode unlock? [Y/n, default y]: ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "n", "no":
		return false, nil
	}
	return true, nil
}

// printWelcome prints the onboarding banner. Goes to stderr so it
// composes with the stdout-printing config-write flow without polluting
// scriptable output.
func printWelcome(title string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "claude-ds %s — %s\n", VERSION, title)
	fmt.Fprintln(os.Stderr, "================================================")
}

// checkLiveness performs the actual API-key liveness probe — a single
// 5-second POST to <baseURL>/v1/messages with max_tokens=1. Mirrors
// `_cds_validate_api_key` from the Bash launcher exactly: 2xx → ok,
// 401/403 → unauth, network failure → network, other non-2xx →
// advisory.
func checkLiveness(token, baseURL, model string) livenessResult {
	body := fmt.Sprintf(`{"model":"%s","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`,
		jsonEscape(model))
	url := strings.TrimRight(baseURL, "/") + "/v1/messages"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return livenessNetwork
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return livenessNetwork
	}
	defer resp.Body.Close()
	// Drain the body so the connection can be reused. Best-effort.
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return livenessOK
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return livenessUnauth
	default:
		return livenessAdvisory
	}
}

// jsonEscape escapes a string for safe embedding in a JSON string
// literal. We avoid the encoding/json round-trip here because the
// liveness body is fixed-shape and we'd rather not pull a marshaler in
// for one model id — same call pattern as the Bash launcher's `printf`.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
