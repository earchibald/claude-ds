---
title: claude-ds OTLP observability — metrics, spans, logs
status: recommendations
date: 2026-05-06
related: [CDS-25, CDS-9, CDS-15, CDS-12]
---

# claude-ds OTLP observability — metrics, spans, logs

Recommendations doc for the OTLP integration delegated by **CDS-25**. Implementation lands in **CDS-12** (config keys), **CDS-9** (provider construction in `main.go`), and **CDS-15** (proxy instrumentation). Source of truth for the proxy's request lifecycle: `docs/superpowers/specs/2026-05-05-claude-ds-go-rewrite-design.md` ("Reasoning-effort proxy" section) and the Python reference at `claude-ds-proxy.py`.

## 1. Goals & non-goals

- **Consumer.** The user running `claude-ds`, plus a future debugging agent that reads metrics + traces + logs to root-cause regressions. Not multi-tenant, not a customer-facing SLO surface.
- **Must surface.** (a) DeepSeek upstream 5xx / unreachable; (b) slow time-to-first-byte on stream; (c) transform regressions (effort regime mis-applied, wire-model not rewritten, vision route missed); (d) file-cache miss when a `file_id` is referenced but not stored; (e) client disconnects mid-stream; (f) header pipeline stripping the wrong beta token.
- **Must enable.** RED dashboards on both incoming and upstream, a per-request span tree that shows which leg owns latency, and the ability to diff one slow trace against one fast trace and see the transform/network split at a glance.
- **Out of scope.** Per-token cost accounting (no body inspection beyond what the transforms already do). PII-bearing payloads (prompt text, tool args, file bytes) never leave the process as attribute or log values. No tracing of `--doctor` or `claude-ds-proxy` lifecycle test runs (gated via `deployment.environment=doctor`).
- **Single decision per area.** OTLP/HTTP exporter only — one fewer dep than gRPC, SigNoz accepts both, HTTP debugs more easily with `tcpdump`. Head-based sampling at 100%. JSON-encoded log records via the OTel logs SDK + OTLP/HTTP `/v1/logs`.

## 2. Resource attributes

Set on the OTel SDK Resource at boot, applied to every signal.

| Key | Value source | Why |
|---|---|---|
| `service.name` | constant `"claude-ds-proxy"` | SigNoz services view groups by this. |
| `service.version` | build-time `-ldflags -X main.version=...` | Pins a regression to a build. |
| `service.instance.id` | `uuid.NewString()` at boot | Distinguishes parallel proxy spawns (each `claude` session gets one). |
| `deployment.environment` | env `OTEL_DEPLOYMENT_ENVIRONMENT`, default `"local"`, forced to `"doctor"` when `CLAUDE_DS_DIAGNOSTIC_MODE=1` (set by both `--doctor` and `--setup`, plus any future synthetic-traffic surfaces) | Filters test runs out of dashboards. |
| `host.name` | `os.Hostname()` | Multi-machine fan-in (laptop + workstation). |
| `claude_ds.proxy.effort_default` | `EFFORT_DEFAULT` env at boot | A spec change is a config change; surface it without diffing logs. |
| `claude_ds.proxy.vision_model` | `VISION_MODEL` env at boot | Same reasoning — vision-route behaviour pivots on this. |
| `claude_ds.upstream.host` | parsed from `UPSTREAM_BASE_URL` | Pre-bakes upstream identity so dashboards filter cleanly. |
| `claude_ds.build.commit` | `-ldflags -X main.commit=...` | Cross-references with git. |

Skip `process.runtime.*` (Go runtime metrics live in a separate contrib package, opt in only) and `telemetry.sdk.*` (OTel adds these automatically).

## 3. Metric inventory

UCUM units throughout. Histogram bucket boundaries are explicit and global, configured via per-instrument views.

### Duration histogram bucket boundaries (ms)

```
[1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000]
```

Justification — transforms run sub-ms to ~5 ms (JSON re-marshal of message tree), upstream first-byte sits in the 200 ms – 2 s band, full streaming completions for max-effort runs reach 30–60 s. The 60 s and 120 s buckets catch the long tail without blowing cardinality. Below 1 ms is one bucket because nothing the proxy does is faster and worth resolving.

### Size histogram bucket boundaries (bytes)

```
[256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216]
```

Covers an empty-message ping (256 B) through a 16 MB image-bearing request.

### Incoming HTTP RED

