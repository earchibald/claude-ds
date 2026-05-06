// HTTP proxy server (CDS-15).
//
// Lifecycle, handler dispatch, upstream forwarding, SSE streaming, and
// OTLP instrumentation per CDS-25.
//
// The proxy is a `net/http.Server` running in a background goroutine
// bound to 127.0.0.1:0 (kernel-assigned port). The bound port is
// returned to the caller via Addr() and threaded into ANTHROPIC_BASE_URL
// so the launched `claude` CLI hits us first.
//
// Routing:
//   POST /v1/messages → messagesHandler — body rewriting (file_id →
//     base64 via CDS-18 stub, vision routing via CDS-19 stub, effort
//     spec via CDS-11), header pipeline, upstream forward, SSE stream.
//   POST /v1/files    → filesHandler — Files API mock (CDS-16, lives in
//     files.go; this file only registers it on the mux).
//   *                 → passthroughHandler — header pipeline + verbatim
//     forward.
//
// OTLP instrumentation per docs/superpowers/specs/2026-05-06-claude-ds-otlp-observability.md:
//   - otelhttp.NewHandler wraps the inbound mux (free http.server.* RED metrics).
//   - otelhttp.NewTransport wraps the upstream client (free http.client.* RED metrics).
//   - claude_ds.transform.* spans + claude_ds.transform.{count,duration} metrics for
//     each pipeline step (file_id_to_base64, wire_model, vision_route, effort,
//     header_pipeline).
//   - claude_ds.stream.{ttfb.duration,duration,chunk.count,bytes,client_disconnect.count}
//     histograms + counter for the SSE relay.
//   - claude_ds.{effort,wire_model,vision,header}.* counters per the doc.
//   - Strict redaction: only counts, sizes, and mutation flags reach the wire — never
//     bodies, message content, tool args/results, file bytes, Authorization values,
//     or x-api-key values. Header NAMES are recorded; values are not. Build the
//     redacted attribute helpers in this file (see endpointAttr / transformAttrs).
//
// Until CDS-23 constructs real OTel providers in main.go, the global no-op
// providers swallow everything; this file is provider-agnostic by design.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	otelloggl "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName is the meter / tracer / logger scope used for every
// signal emitted by this file. Matches the resource service name from
// CDS-12 / CDS-25 (`claude-ds-proxy`) so SigNoz dashboards filter on a
// single value across both resource and scope.
const instrumentationName = "claude-ds-proxy"

// upstreamReadChunkSize is the byte budget per Read() from the upstream
// response body during SSE relay. Matches the 8 KB chunk size the Python
// proxy uses (claude-ds-proxy.py:887). Small enough to stream first SSE
// events with sub-ms TTFB; large enough that per-syscall overhead is
// negligible for long completions.
const upstreamReadChunkSize = 8192

// upstreamTimeout is the wall-clock deadline for a single upstream
// request including the streamed response. 600 s mirrors the Python
// proxy (claude-ds-proxy.py:853) and matches the longest reasonable
// max-effort completion at the slowest models.
const upstreamTimeout = 600 * time.Second

// shutdownGrace is the budget for in-flight requests during graceful
// shutdown. Tests use small values; production keeps requests alive for
// the full 600 s timeout above. Callers pass their own context to
// Shutdown — this is the per-request override the standard library
// honours for `srv.Shutdown(ctx)`.
const defaultShutdownGrace = 5 * time.Second

// ProxyOpts configures NewProxy. Zero values are valid: Debug off, no
// ShouldStart override (the default rule is "start when an effort spec
// is non-off OR otlp_endpoints is non-empty"), no OnReady callback.
type ProxyOpts struct {
	// Debug toggles per-decision header / transform logging. Names
	// only — never values. When unset, falls back to cfg.ProxyDebug.
	Debug bool

	// ShouldStart, if non-nil, overrides the default lifecycle gate.
	// Returning false from NewProxy yields (nil, nil), letting the
	// caller skip the ANTHROPIC_BASE_URL setup.
	ShouldStart func(cfg *Config) bool

	// OnReady, if non-nil, is invoked once the listener is bound and
	// the goroutine has begun serving. The argument is the
	// `127.0.0.1:<port>` address suitable for ANTHROPIC_BASE_URL.
	OnReady func(addr string)
}

