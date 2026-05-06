package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// stubResolve replaces resolveFn for the duration of a test. The default
// is a passthrough so secret refs work as plain values. Mutation is
// guarded by resolveFnMu so parallel tests (and any future async
// readers reaching the global via callResolve) don't race.
func stubResolve(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	resolveFnMu.Lock()
	prev := resolveFn
	resolveFn = fn
	resolveFnMu.Unlock()
	t.Cleanup(func() {
		resolveFnMu.Lock()
		resolveFn = prev
		resolveFnMu.Unlock()
	})
}

// writeTempConfig writes `contents` to a fresh temp config and returns
// its absolute path. Mode is 0600 to match the launcher's contract.
func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------
// ValidateLine
// ---------------------------------------------------------------------

func TestConfigValidateLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"empty", "", true},
		{"blank", "   ", true},
		{"comment", "# a note", true},
		{"indented_comment", "    # indented", true},
		{"valid_simple", "model=deepseek-v4-pro", true},
		{"valid_with_url", "base_url=https://api.deepseek.com/anthropic", true},
		{"valid_underscore", "_schema=1", true},
		{"valid_with_equals_in_value", "otlp_resource_attributes=tier=prod,zone=eu1", true},
		{"missing_value", "model=", false},
		{"missing_eq", "modelvalue", false},
		{"leading_digit", "1bad=value", false},
		{"leading_space_key", " model=x", false},
		{"trailing_garbage_before_eq", "mod el=x", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := ValidateLine(c.line)
			if got != c.want {
				t.Fatalf("ValidateLine(%q) = %v, want %v", c.line, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// LoadConfig — happy path on a Bash-era config
// ---------------------------------------------------------------------

func TestConfigLoadParsesAllKnownKeys(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	body := strings.Join([]string{
		"_schema=1",
		"# identity",
		"api_key_ref=op://home-kubernetes/DeepSeek/credential",
		"base_url=https://api.example.com/anthropic",
		"model=deepseek-v4-pro",
		"model_opus=deepseek-reasoner",
		"model_sonnet=deepseek-chat",
		"model_haiku=deepseek-chat",
		"model_small_fast=deepseek-chat",
		"capabilities=full",
		"unlock_auto_mode=1",
		"",
		"# proxy",
		"proxy_effort=auto:high",
		"proxy_effort_opus=max",
		"proxy_effort_sonnet=high",
		"proxy_effort_haiku=auto",
		"proxy_effort_small_fast=off",
		"proxy_strip_thinking=true",
		"proxy_bind=127.0.0.1",
		"proxy_debug=true",
		"update_check_interval=12",
		"",
		"# observability",
		"otlp_endpoints=http://signoz.local:30318,http://otel.example.com:4318",
		"otlp_headers=signoz-access-token: secret-abc; x-tenant: home",
		"otlp_service_name=claude-ds-proxy",
		"otlp_deployment_environment=prod",
		"otlp_resource_attributes=service.namespace=claude-ds, host.role=workstation",
		"otlp_protocol=http",
		"",
	}, "\n")
	path := writeTempConfig(t, body)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Schema", cfg.Schema, 1},
		{"APIKeyRef", cfg.APIKeyRef, "op://home-kubernetes/DeepSeek/credential"},
		{"BaseURL", cfg.BaseURL, "https://api.example.com/anthropic"},
		{"Model", cfg.Model, "deepseek-v4-pro"},
		{"ModelOpus", cfg.ModelOpus, "deepseek-reasoner"},
		{"Capabilities", cfg.Capabilities, "full"},
		{"UnlockAutoMode", cfg.UnlockAutoMode, true},
		{"ProxyEffort", cfg.ProxyEffort, "auto:high"},
		{"ProxyEffortOpus", cfg.ProxyEffortOpus, "max"},
		{"ProxyStripThinking", cfg.ProxyStripThinking, "true"},
		{"ProxyBind", cfg.ProxyBind, "127.0.0.1"},
		{"ProxyDebug", cfg.ProxyDebug, true},
		{"UpdateCheckInterval", cfg.UpdateCheckInterval, 12},
		{"OTLPEndpoints", cfg.OTLPEndpoints, []string{"http://signoz.local:30318", "http://otel.example.com:4318"}},
		{"OTLPServiceName", cfg.OTLPServiceName, "claude-ds-proxy"},
		{"OTLPDeploymentEnvironment", cfg.OTLPDeploymentEnvironment, "prod"},
		{"OTLPProtocol", cfg.OTLPProtocol, "http"},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, c.got, c.want)
		}
	}

	wantHeaders := map[string]string{
		"signoz-access-token": "secret-abc",
		"x-tenant":            "home",
	}
	if !reflect.DeepEqual(cfg.OTLPHeaders, wantHeaders) {
		t.Errorf("OTLPHeaders: got %#v, want %#v", cfg.OTLPHeaders, wantHeaders)
	}
	wantRA := map[string]string{
		"service.namespace": "claude-ds",
		"host.role":         "workstation",
	}
	if !reflect.DeepEqual(cfg.OTLPResourceAttributes, wantRA) {
		t.Errorf("OTLPResourceAttributes: got %#v, want %#v", cfg.OTLPResourceAttributes, wantRA)
	}
	if len(cfg.Unknown) != 0 {
		t.Errorf("Unknown should be empty, got %#v", cfg.Unknown)
	}
}

