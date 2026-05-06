// CDS-23 — main.go integration tests.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestParseArgs_SubcommandDispatch covers each entry-point flag.
func TestParseArgs_SubcommandDispatch(t *testing.T) {
	cases := []struct {
		args []string
		want subcommand
	}{
		{[]string{"--version"}, subcommandVersion},
		{[]string{"-V"}, subcommandVersion},
		{[]string{"--help"}, subcommandHelp},
		{[]string{"-h"}, subcommandHelp},
		{[]string{"--print-schema"}, subcommandPrintSchema},
		{[]string{"--doctor"}, subcommandDoctor},
		{[]string{"--setup"}, subcommandSetup},
		{[]string{"--rotate-key"}, subcommandRotateKey},
		{[]string{"--reset-password"}, subcommandRotateKey},
		{[]string{"upgrade"}, subcommandUpgrade},
		{[]string{"update"}, subcommandUpgrade},
		{[]string{"--", "anything"}, subcommandLaunch},
	}
	for _, tc := range cases {
		got := parseArgs(tc.args).cmd
		if got != tc.want {
			t.Errorf("parseArgs(%v).cmd = %d, want %d", tc.args, got, tc.want)
		}
	}
}

// TestParseArgs_DoubleDashTerminator — args after `--` forward verbatim.
func TestParseArgs_DoubleDashTerminator(t *testing.T) {
	p := parseArgs([]string{"--", "--doctor", "foo"})
	if p.cmd != subcommandLaunch {
		t.Errorf("expected launch dispatch, got %d", p.cmd)
	}
	if got := strings.Join(p.passthrough, " "); got != "--doctor foo" {
		t.Errorf("passthrough = %q, want %q", got, "--doctor foo")
	}
}

// TestParseArgs_ProxyOnSpec — `--proxy-on=auto` parses the spec.
func TestParseArgs_ProxyOnSpec(t *testing.T) {
	p := parseArgs([]string{"--proxy-on=auto:7"})
	if !p.proxyOnSet {
		t.Fatal("proxyOnSet not set")
	}
	if p.proxyOnSpec != "auto:7" {
		t.Errorf("proxyOnSpec = %q, want auto:7", p.proxyOnSpec)
	}
}

// TestParseArgs_ProxyOnNoSpec — bare `--proxy-on` flips the flag with empty spec.
func TestParseArgs_ProxyOnNoSpec(t *testing.T) {
	p := parseArgs([]string{"--proxy-on"})
	if !p.proxyOnSet {
		t.Fatal("proxyOnSet not set")
	}
	if p.proxyOnSpec != "" {
		t.Errorf("proxyOnSpec = %q, want empty", p.proxyOnSpec)
	}
}

// TestApplyFlagOverrides_ProxyOff zeros out every effort spec.
func TestApplyFlagOverrides_ProxyOff(t *testing.T) {
	cfg := &Config{
		ProxyEffort:          "auto",
		ProxyEffortOpus:      "max",
		ProxyEffortSonnet:    "high",
		ProxyEffortHaiku:     "auto",
		ProxyEffortSmallFast: "none",
	}
	applyFlagOverrides(cfg, parsedArgs{proxyOffSet: true})
	for _, s := range []string{
		cfg.ProxyEffort, cfg.ProxyEffortOpus, cfg.ProxyEffortSonnet,
		cfg.ProxyEffortHaiku, cfg.ProxyEffortSmallFast,
	} {
		if s != "off" {
			t.Errorf("expected all effort specs == off, got %q", s)
		}
	}
}

// TestApplyFlagOverrides_ProxyOnDefault — bare --proxy-on uses "auto".
func TestApplyFlagOverrides_ProxyOnDefault(t *testing.T) {
	cfg := &Config{ProxyEffort: "off"}
	applyFlagOverrides(cfg, parsedArgs{proxyOnSet: true, proxyOnSpec: ""})
	if cfg.ProxyEffort != "auto" {
		t.Errorf("expected ProxyEffort=auto, got %q", cfg.ProxyEffort)
	}
}