// RewriteInfo summarises the file_id_to_base64 + wire_model + effort
// transforms applied to a /v1/messages body. CDS-18 (request rewriting)
// will populate the fields; for now the stub returns a zero value.
//
// Every field is a count, flag, or bounded enum — body content never
// leaves the rewrite step.
type RewriteInfo struct {
	// FileIDLookups is the number of `source.file_id` references the
	// rewriter encountered in this request body.
	FileIDLookups int
	// FileIDHits is the count of those references that hit the local
	// files cache and were substituted with base64.
	FileIDHits int
	// FileIDMisses is FileIDLookups - FileIDHits — a real bug when
	// non-zero (see the observability doc).
	FileIDMisses int

	// ModelRequested is the model id as it arrived from claude.
	ModelRequested string
	// ModelUpstream is the model id post-rewrite (after WIRE_MODEL_MAP
	// + catchall + vision routing).
	ModelUpstream string
	// WireModelKind is one of "map", "catchall", "noop" — see
	// claude_ds.wire_model.kind in the observability doc.
	WireModelKind string

	// EffortBucket is the source bucket detected via BucketFromThinking.
	EffortBucket Bucket
	// EffortRegime is the regime applied via ApplyRegime, or
	// RegimeOff for passthrough.
	EffortRegime Regime
	// EffortPreviousValue is the prior `reasoning_effort` field
	// (empty when absent). Recorded as a span attribute, never as a
	// metric label.
	EffortPreviousValue string

	// Mutated is true when any transform actually changed the body.
	Mutated bool
}

// VisionInfo summarises the vision-routing decision. CDS-19 will fill
// the fields; for now the stub returns a zero value.
type VisionInfo struct {
	// Routed is true when the request was redirected to VISION_MODEL.
	Routed bool
	// ImagesCollected is the count of image content blocks the
	// detector found (direct or via tool_result).
	ImagesCollected int
	// ModelFrom / ModelTo are the pre/post-route model ids — bounded
	// by the user's WIRE_MODEL_MAP and VISION_MODEL config.
	ModelFrom string
	ModelTo   string
}

// Proxy is the running HTTP proxy. The zero value is not usable — call
// NewProxy then Start.
type Proxy struct {
	cfg  *Config
	opts ProxyOpts

	srv *http.Server
	ln  net.Listener

	upstream     *url.URL
	httpClient   *http.Client
	headerOpts   HeaderOpts
	transformMap map[string]Resolver

	// OTel instruments — created once at NewProxy time, reused across
	// every request so the SDK doesn't churn allocations.
	tracer  trace.Tracer
	meter   metric.Meter
	logger  otellog.Logger
	metrics *proxyMetrics

	started atomic.Bool
}

// proxyMetrics is the bag of pre-registered OTel instruments. All names
// match the inventory in the CDS-25 observability doc verbatim; new
// counters added here MUST also be reflected there.
type proxyMetrics struct {
	// Transform pipeline (claude_ds.transform.*).
	transformCount    metric.Int64Counter
	transformDuration metric.Float64Histogram

	// Effort.
	effortApplied metric.Int64Counter

	// Wire-model rewrite.
	wireModelRewrite metric.Int64Counter

	// Vision routing.
	visionRoute metric.Int64Counter

	// Header pipeline beta-token strips.
	headerBetaStripped metric.Int64Counter

	// Streaming relay.
	streamTTFB              metric.Float64Histogram
	streamDuration          metric.Float64Histogram
	streamChunks            metric.Int64Histogram
	streamBytes             metric.Int64Histogram
	streamClientDisconnects metric.Int64Counter

	// Upstream errors (rides on http.client.* via error.type, plus
	// this dedicated counter for unreachable / non-HTTP failures).
	upstreamError metric.Int64Counter
}

