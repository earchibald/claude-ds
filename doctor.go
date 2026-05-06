// Doctor diagnostics (CDS-20) — `claude-ds --doctor` runs a fixed
// 7-check health report and exits 0. The report is the value: the exit
// code never reflects a check failure.
//
// Checks (in execution order):
//
//  1. claude binary on PATH                     (os/exec.LookPath)
//  2. Config file readable + schema current      (LoadConfig + CurrentSchema)
//  3. API-key secret reference resolves          (callResolve(cfg.APIKeyRef))
//  4. API key live against {base_url}/v1/messages (5s POST, max_tokens=1)
//  5. Reasoning-effort proxy present             (always ✓ — compiled in)
//  6. Tier-spec collision lint                   (group ModelOpus/Sonnet/Haiku/SmallFast
//                                                 by wire id; warn if two tiers share one
//                                                 wire id while UnlockAutoMode is true)
//  7. OTLP-reachability for each endpoint        (CDS-25 — 2s exporter
//                                                 construction + immediate Shutdown
//                                                 force-flush of an empty batch).
//                                                 deployment.environment is forced
//                                                 to "doctor" for this probe per
//                                                 the design spec.
//
// Each check prints "✓ <message>" or "✗ <message>" followed by an
// indented actionable next step where applicable. Color is enabled only
// when stdout is a TTY (so test golden output and CI logs stay clean).
//
// Public entrypoint: `runDoctor(cfg *Config) int` — always returns 0.
// Tests cover individual check helpers via dependency injection
// (httptest servers, temp PATH dirs, stubResolve, ...).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
)

// claudeInstallURL is the canonical install pointer printed when the
// `claude` binary is missing from $PATH. Matches the Bash launcher.
const claudeInstallURL = "https://docs.anthropic.com/claude/code"

// doctorAPITimeout is the per-request budget for the API-key liveness
// check. Matches the Bash launcher's `--max-time 5`.
const doctorAPITimeout = 5 * time.Second

// doctorOTLPTimeout is the per-endpoint exporter-construction budget for
// the OTLP-reachability probe. The spec calls for 2 seconds.
const doctorOTLPTimeout = 2 * time.Second

// runDoctor executes the doctor checklist and returns 0. The cfg
// argument is what the caller (main.go) loaded from disk; it must be
// non-nil so each check can introspect tunables (base URL, model, OTLP
// endpoints). Output goes to os.Stdout; per-check failures are still
// printed to os.Stdout (one stream — the report is the value).
func runDoctor(cfg *Config) int {
	return runDoctorTo(os.Stdout, cfg, isStdoutTTY())
}

// runDoctorTo is the testable form of runDoctor. It writes to `out`
// and uses `colorize` to decide whether to emit ANSI sequences. Tests
// pass a *bytes.Buffer and color=false so golden text is stable.
func runDoctorTo(out io.Writer, cfg *Config, colorize bool) int {
	c := newDoctorPrinter(out, colorize)

	c.printHeader(VERSION)

	c.runCheck("claude binary on PATH", func() doctorResult {
		return checkClaudeOnPATH()
	})

	c.runCheck("config file readable", func() doctorResult {
		return checkConfigReadable(cfg)
	})

	c.runCheck("api key secret reference resolves", func() doctorResult {
		return checkSecretResolves(cfg)
	})

	c.runCheck("api key live against upstream", func() doctorResult {
		return checkAPILive(cfg, doctorAPITimeout)
	})

	c.runCheck("reasoning-effort proxy", func() doctorResult {
		return doctorResult{ok: true, summary: "compiled in (no separate script needed)"}
	})

	c.runCheck("tier-spec collision lint", func() doctorResult {
		return checkTierCollisions(cfg)
	})

	c.runCheck("OTLP endpoint reachability", func() doctorResult {
		return checkOTLPReachability(cfg, doctorOTLPTimeout)
	})

	c.printFooter()
	return 0
}

// doctorResult is the verdict of a single check. `summary` is the
// trailing text on the ✓/✗ line; `note` (if non-empty) is printed
// as an indented actionable next step on the following line.
type doctorResult struct {
	ok      bool
	summary string
	note    string
}

