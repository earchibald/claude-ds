// OpenTelemetry provider lifecycle (CDS-23 / CDS-25).
//
// `buildOTLPProviders` constructs the SDK MeterProvider, TracerProvider,
// and LoggerProvider for this process and registers them globally so the
// proxy's `otel.Tracer(...)`, `otel.Meter(...)`, and
// `otelloggl.Logger(...)` calls (proxy.go) resolve to real exporters.
//
// Lifecycle is owned by `main.go`: providers are constructed BEFORE the
// proxy goroutine launches and torn down via `defer shutdown(ctx)` with
// a 1-second context timeout (per
// docs/superpowers/specs/2026-05-06-claude-ds-otlp-observability.md §10
// "Provider lifecycle on shutdown — synchronous drain, 1 s ceiling").
//
// When `cfg.OTLPEndpoints` is empty, no SDK provider is built. The
// global TracerProvider/MeterProvider remain the OTel default no-ops,
// and we install a no-op LoggerProvider for logs. The returned shutdown
// is a no-op so callers can `defer shutdown(...)` unconditionally.
//
// Strict redaction: this file resolves OTLP header values from
// `cfg.OTLPHeaders` and passes them straight to the exporter's
// `WithHeaders` option. Header values must NEVER reach a log.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	logapi "go.opentelemetry.io/otel/log"
	logembed "go.opentelemetry.io/otel/log/embedded"
	otelloggl "go.opentelemetry.io/otel/log/global"
	logsdk "go.opentelemetry.io/otel/sdk/log"
	metricsdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// BUILD_COMMIT is the short git SHA of the build, populated at link time
// via `-ldflags="-X main.BUILD_COMMIT=$(git rev-parse --short HEAD)"`.
// Empty when the binary was built without the ldflag (dev builds).
var BUILD_COMMIT = ""

// OTLPShutdownFunc drains in-flight signals and stops every registered
// provider. Always non-nil: when `buildOTLPProviders` constructs no-op
// providers, the returned function is a no-op too.
type OTLPShutdownFunc func(ctx context.Context) error

const (
	// otlpBatchTimeout — BatchSpan/LogProcessor flush interval. 250 ms
	// keeps the at-exit drain bounded so the typical shutdown finishes
	// well below the 1 s ceiling (observability doc §10).
	otlpBatchTimeout = 250 * time.Millisecond

	// otlpMaxExportBatchSize — per-batch event cap (observability doc §10).
	otlpMaxExportBatchSize = 256
)

// buildOTLPProviders constructs and registers the three providers
// globally. Returned shutdown chains each provider's Shutdown and
// tolerates per-provider errors so a slow exporter doesn't starve
// another's drain.
//
// `extraResource` is merged into the resource attribute set, caller-key
// wins. The `--doctor` path passes
// `{"deployment.environment": "doctor"}`; the regular launch path
// passes nil.
//
// Empty `cfg.OTLPEndpoints` short-circuits to no-op providers — every
// emit becomes a swallow and the returned shutdown is a no-op.
func buildOTLPProviders(cfg *Config, extraResource map[string]string) (OTLPShutdownFunc, error) {
	if cfg == nil {
		return noopShutdown, nil
	}

	// Empty endpoints → install a no-op LoggerProvider; the default
	// global Tracer/Meter providers are already no-ops.
	if len(cfg.OTLPEndpoints) == 0 {
		otelloggl.SetLoggerProvider(noopLoggerProvider{})
		return noopShutdown, nil
	}

	res, err := buildResource(cfg, extraResource)
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	ctx := context.Background()

	var (
		traceExporters  []tracesdk.SpanExporter
		metricExporters []metricsdk.Exporter
		logExporters    []logsdk.Exporter
	)
	for _, ep := range cfg.OTLPEndpoints {
		traceOpts, metricOpts, logOpts, ferr := exporterOpts(ep, cfg.OTLPHeaders)
		if ferr != nil {
			return nil, fmt.Errorf("otel: endpoint %q: %w", ep, ferr)
		}
		te, ferr := otlptracehttp.New(ctx, traceOpts...)
		if ferr != nil {
			return nil, fmt.Errorf("otel: trace exporter for %q: %w", ep, ferr)
		}
		me, ferr := otlpmetrichttp.New(ctx, metricOpts...)
		if ferr != nil {
			return nil, fmt.Errorf("otel: metric exporter for %q: %w", ep, ferr)
		}
		le, ferr := otlploghttp.New(ctx, logOpts...)
		if ferr != nil {
			return nil, fmt.Errorf("otel: log exporter for %q: %w", ep, ferr)
		}
		traceExporters = append(traceExporters, te)
		metricExporters = append(metricExporters, me)
		logExporters = append(logExporters, le)
	}

	tpOpts := []tracesdk.TracerProviderOption{
		tracesdk.WithResource(res),
		tracesdk.WithSampler(tracesdk.ParentBased(tracesdk.AlwaysSample())),
	}
	for _, te := range traceExporters {
		tpOpts = append(tpOpts,
			tracesdk.WithBatcher(te,
				tracesdk.WithBatchTimeout(otlpBatchTimeout),
				tracesdk.WithMaxExportBatchSize(otlpMaxExportBatchSize),
			),
		)
	}
	tp := tracesdk.NewTracerProvider(tpOpts...)

	mpOpts := []metricsdk.Option{metricsdk.WithResource(res)}
	for _, me := range metricExporters {
		mpOpts = append(mpOpts, metricsdk.WithReader(metricsdk.NewPeriodicReader(me)))
	}
	mp := metricsdk.NewMeterProvider(mpOpts...)

	lpOpts := []logsdk.LoggerProviderOption{logsdk.WithResource(res)}
	for _, le := range logExporters {
		lpOpts = append(lpOpts,
			logsdk.WithProcessor(logsdk.NewBatchProcessor(le,
				logsdk.WithExportInterval(otlpBatchTimeout),
				logsdk.WithExportMaxBatchSize(otlpMaxExportBatchSize),
			)),
		)
	}
	lp := logsdk.NewLoggerProvider(lpOpts...)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otelloggl.SetLoggerProvider(lp)

	shutdown := func(ctx context.Context) error {
		var firstErr error
		if err := tp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := mp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := lp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	return shutdown, nil
}

// exporterOpts builds the per-endpoint option lists for trace, metric,
// and log HTTP exporters. The endpoint string can be a bare host:port
// (interpreted as `http://`-with-default-paths) or a full URL.
func exporterOpts(endpoint string, headers map[string]string) (
	[]otlptracehttp.Option,
	[]otlpmetrichttp.Option,
	[]otlploghttp.Option,
	error,
) {
	var (
		traceOpts  []otlptracehttp.Option
		metricOpts []otlpmetrichttp.Option
		logOpts    []otlploghttp.Option
	)
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse endpoint URL: %w", err)
		}
		if u.Host == "" {
			return nil, nil, nil, fmt.Errorf("endpoint URL %q has no host", endpoint)
		}
		traceOpts = append(traceOpts, otlptracehttp.WithEndpointURL(endpoint))
		metricOpts = append(metricOpts, otlpmetrichttp.WithEndpointURL(endpoint))
		logOpts = append(logOpts, otlploghttp.WithEndpointURL(endpoint))
		if u.Scheme == "http" {
			traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
			metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
			logOpts = append(logOpts, otlploghttp.WithInsecure())
		}
	} else {
		traceOpts = append(traceOpts, otlptracehttp.WithEndpoint(endpoint), otlptracehttp.WithInsecure())
		metricOpts = append(metricOpts, otlpmetrichttp.WithEndpoint(endpoint), otlpmetrichttp.WithInsecure())
		logOpts = append(logOpts, otlploghttp.WithEndpoint(endpoint), otlploghttp.WithInsecure())
	}
	if len(headers) > 0 {
		traceOpts = append(traceOpts, otlptracehttp.WithHeaders(headers))
		metricOpts = append(metricOpts, otlpmetrichttp.WithHeaders(headers))
		logOpts = append(logOpts, otlploghttp.WithHeaders(headers))
	}
	return traceOpts, metricOpts, logOpts, nil
}