// NewProxy builds the proxy from cfg + opts. Returns (nil, nil) when the
// lifecycle gate decides the proxy isn't needed (all effort specs off
// AND otlp_endpoints empty). Caller must check for nil before invoking
// Start / Addr / Shutdown.
//
// The listener is bound here, not in Start, so the kernel-assigned port
// is available to callers that need to thread ANTHROPIC_BASE_URL into
// the child process before the goroutine has a chance to run.
func NewProxy(cfg *Config, opts ProxyOpts) (*Proxy, error) {
	if cfg == nil {
		return nil, errors.New("proxy: nil config")
	}

	gate := opts.ShouldStart
	if gate == nil {
		gate = defaultShouldStart
	}
	if !gate(cfg) {
		return nil, nil
	}

	if !opts.Debug {
		opts.Debug = cfg.ProxyDebug
	}

	upstream, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse base_url %q: %w", cfg.BaseURL, err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("proxy: base_url %q missing scheme or host", cfg.BaseURL)
	}

	bind := cfg.ProxyBind
	if bind == "" {
		bind = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", bind+":0")
	if err != nil {
		return nil, fmt.Errorf("proxy: bind %s:0: %w", bind, err)
	}

	// Per-model effort resolvers (CDS-11). ParseMap tolerates an empty
	// string and returns an empty map.
	tmap, err := ParseMap("")
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("proxy: parse effort map: %w", err)
	}

	// Header pipeline opts. CDS-23 will load them from the env once
	// main.go is wired; for now we honour cfg.ProxyDebug + caller opts
	// and let the caller layer extra strip/add lists on top via env.
	hopts := LoadHeaderOptsFromEnv()
	hopts.Debug = opts.Debug

	tracer := otel.Tracer(instrumentationName)
	meter := otel.Meter(instrumentationName)
	logger := otelloggl.Logger(instrumentationName)

	metrics, err := newProxyMetrics(meter)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("proxy: register metrics: %w", err)
	}

	p := &Proxy{
		cfg:          cfg,
		opts:         opts,
		ln:           ln,
		upstream:     upstream,
		headerOpts:   hopts,
		transformMap: tmap,
		tracer:       tracer,
		meter:        meter,
		logger:       logger,
		metrics:      metrics,
	}

	// Upstream client — otelhttp.NewTransport gives us free
	// http.client.{request,response.body.size,connect}.duration metrics
	// + connect/send/stream child spans.
	p.httpClient = &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   upstreamTimeout,
	}

	mux := http.NewServeMux()
	mux.Handle(
		"POST /v1/messages",
		otelhttp.WithRouteTag("/v1/messages", http.HandlerFunc(p.messagesHandler)),
	)
	mux.Handle(
		"POST /v1/files",
		otelhttp.WithRouteTag("/v1/files", http.HandlerFunc(filesHandler)),
	)
	mux.Handle("/", otelhttp.WithRouteTag("passthrough", http.HandlerFunc(p.passthroughHandler)))

	// Inbound otelhttp wrapper. Custom span-name formatter keeps
	// http.route low-cardinality (matches the observability doc).
	rootHandler := otelhttp.NewHandler(
		mux,
		instrumentationName,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + spanRouteFor(r.URL.Path)
		}),
	)

	p.srv = &http.Server{
		Handler:           rootHandler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	// HTTP/1.1 length-by-EOF framing — match the Python proxy. Each
	// response sets "Connection: close"; disabling keep-alives at the
	// server level guarantees Go writes the header instead of silently
	// stripping it (it would otherwise manage the connection lifecycle
	// itself).
	p.srv.SetKeepAlivesEnabled(false)

	return p, nil
}

// Start begins serving in a background goroutine. Returns immediately
// after the goroutine is launched; the listener was bound in NewProxy
// so Addr() is already valid by the time Start returns.
func (p *Proxy) Start(_ context.Context) error {
	if p == nil || p.srv == nil {
		return errors.New("proxy: not initialised")
	}
	if !p.started.CompareAndSwap(false, true) {
		return errors.New("proxy: already started")
	}
	go func() {
		_ = p.srv.Serve(p.ln)
	}()
	if p.opts.OnReady != nil {
		p.opts.OnReady(p.Addr())
	}
	return nil
}

// Addr returns the bound `host:port`. Safe to call after NewProxy and
// before Start; the listener was bound in NewProxy.
func (p *Proxy) Addr() string {
	if p == nil || p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// Shutdown gracefully stops the server. Honours the supplied context
// deadline; the design spec (Provider lifecycle) gives the caller 1 s
// for the OTel exporters and a separate budget for in-flight requests.
func (p *Proxy) Shutdown(ctx context.Context) error {
	if p == nil || p.srv == nil {
		return nil
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), defaultShutdownGrace)
		defer cancel()
	}
	return p.srv.Shutdown(ctx)
}

// defaultShouldStart is the lifecycle gate from the issue note: start
// when ANY of the effort specs is non-off OR otlp_endpoints is set.
//
// "Non-off" means a non-empty value that is not the literal string "off"
// (case-insensitive). The four per-tier overrides are checked the same
// way as the global default.
func defaultShouldStart(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	if len(cfg.OTLPEndpoints) > 0 {
		return true
	}
	for _, s := range []string{
		cfg.ProxyEffort,
		cfg.ProxyEffortOpus,
		cfg.ProxyEffortSonnet,
		cfg.ProxyEffortHaiku,
		cfg.ProxyEffortSmallFast,
	} {
		if isEffortActive(s) {
			return true
		}
	}
	return false
}

// isEffortActive reports whether a spec value would cause the proxy to
// rewrite anything. Mirrors the ParseSpec passthrough detection: empty
// or "off" → inactive.
func isEffortActive(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return !strings.EqualFold(s, "off")
}

// spanRouteFor maps an inbound URL path to the bounded http.route enum
// the observability doc requires. Never returns a raw path — that would
// blow up SigNoz cardinality on the services view.
func spanRouteFor(path string) string {
	switch {
	case path == "/v1/messages":
		return "/v1/messages"
	case path == "/v1/files":
		return "/v1/files"
	default:
		return "passthrough"
	}
}

// endpointAttr returns the `claude_ds.endpoint` attribute for a path.
// Bounded enum: messages | files | passthrough.
func endpointAttr(path string) attribute.KeyValue {
	switch path {
	case "/v1/messages":
		return attribute.String("claude_ds.endpoint", "messages")
	case "/v1/files":
		return attribute.String("claude_ds.endpoint", "files")
	default:
		return attribute.String("claude_ds.endpoint", "passthrough")
	}
}