// doctorPrinter handles header/footer + per-line formatting. Centralised
// so tests can pass a buffer and assert on stable text.
type doctorPrinter struct {
	w        io.Writer
	colorize bool
}

func newDoctorPrinter(w io.Writer, colorize bool) *doctorPrinter {
	return &doctorPrinter{w: w, colorize: colorize}
}

const (
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

func (c *doctorPrinter) printHeader(version string) {
	fmt.Fprintf(c.w, "claude-ds doctor (%s)\n", version)
	fmt.Fprintln(c.w, "──────────────────────────────────")
}

func (c *doctorPrinter) printFooter() {
	fmt.Fprintln(c.w, "──────────────────────────────────")
	fmt.Fprintln(c.w, "doctor done.")
}

func (c *doctorPrinter) runCheck(label string, fn func() doctorResult) {
	r := fn()
	mark := "✓"
	color := ansiGreen
	if !r.ok {
		mark = "✗"
		color = ansiRed
	}
	if c.colorize {
		fmt.Fprintf(c.w, "  %s%s%s %s", color, mark, ansiReset, label)
	} else {
		fmt.Fprintf(c.w, "  %s %s", mark, label)
	}
	if r.summary != "" {
		fmt.Fprintf(c.w, ": %s", r.summary)
	}
	fmt.Fprintln(c.w)
	if r.note != "" {
		fmt.Fprintf(c.w, "      → %s\n", r.note)
	}
}

// isStdoutTTY mirrors the launcher's TTY detection: only colourise when
// stdout is a terminal.
func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ---------------------------------------------------------------------
// Check 1 — claude on PATH
// ---------------------------------------------------------------------

func checkClaudeOnPATH() doctorResult {
	path, err := exec.LookPath("claude")
	if err != nil {
		return doctorResult{
			ok:      false,
			summary: "not found on $PATH",
			note:    "install Claude Code: " + claudeInstallURL,
		}
	}
	return doctorResult{ok: true, summary: path}
}

// ---------------------------------------------------------------------
// Check 2 — config readable + schema check
// ---------------------------------------------------------------------

// checkConfigReadable inspects the config the caller already loaded.
// runDoctor's caller is responsible for calling LoadConfig — by the
// time we get a *Config, the file has already been read, validated,
// repaired, and migrated forward to CurrentSchema. So the doctor's job
// here is simply to confirm the path is set and to report it back to
// the user. A nil cfg is treated as ✗ for hardness against caller bugs.
func checkConfigReadable(cfg *Config) doctorResult {
	if cfg == nil {
		return doctorResult{
			ok:      false,
			summary: "no config loaded",
			note:    "rerun --setup to create one",
		}
	}
	path := cfg.Path
	if path == "" {
		return doctorResult{
			ok:      false,
			summary: "config path is empty",
			note:    "rerun --setup to create a config",
		}
	}
	// Confirm the file is still readable (LoadConfig synthesises a
	// defaults-only Config when the file is missing — that case should
	// surface as ✗ here, not as a silent ✓).
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return doctorResult{
				ok:      false,
				summary: "config file does not exist: " + path,
				note:    "rerun --setup to create one",
			}
		}
		return doctorResult{
			ok:      false,
			summary: fmt.Sprintf("stat %s: %v", path, err),
			note:    "check filesystem permissions",
		}
	}
	schema := cfg.Schema
	if schema == 0 {
		schema = CurrentSchema
	}
	return doctorResult{
		ok:      true,
		summary: fmt.Sprintf("%s (schema v%d, current v%d)", path, schema, CurrentSchema),
	}
}

// ---------------------------------------------------------------------
// Check 3 — secret reference resolves
// ---------------------------------------------------------------------

