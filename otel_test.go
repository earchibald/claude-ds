// CDS-23 / CDS-25 — OTLP provider lifecycle tests.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

// TestBuildOTLPProviders_NoEndpoints exercises the empty-endpoints
// short-circuit. The shutdown function must be a no-op and exporters
// must NOT be constructed (verified indirectly by completing instantly).
func TestBuildOTLPProviders_NoEndpoints(t *testing.T) {
	cfg := &Config{
		OTLPEndpoints: nil,
		Model:         "deepseek-v4-pro",
	}
	shutdown, err := buildOTLPProviders(cfg, nil)
	if err != nil {
		t.Fatalf("buildOTLPProviders: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil")
	}
	// No-op shutdown should complete instantly under any context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("noop shutdown returned err: %v", err)
	}

	// otel.Tracer must still return a usable tracer (the global default
	// no-op TracerProvider).
	tr := otel.Tracer("test")
	if tr == nil {
		t.Fatal("global Tracer is nil after no-op build")
	}
}

// TestBuildOTLPProviders_OneEndpoint constructs a real exporter against
// an httptest collector and verifies that shutdown completes within the
// 1-second budget.
func TestBuildOTLPProviders_OneEndpoint(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &Config{
		OTLPEndpoints:             []string{srv.URL},
		OTLPServiceName:           "claude-ds-proxy",
		OTLPDeploymentEnvironment: "test",
		Model:                     "deepseek-v4-pro",
		BaseURL:                   "https://api.deepseek.com/anthropic",
		ProxyEffort:               "auto",
		VisionModel:               "deepseek-chat",
	}
	shutdown, err := buildOTLPProviders(cfg, nil)
	if err != nil {
		t.Fatalf("buildOTLPProviders: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil")
	}

	// Emit a span so there's something to drain.
	tr := otel.Tracer("claude-ds-proxy-test")
	_, span := tr.Start(context.Background(), "test-span")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	if err := shutdown(ctx); err != nil {
		t.Logf("shutdown returned (allowed): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v — exceeded 1.5s ceiling", elapsed)
	}
}

// TestBuildOTLPProviders_TwoEndpoints verifies the CSV fan-out path —
// each endpoint should produce its own exporter.
func TestBuildOTLPProviders_TwoEndpoints(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvB.Close()

	cfg := &Config{
		OTLPEndpoints: []string{srvA.URL, srvB.URL},
		Model:         "deepseek-v4-pro",
	}
	shutdown, err := buildOTLPProviders(cfg, nil)
	if err != nil {
		t.Fatalf("buildOTLPProviders: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	if shutdown == nil {
		t.Fatal("shutdown is nil")
	}
}

// TestBuildResource_ContainsRequiredAttrs verifies the resource has the
// load-bearing attributes from acceptance criterion #11.
func TestBuildResource_ContainsRequiredAttrs(t *testing.T) {
	cfg := &Config{
		OTLPServiceName:           "claude-ds-proxy",
		OTLPDeploymentEnvironment: "local",
		Model:                     "deepseek-v4-pro",
		BaseURL:                   "https://api.deepseek.com/anthropic",
		ProxyEffort:               "auto",
		VisionModel:               "deepseek-chat",
	}
	res, err := buildResource(cfg, nil)
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	have := map[string]string{}
	for _, kv := range res.Attributes() {
		have[string(kv.Key)] = kv.Value.AsString()
	}
	wantPresent := []string{
		"service.name",
		"service.version",
		"service.instance.id",
		"deployment.environment",
		"host.name",
		"claude_ds.proxy.effort_default",
		"claude_ds.proxy.vision_model",
		"claude_ds.upstream.host",
		"claude_ds.build.commit",
	}
	for _, k := range wantPresent {
		if _, ok := have[k]; !ok {
			t.Errorf("resource missing attribute %q", k)
		}
	}
	if got := have["service.name"]; got != "claude-ds-proxy" {
		t.Errorf("service.name = %q, want claude-ds-proxy", got)
	}
	if got := have["claude_ds.upstream.host"]; got != "api.deepseek.com" {
		t.Errorf("claude_ds.upstream.host = %q, want api.deepseek.com", got)
	}
	if got := have["claude_ds.proxy.effort_default"]; got != "auto" {
		t.Errorf("claude_ds.proxy.effort_default = %q, want auto", got)
	}
}

// TestBuildResource_DiagnosticModeOverride verifies that
// CLAUDE_DS_DIAGNOSTIC_MODE=1 forces deployment.environment=doctor.
func TestBuildResource_DiagnosticModeOverride(t *testing.T) {
	t.Setenv("CLAUDE_DS_DIAGNOSTIC_MODE", "1")
	cfg := &Config{
		OTLPDeploymentEnvironment: "production",
	}
	res, err := buildResource(cfg, nil)
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	for _, kv := range res.Attributes() {
		if string(kv.Key) == "deployment.environment" {
			if kv.Value.AsString() != "doctor" {
				t.Errorf("deployment.environment = %q, want doctor", kv.Value.AsString())
			}
			return
		}
	}
	t.Fatal("deployment.environment not set")
}

// TestBuildResource_ExtraOverridesEnvironment verifies that the
// extraResource map (used by --doctor) overrides the cfg-derived
// deployment.environment.
func TestBuildResource_ExtraOverridesEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_DS_DIAGNOSTIC_MODE", "")
	cfg := &Config{OTLPDeploymentEnvironment: "local"}
	res, err := buildResource(cfg, map[string]string{
		"deployment.environment": "doctor",
	})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	got := ""
	for _, kv := range res.Attributes() {
		if string(kv.Key) == "deployment.environment" {
			got = kv.Value.AsString()
		}
	}
	if got != "doctor" {
		t.Errorf("deployment.environment = %q, want doctor (extra wins)", got)
	}
}