// newProxyMetrics registers the claude_ds.* instruments listed in the
// observability doc. http.{server,client}.* come for free from
// otelhttp; we never register them by hand.
func newProxyMetrics(m metric.Meter) (*proxyMetrics, error) {
	mt := &proxyMetrics{}
	var err error

	if mt.transformCount, err = m.Int64Counter(
		"claude_ds.transform.count",
		metric.WithUnit("1"),
		metric.WithDescription("Per-step transform invocations on /v1/messages."),
	); err != nil {
		return nil, err
	}
	if mt.transformDuration, err = m.Float64Histogram(
		"claude_ds.transform.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Per-step transform latency on /v1/messages."),
	); err != nil {
		return nil, err
	}
	if mt.effortApplied, err = m.Int64Counter(
		"claude_ds.effort.regime.applied",
		metric.WithUnit("1"),
		metric.WithDescription("Distribution of effort regimes after spec resolution."),
	); err != nil {
		return nil, err
	}
	if mt.wireModelRewrite, err = m.Int64Counter(
		"claude_ds.wire_model.rewrite.count",
		metric.WithUnit("1"),
		metric.WithDescription("Wire-model rewrites applied to /v1/messages bodies."),
	); err != nil {
		return nil, err
	}
	if mt.visionRoute, err = m.Int64Counter(
		"claude_ds.vision.route.count",
		metric.WithUnit("1"),
		metric.WithDescription("Vision-routing decisions on /v1/messages bodies."),
	); err != nil {
		return nil, err
	}
	if mt.headerBetaStripped, err = m.Int64Counter(
		"claude_ds.header.beta.stripped",
		metric.WithUnit("1"),
		metric.WithDescription("Per-token strips of anthropic-beta values."),
	); err != nil {
		return nil, err
	}
	if mt.streamTTFB, err = m.Float64Histogram(
		"claude_ds.stream.ttfb.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Upstream-request-sent → first SSE byte forwarded."),
	); err != nil {
		return nil, err
	}
	if mt.streamDuration, err = m.Float64Histogram(
		"claude_ds.stream.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("First byte to last byte of streamed response."),
	); err != nil {
		return nil, err
	}
	if mt.streamChunks, err = m.Int64Histogram(
		"claude_ds.stream.chunk.count",
		metric.WithUnit("1"),
		metric.WithDescription("Chunk-count distribution per response."),
	); err != nil {
		return nil, err
	}
	if mt.streamBytes, err = m.Int64Histogram(
		"claude_ds.stream.bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Total streamed body bytes."),
	); err != nil {
		return nil, err
	}
	if mt.streamClientDisconnects, err = m.Int64Counter(
		"claude_ds.stream.client_disconnect.count",
		metric.WithUnit("1"),
		metric.WithDescription("Client closed the connection mid-stream."),
	); err != nil {
		return nil, err
	}
	if mt.upstreamError, err = m.Int64Counter(
		"claude_ds.upstream.error.count",
		metric.WithUnit("1"),
		metric.WithDescription("Upstream-unreachable / non-HTTP failures."),
	); err != nil {
		return nil, err
	}
	return mt, nil
}

// ---------- Handlers --------------------------------------------------------

// messagesHandler implements POST /v1/messages.
//
// Pipeline (mirrors the design spec "Request routing" subsection):
//  1. Read body (bounded by net/http MaxBytesReader-style guard at the
//     listener level; the proxy does not impose a body cap of its own
//     since claude is a trusted client).
//  2. rewriteBody (CDS-18 stub) — file_id → base64, wire-model rewrite.
//  3. routeVision (CDS-19 stub) — vision routing if images present.
//  4. Effort spec via BucketFromThinking + ApplyRegime (CDS-11) when
//     vision did not route.
//  5. Header pipeline.
//  6. Upstream forward + SSE relay.
func (p *Proxy) messagesHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rootSpan := trace.SpanFromContext(ctx)
	rootSpan.SetAttributes(
		endpointAttr(r.URL.Path),
		attribute.String("claude_ds.proxy.effort_default", p.cfg.ProxyEffort),
		attribute.String("claude_ds.upstream.host", p.upstream.Host),
	)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		rootSpan.RecordError(err)
		rootSpan.SetStatus(codes.Error, "read inbound body")
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	rootSpan.SetAttributes(attribute.Int("claude_ds.body.input_size", len(body)))

	// 1. file_id → base64 + wire-model rewrite (CDS-18 stub).
	body, rewriteInfo := p.runRewrite(ctx, body)

	// 2. Vision routing (CDS-19 stub).
	body, visionInfo := p.runVision(ctx, body)

	// 3. Effort spec resolution (skip if vision routed — vision models
	//    don't support extended thinking per the design spec).
	if !visionInfo.Routed {
		body = p.runEffort(ctx, body, &rewriteInfo)
	}

	rootSpan.SetAttributes(
		attribute.String("claude_ds.model.requested", rewriteInfo.ModelRequested),
		attribute.String("claude_ds.model.upstream", rewriteInfo.ModelUpstream),
		attribute.String("claude_ds.effort.bucket", string(rewriteInfo.EffortBucket)),
		attribute.String("claude_ds.effort.regime", regimeForAttribute(rewriteInfo.EffortRegime)),
		attribute.Bool("claude_ds.vision.routed", visionInfo.Routed),
		attribute.Int("claude_ds.files.lookup.hits", rewriteInfo.FileIDHits),
		attribute.Int("claude_ds.files.lookup.misses", rewriteInfo.FileIDMisses),
	)

	// 4. Header pipeline + upstream forward.
	p.forwardUpstream(ctx, w, r, body, true /* allowStream */)
}