// TestApplyFlagOverrides_ProxyOnSpec — --proxy-on=<spec> wins.
func TestApplyFlagOverrides_ProxyOnSpec(t *testing.T) {
	cfg := &Config{ProxyEffort: "off"}
	applyFlagOverrides(cfg, parsedArgs{proxyOnSet: true, proxyOnSpec: "max"})
	if cfg.ProxyEffort != "max" {
		t.Errorf("expected ProxyEffort=max, got %q", cfg.ProxyEffort)
	}
}

// TestBuildClaudeEnv_AutoModeUnlock verifies the spoofed model env vars
// match the spec exactly.
func TestBuildClaudeEnv_AutoModeUnlock(t *testing.T) {
	cfg := &Config{
		Model:          "deepseek-v4-pro",
		BaseURL:        "https://api.deepseek.com/anthropic",
		UnlockAutoMode: true,
	}
	env := buildClaudeEnv(cfg, "tok")

	want := map[string]string{
		"ANTHROPIC_MODEL":                "claude-opus-4-7",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "claude-opus-4-7",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4-6",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "claude-haiku-4-5",
		"ANTHROPIC_SMALL_FAST_MODEL":     "claude-haiku-4-5",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
	// Picker labels and descriptions present.
	for _, k := range []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME",
		"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION",
	} {
		if env[k] == "" {
			t.Errorf("env[%s] missing", k)
		}
	}
	if !strings.Contains(env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"], "DeepSeek (opus tier · deepseek-v4-pro)") {
		t.Errorf("opus name missing tier label: %q", env["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME"])
	}
}

// TestBuildClaudeEnv_NoAutoMode — without unlock, all tiers point at the
// resolved model, no NAME/DESCRIPTION envs.
func TestBuildClaudeEnv_NoAutoMode(t *testing.T) {
	cfg := &Config{
		Model:          "deepseek-v4-pro",
		BaseURL:        "https://api.deepseek.com/anthropic",
		UnlockAutoMode: false,
	}
	env := buildClaudeEnv(cfg, "tok")
	if env["ANTHROPIC_MODEL"] != "deepseek-v4-pro" {
		t.Errorf("ANTHROPIC_MODEL = %q, want deepseek-v4-pro", env["ANTHROPIC_MODEL"])
	}
	for _, k := range []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME",
	} {
		if _, ok := env[k]; ok {
			t.Errorf("expected %s unset under no-auto-mode", k)
		}
	}
}