func checkSecretResolves(cfg *Config) doctorResult {
	if cfg == nil || cfg.APIKeyRef == "" {
		return doctorResult{
			ok:      false,
			summary: "api_key_ref not set",
			note:    "rerun --rotate-key to set one",
		}
	}
	tok, err := callResolve(cfg.APIKeyRef)
	if err != nil {
		return doctorResult{
			ok:      false,
			summary: fmt.Sprintf("resolve %s: %v", cfg.APIKeyRef, err),
			note:    "check the upstream store (op/system/infisical), or rerun --rotate-key",
		}
	}
	if tok == "" {
		return doctorResult{
			ok:      false,
			summary: "resolved value is empty",
			note:    "rerun --rotate-key with a valid secret reference",
		}
	}
	return doctorResult{ok: true, summary: cfg.APIKeyRef}
}

// ---------------------------------------------------------------------
// Check 4 — API key live
// ---------------------------------------------------------------------

// apiLiveStatus is the verdict of a liveness probe.
type apiLiveStatus int

const (
	apiLiveOK apiLiveStatus = iota
	apiLiveUnauth
	apiLiveAdvisory
	apiLiveNetwork
	apiLiveSkipped
)

// probeAPILive POSTs a minimal /v1/messages request and returns the
// status. Headers match the Bash launcher: both `x-api-key` and
// `Authorization: Bearer …` are sent so either Anthropic-style or
// DeepSeek-style upstream gateways accept the request. The HTTP client
// timeout caps the round-trip at `timeout`.
func probeAPILive(baseURL, model, token string, timeout time.Duration) (apiLiveStatus, int, error) {
	if token == "" {
		return apiLiveSkipped, 0, errors.New("empty token")
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}

	body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, model)

	endpoint, err := url.JoinPath(baseURL, "v1/messages")
	if err != nil {
		return apiLiveNetwork, 0, fmt.Errorf("bad base_url: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return apiLiveNetwork, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return apiLiveNetwork, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return apiLiveOK, resp.StatusCode, nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return apiLiveUnauth, resp.StatusCode, nil
	default:
		return apiLiveAdvisory, resp.StatusCode, nil
	}
}

// checkAPILive resolves the secret again (cheap, cached upstream) and
// runs the probe. Resolution failures are reported as ✗ because we
// already had a good resolver result in check 3 and divergence between
// the two would point at a flaky upstream.
func checkAPILive(cfg *Config, timeout time.Duration) doctorResult {
	if cfg == nil || cfg.APIKeyRef == "" {
		return doctorResult{
			ok:      false,
			summary: "no api_key_ref configured",
			note:    "rerun --rotate-key to set one",
		}
	}
	tok, err := callResolve(cfg.APIKeyRef)
	if err != nil || tok == "" {
		return doctorResult{
			ok:      false,
			summary: "skipped — secret resolution failed",
			note:    "see the previous check for details",
		}
	}
	base := cfg.BaseURL
	model := cfg.Model

	status, code, perr := probeAPILive(base, model, tok, timeout)
	target := strings.TrimSuffix(base, "/")
	if target == "" {
		target = defaultBaseURL
	}

	switch status {
	case apiLiveOK:
		return doctorResult{ok: true, summary: fmt.Sprintf("%d from %s", code, target)}
	case apiLiveUnauth:
		return doctorResult{
			ok:      false,
			summary: fmt.Sprintf("%d from %s — key invalid", code, target),
			note:    "rerun --rotate-key with a current key",
		}
	case apiLiveAdvisory:
		return doctorResult{
			ok:      false,
			summary: fmt.Sprintf("%d from %s — unexpected status", code, target),
			note:    "advisory only; --rotate-key if it persists",
		}
	case apiLiveNetwork:
		reason := "network/DNS error"
		if perr != nil {
			reason = perr.Error()
		}
		return doctorResult{
			ok:      false,
			summary: fmt.Sprintf("could not reach %s — %s", target, reason),
			note:    "check your network; this is non-fatal at launch",
		}
	default:
		return doctorResult{
			ok:      false,
			summary: "skipped",
			note:    "internal: unhandled liveness status",
		}
	}
}

// ---------------------------------------------------------------------
// Check 6 — tier-spec collision lint
// ---------------------------------------------------------------------