| Name | Instrument | Unit | Attributes | Description |
|---|---|---|---|---|
| `http.server.request.count` | counter | `1` | `http.request.method`, `http.route`, `http.response.status_code`, `error.type` | Total inbound requests. |
| `http.server.active_requests` | up-down counter | `1` | `http.request.method`, `http.route` | In-flight requests. |
| `http.server.request.duration` | histogram | `ms` | as above | Wall-time from accept to last byte written. |
| `http.server.request.body.size` | histogram | `By` | `http.request.method`, `http.route` | Inbound body bytes. |
| `http.server.response.body.size` | histogram | `By` | `http.request.method`, `http.route`, `http.response.status_code` | Bytes written back to client. |

`http.route` is one of `/v1/messages`, `/v1/files`, `passthrough`. Never a raw path (cardinality cap).

### Upstream HTTP RED

| Name | Instrument | Unit | Attributes | Description |
|---|---|---|---|---|
| `http.client.request.count` | counter | `1` | `http.request.method`, `server.address`, `claude_ds.endpoint`, `claude_ds.model.upstream`, `http.response.status_code`, `error.type` | One per dispatched upstream request. |
| `http.client.request.duration` | histogram | `ms` | same | Upstream wall time (connect → last byte). |
| `http.client.connect.duration` | histogram | `ms` | `server.address` | Pure TCP+TLS dial; isolates network from upstream compute. |
| `http.client.response.body.size` | histogram | `By` | as count | Bytes received from upstream. |

`claude_ds.model.upstream` is the model **after** wire-model rewrite and vision routing. The pre-rewrite value belongs on the span, not the metric.

### Transform pipeline

| Name | Instrument | Unit | Attributes | Description |
|---|---|---|---|---|
| `claude_ds.transform.count` | counter | `1` | `claude_ds.transform.step`, `claude_ds.transform.mutated` (`true`/`false`), `error.type` | One per transform step invocation. |
| `claude_ds.transform.duration` | histogram | `ms` | `claude_ds.transform.step`, `claude_ds.transform.mutated` | Step latency. |
| `claude_ds.effort.regime.applied` | counter | `1` | `claude_ds.effort.regime` (closed enum: `none`/`high`/`max`/`passthrough`/`unknown`), `claude_ds.effort.bucket` (`none`/`high`/`max`), `claude_ds.model.upstream` | Distribution of regimes after resolution. Adding a new regime value requires a same-PR dashboard update; raw spec strings (`auto:7`, `matrix(...)`) live on logs only, never as a span/metric attribute. |
| `claude_ds.wire_model.rewrite.count` | counter | `1` | `claude_ds.wire_model.from`, `claude_ds.wire_model.to`, `claude_ds.wire_model.kind` (`map`/`catchall`/`noop`) | Catches silent-downgrade bugs. |
| `claude_ds.vision.route.count` | counter | `1` | `claude_ds.vision.routed` (`true`/`false`), `claude_ds.model.from`, `claude_ds.model.to` | Confirms image-bearing requests landed on the vision model. |
| `claude_ds.header.beta.stripped` | counter | `1` | `claude_ds.header.beta_value` | One bump per stripped beta token. |

`claude_ds.transform.step` ∈ `{file_id_to_base64, wire_model, vision_route, effort, header_pipeline}`. Fixed enum — no free strings.

### Streaming

| Name | Instrument | Unit | Attributes | Description |
|---|---|---|---|---|
| `claude_ds.stream.ttfb.duration` | histogram | `ms` | `claude_ds.model.upstream`, `claude_ds.endpoint` | Upstream-request-sent → first SSE byte forwarded. The single most useful UX metric. |
| `claude_ds.stream.duration` | histogram | `ms` | as above | First byte to last byte. |
| `claude_ds.stream.chunk.count` | histogram | `1` | as above | Chunk-count distribution per response. |
| `claude_ds.stream.bytes` | histogram | `By` | as above | Total streamed body bytes. |
| `claude_ds.stream.client_disconnect.count` | counter | `1` | `claude_ds.endpoint`, `claude_ds.disconnect.cause` (closed enum: `client_eof`/`client_cancel`/`upstream_error`/`timeout`/`unknown`) | Bumped when the relay write returns broken-pipe / reset. No upstream-model attribution (drill into traces filtered by `event.name = "stream.client_disconnect"` for per-model breakdown). |

### Files API mock

