package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// scriptedPrompt installs a scripted promptFn / passwordFn for the
// duration of the test. Each call pops the next line from `answers`;
// running off the end of the script returns "" so a missing branch
// degrades to the default-Y/auto-pick path. Restores the originals on
// test teardown.
func scriptedPrompt(t *testing.T, answers []string) {
	t.Helper()
	idx := 0
	promptMu.Lock()
	prevPrompt := promptFn
	prevPassword := passwordFn
	promptFn = func(prompt string) (string, error) {
		if idx >= len(answers) {
			return "", nil
		}
		a := answers[idx]
		idx++
		return a, nil
	}
	passwordFn = func(prompt string) (string, error) {
		if idx >= len(answers) {
			return "", nil
		}
		a := answers[idx]
		idx++
		return a, nil
	}
	promptMu.Unlock()
	t.Cleanup(func() {
		promptMu.Lock()
		promptFn = prevPrompt
		passwordFn = prevPassword
		promptMu.Unlock()
	})
}

// scriptedLiveness pins the liveness check to a fixed sequence of
// outcomes so tests don't actually hit the network. Each call pops the
// next outcome; running off the end returns livenessOK.
func scriptedLiveness(t *testing.T, results []livenessResult) *int32 {
	t.Helper()
	var idx int32
	promptMu.Lock()
	prev := livenessFn
	livenessFn = func(token, baseURL, model string) livenessResult {
		i := atomic.AddInt32(&idx, 1) - 1
		if int(i) >= len(results) {
			return livenessOK
		}
		return results[i]
	}
	promptMu.Unlock()
	t.Cleanup(func() {
		promptMu.Lock()
		livenessFn = prev
		promptMu.Unlock()
	})
	return &idx
}

// stubResolveFn pins callResolve to return a fixed value (or error) for
// every call, regardless of input. Useful for onboarding tests that
// don't want to shell out to `op`/`security`/`infisical`.
func stubResolveFn(t *testing.T, value string, err error) {
	t.Helper()
	resolveFnMu.Lock()
	prev := resolveFn
	resolveFn = func(ref string) (string, error) { return value, err }
	resolveFnMu.Unlock()
	t.Cleanup(func() {
		resolveFnMu.Lock()
		resolveFn = prev
		resolveFnMu.Unlock()
	})
}

func TestRunOnboardingFlow_BareKey_OK(t *testing.T) {
	stubResolveFn(t, "sk-test", nil)
	scriptedLiveness(t, []livenessResult{livenessOK})

	// Scheme=op (deterministic, no keychain side effects), then proxy=a, auto=Y.
	scriptedPrompt(t, []string{
		"op",                              // scheme
		"op://Vault/Item/credential",      // ref
		"a",                               // proxy choice → auto:high
		"y",                               // auto-mode unlock
	})

	cfg, err := runOnboardingFlow(nil)
	if err != nil {
		t.Fatalf("runOnboardingFlow: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg")
	}
	if cfg.APIKeyRef != "op://Vault/Item/credential" {
		t.Errorf("APIKeyRef = %q, want op://Vault/Item/credential", cfg.APIKeyRef)
	}
	if cfg.ProxyEffort != "auto:high" {
		t.Errorf("ProxyEffort = %q, want auto:high", cfg.ProxyEffort)
	}
	if !cfg.UnlockAutoMode {
		t.Errorf("UnlockAutoMode = false, want true")
	}
	if cfg.Schema != CurrentSchema {
		t.Errorf("Schema = %d, want %d", cfg.Schema, CurrentSchema)
	}
}

func TestRunOnboardingFlow_AutoModeOff(t *testing.T) {
	stubResolveFn(t, "sk-test", nil)
	scriptedLiveness(t, []livenessResult{livenessOK})

	scriptedPrompt(t, []string{
		"op",
		"op://Vault/Item/credential",
		"s", // skip proxy
		"n", // disable auto-mode
	})

	cfg, err := runOnboardingFlow(nil)
	if err != nil {
		t.Fatalf("runOnboardingFlow: %v", err)
	}
	if cfg.ProxyEffort != "off" {
		t.Errorf("ProxyEffort = %q, want off", cfg.ProxyEffort)
	}
	if cfg.UnlockAutoMode {
		t.Errorf("UnlockAutoMode = true, want false")
	}
}