// passthroughHandler implements every request that isn't /v1/messages
// or /v1/files. Body is forwarded verbatim through the header pipeline.
func (p *Proxy) passthroughHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rootSpan := trace.SpanFromContext(ctx)
	rootSpan.SetAttributes(
		endpointAttr(r.URL.Path),
		attribute.String("claude_ds.upstream.host", p.upstream.Host),
	)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		rootSpan.RecordError(err)
		rootSpan.SetStatus(codes.Error, "read inbound body")
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	rootSpan.SetAttributes(attribute.Int("claude_ds.body.input_size", len(body)))
	p.forwardUpstream(ctx, w, r, body, true /* allowStream */)
}

// ---------- Transform spans -------------------------------------------------

// runRewrite invokes the CDS-18 stub for file_id_to_base64 + wire_model
// rewriting under a single composite span tree. Each substep emits its
// own span so the trace tree matches the observability doc exactly.
func (p *Proxy) runRewrite(ctx context.Context, body []byte) ([]byte, RewriteInfo) {
	// file_id_to_base64 span — even though the stub is a no-op today,
	// emitting the span unconditionally keeps the trace shape stable
	// across the CDS-18 transition.
	ctx, fileSpan := p.tracer.Start(ctx, "claude_ds.transform.file_id_to_base64")
	fileStart := time.Now()
	body2, info, err := rewriteBody(body, p.cfg)
	fileDur := time.Since(fileStart).Seconds() * 1000
	if err != nil {
		fileSpan.RecordError(err)
		fileSpan.SetStatus(codes.Error, "rewrite body")
		fileSpan.SetAttributes(attribute.String("claude_ds.transform.error", err.Error()))
	}
	fileSpan.SetAttributes(
		attribute.String("claude_ds.transform.step", "file_id_to_base64"),
		attribute.Int("claude_ds.files.lookup.count", info.FileIDLookups),
		attribute.Int("claude_ds.files.substitutions", info.FileIDHits),
	)
	p.metrics.transformCount.Add(ctx, 1, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "file_id_to_base64"),
		attribute.Bool("claude_ds.transform.mutated", info.FileIDHits > 0),
	))
	p.metrics.transformDuration.Record(ctx, fileDur, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "file_id_to_base64"),
		attribute.Bool("claude_ds.transform.mutated", info.FileIDHits > 0),
	))
	fileSpan.End()

	// wire_model span. The stub may have rewritten the model id; even
	// if not, emit the span so the trace tree shape stays stable.
	ctx, wireSpan := p.tracer.Start(ctx, "claude_ds.transform.wire_model")
	wireSpan.SetAttributes(
		attribute.String("claude_ds.transform.step", "wire_model"),
		attribute.String("claude_ds.wire_model.from", info.ModelRequested),
		attribute.String("claude_ds.wire_model.to", info.ModelUpstream),
		attribute.String("claude_ds.wire_model.kind", wireModelKindOrDefault(info.WireModelKind)),
	)
	if info.WireModelKind != "" && info.ModelRequested != info.ModelUpstream {
		wireSpan.SetAttributes(
			attribute.Bool("claude_ds.wire_model.catch_all_used", info.WireModelKind == "catchall"),
		)
		p.metrics.wireModelRewrite.Add(ctx, 1, metric.WithAttributes(
			attribute.String("claude_ds.wire_model.from", info.ModelRequested),
			attribute.String("claude_ds.wire_model.to", info.ModelUpstream),
			attribute.String("claude_ds.wire_model.kind", info.WireModelKind),
			attribute.Bool("claude_ds.wire_model.catch_all_used", info.WireModelKind == "catchall"),
		))
	}
	wireSpan.End()
	_ = ctx

	return body2, info
}