// ---------------------------------------------------------------------
// LoadConfig — defaults populate when keys are absent
// ---------------------------------------------------------------------

func TestConfigLoadAppliesDefaults(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	// Minimal file: schema only.
	path := writeTempConfig(t, "_schema=1\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL default: got %q, want %q", cfg.BaseURL, defaultBaseURL)
	}
	if cfg.Model != defaultModel {
		t.Errorf("Model default: got %q, want %q", cfg.Model, defaultModel)
	}
	if cfg.ProxyEffort != defaultProxyEffort {
		t.Errorf("ProxyEffort default: got %q, want %q", cfg.ProxyEffort, defaultProxyEffort)
	}
	if cfg.ProxyBind != defaultProxyBind {
		t.Errorf("ProxyBind default: got %q, want %q", cfg.ProxyBind, defaultProxyBind)
	}
	if cfg.UpdateCheckInterval != defaultUpdateCheckInterval {
		t.Errorf("UpdateCheckInterval default: got %d, want %d", cfg.UpdateCheckInterval, defaultUpdateCheckInterval)
	}
	if cfg.OTLPServiceName != defaultOTLPServiceName {
		t.Errorf("OTLPServiceName default: got %q", cfg.OTLPServiceName)
	}
	if cfg.OTLPDeploymentEnvironment != defaultOTLPDeploymentEnvironment {
		t.Errorf("OTLPDeploymentEnvironment default: got %q", cfg.OTLPDeploymentEnvironment)
	}
	if cfg.OTLPProtocol != defaultOTLPProtocol {
		t.Errorf("OTLPProtocol default: got %q", cfg.OTLPProtocol)
	}
	if len(cfg.OTLPEndpoints) != 0 {
		t.Errorf("OTLPEndpoints default: want empty, got %#v", cfg.OTLPEndpoints)
	}
}

// LoadConfig on a *missing* file returns defaults, not error.
func TestConfigLoadMissingFileGivesDefaults(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	path := filepath.Join(t.TempDir(), "does-not-exist")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on missing: %v", err)
	}
	if cfg.BaseURL != defaultBaseURL {
		t.Errorf("BaseURL: got %q", cfg.BaseURL)
	}
}

// ---------------------------------------------------------------------
// Damaged config repair
// ---------------------------------------------------------------------