func TestRunOnboardingFlow_LivenessRetry_3Attempts(t *testing.T) {
	stubResolveFn(t, "sk-test", nil)
	idx := scriptedLiveness(t, []livenessResult{
		livenessUnauth, livenessUnauth, livenessUnauth,
	})

	// Each unauth round: scheme=op, ref. After 3rd failure: fall through
	// to proxy + auto-mode prompts (no "try again" because the loop bails
	// at the maxAttempts boundary).
	scriptedPrompt(t, []string{
		"op", "op://V/I/F", "y", // attempt 1: scheme, ref, "try another? Y"
		"op", "op://V/I/F", "y", // attempt 2: scheme, ref, "try another? Y"
		"op", "op://V/I/F", // attempt 3: scheme, ref (no retry prompt — bail)
		"a", "y", // proxy + auto-mode
	})

	cfg, err := runOnboardingFlow(nil)
	if err != nil {
		t.Fatalf("runOnboardingFlow: %v", err)
	}
	if got := atomic.LoadInt32(idx); got != 3 {
		t.Errorf("liveness called %d times, want 3", got)
	}
	if cfg.APIKeyRef != "op://V/I/F" {
		t.Errorf("APIKeyRef = %q, want op://V/I/F", cfg.APIKeyRef)
	}
}

func TestRunOnboardingFlow_LivenessRetry_AbortAfterFirstFail(t *testing.T) {
	stubResolveFn(t, "sk-test", nil)
	scriptedLiveness(t, []livenessResult{livenessUnauth})

	scriptedPrompt(t, []string{
		"op", "op://V/I/F",
		"n",      // user says "don't try again"
		"a", "y", // proxy + auto-mode
	})

	cfg, err := runOnboardingFlow(nil)
	if err != nil {
		t.Fatalf("runOnboardingFlow: %v", err)
	}
	if cfg.APIKeyRef != "op://V/I/F" {
		t.Errorf("APIKeyRef = %q, want op://V/I/F", cfg.APIKeyRef)
	}
}

func TestPromptProxyChoice_CustomValid(t *testing.T) {
	scriptedPrompt(t, []string{
		"c",
		"auto",
	})
	got, err := promptProxyChoice()
	if err != nil {
		t.Fatalf("promptProxyChoice: %v", err)
	}
	if got != "auto" {
		t.Errorf("got %q, want auto", got)
	}
}

func TestPromptProxyChoice_CustomInvalidThenValid(t *testing.T) {
	scriptedPrompt(t, []string{
		"c", "garbage-spec", // invalid → re-prompt
		"c", "auto:max", // valid
	})
	got, err := promptProxyChoice()
	if err != nil {
		t.Fatalf("promptProxyChoice: %v", err)
	}
	if got != "auto:max" {
		t.Errorf("got %q, want auto:max", got)
	}
}

func TestPromptProxyChoice_DefaultA(t *testing.T) {
	scriptedPrompt(t, []string{""})
	got, err := promptProxyChoice()
	if err != nil {
		t.Fatalf("promptProxyChoice: %v", err)
	}
	if got != "auto:high" {
		t.Errorf("got %q, want auto:high", got)
	}
}