// runVision invokes the CDS-19 stub under a claude_ds.transform.vision_route
// span. The stub returns the body unchanged + an empty VisionInfo until
// CDS-19 lands.
func (p *Proxy) runVision(ctx context.Context, body []byte) ([]byte, VisionInfo) {
	ctx, span := p.tracer.Start(ctx, "claude_ds.transform.vision_route")
	defer span.End()
	start := time.Now()
	body2, info, err := routeVision(body, p.cfg)
	dur := time.Since(start).Seconds() * 1000
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "route vision")
		span.SetAttributes(attribute.String("claude_ds.transform.error", err.Error()))
	}
	span.SetAttributes(
		attribute.String("claude_ds.transform.step", "vision_route"),
		attribute.Bool("claude_ds.transform.mutated", info.Routed),
		attribute.Bool("claude_ds.vision.routed", info.Routed),
		attribute.Int("claude_ds.vision.images_collected", info.ImagesCollected),
	)
	p.metrics.transformCount.Add(ctx, 1, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "vision_route"),
		attribute.Bool("claude_ds.transform.mutated", info.Routed),
	))
	p.metrics.transformDuration.Record(ctx, dur, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "vision_route"),
		attribute.Bool("claude_ds.transform.mutated", info.Routed),
	))
	p.metrics.visionRoute.Add(ctx, 1, metric.WithAttributes(
		attribute.Bool("claude_ds.vision.routed", info.Routed),
		attribute.String("claude_ds.model.from", info.ModelFrom),
		attribute.String("claude_ds.model.to", info.ModelTo),
	))
	return body2, info
}

// runEffort applies the effort regime to the body. Records the
// claude_ds.transform.effort span + claude_ds.effort.regime.applied
// counter. Mutates `info` in place with the resolved bucket / regime so
// the root span attribute setter sees the canonical values.
func (p *Proxy) runEffort(ctx context.Context, body []byte, info *RewriteInfo) []byte {
	ctx, span := p.tracer.Start(ctx, "claude_ds.transform.effort")
	defer span.End()
	start := time.Now()

	// Resolve spec for this model. Per-tier override (CDS-18) wins;
	// otherwise fall back to cfg.ProxyEffort (EFFORT_DEFAULT).
	model := info.ModelUpstream
	if model == "" {
		model = info.ModelRequested
	}
	tier := modelTier(p.cfg, model)
	specRaw := effortSpecForModel(p.cfg, model)
	resolver, err := ParseSpec(specRaw)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "parse effort spec")
		span.SetAttributes(attribute.String("claude_ds.transform.error", err.Error()))
		span.SetAttributes(
			attribute.String("claude_ds.transform.step", "effort"),
			attribute.Bool("claude_ds.transform.mutated", false),
		)
		return body
	}
	if resolver == nil {
		// Passthrough — record the no-op and return.
		info.EffortBucket = ""
		info.EffortRegime = RegimeOff
		span.SetAttributes(
			attribute.String("claude_ds.transform.step", "effort"),
			attribute.Bool("claude_ds.transform.mutated", false),
			attribute.String("claude_ds.effort.regime", "passthrough"),
			attribute.String("claude_ds.model.tier", tier),
		)
		p.metrics.transformCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String("claude_ds.transform.step", "effort"),
			attribute.Bool("claude_ds.transform.mutated", false),
		))
		dur := time.Since(start).Seconds() * 1000
		p.metrics.transformDuration.Record(ctx, dur, metric.WithAttributes(
			attribute.String("claude_ds.transform.step", "effort"),
			attribute.Bool("claude_ds.transform.mutated", false),
		))
		return body
	}

	// Decode → bucket → apply → re-encode. JSON-only: rewriteBody
	// already passed non-JSON bodies through, so invalid JSON here is
	// a real error.
	var obj map[string]any
	if jerr := json.Unmarshal(body, &obj); jerr != nil {
		span.RecordError(jerr)
		span.SetStatus(codes.Error, "decode JSON")
		span.SetAttributes(
			attribute.String("claude_ds.transform.step", "effort"),
			attribute.Bool("claude_ds.transform.mutated", false),
			attribute.String("claude_ds.transform.error", jerr.Error()),
		)
		return body
	}
	bucket := BucketFromThinking(obj)
	regime := resolver(bucket)
	// Capture incoming reasoning_effort (non-redacting — the value is a
	// closed enum from the Anthropic API, never user data).
	previous := "<absent>"
	if v, ok := obj["reasoning_effort"].(string); ok {
		previous = v
	}
	if regime != RegimeOff {
		ApplyRegime(obj, regime)
		body2, jerr := json.Marshal(obj)
		if jerr != nil {
			span.RecordError(jerr)
			span.SetStatus(codes.Error, "re-encode JSON")
		} else {
			body = body2
			info.Mutated = true
		}
	}
	info.EffortBucket = bucket
	info.EffortRegime = regime
	info.EffortPreviousValue = previous

	span.SetAttributes(
		attribute.String("claude_ds.transform.step", "effort"),
		attribute.Bool("claude_ds.transform.mutated", regime != RegimeOff),
		attribute.String("claude_ds.effort.bucket", string(bucket)),
		attribute.String("claude_ds.effort.regime", regimeForAttribute(regime)),
		attribute.String("claude_ds.effort.previous_value", previous),
		attribute.String("claude_ds.model.tier", tier),
	)
	p.metrics.transformCount.Add(ctx, 1, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "effort"),
		attribute.Bool("claude_ds.transform.mutated", regime != RegimeOff),
	))
	dur := time.Since(start).Seconds() * 1000
	p.metrics.transformDuration.Record(ctx, dur, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "effort"),
		attribute.Bool("claude_ds.transform.mutated", regime != RegimeOff),
	))
	p.metrics.effortApplied.Add(ctx, 1, metric.WithAttributes(
		attribute.String("claude_ds.effort.regime", regimeForAttribute(regime)),
		attribute.String("claude_ds.effort.bucket", string(bucket)),
		attribute.String("claude_ds.model.upstream", info.ModelUpstream),
	))
	return body
}