func TestConfigDamagedFileIsRepaired(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	body := strings.Join([]string{
		"_schema=1",
		"model=deepseek-v4-pro",
		"this-line-is-not-valid",        // malformed: dashes in key
		"another bad line with spaces",  // malformed: no =
		"proxy_effort=auto",
		"# comment ok",
		"",
	}, "\n")
	path := writeTempConfig(t, body)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Model != "deepseek-v4-pro" {
		t.Errorf("Model after repair: got %q", cfg.Model)
	}
	if cfg.ProxyEffort != "auto" {
		t.Errorf("ProxyEffort after repair: got %q", cfg.ProxyEffort)
	}

	// A `.broken.<timestamp>.bak` file must exist next to the config.
	matches, err := filepath.Glob(path + ".broken.*.bak")
	if err != nil {
		t.Fatalf("glob broken bak: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one .broken.*.bak, got %d (%v)", len(matches), matches)
	}

	// Repaired file: every non-comment line is valid.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	for lineno, raw := range strings.Split(string(data), "\n") {
		if !ValidateLine(raw) {
			t.Errorf("repaired line %d still invalid: %q", lineno+1, raw)
		}
	}

	// Repaired file declares schema explicitly.
	if !strings.Contains(string(data), "_schema=1") {
		t.Errorf("repaired file missing _schema=1:\n%s", data)
	}
}

// Damaged-line repair drops unknown keys (matches the Bash launcher).
func TestConfigRepairDropsUnknownKeys(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	// Mix a malformed line in to force the repair flow, plus an unknown
	// known-shaped key. The Bash launcher drops unknown keys on repair.
	body := strings.Join([]string{
		"_schema=1",
		"model=deepseek-v4-pro",
		"bogus key with spaces",
		"made_up_key=42",
		"",
	}, "\n")
	path := writeTempConfig(t, body)

	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "made_up_key=") {
		t.Errorf("repaired file kept unknown key:\n%s", data)
	}
}

// ---------------------------------------------------------------------
// Schema 0 → 1 migration
// ---------------------------------------------------------------------

func TestConfigSchemaZeroMigratesToOne(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	body := strings.Join([]string{
		"# legacy v0 config — no _schema line",
		"model=deepseek-v4-pro",
		"proxy_effort=auto",
		"",
	}, "\n")
	path := writeTempConfig(t, body)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Schema != 1 {
		t.Errorf("Schema after migration: got %d, want 1", cfg.Schema)
	}

	// Backup at <path>.v0.bak must exist (the literal old version).
	if _, err := os.Stat(path + ".v0.bak"); err != nil {
		t.Errorf("expected v0.bak backup, got: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated: %v", err)
	}
	// First non-comment line should be the schema declaration.
	lines := strings.Split(string(data), "\n")
	if lines[0] != "_schema=1" {
		t.Errorf("first line should be _schema=1, got %q", lines[0])
	}

	// Original keys preserved.
	if !strings.Contains(string(data), "model=deepseek-v4-pro") {
		t.Errorf("model line missing post-migrate:\n%s", data)
	}
	if !strings.Contains(string(data), "proxy_effort=auto") {
		t.Errorf("proxy_effort line missing post-migrate:\n%s", data)
	}
}

// A future schema number on disk must error rather than silently
// downgrade — the user has a newer claude-ds elsewhere.
func TestConfigFutureSchemaErrors(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	path := writeTempConfig(t, "_schema=999\nmodel=deepseek-v4-pro\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig accepted a future schema; want error")
	}
}

// ---------------------------------------------------------------------
// Unknown keys: warn + preserve through round-trip
// ---------------------------------------------------------------------

func TestConfigUnknownKeysRoundTrip(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	body := strings.Join([]string{
		"_schema=1",
		"model=deepseek-v4-pro",
		"experimental_flag=on",
		"future_setting=tomorrow",
		"",
	}, "\n")
	path := writeTempConfig(t, body)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Unknown keys should be present in cfg.Unknown.
	wantUnknown := map[string]string{
		"experimental_flag": "on",
		"future_setting":    "tomorrow",
	}
	if !reflect.DeepEqual(cfg.Unknown, wantUnknown) {
		t.Fatalf("Unknown: got %#v, want %#v", cfg.Unknown, wantUnknown)
	}

	// Round-trip: write, then re-read; the unknown keys should survive.
	rtPath := filepath.Join(t.TempDir(), "config")
	cfg.Path = rtPath
	if err := WriteConfig(rtPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	// Mode check: the spec mandates 0600.
	info, err := os.Stat(rtPath)
	if err != nil {
		t.Fatalf("stat written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("written file mode: got %o, want 0600", info.Mode().Perm())
	}

	cfg2, err := LoadConfig(rtPath)
	if err != nil {
		t.Fatalf("LoadConfig round-trip: %v", err)
	}
	if !reflect.DeepEqual(cfg2.Unknown, wantUnknown) {
		t.Errorf("Unknown after round-trip: got %#v, want %#v", cfg2.Unknown, wantUnknown)
	}
}

// ---------------------------------------------------------------------
// OTel env-var overrides
// ---------------------------------------------------------------------

func TestConfigEnvOverrideEndpoint(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return s, nil })

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://signoz.local:30318")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	path := writeTempConfig(t, "_schema=1\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !contains(cfg.OTLPEndpoints, "http://signoz.local:30318") {
		t.Errorf("env endpoint not merged: %#v", cfg.OTLPEndpoints)
	}
}