// TestExporterOpts_FullURL exercises the WithEndpointURL path.
func TestExporterOpts_FullURL(t *testing.T) {
	traceOpts, metricOpts, logOpts, err := exporterOpts("http://signoz.local:30318", nil)
	if err != nil {
		t.Fatalf("exporterOpts: %v", err)
	}
	if len(traceOpts) == 0 || len(metricOpts) == 0 || len(logOpts) == 0 {
		t.Fatal("expected non-empty option slices")
	}
}

// TestExporterOpts_BareHostPort exercises the WithEndpoint path.
func TestExporterOpts_BareHostPort(t *testing.T) {
	traceOpts, metricOpts, logOpts, err := exporterOpts("signoz.local:30318", nil)
	if err != nil {
		t.Fatalf("exporterOpts: %v", err)
	}
	if len(traceOpts) == 0 || len(metricOpts) == 0 || len(logOpts) == 0 {
		t.Fatal("expected non-empty option slices")
	}
}

// TestExporterOpts_HeadersPassthrough verifies header maps reach
// each exporter's WithHeaders option.
func TestExporterOpts_HeadersPassthrough(t *testing.T) {
	headers := map[string]string{"signoz-access-token": "secret"}
	traceOpts, metricOpts, logOpts, err := exporterOpts("http://example.com", headers)
	if err != nil {
		t.Fatalf("exporterOpts: %v", err)
	}
	// Smoke-check that we got more options than the no-header path.
	bare, _, _, _ := exporterOpts("http://example.com", nil)
	if !(len(traceOpts) > len(bare) && len(metricOpts) > 0 && len(logOpts) > 0) {
		t.Fatal("expected WithHeaders to add an option")
	}
}

// TestNoopShutdown — sanity check.
func TestNoopShutdown(t *testing.T) {
	if err := noopShutdown(context.Background()); err != nil {
		t.Fatalf("noopShutdown: %v", err)
	}
}

// TestShutdownOTLPWithGrace_NilFn — must not panic.
func TestShutdownOTLPWithGrace_NilFn(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("shutdownOTLPWithGrace(nil) panicked: %v", r)
		}
	}()
	shutdownOTLPWithGrace(nil)
}

// TestShutdownOTLPWithGrace_HonorsBudget verifies the 1-second cap on
// the wrapper. A pathological shutdown that blocks longer than 1 s
// should see its context cancelled.
func TestShutdownOTLPWithGrace_HonorsBudget(t *testing.T) {
	called := atomic.Bool{}
	deadlineExceeded := atomic.Bool{}
	fn := func(ctx context.Context) error {
		called.Store(true)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			deadlineExceeded.Store(true)
		}
		return nil
	}
	start := time.Now()
	shutdownOTLPWithGrace(fn)
	elapsed := time.Since(start)
	if !called.Load() {
		t.Fatal("shutdown fn was not called")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown wrapper exceeded its budget: %v", elapsed)
	}
	if !deadlineExceeded.Load() {
		t.Errorf("expected ctx.Done to fire within 1s")
	}
}

// TestExporterOpts_BadURL — malformed URL surfaces an error.
func TestExporterOpts_BadURL(t *testing.T) {
	_, _, _, err := exporterOpts("http://", nil)
	if err == nil {
		t.Fatal("expected error for empty-host URL")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error missing 'host' detail: %v", err)
	}
}
