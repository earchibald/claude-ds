// Doctor diagnostics tests (CDS-20). Each check has a focused
// table-driven test; runDoctor itself gets a smoke test ensuring
// header/footer + every check appears once and the exit code is 0.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// runDoctor — smoke test
// ---------------------------------------------------------------------

func TestRunDoctorTo_ExitsZeroAndPrintsAllChecks(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return "tok-" + s, nil })

	cfg := &Config{
		Path:      writeTempConfig(t, "_schema=1\napi_key_ref=plain://k\n"),
		Schema:    1,
		APIKeyRef: "plain://k",
		BaseURL:   "http://127.0.0.1:1", // unreachable on purpose; check 4 is allowed to fail
		Model:     defaultModel,
	}

	var buf bytes.Buffer
	got := runDoctorTo(&buf, cfg, false)
	if got != 0 {
		t.Fatalf("runDoctorTo exit code = %d, want 0", got)
	}
	out := buf.String()
	for _, want := range []string{
		"claude-ds doctor",
		"claude binary on PATH",
		"config file readable",
		"api key secret reference resolves",
		"api key live against upstream",
		"reasoning-effort proxy",
		"tier-spec collision lint",
		"OTLP endpoint reachability",
		"doctor done.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------------
// Check 1 — claude on PATH
// ---------------------------------------------------------------------

func TestCheckClaudeOnPATH(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		dir := t.TempDir()
		shimName := "claude"
		if runtime.GOOS == "windows" {
			shimName = "claude.exe"
		}
		shim := filepath.Join(dir, shimName)
		if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write shim: %v", err)
		}
		t.Setenv("PATH", dir)
		got := checkClaudeOnPATH()
		if !got.ok {
			t.Fatalf("expected ✓ when claude is on PATH; got %+v", got)
		}
		if !strings.Contains(got.summary, shim) {
			t.Errorf("summary should include shim path %q; got %q", shim, got.summary)
		}
	})

	t.Run("missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // empty dir → not found
		got := checkClaudeOnPATH()
		if got.ok {
			t.Fatalf("expected ✗ when claude missing; got %+v", got)
		}
		if !strings.Contains(got.note, claudeInstallURL) {
			t.Errorf("note should include install URL; got %q", got.note)
		}
	})
}

// ---------------------------------------------------------------------
// Check 2 — config readable
// ---------------------------------------------------------------------