| Name | Instrument | Unit | Attributes | Description |
|---|---|---|---|---|
| `claude_ds.files.upload.count` | counter | `1` | `claude_ds.files.media_type` (top-level only — `image`, `application`, `text`) | Ingest rate. |
| `claude_ds.files.upload.size` | histogram | `By` | `claude_ds.files.media_type` | Per-upload size. |
| `claude_ds.files.cache.entries` | observable gauge | `1` | — | Live entry count. |
| `claude_ds.files.cache.bytes` | observable gauge | `By` | — | Sum of base64 payload sizes (post-encode, ~33% bloat — that's the actual memory cost). |
| `claude_ds.files.lookup.count` | counter | `1` | `claude_ds.files.lookup.outcome` (`hit`/`miss`) | One per `file_id` referenced inside `_rewrite_file_sources`. A `miss` is a real bug. |

Eviction counter is omitted while the cache is unbounded. Add `claude_ds.files.cache.evictions{reason}` if/when bounded.

### Errors

Errors ride on the existing counters via `error.type` ∈ `{upstream_unreachable, upstream_5xx, upstream_4xx, transform_invalid_json, transform_schema_mismatch, client_disconnect, timeout}`. No separate error-only metric.

### Lifecycle

| Name | Instrument | Unit | Attributes | Description |
|---|---|---|---|---|
| `claude_ds.proxy.start_time` | observable counter (set once via callback at registration) | `s` | — | Unix-seconds boot timestamp. Confirms a proxy was actually restarted between two debugging sessions. |

Skip Go runtime metrics for now. Opt in via `go.opentelemetry.io/contrib/instrumentation/runtime` if a leak is suspected — one line of wiring.

## 4. Span graph

Spans are recorded **always**, regardless of `PROXY_DEBUG`. Debug stays in logs. Sampling is 100%.

### POST /v1/messages (hot path)

```
http.server.request                                       (root, server kind)
├── claude_ds.transform.file_id_to_base64                 (internal)
├── claude_ds.transform.wire_model                        (internal)
├── claude_ds.transform.vision_route                      (internal)
├── claude_ds.transform.effort                            (internal)
├── claude_ds.transform.header_pipeline                   (internal)
├── http.client.request                                   (client kind)
│   ├── http.client.connect                               (internal)
│   ├── http.client.send                                  (internal — request bytes on wire)
│   └── http.client.stream                                (internal — first byte to last byte)
└── claude_ds.stream.relay                                (internal — proxy → client write loop)
```

`http.client.connect` and `http.client.send` come from the OTel `net/http` round-tripper instrumentation; do not hand-roll. `http.client.stream` is custom — started after `getresponse()`, ended on EOF or client disconnect, records TTFB as a span event named `first_byte` instead of yet another span. `claude_ds.stream.relay` overlaps `http.client.stream` deliberately: one measures upstream-side read latency, the other measures client-side write latency; a wedge between them implicates the local socket / Claude Code's reader.

### POST /v1/files (mocked, no upstream)

```
http.server.request
├── claude_ds.files.parse_multipart
└── claude_ds.files.cache_store
```

No `http.client.*` span. The absence is the signal — any `/v1/files` trace with an upstream span is a bug.

### Passthrough (everything else)

```
http.server.request
├── claude_ds.transform.header_pipeline
├── http.client.request
│   ├── http.client.connect
│   ├── http.client.send
│   └── http.client.stream
└── claude_ds.stream.relay
```

No body-rewrite spans — fewer children = trivially distinguishable from `/v1/messages` in the SigNoz trace explorer.

## 5. Span attributes & semconv mapping

### HTTP semconv (OTel stable, GA semantic conventions)

On `http.server.request`:

| Attribute | Value |
|---|---|
| `http.request.method` | `POST`/`GET`/etc. |
| `url.path` | `/v1/messages` etc. — no query string. |
| `url.scheme` | `http`. |
| `server.address` | `127.0.0.1`. |
| `server.port` | actual bound port. |
| `network.protocol.version` | `1.1`. |
| `http.response.status_code` | int. |
| `http.route` | `/v1/messages`, `/v1/files`, `passthrough`. |
| `error.type` | one enum value from §3. |
| `user_agent.original` | from `User-Agent`. Useful for distinguishing claude CLI versions. |

On `http.client.request`:

| Attribute | Value |
|---|---|
| `http.request.method`, `http.response.status_code`, `network.protocol.version`, `error.type` | as above. |
| `server.address` | upstream host (e.g. `api.deepseek.com`). |
| `server.port` | 443. |
| `url.full` | omit — the path is interesting; the full URL is redundant with `server.address`. |
| `url.template` | upstream path prefix + claude path. |

### claude-ds-specific span attributes

On the root `http.server.request` for `/v1/messages`:

| Attribute | Description |
|---|---|
| `claude_ds.model.requested` | model id as it arrived from claude (pre-rewrite). |
| `claude_ds.model.upstream` | model id sent on the wire (post-rewrite + vision). |
| `claude_ds.effort.bucket` | `none`/`high`/`max` from `_bucket_from_thinking`. |
| `claude_ds.effort.regime` | resolved regime (closed enum: `none`/`high`/`max`/`passthrough`/`unknown`). |
| `claude_ds.vision.routed` | bool. |
| `claude_ds.files.lookup.hits` / `claude_ds.files.lookup.misses` | int counts during this request. |
| `claude_ds.stream.ttfb_ms` | numeric duplicate of the histogram contribution; embedded so a single trace tells you TTFB without cross-referencing metrics. |
| `claude_ds.stream.bytes` | total streamed bytes. |
| `claude_ds.stream.chunks` | chunk count. |
| `claude_ds.stream.client_disconnected` | bool. |

On each `claude_ds.transform.*` span:

| Attribute | Description |
|---|---|
| `claude_ds.transform.step` | enum (matches metric attribute). |
| `claude_ds.transform.mutated` | `true` if the body changed; `false` for a no-op pass-through. |
| `claude_ds.transform.error` | error string only when failed; otherwise absent. |

Step-specific extras: `wire_model` adds `claude_ds.wire_model.from` / `.to` / `.kind`. `vision_route` adds `claude_ds.vision.images_collected` (int). `effort` adds `claude_ds.effort.regime`, `claude_ds.effort.bucket`, `claude_ds.effort.previous_value` (the prior `reasoning_effort` field, or `<absent>`). `file_id_to_base64` adds `claude_ds.files.substitutions` (int).

### Redaction rules (load-bearing)

The implementer MUST NOT record any of the following as a span attribute, metric attribute, or log field:

- The request or response body, in whole or in fragments.
- Any `messages[].content` text, tool-use args, or tool-result content.
- The `Authorization` or `x-api-key` header value, in any form (including length, hash, or first-N chars). Header *names* are fine; values are not.
- `file_id` strings on metrics. They're not secrets but they're high cardinality and only useful inside a single trace; put them on a span event if needed.
- File contents, base64 or otherwise.

Span attributes that summarise these (counts, sizes, mutation flags) are fine. The enforcement rule is: no payload content escapes the process.

## 6. Logs

| Signal | When | Example |
|---|---|---|
| Metric | Continuous quantitative state. | `http.client.request.duration` for every upstream call. |
| Span | Per-request causal structure with attributes summarising what happened. | `claude_ds.transform.effort` with `regime=high`. |
| Log | Discrete events that don't fit a counter and aren't worth a child span. | `vision_route: image detected, model deepseek-chat → deepseek-chat (no swap)`. |

Use the OTel logs SDK with the OTLP/HTTP logs exporter pointing at the same SigNoz endpoint. Keep a JSON-encoded stderr fallback so existing humans-watch-stderr workflows still work. Every log record auto-includes `trace_id` and `span_id` when emitted inside a span context (the OTel logs bridge handles this — do not reinvent).

`PROXY_DEBUG=1` flips the level threshold to `DEBUG`. Default is `INFO`. The previous Python `_log` calls map to `DEBUG`; new structured events introduced for OTel use `INFO` only when an operator should see them without opting in (e.g. `proxy.boot`, `upstream.unreachable`).

## 7. Sampling & cardinality discipline

- **Sampling.** `ParentBased(AlwaysOn)` at 100%. Single user, low QPS (typically one in-flight request, peak ~4 with parallel tools). Storage cost is negligible; debug value is the whole point.
- **Cardinality caps.**
  - `claude_ds.model.upstream` is bounded (a handful of DeepSeek models). `claude_ds.model.requested` is bounded by the closed set claude CLI emits.
  - `claude_ds.wire_model.from` / `.to` are bounded by the user's `WIRE_MODEL_MAP`. Validate at boot that the map has ≤ 32 entries; refuse to start otherwise.
  - **Never** put trace ids, span ids, request ids, `file_id`s, or `User-Agent` strings on metric attributes — they belong on spans.
  - `http.route` is the bounded 3-value enum, never a raw path.
- **Bucketing for reasoning_effort.** Use the closed enum (`none`, `high`, `max`, `passthrough`). If a future code path computes a fifth value, the metric attribute writer maps it to `"other"` and a log warning fires.

## 8. Failure mode → signal map

| Failure | Metric | Span / attribute | Log |
|---|---|---|---|
| DeepSeek upstream 5xx | `http.client.request.count{http.response.status_code=5xx}` ↑, `http.server.request.count{http.response.status_code=502}` ↑ | `http.client.request` span: `error.type=upstream_5xx`, `http.response.status_code=5xx`. | `INFO upstream.error status=… host=…`. |
| DeepSeek unreachable (DNS/TCP/TLS) | `http.client.request.count{error.type=upstream_unreachable}` ↑, `http.client.connect.duration` p99 spike or absent. | `http.client.connect` span: `error.type` set, status code unset. | `WARN upstream.unreachable cause=…`. |
| Slow first byte | `claude_ds.stream.ttfb.duration` p95 ↑. | `claude_ds.stream.ttfb_ms` on root span; `first_byte` event on `http.client.stream`. | none — read from the trace. |
| Client disconnect mid-stream | `claude_ds.stream.client_disconnect.count` ↑. | Root span: `claude_ds.stream.client_disconnected=true`; `claude_ds.stream.relay` ends early. | `INFO stream.client_closed bytes=… chunks=…`. |
| Vision mis-routed (image present, not routed) | `claude_ds.vision.route.count{routed=false}` ↑ relative to `claude_ds.files.upload.count{media_type=image}`. | Root: `claude_ds.vision.routed=false` while `claude_ds.transform.vision_route` has `images_collected>0`. | `WARN vision.skipped images=… model=…`. |
| Wire-model not rewritten / silent downgrade | `claude_ds.wire_model.rewrite.count{kind=noop}` ↑ when `model.requested` matches a known claude-* canonical id. | Root: `model.requested ≠ model.upstream` is the *good* case; equal-and-claude-* is the bug. | `WARN wire_model.unmapped requested=…`. |
| Files cache miss on `file_id` reference | `claude_ds.files.lookup.count{outcome=miss}` ↑. | `claude_ds.transform.file_id_to_base64`: `claude_ds.files.substitutions` lower than `source.file_id` count; child event `cache_miss file_id=…`. | `WARN files.lookup_miss file_id=…`. |
| Transform invalid JSON | `claude_ds.transform.count{step=…, error.type=transform_invalid_json}` ↑. | Transform span: `error.type` + `claude_ds.transform.error`. | `WARN transform.parse_error step=…`. |
| Header pipeline regression (extra beta stripped) | `claude_ds.header.beta.stripped{value=…}` rate change vs baseline. | `claude_ds.transform.header_pipeline`: `claude_ds.header.beta_values_stripped=[…]`. | none — metric is enough. |

## 9. SigNoz-specific notes

- **Exporter.** OTLP/HTTP, base URL `http://signoz.local:30318` (verified reachable from this workstation; receiver accepts traces, logs, metrics with the standard OTel `partialSuccess` envelope; no auth required on the self-hosted cluster). Three signal paths (`/v1/traces`, `/v1/metrics`, `/v1/logs`) on the same exporter. Default protobuf encoding.
- **Future managed-SigNoz path.** The exporter MUST read `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS` from the environment, with values resolvable through the existing secretref system (`op://`, `system://`, `infisical://`, plaintext). Concretely: parse `OTEL_EXPORTER_OTLP_HEADERS` after secretref resolution so `signoz-access-token=op://home-kubernetes/SigNoz access token (home cluster)/credential` works. The token value must be loaded into a single env var at process start, never echoed.
- **SigNoz views that work without custom config.**
  - **Services**: `claude-ds-proxy` appears with RED stats from `http.server.request.*`.
  - **Service map**: edge to `api.deepseek.com` rendered from `http.client.*` semconv (SigNoz draws the edge from `server.address` on client spans).
  - **Traces explorer**: filter `service.name=claude-ds-proxy`, group by `claude_ds.effort.regime` to compare regimes.
  - **Dashboards**: a 6-panel "claude-ds proxy" board (incoming p95, upstream p95, TTFB p95, transform-step p95 stacked, error-class breakdown, files cache size) is mechanical from the metric names above.
  - **Alerts**: thresholds on `http.client.request.count{error.type=upstream_unreachable}` rate > 0 over 5 min; `claude_ds.files.lookup.count{outcome=miss}` rate > 0 over 5 min; `claude_ds.stream.ttfb.duration` p95 > 5 s over 10 min.

## 10. Resolved decisions

Settled in the CDS-25 review session (2026-05-06). These are load-bearing — CDS-12 / CDS-15 / CDS-23 quote them.

- **Provider lifecycle on shutdown — synchronous drain, 1 s ceiling.** `MeterProvider`/`TracerProvider`/`LoggerProvider` constructed in `main.go` before the proxy goroutine launches; `Shutdown(ctx)` invoked via `defer` with `context.WithTimeout(ctx, 1*time.Second)`. To make the typical-case fast, configure `BatchSpanProcessor.BatchTimeout = 250 ms` and `MaxExportBatchSize = 256` so the in-memory buffer at exit holds at most ~250 ms of spans — typical loopback drain completes in <100 ms (imperceptible to users). The 1 s ceiling exists for the failure mode (SigNoz unreachable / slow), not the common case. True async drain (detached subprocess, sidecar collector) was considered and rejected as overengineering for a single-user dev tool; revisit if we ever export to a remote backend with >50 ms RTT.
- **Diagnostic mode env var — `CLAUDE_DS_DIAGNOSTIC_MODE=1` set by both `--doctor` and `--setup`.** Either entry point sets the env var before constructing OTel providers; the `Resource` then forces `deployment.environment=doctor`. Reusing the existing `--doctor` vocabulary (rather than introducing a third name like `diagnostic`) keeps dashboard filters readable. Any future synthetic-traffic surface (e.g. `--proxy-test`) opts in by setting the same env var.
- **Effort regime cardinality — closed enum, expansion requires dashboard PR.** `claude_ds.effort.regime` ∈ `{none, high, max, passthrough, unknown}`. `unknown` is a permanent backstop for "ID we shipped without updating the enum" — alert on it. New regime values require a same-PR update to SigNoz dashboards (the enum is a public observability contract). Parametric spec details (`auto:7`, `matrix(...)`) live on log lines only — never as span or metric attributes — so the indexed cardinality stays bounded.
- **Disconnect metric attribution — endpoint + cause, no model.** `claude_ds.stream.client_disconnect.count` is attributed by `{claude_ds.endpoint, claude_ds.disconnect.cause}`. `disconnect.cause` is a closed enum `{client_eof, client_cancel, upstream_error, timeout, unknown}` — 5 values, real diagnostic value. Upstream-model attribution is deliberately absent: the metric answers "is the disconnect rate creeping up?" (an aggregate question), and per-model breakdowns are available on demand by drilling into traces filtered by `event.name = "stream.client_disconnect"`. The metric stays cheap; the deep-dive lives on traces.
- **Wrapper-level span — skip.** No session-wide `claude_ds.session` parent span. The wrapper exits when `claude` does, so a wrapper span would last hours of user think-time and would re-parent every request trace, breaking the "one root trace per request" invariant SigNoz relies on. Specific short-lived spans for individual operations (proxy spawn, doctor checks, config load) are fine when CDS-23 deems them useful, but no wrapping parent. If end-to-end "click to first byte" timing is ever wanted, ship it as a one-shot `claude_ds.startup.ms` histogram metric — not a span.
- **Transport — `otlp_protocol={http,grpc}`, default `http`, both always vendored.** OTLP/HTTP is the default (easier to debug on loopback with `tcpdump -A`; thinner dependency surface). gRPC is opt-in via the `otlp_protocol` config key or the OTel-standard `OTEL_EXPORTER_OTLP_PROTOCOL` env var (env wins on conflict, matching SDK convention). Both protocol exporter sets (`otlptracehttp`/`otlpmetrichttp`/`otlploghttp` and the `*grpc` siblings) are always vendored — no build tags. Binary-size cost is trivial (~3 MB); build-tag complexity is not. Endpoint format differs by protocol (HTTP wants `http://signoz.local:30318` with paths appended by the SDK; gRPC wants `signoz.local:30317` plus `WithInsecure()`); CDS-12 validates the `otlp_endpoints` shape per protocol.