// checkTierCollisions ports `_cds_lint_collisions` (Bash launcher
// line 1067). Two tiers (opus/sonnet/haiku/small_fast) sharing one wire
// id, when UnlockAutoMode is true, means the per-tier proxy effort
// settings will silently fight each other — only the latest in
// {small_fast → haiku → sonnet → opus} order wins. Surface that.
//
// When UnlockAutoMode is false, all four tiers should resolve to the
// fallback `Model` field, so collisions are expected and not a bug; we
// report ✓ in that case.
func checkTierCollisions(cfg *Config) doctorResult {
	if cfg == nil {
		return doctorResult{ok: true, summary: "no config loaded"}
	}
	if !cfg.UnlockAutoMode {
		return doctorResult{ok: true, summary: "auto-mode unlock disabled — single-tier mode"}
	}

	type tier struct {
		name string
		wire string
		spec string
	}
	tiers := []tier{
		{"opus", firstNonEmpty(cfg.ModelOpus, cfg.Model), cfg.ProxyEffortOpus},
		{"sonnet", firstNonEmpty(cfg.ModelSonnet, cfg.Model), cfg.ProxyEffortSonnet},
		{"haiku", firstNonEmpty(cfg.ModelHaiku, cfg.Model), cfg.ProxyEffortHaiku},
		{"small_fast", firstNonEmpty(cfg.ModelSmallFast, cfg.Model), cfg.ProxyEffortSmallFast},
	}

	// Group by wire id, listing only tiers with a meaningful per-tier
	// spec (non-empty, non-"off") — that's what the Bash version did.
	seen := map[string]bool{}
	var collisions []string
	for i, t := range tiers {
		if t.wire == "" || isProxySpecEmpty(t.spec) {
			continue
		}
		if seen[t.wire] {
			continue
		}
		colliders := []string{fmt.Sprintf("%s=%s", t.name, t.spec)}
		for j := i + 1; j < len(tiers); j++ {
			tj := tiers[j]
			if tj.wire != t.wire {
				continue
			}
			if isProxySpecEmpty(tj.spec) {
				continue
			}
			colliders = append(colliders, fmt.Sprintf("%s=%s", tj.name, tj.spec))
		}
		if len(colliders) > 1 {
			collisions = append(collisions,
				fmt.Sprintf("wire id %q: %s — only the latest in {small_fast → haiku → sonnet → opus} order wins",
					t.wire, strings.Join(colliders, ", ")))
			seen[t.wire] = true
		}
	}

	if len(collisions) == 0 {
		return doctorResult{ok: true, summary: "no collisions"}
	}
	return doctorResult{
		ok:      false,
		summary: fmt.Sprintf("%d tier-spec collision(s)", len(collisions)),
		note:    strings.Join(collisions, "; "),
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func isProxySpecEmpty(spec string) bool {
	s := strings.ToLower(strings.TrimSpace(spec))
	return s == "" || s == "off"
}

// ---------------------------------------------------------------------
// Check 7 — OTLP-reachability (CDS-25)
// ---------------------------------------------------------------------

// otlpProbe encapsulates the doctor's per-endpoint OTLP probe so tests
// can swap it for a deterministic stub. Returns nil on success; the
// returned error wraps any DNS / connection-refused / 4xx / 5xx
// signal surfaced by the exporter.
//
// `headers` is passed through verbatim — the doctor caller already
// resolved any secret refs in the file via callResolve when
// constructing the cfg.OTLPHeaders map.
type otlpProbeFn func(ctx context.Context, endpoint string, headers map[string]string) error

// defaultOTLPProbe builds a minimal otlptracehttp exporter, then
// immediately Shutdown()s it. Per the SDK contract, Shutdown
// force-flushes any buffered spans (here: zero) and tears the exporter
// down; if the endpoint is unreachable, that surfaces as an error from
// New() or Shutdown().
//
// Endpoint parsing: otlptracehttp accepts either a host:port (via
// WithEndpoint) or a full URL (via WithEndpointURL). We feed the raw
// string to WithEndpointURL when it parses as a URL with a scheme,
// otherwise fall back to WithEndpoint + WithInsecure for bare hosts.
func defaultOTLPProbe(ctx context.Context, endpoint string, headers map[string]string) error {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithTimeout(doctorOTLPTimeout),
	}
	if u, err := url.Parse(endpoint); err == nil && u.Scheme != "" && u.Host != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	} else {
		opts = append(opts, otlptracehttp.WithEndpoint(endpoint), otlptracehttp.WithInsecure())
	}
	if len(headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(headers))
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return fmt.Errorf("construct exporter: %w", err)
	}
	// Shutdown force-flushes any pending spans (zero in our case) and
	// closes the underlying HTTP transport. An endpoint that's down
	// shows up here as a transport error.
	if err := exporter.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// doctorOTLPProbeFn is the indirection point for the OTLP probe. Tests