// regimeForAttribute maps Regime → the closed enum used on metrics +
// span attributes (see §3 / §10 in the observability doc). RegimeOff →
// "passthrough" preserves the metric semantics.
func regimeForAttribute(r Regime) string {
	switch r {
	case RegimeNone:
		return "none"
	case RegimeHigh:
		return "high"
	case RegimeMax:
		return "max"
	case RegimeOff:
		return "passthrough"
	default:
		return "unknown"
	}
}

// wireModelKindOrDefault returns "noop" when the rewrite stub didn't
// classify the rewrite kind. Keeps the metric attribute well-defined.
func wireModelKindOrDefault(k string) string {
	if k == "" {
		return "noop"
	}
	return k
}

// ---------- Upstream forwarding + streaming ---------------------------------

// forwardUpstream applies the header pipeline, dispatches the request
// upstream via the otelhttp-instrumented client, and streams the
// response back to the inbound caller.
func (p *Proxy) forwardUpstream(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	_ bool,
) {
	rootSpan := trace.SpanFromContext(ctx)

	// Header pipeline span — applies to both /v1/messages and
	// passthrough.
	hdrCtx, hdrSpan := p.tracer.Start(ctx, "claude_ds.transform.header_pipeline")
	hdrStart := time.Now()
	originalCount := headerCount(r.Header)
	upstreamHeaders := BuildUpstreamHeaders(r.Header, p.upstream.Host, len(body), p.headerOpts)
	finalCount := headerCount(upstreamHeaders)
	hdrSpan.SetAttributes(
		attribute.String("claude_ds.transform.step", "header_pipeline"),
		attribute.Bool("claude_ds.transform.mutated", originalCount != finalCount),
		attribute.Int("claude_ds.header.in_count", originalCount),
		attribute.Int("claude_ds.header.out_count", finalCount),
	)
	hdrDur := time.Since(hdrStart).Seconds() * 1000
	p.metrics.transformCount.Add(hdrCtx, 1, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "header_pipeline"),
		attribute.Bool("claude_ds.transform.mutated", originalCount != finalCount),
	))
	p.metrics.transformDuration.Record(hdrCtx, hdrDur, metric.WithAttributes(
		attribute.String("claude_ds.transform.step", "header_pipeline"),
		attribute.Bool("claude_ds.transform.mutated", originalCount != finalCount),
	))
	hdrSpan.End()

	// Build the upstream URL. The Python proxy prepends UP_PATH_PREFIX
	// (the path component of UPSTREAM_BASE_URL) to the inbound path —
	// match that here. r.URL.Path is the raw inbound path.
	upstreamURL := *p.upstream
	upstreamURL.Path = strings.TrimRight(p.upstream.Path, "/") + r.URL.Path

	upstreamReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		rootSpan.RecordError(err)
		rootSpan.SetStatus(codes.Error, "build upstream request")
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	upstreamReq.Header = upstreamHeaders
	if upstreamReq.Header.Get("Host") == "" {
		upstreamReq.Host = p.upstream.Host
	}

	// Dispatch.
	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		rootSpan.RecordError(err)
		rootSpan.SetStatus(codes.Error, "upstream unreachable")
		p.metrics.upstreamError.Add(ctx, 1, metric.WithAttributes(
			endpointAttr(r.URL.Path),
			attribute.String("error.type", "upstream_unreachable"),
		))
		p.emitLog(ctx, otellog.SeverityWarn, "upstream.unreachable",
			otellog.String("claude_ds.upstream.host", p.upstream.Host),
			otellog.String("error.type", "upstream_unreachable"),
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Forward response — copy headers (less hop-by-hop + content-length),
	// add Connection: close (matches Python length-by-EOF framing).
	dst := w.Header()
	for name, values := range resp.Header {
		canon := name
		if isHopByHop(canon) || strings.EqualFold(canon, "Content-Length") {
			continue
		}
		for _, v := range values {
			dst.Add(canon, v)
		}
	}
	dst.Set("Connection", "close")
	w.WriteHeader(resp.StatusCode)

	// Stream the response body. http.client.stream span is custom per
	// the observability doc — TTFB recorded as both an event and as a
	// root-span attribute.
	streamCtx, streamSpan := p.tracer.Start(ctx, "http.client.stream")
	defer streamSpan.End()

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, upstreamReadChunkSize)
	var (
		streamStart       = time.Now()
		firstByte         time.Time
		firstByteRecorded bool
		totalBytes        int64
		chunks            int64
		clientDisconnect  bool
		disconnectCause   = "client_eof"
	)