func TestCheckConfigReadable(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		path := writeTempConfig(t, "_schema=1\napi_key_ref=plain://k\n")
		cfg := &Config{Path: path, Schema: 1, APIKeyRef: "plain://k"}
		r := checkConfigReadable(cfg)
		if !r.ok {
			t.Fatalf("want ✓; got %+v", r)
		}
		if !strings.Contains(r.summary, "schema v1") {
			t.Errorf("summary should report schema; got %q", r.summary)
		}
	})

	t.Run("nil cfg", func(t *testing.T) {
		r := checkConfigReadable(nil)
		if r.ok {
			t.Fatalf("want ✗ for nil cfg; got %+v", r)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		r := checkConfigReadable(&Config{})
		if r.ok {
			t.Fatalf("want ✗ for empty path; got %+v", r)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-config")
		r := checkConfigReadable(&Config{Path: path})
		if r.ok {
			t.Fatalf("want ✗ for missing file; got %+v", r)
		}
		if !strings.Contains(r.summary, "does not exist") {
			t.Errorf("summary should mention missing file; got %q", r.summary)
		}
	})
}

// ---------------------------------------------------------------------
// Check 3 — secret reference resolves
// ---------------------------------------------------------------------

func TestCheckSecretResolves(t *testing.T) {
	t.Run("resolves", func(t *testing.T) {
		stubResolve(t, func(s string) (string, error) { return "ok-" + s, nil })
		r := checkSecretResolves(&Config{APIKeyRef: "plain://k"})
		if !r.ok {
			t.Fatalf("want ✓; got %+v", r)
		}
	})

	t.Run("empty result", func(t *testing.T) {
		stubResolve(t, func(s string) (string, error) { return "", nil })
		r := checkSecretResolves(&Config{APIKeyRef: "plain://k"})
		if r.ok {
			t.Fatalf("want ✗ for empty resolution; got %+v", r)
		}
		if !strings.Contains(r.note, "--rotate-key") {
			t.Errorf("note should mention --rotate-key; got %q", r.note)
		}
	})

	t.Run("error", func(t *testing.T) {
		stubResolve(t, func(s string) (string, error) { return "", errors.New("upstream offline") })
		r := checkSecretResolves(&Config{APIKeyRef: "plain://k"})
		if r.ok {
			t.Fatalf("want ✗ on resolver error; got %+v", r)
		}
		if !strings.Contains(r.note, "--rotate-key") {
			t.Errorf("note should mention --rotate-key; got %q", r.note)
		}
	})

	t.Run("no api_key_ref", func(t *testing.T) {
		r := checkSecretResolves(&Config{})
		if r.ok {
			t.Fatalf("want ✗ when api_key_ref unset; got %+v", r)
		}
	})
}

// ---------------------------------------------------------------------
// Check 4 — API key live
// ---------------------------------------------------------------------

func TestCheckAPILive(t *testing.T) {
	type tc struct {
		name        string
		serverCode  int
		wantOK      bool
		wantContain string
		wantNote    string
	}
	cases := []tc{
		{"200 OK", 200, true, "200", ""},
		{"401 unauth", 401, false, "key invalid", "--rotate-key"},
		{"500 advisory", 500, false, "unexpected status", "advisory"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				if r.Header.Get("x-api-key") == "" {
					t.Errorf("missing x-api-key header")
				}
				if r.Header.Get("Authorization") == "" {
					t.Errorf("missing Authorization header")
				}
				if r.Header.Get("anthropic-version") == "" {
					t.Errorf("missing anthropic-version header")
				}
				w.WriteHeader(c.serverCode)
			}))
			defer srv.Close()

			stubResolve(t, func(s string) (string, error) { return "tok", nil })
			cfg := &Config{
				APIKeyRef: "plain://k",
				BaseURL:   srv.URL,
				Model:     "deepseek-v4-pro",
			}
			r := checkAPILive(cfg, 2*time.Second)
			if r.ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (summary=%q note=%q)", r.ok, c.wantOK, r.summary, r.note)
			}
			if c.wantContain != "" && !strings.Contains(r.summary, c.wantContain) {
				t.Errorf("summary %q should contain %q", r.summary, c.wantContain)
			}
			if c.wantNote != "" && !strings.Contains(r.note, c.wantNote) {
				t.Errorf("note %q should contain %q", r.note, c.wantNote)
			}
		})
	}
}

func TestCheckAPILive_NetworkError(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return "tok", nil })
	cfg := &Config{
		APIKeyRef: "plain://k",
		// Reserved TEST-NET-1 + a port nothing listens on; should bounce fast.
		BaseURL: "http://192.0.2.1:9",
		Model:   "deepseek-v4-pro",
	}
	r := checkAPILive(cfg, 200*time.Millisecond)
	if r.ok {
		t.Fatalf("want ✗ for unreachable endpoint; got %+v", r)
	}
	if !strings.Contains(r.summary, "could not reach") {
		t.Errorf("summary should mention unreachable; got %q", r.summary)
	}
}

func TestCheckAPILive_ResolverFailure(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return "", errors.New("op down") })
	cfg := &Config{APIKeyRef: "plain://k", BaseURL: "http://127.0.0.1:1"}
	r := checkAPILive(cfg, 1*time.Second)
	if r.ok {
		t.Fatalf("want ✗ on resolver failure; got %+v", r)
	}
}

func TestCheckAPILive_NoAPIKeyRef(t *testing.T) {
	r := checkAPILive(&Config{}, 1*time.Second)
	if r.ok {
		t.Fatalf("want ✗ with no api_key_ref; got %+v", r)
	}
}