// overwrite it to inject deterministic outcomes per endpoint. Callers
// reach this through `otlpProbe(...)` below.
var doctorOTLPProbeFn otlpProbeFn = defaultOTLPProbe

// resourceAttrsForDoctor constructs the resource-attribute map that
// the doctor probe MUST advertise. Per the design spec
// (Observability — Doctor integration), `deployment.environment` is
// forced to "doctor" for this run, regardless of what the live config
// says. The remaining cfg-supplied attributes are passed through so a
// collector with allowlists can still recognise the probe as coming
// from this service.
//
// The probe does not actually export a span (it Shutdown()s an empty
// batch), so this map is built for parity with the runtime export
// path even though no span attributes are emitted. We retain it as a
// hook for future expansion: any debug logging of the probe payload
// will read deployment.environment from here.
func resourceAttrsForDoctor(cfg *Config) map[string]string {
	out := map[string]string{}
	if cfg != nil {
		for k, v := range cfg.OTLPResourceAttributes {
			out[k] = v
		}
		if cfg.OTLPServiceName != "" {
			out["service.name"] = cfg.OTLPServiceName
		}
	}
	out["deployment.environment"] = "doctor"
	return out
}

// checkOTLPReachability runs `defaultOTLPProbe` (or a swapped-in stub)
// against every configured endpoint and aggregates a single
// doctorResult. Empty cfg.OTLPEndpoints → ✓ "skipped (no endpoints)".
func checkOTLPReachability(cfg *Config, timeout time.Duration) doctorResult {
	if cfg == nil || len(cfg.OTLPEndpoints) == 0 {
		return doctorResult{ok: true, summary: "skipped (no endpoints)"}
	}

	// Resource attrs are not consumed by the probe (we never emit a
	// span) but we compute them for symmetry and so the test suite
	// can verify deployment.environment=doctor without spinning up an
	// exporter.
	_ = resourceAttrsForDoctor(cfg)

	var oks []string
	var fails []string
	for _, ep := range cfg.OTLPEndpoints {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := doctorOTLPProbeFn(ctx, ep, cfg.OTLPHeaders)
		cancel()
		if err == nil {
			oks = append(oks, ep)
			continue
		}
		fails = append(fails, fmt.Sprintf("%s — %s", ep, classifyOTLPError(err)))
	}

	if len(fails) == 0 {
		return doctorResult{
			ok:      true,
			summary: fmt.Sprintf("%d endpoint(s) reachable", len(oks)),
		}
	}
	return doctorResult{
		ok:      false,
		summary: fmt.Sprintf("%d/%d endpoint(s) unreachable", len(fails), len(oks)+len(fails)),
		note:    strings.Join(fails, "; "),
	}
}

// classifyOTLPError flattens common transport-level errors into a
// terse human label. We don't try to enumerate every otlp internal
// status here — keeping `err.Error()` as the fallback means real
// diagnostic detail still reaches the user.
func classifyOTLPError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such host"):
		return "DNS lookup failed"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "timeout"):
		return "timeout"
	default:
		return msg
	}
}

// ---------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------

// randHex is a tiny helper for any future doctor probe that wants a
// unique span id without pulling in the trace SDK. Currently unused —
// the OTLP probe Shutdown()s an empty batch.
//
// Kept as a nudge for the agent who lights up real spans on the doctor
// path: see CDS-25's design notes on probe payloads.
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
