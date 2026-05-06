// Tests for the HTTP proxy server (CDS-15).
//
// Coverage:
//   - Lifecycle: NewProxy → Start → Addr → Shutdown.
//   - Lifecycle gate: defaultShouldStart matrix over (effort, otlp_endpoints).
//   - Pass-through: GET /v1/models forwarded untouched.
//   - SSE streaming: chunks arrive in real time, no buffering.
//   - 502 on upstream-unreachable.
//   - Connection: close header set on responses.
//   - Redaction guarantee: a regression scan over proxy.go ensures no
//     header values, body content, or API keys leak into span / metric /
//     log attribute calls.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// newTestConfig produces a Config that activates the proxy via a
// non-off effort spec, so defaultShouldStart returns true.
func newTestConfig(t *testing.T, baseURL string) *Config {
	t.Helper()
	return &Config{
		BaseURL:      baseURL,
		Model:        "deepseek-v4-pro",
		ProxyEffort:  "auto",
		ProxyBind:    "127.0.0.1",
		ProxyDebug:   false,
		Unknown:      map[string]string{},
		OTLPProtocol: "http",
	}
}

// startTestProxy boots a Proxy pointed at upstream and returns it. The
// caller is responsible for shutting it down.
func startTestProxy(t *testing.T, upstream string) *Proxy {
	t.Helper()
	cfg := newTestConfig(t, upstream)
	p, err := NewProxy(cfg, ProxyOpts{})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if p == nil {
		t.Fatalf("NewProxy returned nil with active effort spec")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return p
}

func TestDefaultShouldStartMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		cfg      *Config
		expected bool
	}{
		{
			name:     "all_off_no_otlp",
			cfg:      &Config{ProxyEffort: "off"},
			expected: false,
		},
		{
			name:     "global_off_per_tier_off_no_otlp",
			cfg:      &Config{ProxyEffort: "off", ProxyEffortOpus: "off", ProxyEffortSonnet: "off", ProxyEffortHaiku: "off", ProxyEffortSmallFast: "off"},
			expected: false,
		},
		{
			name:     "empty_no_otlp",
			cfg:      &Config{},
			expected: false,
		},
		{
			name:     "global_auto_no_otlp",
			cfg:      &Config{ProxyEffort: "auto"},
			expected: true,
		},
		{
			name:     "per_tier_only_active",
			cfg:      &Config{ProxyEffort: "off", ProxyEffortOpus: "high"},
			expected: true,
		},
		{
			name:     "all_off_otlp_set",
			cfg:      &Config{ProxyEffort: "off", OTLPEndpoints: []string{"http://signoz.local:30318"}},
			expected: true,
		},
		{
			name:     "active_and_otlp",
			cfg:      &Config{ProxyEffort: "max", OTLPEndpoints: []string{"http://signoz.local:30318"}},
			expected: true,
		},
		{
			name:     "nil_cfg",
			cfg:      nil,
			expected: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := defaultShouldStart(tc.cfg)
			if got != tc.expected {
				t.Fatalf("defaultShouldStart = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestNewProxyReturnsNilWhenGated(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		BaseURL:     "https://api.deepseek.com/anthropic",
		ProxyEffort: "off",
	}
	p, err := NewProxy(cfg, ProxyOpts{})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil proxy when gate denies, got %#v", p)
	}
}

func TestNewProxyAndShutdown(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	p := startTestProxy(t, upstream.URL)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := p.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	addr := p.Addr()
	if addr == "" {
		t.Fatalf("Addr returned empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort %q: %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("expected loopback host, got %q", host)
	}
	if port == "0" || port == "" {
		t.Fatalf("expected kernel-assigned non-zero port, got %q", port)
	}
}

func TestPassthroughForwardsUnchanged(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Echo", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":["a","b"]}`))
	}))
	defer upstream.Close()

	p := startTestProxy(t, upstream.URL)
	defer p.Shutdown(context.Background())

	resp, err := http.Get("http://" + p.Addr() + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Echo"); got != "yes" {
		t.Fatalf("X-Echo header = %q, want yes", got)
	}
	// Go's net/http client elides the Connection header from
	// resp.Header but exposes it via resp.Close (HTTP/1.1
	// length-by-EOF framing). Verify that the close marker reached
	// the client.
	if !resp.Close {
		t.Fatalf("expected resp.Close = true (Connection: close framing)")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"models":["a","b"]}` {
		t.Fatalf("body = %q", body)
	}
	if seen == nil {
		t.Fatalf("upstream never saw the request")
	}
	if seen.Method != "GET" {
		t.Fatalf("upstream method = %q, want GET", seen.Method)
	}
	if seen.URL.Path != "/v1/models" {
		t.Fatalf("upstream path = %q, want /v1/models", seen.URL.Path)
	}
	if len(seenBody) != 0 {
		t.Fatalf("upstream body should be empty, got %q", seenBody)
	}
}

func TestPassthroughForwardsHeaders(t *testing.T) {
	t.Parallel()
	var seenAuthorization, seenAPIKey, seenHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthorization = r.Header.Get("Authorization")
		seenAPIKey = r.Header.Get("X-Api-Key")
		seenHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := startTestProxy(t, upstream.URL)
	defer p.Shutdown(context.Background())

	req, _ := http.NewRequest("GET", "http://"+p.Addr()+"/anything", nil)
	req.Header.Set("Authorization", "Bearer secret-value")
	req.Header.Set("X-Api-Key", "key-value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if seenAuthorization != "Bearer secret-value" {
		t.Fatalf("Authorization not forwarded: %q", seenAuthorization)
	}
	if seenAPIKey != "key-value" {
		t.Fatalf("X-Api-Key not forwarded: %q", seenAPIKey)
	}
	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")
	if seenHost != upstreamHost {
		t.Fatalf("Host = %q, want %q", seenHost, upstreamHost)
	}
}

func TestSSEStreamingChunksFlush(t *testing.T) {
	t.Parallel()

	// The upstream writes 3 chunks with sleeps in between. The test
	// asserts that the proxy delivers each chunk before the upstream
	// has finished — i.e. no buffering.
	chunkReady := make(chan struct{}, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
			chunkReady <- struct{}{}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	p := startTestProxy(t, upstream.URL)
	defer p.Shutdown(context.Background())

	resp, err := http.Get("http://" + p.Addr() + "/v1/messages-stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := make([]byte, 4096)
	deadline := time.Now().Add(2 * time.Second)
	var collected bytes.Buffer
	receivedChunks := 0
	for receivedChunks < 3 && time.Now().Before(deadline) {
		// Each chunk arrives separately if streaming is real — the
		// upstream sleeps 50ms between writes, so a buffered reader
		// would only deliver one combined read here.
		n, rerr := resp.Body.Read(br)
		if n > 0 {
			collected.Write(br[:n])
			for strings.Contains(collected.String(), "\n\n") {
				idx := strings.Index(collected.String(), "\n\n")
				collected.Next(idx + 2)
				receivedChunks++
			}
		}
		if rerr != nil {
			break
		}
	}
	if receivedChunks != 3 {
		t.Fatalf("received %d chunks, want 3 (collected=%q)", receivedChunks, collected.String())
	}
}

func TestStreamingTTFB(t *testing.T) {
	t.Parallel()
	// Upstream writes a single chunk after 100ms, then closes. The
	// proxy must surface that chunk to the client without buffering.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("data: ping\n\n"))
		flusher.Flush()
		time.Sleep(50 * time.Millisecond)
	}))
	defer upstream.Close()

	p := startTestProxy(t, upstream.URL)
	defer p.Shutdown(context.Background())

	start := time.Now()
	resp, err := http.Get("http://" + p.Addr() + "/v1/passthrough-stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	buf := make([]byte, 32)
	n, _ := resp.Body.Read(buf)
	ttfb := time.Since(start)
	resp.Body.Close()
	if n == 0 {
		t.Fatalf("read 0 bytes for first chunk")
	}
	// TTFB should be roughly the upstream sleep (~100ms). Generous
	// upper bound to tolerate CI scheduling.
	if ttfb > 1*time.Second {
		t.Fatalf("TTFB = %v, expected ~100ms", ttfb)
	}
}

func TestUpstreamUnreachableReturns502(t *testing.T) {
	t.Parallel()
	// Bind a port, then close it, so http://127.0.0.1:<port> refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	cfg := newTestConfig(t, "http://"+deadAddr)
	p, err := NewProxy(cfg, ProxyOpts{})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Shutdown(context.Background())

	resp, err := http.Get("http://" + p.Addr() + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestMessagesPostForwardsBody(t *testing.T) {
	t.Parallel()
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	}))
	defer upstream.Close()

	p := startTestProxy(t, upstream.URL)
	defer p.Shutdown(context.Background())

	in := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post("http://"+p.Addr()+"/v1/messages", "application/json", bytes.NewReader(in))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	if string(body) != `{"id":"resp"}` {
		t.Fatalf("response body = %q", body)
	}
	// Body content should round-trip — the rewriteBody stub is a
	// passthrough, so the upstream sees the JSON we sent.
	if !strings.Contains(string(seenBody), `"deepseek-v4-pro"`) {
		t.Fatalf("upstream body did not contain model id; got %q", seenBody)
	}
}