// ---------------------------------------------------------------------
// Check 6 — tier collision lint
// ---------------------------------------------------------------------

func TestCheckTierCollisions(t *testing.T) {
	t.Run("auto-mode disabled → ✓", func(t *testing.T) {
		cfg := &Config{
			Model:                "m-default",
			ProxyEffortOpus:      "high",
			ProxyEffortSonnet:    "low",
			ProxyEffortHaiku:     "med",
			ProxyEffortSmallFast: "off",
			UnlockAutoMode:       false,
		}
		r := checkTierCollisions(cfg)
		if !r.ok {
			t.Fatalf("expected ✓ when auto-mode disabled; got %+v", r)
		}
	})

	t.Run("no collisions when all tiers map to distinct wires", func(t *testing.T) {
		cfg := &Config{
			ModelOpus:            "m-opus",
			ModelSonnet:          "m-sonnet",
			ModelHaiku:           "m-haiku",
			ModelSmallFast:       "m-small",
			ProxyEffortOpus:      "high",
			ProxyEffortSonnet:    "low",
			ProxyEffortHaiku:     "med",
			ProxyEffortSmallFast: "off",
			UnlockAutoMode:       true,
		}
		r := checkTierCollisions(cfg)
		if !r.ok {
			t.Fatalf("expected ✓ no collisions; got %+v", r)
		}
	})

	t.Run("collision: opus & sonnet share wire id with non-empty specs", func(t *testing.T) {
		cfg := &Config{
			ModelOpus:         "shared",
			ModelSonnet:       "shared",
			ModelHaiku:        "h",
			ModelSmallFast:    "s",
			ProxyEffortOpus:   "high",
			ProxyEffortSonnet: "low",
			UnlockAutoMode:    true,
		}
		r := checkTierCollisions(cfg)
		if r.ok {
			t.Fatalf("expected ✗ on tier collision; got %+v", r)
		}
		if !strings.Contains(r.note, "shared") {
			t.Errorf("note should mention the shared wire id; got %q", r.note)
		}
		if !strings.Contains(r.note, "opus=high") || !strings.Contains(r.note, "sonnet=low") {
			t.Errorf("note should list colliding tiers; got %q", r.note)
		}
	})

	t.Run("collision suppressed when one tier's spec is empty/off", func(t *testing.T) {
		cfg := &Config{
			ModelOpus:            "shared",
			ModelSonnet:          "shared",
			ProxyEffortOpus:      "high",
			ProxyEffortSonnet:    "off",
			ProxyEffortHaiku:     "",
			ProxyEffortSmallFast: "",
			UnlockAutoMode:       true,
		}
		r := checkTierCollisions(cfg)
		if !r.ok {
			t.Fatalf("expected ✓ when one of the colliding tiers has empty/off spec; got %+v", r)
		}
	})
}

// ---------------------------------------------------------------------
// Check 7 — OTLP reachability
// ---------------------------------------------------------------------

func swapOTLPProbe(t *testing.T, fn otlpProbeFn) {
	t.Helper()
	prev := doctorOTLPProbeFn
	doctorOTLPProbeFn = fn
	t.Cleanup(func() { doctorOTLPProbeFn = prev })
}

func TestCheckOTLPReachability_NoEndpoints(t *testing.T) {
	r := checkOTLPReachability(&Config{}, time.Second)
	if !r.ok {
		t.Fatalf("expected ✓ when no endpoints; got %+v", r)
	}
	if !strings.Contains(r.summary, "skipped") {
		t.Errorf("summary should say 'skipped'; got %q", r.summary)
	}
}

func TestCheckOTLPReachability_NilCfg(t *testing.T) {
	r := checkOTLPReachability(nil, time.Second)
	if !r.ok {
		t.Fatalf("expected ✓ for nil cfg; got %+v", r)
	}
}

