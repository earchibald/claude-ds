// Package main: dependency anchors.
//
// This file pins the external packages downstream phases (CDS-12, CDS-13,
// CDS-15, CDS-23) will import for OTLP observability and password input.
// Vendoring them now via blank imports lets `go mod tidy` keep them as
// direct requires in go.mod, so future agents can grep for the import
// path and find the version pin without spelunking through `// indirect`
// chains.
//
// Each blank import is paired with the CDS-NN issue that owns the real
// import (and concrete API usage). When that issue lands, replace the
// blank import with a normal one in the relevant file.
package main

import (
	// CDS-23 — Provider lifecycle in main.go.
	_ "go.opentelemetry.io/otel"
	_ "go.opentelemetry.io/otel/sdk"

	// CDS-23 — Provider construction (meter, tracer, logger).
	_ "go.opentelemetry.io/otel/sdk/log"
	_ "go.opentelemetry.io/otel/sdk/metric"

	// CDS-23 — OTLP/HTTP exporter trio. gRPC variants intentionally omitted.
	_ "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	_ "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	_ "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"

	// CDS-15 — Proxy instrumentation; otelhttp.NewHandler / otelhttp.NewTransport.
	_ "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	// CDS-13 — Password input via golang.org/x/term.ReadPassword.
	_ "golang.org/x/term"
)