// TestCheckLiveness_HTTPCodes drives the real checkLiveness against an
// httptest server and verifies the status-code → livenessResult mapping
// matches the Bash launcher's `_cds_validate_api_key` exactly.
func TestCheckLiveness_HTTPCodes(t *testing.T) {
	cases := []struct {
		name string
		code int
		want livenessResult
	}{
		{"ok-200", 200, livenessOK},
		{"ok-202", 202, livenessOK},
		{"unauth-401", 401, livenessUnauth},
		{"unauth-403", 403, livenessUnauth},
		{"advisory-400", 400, livenessAdvisory},
		{"advisory-500", 500, livenessAdvisory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			got := checkLiveness("sk-test", srv.URL, "deepseek-v4-pro")
			if got != tc.want {
				t.Errorf("checkLiveness = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCheckLiveness_NetworkFailure(t *testing.T) {
	// Use a port that's vanishingly unlikely to have anything bound.
	got := checkLiveness("sk-test", "http://127.0.0.1:1", "deepseek-v4-pro")
	if got != livenessNetwork {
		t.Errorf("checkLiveness = %d, want livenessNetwork", got)
	}
}

func TestRunRotateKey_PreservesProxyAndAutoMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "claude-ds", "config")

	// Seed an existing config with non-default proxy + auto-mode + per-tier
	// values + an Unknown key. Rotate-key must preserve every field except
	// api_key_ref.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seedContents := strings.Join([]string{
		"_schema=1",
		"api_key_ref=system://OLD",
		"proxy_effort=auto:max",
		"proxy_effort_opus=max",
		"proxy_effort_small_fast=none",
		"unlock_auto_mode=1",
		"vision_model=deepseek-vision",
		"future_unknown_key=preserved",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(seedContents), 0o600); err != nil {
		t.Fatal(err)
	}

	stubResolveFn(t, "sk-new", nil)
	scriptedLiveness(t, []livenessResult{livenessOK})
	scriptedPrompt(t, []string{
		"op", "op://Vault/New/credential",
	})

	if err := runRotateKey(); err != nil {
		t.Fatalf("runRotateKey: %v", err)
	}

	// Reload and verify.
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.APIKeyRef != "op://Vault/New/credential" {
		t.Errorf("APIKeyRef = %q, want op://Vault/New/credential", cfg.APIKeyRef)
	}
	if cfg.ProxyEffort != "auto:max" {
		t.Errorf("ProxyEffort = %q, want auto:max (preserved)", cfg.ProxyEffort)
	}
	if cfg.ProxyEffortOpus != "max" {
		t.Errorf("ProxyEffortOpus = %q, want max (preserved)", cfg.ProxyEffortOpus)
	}
	if cfg.ProxyEffortSmallFast != "none" {
		t.Errorf("ProxyEffortSmallFast = %q, want none (preserved)", cfg.ProxyEffortSmallFast)
	}
	if !cfg.UnlockAutoMode {
		t.Errorf("UnlockAutoMode = false, want true (preserved)")
	}
	if cfg.VisionModel != "deepseek-vision" {
		t.Errorf("VisionModel = %q, want deepseek-vision (preserved)", cfg.VisionModel)
	}
	if got, ok := cfg.Unknown["future_unknown_key"]; !ok || got != "preserved" {
		t.Errorf("Unknown[future_unknown_key] = %q (ok=%v), want preserved", got, ok)
	}
}

func TestRunSetup_ExistingConfig_DeclineRerun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "claude-ds", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("_schema=1\napi_key_ref=system://EXISTING\nunlock_auto_mode=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptedPrompt(t, []string{"n"}) // decline re-run

	cfg, err := runSetup()
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil cfg on decline, got %+v", cfg)
	}
}

func TestRunSetup_ExistingConfig_AcceptRerun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "claude-ds", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("_schema=1\napi_key_ref=system://EXISTING\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stubResolveFn(t, "sk-test", nil)
	scriptedLiveness(t, []livenessResult{livenessOK})
	scriptedPrompt(t, []string{
		"y",                          // accept re-run
		"op", "op://Vault/New/cred",  // scheme + ref
		"m", "y",                     // proxy auto:max + auto-mode
	})

	cfg, err := runSetup()
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg on accept")
	}
	if cfg.APIKeyRef != "op://Vault/New/cred" {
		t.Errorf("APIKeyRef = %q", cfg.APIKeyRef)
	}
	if cfg.ProxyEffort != "auto:max" {
		t.Errorf("ProxyEffort = %q, want auto:max", cfg.ProxyEffort)
	}
	if cfg.Path != path {
		t.Errorf("cfg.Path = %q, want %q", cfg.Path, path)
	}
}

func TestRunSetup_NoExistingConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "claude-ds", "config")

	stubResolveFn(t, "sk-test", nil)
	scriptedLiveness(t, []livenessResult{livenessOK})
	scriptedPrompt(t, []string{
		"op", "op://Vault/New/cred", "a", "y",
	})

	cfg, err := runSetup()
	if err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg on first run")
	}
	if cfg.Path != path {
		t.Errorf("cfg.Path = %q, want %q", cfg.Path, path)
	}
}

func TestConfigPath_XDGOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	got, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	want := "/tmp/xdg-test/claude-ds/config"
	if got != want {
		t.Errorf("configPath = %q, want %q", got, want)
	}
}

func TestConfigPath_HomeDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/fake-home")
	got, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	want := "/tmp/fake-home/.config/claude-ds/config"
	if got != want {
		t.Errorf("configPath = %q, want %q", got, want)
	}
}