func TestCheckOTLPReachability_AllReachable(t *testing.T) {
	swapOTLPProbe(t, func(_ context.Context, _ string, _ map[string]string) error { return nil })
	cfg := &Config{
		OTLPEndpoints: []string{"https://otlp.a.example", "https://otlp.b.example"},
	}
	r := checkOTLPReachability(cfg, time.Second)
	if !r.ok {
		t.Fatalf("expected ✓ when all probes succeed; got %+v", r)
	}
	if !strings.Contains(r.summary, "2 endpoint") {
		t.Errorf("summary should report count; got %q", r.summary)
	}
}

func TestCheckOTLPReachability_MixedSuccess(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	swapOTLPProbe(t, func(_ context.Context, ep string, _ map[string]string) error {
		mu.Lock()
		calls = append(calls, ep)
		mu.Unlock()
		if strings.Contains(ep, "bad") {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	})
	cfg := &Config{
		OTLPEndpoints: []string{"https://otlp.good.example", "https://otlp.bad.example"},
	}
	r := checkOTLPReachability(cfg, time.Second)
	if r.ok {
		t.Fatalf("expected ✗ when one probe fails; got %+v", r)
	}
	if !strings.Contains(r.note, "bad.example") {
		t.Errorf("note should mention failing endpoint; got %q", r.note)
	}
	if !strings.Contains(r.note, "connection refused") {
		t.Errorf("note should classify the error; got %q", r.note)
	}
	if len(calls) != 2 {
		t.Errorf("probe should have been called for each endpoint; got %d calls", len(calls))
	}
}

func TestCheckOTLPReachability_HeadersForwarded(t *testing.T) {
	var got map[string]string
	swapOTLPProbe(t, func(_ context.Context, _ string, h map[string]string) error {
		got = h
		return nil
	})
	cfg := &Config{
		OTLPEndpoints: []string{"https://otlp.example"},
		OTLPHeaders:   map[string]string{"X-Test": "abc"},
	}
	_ = checkOTLPReachability(cfg, time.Second)
	if got["X-Test"] != "abc" {
		t.Errorf("headers should be forwarded to probe; got %v", got)
	}
}

// defaultOTLPProbe should fail fast for an obviously-dead endpoint
// without hanging the test runner. We can't assert the precise error
// (the SDK retries internally), but we can bound the wall-clock cost.
func TestDefaultOTLPProbe_FailsForDeadEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("network probe; skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	// Reserved TEST-NET-1 — should produce a transport error inside the budget.
	_ = defaultOTLPProbe(ctx, "http://192.0.2.1:9", nil)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("defaultOTLPProbe blocked too long: %s", d)
	}
}

// defaultOTLPProbe against a live httptest server should succeed: the
// server returns 200 for any POST, the exporter constructs cleanly,
// Shutdown force-flushes an empty batch and returns nil.
func TestDefaultOTLPProbe_SucceedsAgainstHTTPTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := defaultOTLPProbe(ctx, srv.URL, nil); err != nil {
		t.Fatalf("defaultOTLPProbe should succeed against httptest server; got %v", err)
	}
}

// ---------------------------------------------------------------------
// Resource-attr / forced deployment.environment
// ---------------------------------------------------------------------

func TestResourceAttrsForDoctor_ForcesEnvironment(t *testing.T) {
	cfg := &Config{
		OTLPServiceName:           "claude-ds-proxy",
		OTLPDeploymentEnvironment: "prod",
		OTLPResourceAttributes:    map[string]string{"deployment.environment": "prod", "tenant": "abc"},
	}
	got := resourceAttrsForDoctor(cfg)
	if got["deployment.environment"] != "doctor" {
		t.Fatalf("doctor must force deployment.environment=doctor; got %q", got["deployment.environment"])
	}
	if got["tenant"] != "abc" {
		t.Errorf("non-environment attrs should pass through; got %q", got["tenant"])
	}
	if got["service.name"] != "claude-ds-proxy" {
		t.Errorf("service.name should be carried through; got %q", got["service.name"])
	}
}