streamLoop:
	for {
		// Honour client disconnect via context cancellation.
		select {
		case <-r.Context().Done():
			clientDisconnect = true
			disconnectCause = "client_cancel"
			break streamLoop
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if !firstByteRecorded {
				firstByte = time.Now()
				firstByteRecorded = true
				ttfbMs := firstByte.Sub(streamStart).Seconds() * 1000
				streamSpan.AddEvent("first_byte")
				rootSpan.SetAttributes(attribute.Float64("claude_ds.stream.ttfb_ms", ttfbMs))
				p.metrics.streamTTFB.Record(streamCtx, ttfbMs, metric.WithAttributes(
					endpointAttr(r.URL.Path),
					attribute.String("claude_ds.model.upstream", upstreamModelFromCtx(ctx)),
				))
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				clientDisconnect = true
				disconnectCause = "client_eof"
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			totalBytes += int64(n)
			chunks++
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			// Upstream-side error mid-stream.
			disconnectCause = "upstream_error"
			rootSpan.RecordError(readErr)
			break
		}
	}

	dur := time.Since(streamStart).Seconds() * 1000
	rootSpan.SetAttributes(
		attribute.Int64("claude_ds.stream.bytes", totalBytes),
		attribute.Int64("claude_ds.stream.chunks", chunks),
		attribute.Bool("claude_ds.stream.client_disconnected", clientDisconnect),
	)
	p.metrics.streamDuration.Record(streamCtx, dur, metric.WithAttributes(
		endpointAttr(r.URL.Path),
	))
	p.metrics.streamChunks.Record(streamCtx, chunks, metric.WithAttributes(
		endpointAttr(r.URL.Path),
	))
	p.metrics.streamBytes.Record(streamCtx, totalBytes, metric.WithAttributes(
		endpointAttr(r.URL.Path),
	))
	if clientDisconnect {
		p.metrics.streamClientDisconnects.Add(streamCtx, 1, metric.WithAttributes(
			endpointAttr(r.URL.Path),
			attribute.String("claude_ds.disconnect.cause", disconnectCause),
		))
		p.emitLog(streamCtx, otellog.SeverityInfo, "stream.client_closed",
			otellog.Int64("claude_ds.stream.bytes", totalBytes),
			otellog.Int64("claude_ds.stream.chunks", chunks),
			otellog.String("claude_ds.disconnect.cause", disconnectCause),
		)
	}
}

// upstreamModelFromCtx is a hook for retrieving the post-rewrite model
// id from the request context. Today it returns an empty string because
// the rewrite info doesn't propagate through context; CDS-23 will wire
// a context-key carrier when it integrates everything in main.go.
func upstreamModelFromCtx(_ context.Context) string {
	return ""
}

// headerCount reports the total number of header values across all
// names. Counts only — never the values themselves.
func headerCount(h http.Header) int {
	n := 0
	for _, values := range h {
		n += len(values)
	}
	return n
}

// emitLog wraps the OTel logs SDK so we keep redaction in one place.
// Body content / header values / API keys never reach this function;
// only counts, sizes, flags, and bounded-enum strings should be passed
// as attributes.
func (p *Proxy) emitLog(ctx context.Context, sev otellog.Severity, msg string, attrs ...otellog.KeyValue) {
	if p.logger == nil {
		return
	}
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(sev)
	rec.SetBody(otellog.StringValue(msg))
	rec.AddAttributes(attrs...)
	p.logger.Emit(ctx, rec)
}

// ---------- Phase-4 stubs ---------------------------------------------------
//
// rewriteBody is the real CDS-18 implementation — see rewrite.go.
// routeVision is the CDS-19 stub: it returns the body unchanged plus
// an empty VisionInfo (Routed=false). CDS-19 replaces it with vision
// routing; the signature is preserved so this file doesn't need to
// change again.

func routeVision(b []byte, _ *Config) ([]byte, VisionInfo, error) {
	return b, VisionInfo{}, nil
}

// looksLikeJSON is a cheap discriminator to avoid Unmarshal noise on
// non-JSON bodies. Mirrors the Python proxy's `try: json.loads ... except`.
func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}