func TestConfigEnvOverrideHeadersThroughResolver(t *testing.T) {
	calls := map[string]int{}
	stubResolve(t, func(s string) (string, error) {
		calls[s]++
		if s == "op://demo/token" {
			return "RESOLVED-SECRET", nil
		}
		return s, nil
	})

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "signoz-access-token=op://demo/token,x-tenant=home")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	path := writeTempConfig(t, "_schema=1\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.OTLPHeaders["signoz-access-token"] != "RESOLVED-SECRET" {
		t.Errorf("expected resolved secret, got %q", cfg.OTLPHeaders["signoz-access-token"])
	}
	if cfg.OTLPHeaders["x-tenant"] != "home" {
		t.Errorf("expected passthrough header value, got %q", cfg.OTLPHeaders["x-tenant"])
	}
	if calls["op://demo/token"] == 0 {
		t.Errorf("resolver was never called for the secret ref")
	}
}

func TestConfigEnvOverrideResourceAttributesMerge(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return s, nil })

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "host.role=ci, env.kind=ephemeral")

	body := strings.Join([]string{
		"_schema=1",
		"otlp_resource_attributes=service.namespace=claude-ds, host.role=workstation",
		"",
	}, "\n")
	path := writeTempConfig(t, body)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Env should override the file value for `host.role`, and add the
	// new `env.kind`. File-only `service.namespace` survives.
	if cfg.OTLPResourceAttributes["host.role"] != "ci" {
		t.Errorf("host.role: got %q, want ci", cfg.OTLPResourceAttributes["host.role"])
	}
	if cfg.OTLPResourceAttributes["env.kind"] != "ephemeral" {
		t.Errorf("env.kind: got %q, want ephemeral", cfg.OTLPResourceAttributes["env.kind"])
	}
	if cfg.OTLPResourceAttributes["service.namespace"] != "claude-ds" {
		t.Errorf("service.namespace lost: %#v", cfg.OTLPResourceAttributes)
	}
}

// ---------------------------------------------------------------------
// WriteConfig — known-key ordering and default-omission
// ---------------------------------------------------------------------

func TestConfigWriteOrderingAndDefaultOmission(t *testing.T) {
	t.Parallel()
	stubResolve(t, func(s string) (string, error) { return s, nil })

	cfg := &Config{
		Schema:      CurrentSchema,
		Model:       defaultModel, // should be omitted (matches default)
		BaseURL:     "https://api.example.com/anthropic",
		ProxyEffort: "auto:high",
		Unknown: map[string]string{
			"zeta_key":  "z",
			"alpha_key": "a",
		},
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// The schema line must come first among keyed lines.
	body := stripComments(string(data))
	if !strings.HasPrefix(body, "_schema=") {
		t.Errorf("first key should be _schema, got body:\n%s", body)
	}

	// Default-equal value (`model=`) must NOT be emitted.
	if strings.Contains(body, "model=") {
		t.Errorf("default model value should be omitted; body:\n%s", body)
	}

	// Unknown keys appear, sorted.
	idxAlpha := strings.Index(body, "alpha_key=a")
	idxZeta := strings.Index(body, "zeta_key=z")
	if idxAlpha < 0 || idxZeta < 0 || idxAlpha > idxZeta {
		t.Errorf("unknown keys should appear sorted; body:\n%s", body)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func stripComments(s string) string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, line)
	}
	sort.SliceStable(out, func(i, j int) bool { return false }) // preserve order; just join
	return strings.Join(out, "\n")
}