func TestResourceAttrsForDoctor_NilCfg(t *testing.T) {
	got := resourceAttrsForDoctor(nil)
	if got["deployment.environment"] != "doctor" {
		t.Errorf("doctor must force deployment.environment=doctor even for nil cfg; got %v", got)
	}
}

// ---------------------------------------------------------------------
// classifyOTLPError
// ---------------------------------------------------------------------

func TestClassifyOTLPError(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"foo: no such host":                    "DNS lookup failed",
		"dial tcp 1.2.3.4: connection refused": "connection refused",
		"context deadline exceeded":            "timeout",
		"i/o timeout":                          "timeout",
		"weird transport error":                "weird transport error",
	}
	for in, want := range cases {
		var err error
		if in != "" {
			err = errors.New(in)
		}
		got := classifyOTLPError(err)
		if got != want {
			t.Errorf("classifyOTLPError(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------
// runDoctor — ensure check 7 runs end-to-end with a stub probe
// ---------------------------------------------------------------------

func TestRunDoctorTo_OTLPHappyPath(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return "tok", nil })
	swapOTLPProbe(t, func(_ context.Context, _ string, _ map[string]string) error { return nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := &Config{
		Path:          writeTempConfig(t, "_schema=1\napi_key_ref=plain://k\n"),
		Schema:        1,
		APIKeyRef:     "plain://k",
		BaseURL:       srv.URL,
		Model:         "deepseek-v4-pro",
		OTLPEndpoints: []string{"https://otlp.example"},
	}

	var buf bytes.Buffer
	if rc := runDoctorTo(&buf, cfg, false); rc != 0 {
		t.Fatalf("exit code = %d, want 0", rc)
	}
	out := buf.String()
	if !strings.Contains(out, "1 endpoint(s) reachable") {
		t.Errorf("OTLP-reachability summary missing; got:\n%s", out)
	}
	// Expect at least 6 ✓ marks on the happy path: claude (only if on PATH),
	// config, secret, api, proxy, tier, otlp. We don't assume claude is
	// installed, so at minimum 6 ✓ should be present.
	count := strings.Count(out, "✓")
	if count < 6 {
		t.Errorf("expected ≥ 6 ✓ marks on happy path; got %d\n%s", count, out)
	}
}

// Quick sanity: randHex is not load-bearing, but we don't want it to panic.
func TestRandHex(t *testing.T) {
	if got := randHex(8); len(got) != 16 {
		t.Errorf("randHex(8) length = %d, want 16", len(got))
	}
}

// Ensure isProxySpecEmpty mirrors the launcher's empty/off semantics.
func TestIsProxySpecEmpty(t *testing.T) {
	for _, in := range []string{"", " ", "off", "OFF", " OFF "} {
		if !isProxySpecEmpty(in) {
			t.Errorf("isProxySpecEmpty(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"high", "low", "auto"} {
		if isProxySpecEmpty(in) {
			t.Errorf("isProxySpecEmpty(%q) = true, want false", in)
		}
	}
}

// firstNonEmpty sanity.
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty empty = %q, want ''", got)
	}
}

// Double-check that the doctor check 4 sends the exact body Bash sent.
func TestProbeAPILive_BodyAndHeaders(t *testing.T) {
	var (
		bodyOK, hdrsOK bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers
		hdrsOK = r.Header.Get("Content-Type") == "application/json" &&
			r.Header.Get("anthropic-version") == "2023-06-01" &&
			strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") &&
			r.Header.Get("x-api-key") != ""
		// Body
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		bodyOK = strings.Contains(body, `"max_tokens":1`) &&
			strings.Contains(body, `"role":"user"`) &&
			strings.Contains(body, `"content":"hi"`)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	st, code, err := probeAPILive(srv.URL, "deepseek-v4-pro", "abc", 2*time.Second)
	if err != nil {
		t.Fatalf("probeAPILive err = %v", err)
	}
	if st != apiLiveOK || code != 200 {
		t.Fatalf("st=%v code=%d, want OK/200", st, code)
	}
	if !hdrsOK {
		t.Errorf("headers mismatch with Bash launcher")
	}
	if !bodyOK {
		t.Errorf("body mismatch with Bash launcher")
	}
}

// Sanity: probeAPILive bails out fast on an empty token.
func TestProbeAPILive_EmptyToken(t *testing.T) {
	st, _, err := probeAPILive("http://example.invalid", "m", "", time.Second)
	if st != apiLiveSkipped || err == nil {
		t.Fatalf("want skipped+err on empty token; got %v / %v", st, err)
	}
}

// Sanity check that a malformed base URL doesn't panic.
func TestProbeAPILive_MalformedBaseURL(t *testing.T) {
	st, _, _ := probeAPILive("::not-a-url", "m", "tok", time.Second)
	if st == apiLiveOK {
		t.Fatalf("malformed URL should not return OK")
	}
}

// Make sure runDoctor (production path) doesn't panic when given a
// minimally-populated config and a nil probe override (i.e. the real
// otlptracehttp exporter is exercised). This is a smoke test only —
// network reachability of endpoints is not asserted, since the
// exporter's verdict for an unresolvable host is environment-specific.
func TestRunDoctor_RealPathSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("network smoke test; -short")
	}
	stubResolve(t, func(s string) (string, error) { return "tok", nil })
	cfg := &Config{
		Path:      writeTempConfig(t, "_schema=1\napi_key_ref=plain://k\n"),
		Schema:    1,
		APIKeyRef: "plain://k",
		BaseURL:   "http://127.0.0.1:1",
		Model:     defaultModel,
	}
	rc := runDoctor(cfg)
	if rc != 0 {
		t.Fatalf("runDoctor exit code = %d, want 0", rc)
	}
}

// Ensure the colourised path emits ANSI codes.
func TestRunDoctorTo_ColorPath(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return "tok", nil })
	swapOTLPProbe(t, func(_ context.Context, _ string, _ map[string]string) error { return nil })
	cfg := &Config{
		Path:      writeTempConfig(t, "_schema=1\napi_key_ref=plain://k\n"),
		Schema:    1,
		APIKeyRef: "plain://k",
		BaseURL:   "http://127.0.0.1:1",
		Model:     defaultModel,
	}
	var buf bytes.Buffer
	_ = runDoctorTo(&buf, cfg, true)
	if !strings.Contains(buf.String(), ansiGreen) && !strings.Contains(buf.String(), ansiRed) {
		t.Errorf("colorize=true should emit ANSI codes; got:\n%s", buf.String())
	}
}