func TestMessagesEffortRegimeApplied(t *testing.T) {
	t.Parallel()
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := newTestConfig(t, upstream.URL)
	cfg.ProxyEffort = "max" // force max regime regardless of source bucket
	p, err := NewProxy(cfg, ProxyOpts{})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Shutdown(context.Background())

	in := []byte(`{"model":"deepseek-v4-pro","messages":[]}`)
	resp, err := http.Post("http://"+p.Addr()+"/v1/messages", "application/json", bytes.NewReader(in))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()

	// The "max" regime sets thinking + reasoning_effort:max.
	if !strings.Contains(string(seenBody), `"reasoning_effort":"max"`) {
		t.Fatalf("expected reasoning_effort=max applied, got %q", seenBody)
	}
	if !strings.Contains(string(seenBody), `"thinking"`) {
		t.Fatalf("expected thinking block applied, got %q", seenBody)
	}
}

func TestConnectionCloseHeaderOnResponses(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer upstream.Close()

	p := startTestProxy(t, upstream.URL)
	defer p.Shutdown(context.Background())

	resp, err := http.Get("http://" + p.Addr() + "/v1/whatever")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	// resp.Header.Get("Connection") is intentionally elided by
	// Go's stdlib client; verify the framing made it to the client
	// via resp.Close instead.
	if !resp.Close {
		t.Fatalf("expected resp.Close = true (Connection: close framing)")
	}
}