// buildResource composes the Resource attribute set per acceptance #11.
// extraResource overrides config-derived defaults (used by --doctor to
// force deployment.environment=doctor).
func buildResource(cfg *Config, extra map[string]string) (*resource.Resource, error) {
	serviceName := cfg.OTLPServiceName
	if serviceName == "" {
		serviceName = defaultOTLPServiceName
	}

	deploymentEnv := cfg.OTLPDeploymentEnvironment
	if deploymentEnv == "" {
		deploymentEnv = defaultOTLPDeploymentEnvironment
	}
	// CLAUDE_DS_DIAGNOSTIC_MODE forces deployment.environment=doctor
	// (--doctor and --setup set this env var before calling us).
	if os.Getenv("CLAUDE_DS_DIAGNOSTIC_MODE") == "1" {
		deploymentEnv = "doctor"
	}

	hostName, _ := os.Hostname()
	if hostName == "" {
		hostName = "unknown"
	}
	instanceID := uuid.NewString()

	upstreamHost := ""
	if cfg.BaseURL != "" {
		if u, err := url.Parse(cfg.BaseURL); err == nil {
			upstreamHost = u.Host
		}
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(VERSION),
		semconv.ServiceInstanceID(instanceID),
		semconv.DeploymentEnvironment(deploymentEnv),
		semconv.HostName(hostName),
		attribute.String("claude_ds.proxy.effort_default", cfg.ProxyEffort),
		attribute.String("claude_ds.proxy.vision_model", cfg.VisionModel),
		attribute.String("claude_ds.upstream.host", upstreamHost),
		attribute.String("claude_ds.build.commit", BUILD_COMMIT),
	}

	// User-supplied OTel resource attributes (otlp_resource_attributes
	// + OTEL_RESOURCE_ATTRIBUTES env, already secretref-resolved).
	for k, v := range cfg.OTLPResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	// Caller-supplied overrides win (e.g. --doctor).
	for k, v := range extra {
		attrs = append(attrs, attribute.String(k, v))
		if k == "deployment.environment" {
			// keep semconv variant in sync — the caller's plain key
			// wins over the semconv-keyed one above by virtue of being
			// emitted later in the slice.
		}
	}

	return resource.New(context.Background(), resource.WithAttributes(attrs...))
}

// noopShutdown is the trivial shutdown returned when no exporter is
// constructed. Always succeeds.
func noopShutdown(_ context.Context) error { return nil }

// noopLoggerProvider satisfies the logs API LoggerProvider interface
// without doing anything. Used when cfg.OTLPEndpoints is empty so the
// global logger drops all records silently.
type noopLoggerProvider struct {
	logembed.LoggerProvider
}

func (noopLoggerProvider) Logger(string, ...logapi.LoggerOption) logapi.Logger {
	return noopLogger{}
}

type noopLogger struct {
	logembed.Logger
}

func (noopLogger) Emit(context.Context, logapi.Record)                    {}
func (noopLogger) Enabled(context.Context, logapi.EnabledParameters) bool { return false }

// Compile-time interface checks.
var (
	_ logapi.LoggerProvider = noopLoggerProvider{}
	_ logapi.Logger         = noopLogger{}
)