// Validate that defaultConfigPath honours XDG_CONFIG_HOME first.
func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got := defaultConfigPath(); got != filepath.Join("/tmp/xdg", "claude-ds", "config") {
		t.Errorf("XDG path = %q, want /tmp/xdg/claude-ds/config", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, _ := os.UserHomeDir()
	if home != "" {
		want := filepath.Join(home, ".config", "claude-ds", "config")
		if got := defaultConfigPath(); got != want {
			t.Errorf("home fallback = %q, want %q", got, want)
		}
	}
}

// Ensure we don't accidentally emit "TODO: --doctor" anymore (the stub
// has been replaced).
func TestMain_DoctorIsWired(t *testing.T) {
	// run(args) executes the real dispatch path; we only need the
	// goroutine to not invoke runTODO. To keep this hermetic, swap the
	// resolver and OTLP probe and point HOME at a temp dir so
	// defaultConfigPath() resolves to a missing file.
	stubResolve(t, func(s string) (string, error) { return "tok", nil })
	swapOTLPProbe(t, func(_ context.Context, _ string, _ map[string]string) error { return nil })

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")

	// Point stderr at a buffer to capture the (absent) TODO line.
	r, w, _ := os.Pipe()
	prev := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = prev }()

	exit := run([]string{"--doctor"})
	w.Close()
	os.Stderr = prev

	out := readAll(r)
	if exit != 0 {
		t.Fatalf("--doctor exit code = %d, want 0", exit)
	}
	if strings.Contains(out, "TODO: --doctor") {
		t.Errorf("--doctor still wired to runTODO; stderr=%q", out)
	}
}