// TestBuildClaudeEnv_PerTierOverrides — model_opus etc. win over spoofs.
func TestBuildClaudeEnv_PerTierOverrides(t *testing.T) {
	cfg := &Config{
		Model:          "deepseek-v4-pro",
		ModelOpus:      "deepseek-r1",
		ModelSonnet:    "deepseek-r1",
		ModelHaiku:     "deepseek-flash",
		ModelSmallFast: "deepseek-flash",
		UnlockAutoMode: true,
	}
	env := buildClaudeEnv(cfg, "tok")
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "deepseek-r1" {
		t.Errorf("expected per-tier opus override, got %q", env["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
	if env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "deepseek-flash" {
		t.Errorf("expected per-tier haiku override, got %q", env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
	}
	if env["ANTHROPIC_MODEL"] != "claude-opus-4-7" {
		t.Errorf("ANTHROPIC_MODEL should still be the spoof under unlock=true, got %q", env["ANTHROPIC_MODEL"])
	}
}

// TestBuildClaudeEnv_Capabilities — capabilities propagate to all three tiers.
func TestBuildClaudeEnv_Capabilities(t *testing.T) {
	cfg := &Config{
		Model:        "deepseek-v4-pro",
		Capabilities: "thinking,interleaved_thinking",
	}
	env := buildClaudeEnv(cfg, "tok")
	for _, k := range []string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES",
		"ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES",
	} {
		if env[k] != "thinking,interleaved_thinking" {
			t.Errorf("env[%s] = %q, want thinking,interleaved_thinking", k, env[k])
		}
	}
}

// TestEnvSlice_OverridesWin — over map keys win over inherited env.
func TestEnvSlice_OverridesWin(t *testing.T) {
	base := []string{"FOO=base", "BAR=keep"}
	over := map[string]string{"FOO": "over"}
	got := envSlice(over, base)
	want := map[string]string{"FOO": "over", "BAR": "keep"}
	have := map[string]string{}
	for _, kv := range got {
		i := strings.IndexByte(kv, '=')
		have[kv[:i]] = kv[i+1:]
	}
	for k, v := range want {
		if have[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, have[k], v)
		}
	}
}

// TestWarnFlashDowngrade_NoOpWhenAutoModeOff — must not print or sleep
// when unlock_auto_mode is unset.
func TestWarnFlashDowngrade_NoOpWhenAutoModeOff(t *testing.T) {
	cfg := &Config{UnlockAutoMode: false}
	start := time.Now()
	warnFlashDowngrade(cfg)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("warnFlashDowngrade slept %v with unlock=false", elapsed)
	}
}

// TestBuildSystemPrompt_ContainsModelAndAutoMode — verifies dynamic
// substitution.
func TestBuildSystemPrompt_ContainsModelAndAutoMode(t *testing.T) {
	cfg := &Config{Model: "deepseek-r1", UnlockAutoMode: true, BaseURL: "https://api.deepseek.com/anthropic"}
	got := buildSystemPrompt(cfg)
	if !strings.Contains(got, "deepseek-r1") {
		t.Errorf("system prompt missing model id: %q", got)
	}
	if !strings.Contains(got, "Auto-mode is on") {
		t.Errorf("system prompt missing auto-mode status: %q", got)
	}
	if !strings.Contains(got, "claude-ds") {
		t.Errorf("system prompt missing claude-ds header")
	}
}

// TestRunVersion_PrintsHeader — exercises the --version path with stdout
// captured.
func TestRunVersion_PrintsHeader(t *testing.T) {
	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	rc := runVersion(stdout, stderr)
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if !strings.Contains(stdout.String(), "claude-ds "+VERSION) {
		t.Errorf("stdout missing version header: %q", stdout.String())
	}
}

// TestRun_PrintSchema — exercises the --print-schema dispatch.
func TestRun_PrintSchema(t *testing.T) {
	// Capture stdout via a pipe.
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	rc := run([]string{"--print-schema"})
	w.Close()
	os.Stdout = old
	buf := make([]byte, 64)
	n, _ := r.Read(buf)
	if rc != 0 {
		t.Errorf("rc = %d, want 0", rc)
	}
	if !strings.Contains(string(buf[:n]), "1") {
		t.Errorf("expected schema number on stdout, got %q", string(buf[:n]))
	}
}

// TestSignalForwarding_SubprocessReceivesSIGINT spawns a real child
// process and verifies that a SIGINT delivered to the parent's
// goroutine forwards correctly.
func TestSignalForwarding_SubprocessReceivesSIGINT(t *testing.T) {
	// Use `sh -c 'trap "exit 7" INT; sleep 30'` so we can verify the
	// child saw INT (exit code 7).
	cmd := exec.Command("sh", "-c", "trap 'exit 7' INT; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for sh to install the trap. (Kernel signal delivery to a
	// just-forked process can race the trap statement.)
	time.Sleep(150 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected non-zero exit, got nil")
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("not an ExitError: %v", err)
	}
	if ee.ExitCode() != 7 {
		t.Errorf("exit code = %d, want 7 (sh trap fired)", ee.ExitCode())
	}
}

// TestDefaultConfigPath_RespectsXDG honours XDG_CONFIG_HOME.
func TestDefaultConfigPath_RespectsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	got := defaultConfigPath()
	want := filepath.Join("/tmp/xdg", "claude-ds", "config")
	if got != want {
		t.Errorf("defaultConfigPath = %q, want %q", got, want)
	}
}