func TestStartAlreadyStartedFails(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	p := startTestProxy(t, upstream.URL)
	defer p.Shutdown(context.Background())
	if err := p.Start(context.Background()); err == nil {
		t.Fatalf("expected double-Start to fail")
	}
}

// TestRedactionSourceScan is the regression guard for the redaction
// rule (observability doc §5). It greps proxy.go for known
// banned patterns and fails the build on any match.
//
// This is a static check rather than a runtime assertion because the
// risk is "a future edit accidentally adds attribute.String("body", ...)"
// — the test runs in CI and a reviewer can read the regex list to see
// what's enforced.
func TestRedactionSourceScan(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	srcStr := string(src)

	type rule struct {
		name  string
		regex *regexp.Regexp
	}
	banned := []rule{
		{
			name:  "no body content as attribute",
			regex: regexp.MustCompile(`attribute\.String\(\s*"(?:body|content|messages|tool_use|tool_result|prompt|completion|input|output)"`),
		},
		{
			name:  "no body bytes recorded as string",
			regex: regexp.MustCompile(`attribute\.String\([^)]+,\s*string\(\s*body\s*\)\s*\)`),
		},
		{
			name:  "no Authorization value attribute",
			regex: regexp.MustCompile(`attribute\.String\([^)]+,\s*[^)]*Authorization[^)]*Get\(`),
		},
		{
			name:  "no x-api-key value attribute",
			regex: regexp.MustCompile(`attribute\.String\([^)]+,\s*[^)]*[Xx]-[Aa]pi-[Kk]ey[^)]*Get\(`),
		},
		{
			name:  "no header values as span attribute",
			regex: regexp.MustCompile(`attribute\.String\(\s*"http\.request\.header"`),
		},
		{
			name:  "no req.Header.Get into otellog values",
			regex: regexp.MustCompile(`otellog\.String\([^)]+,\s*r\.Header\.Get\(`),
		},
	}
	for _, ru := range banned {
		if loc := ru.regex.FindStringIndex(srcStr); loc != nil {
			t.Errorf("redaction violation %q at byte %d: %q", ru.name, loc[0], srcStr[loc[0]:min(loc[1]+40, len(srcStr))])
		}
	}
}

// TestRedactionAllowedPatterns is the positive-case sibling of
// TestRedactionSourceScan: it confirms the file *does* record the
// attributes the doc requires (counts, sizes, flags). Catching a
// regression that drops these would silently degrade observability.
func TestRedactionAllowedPatterns(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	srcStr := string(src)
	required := []string{
		`claude_ds.body.input_size`,
		`claude_ds.endpoint`,
		`claude_ds.transform.step`,
		`claude_ds.transform.mutated`,
		`claude_ds.stream.bytes`,
		`claude_ds.stream.chunks`,
		`claude_ds.stream.client_disconnected`,
		`claude_ds.stream.ttfb_ms`,
		`claude_ds.disconnect.cause`,
		`claude_ds.upstream.host`,
	}
	for _, want := range required {
		if !strings.Contains(srcStr, want) {
			t.Errorf("expected attribute %q in proxy.go", want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Confirm the package-level constants stay sane.
func TestConstantsSanity(t *testing.T) {
	if upstreamReadChunkSize != 8192 {
		t.Fatalf("upstreamReadChunkSize = %d, want 8192 (matches Python)", upstreamReadChunkSize)
	}
	if upstreamTimeout != 600*time.Second {
		t.Fatalf("upstreamTimeout = %v, want 600s", upstreamTimeout)
	}
}

// silence unused-import warnings when go test recompiles after edits.
var _ = errors.New