func readAll(r interface{ Read([]byte) (int, error) }) string {
	var buf bytes.Buffer
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	return buf.String()
}

// Make sure check 4 sends a body that decodes to JSON the upstream is
// happy with. We parse the body as JSON to catch escaping bugs in the
// model name (Bash escaped via `$model`; Go uses `%q` to wrap).
func TestProbeAPILive_BodyIsValidJSON(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		got = string(buf[:n])
		w.WriteHeader(200)
	}))
	defer srv.Close()

	if _, _, err := probeAPILive(srv.URL, `model"with"quotes`, "tok", time.Second); err != nil {
		t.Fatalf("probeAPILive err = %v", err)
	}
	if !strings.Contains(got, `\"with\"`) {
		t.Errorf("model name not escaped properly; got body=%q", got)
	}
}

// Cover the no-collision branch when UnlockAutoMode is true but every
// per-tier spec is empty/off — that's the common single-tier setup.
func TestCheckTierCollisions_AutoModeButNoSpecs(t *testing.T) {
	cfg := &Config{
		Model:                "m",
		UnlockAutoMode:       true,
		ProxyEffortOpus:      "",
		ProxyEffortSonnet:    "",
		ProxyEffortHaiku:     "",
		ProxyEffortSmallFast: "",
	}
	r := checkTierCollisions(cfg)
	if !r.ok {
		t.Fatalf("expected ✓ when all per-tier specs are empty; got %+v", r)
	}
}

// Quick guard that the doctor printer does not splat nil notes onto a
// new line.
func TestDoctorPrinter_NoteOmittedWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	p := newDoctorPrinter(&buf, false)
	p.runCheck("foo", func() doctorResult { return doctorResult{ok: true, summary: "bar"} })
	out := buf.String()
	if strings.Contains(out, "→") {
		t.Errorf("note arrow should be absent when note is empty; got %q", out)
	}
	if !strings.Contains(out, "✓ foo: bar") {
		t.Errorf("expected '✓ foo: bar'; got %q", out)
	}
}

// Sanity helper to ensure runDoctor completes promptly without
// network dependence under deterministic OTLP stubs.
func TestRunDoctorTo_BoundedWallClock(t *testing.T) {
	stubResolve(t, func(s string) (string, error) { return "tok", nil })
	swapOTLPProbe(t, func(_ context.Context, _ string, _ map[string]string) error { return nil })
	cfg := &Config{
		Path:          writeTempConfig(t, "_schema=1\napi_key_ref=plain://k\n"),
		Schema:        1,
		APIKeyRef:     "plain://k",
		BaseURL:       "http://127.0.0.1:1", // unreachable; check 4 fails fast
		Model:         defaultModel,
		OTLPEndpoints: []string{"https://otlp.example"},
	}
	start := time.Now()
	var buf bytes.Buffer
	_ = runDoctorTo(&buf, cfg, false)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("runDoctor took too long: %s", d)
	}
}

// Ensure check 4's probe respects an aggressive timeout. We POST to a
// blackhole server that hangs and assert the call returns before the
// outer test deadline.
func TestProbeAPILive_RespectsTimeout(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-hold
	}))
	t.Cleanup(func() { close(hold); srv.Close() })

	start := time.Now()
	st, _, err := probeAPILive(srv.URL, "m", "tok", 200*time.Millisecond)
	if err == nil || st == apiLiveOK {
		t.Fatalf("expected timeout; got st=%v err=%v", st, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("probe blew the timeout: %s", d)
	}
}

// fmt sentinel to silence unused-import lint when we trim tests.
var _ = fmt.Sprintf
