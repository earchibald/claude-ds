// Header pipeline — the Go port of the Python proxy's
// _build_upstream_headers / _load_add_headers / _load_strip_headers and
// the constant header sets that drive them.
//
// Semantics (matched verbatim against claude-ds-proxy.py):
//
//  1. Hop-by-hop headers (RFC 7230 §6.1) are always stripped.
//  2. Proxy-managed headers (Host, Content-Length) are always stripped
//     from the inbound side and re-injected with proxy-correct values.
//  3. Caller-supplied PROXY_STRIP_HEADERS adds extra strips.
//  4. The `anthropic-beta` header is comma-split; any field whose
//     lower-cased text contains a substring from the strip list
//     (currently "files-api") is dropped, and the remainder is rejoined.
//  5. PROXY_ADD_HEADERS lets the operator inject arbitrary upstream
//     headers (semicolon-separated `Name: value` pairs).
//  6. When Debug is on, every per-header decision is logged via
//     DebugLog. NAMES ONLY, never values — header values may contain
//     API keys or other secrets.
//
// Pure stdlib. No globals — config is loaded into a HeaderOpts at the
// edge and passed in.
package main

import (
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
)

// hopByHop is the canonicalised set of HTTP/1.1 hop-by-hop headers.
// Stored in canonical-MIME form so a single map lookup against
// textproto.CanonicalMIMEHeaderKey is enough.
var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {}, // canonical form of "TE"
	"Trailer":             {},
	"Trailers":            {}, // some clients use the plural; matches Python's _HOP_BY_HOP
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// proxyManaged headers are always stripped from the inbound side; the
// pipeline re-injects them with proxy-correct values at the end.
var proxyManaged = map[string]struct{}{
	"Host":           {},
	"Content-Length": {},
}

// stripAnthropicBetaSubstrings — any anthropic-beta field value whose
// lower-cased text contains one of these substrings is dropped. The
// proxy implements files-api locally, so the upstream never needs to
// see the corresponding beta token.
var stripAnthropicBetaSubstrings = []string{"files-api"}

// HeaderOpts is the runtime configuration for BuildUpstreamHeaders.
//
// StripExtra is a list of header names (case-insensitive) sourced from
// PROXY_STRIP_HEADERS or callers that want a per-request override.
//
// AddExtra is the ordered list of (Name, Value) pairs to inject into
// every upstream request, sourced from PROXY_ADD_HEADERS. We use a
// slice rather than a map so callers can preserve duplicate-name pairs
// and ordering matters in tests.
//
// Debug toggles the per-header decision log. DebugLog receives the
// formatted line. If DebugLog is nil and Debug is true, the pipeline
// silently no-ops the logging — no panic.
type HeaderOpts struct {
	StripExtra []string
	AddExtra   []HeaderPair
	Debug      bool
	DebugLog   func(string)
}

// HeaderPair is an ordered (name, value) tuple — used for AddExtra so
// duplicate names and insertion order are preserved.
type HeaderPair struct {
	Name  string
	Value string
}

// HeaderStats reports what the header pipeline actually did. Callers use
// this to drive the OTLP `claude_ds.transform.mutated` attribute (true
// iff anything was stripped or injected) and to feed the
// `claude_ds.header.beta.stripped` counter (count of anthropic-beta
// tokens dropped — never the values themselves).
type HeaderStats struct {
	// StripCount is the number of inbound header values dropped:
	// hop-by-hop strips, proxy-managed strips, configured strips, and
	// anthropic-beta tokens removed. A header that is dropped entirely
	// counts each of its values; a beta header that loses N tokens
	// contributes N here.
	StripCount int

	// InjectCount is the number of header values written into the
	// outbound map by the pipeline itself (Host, Content-Length, every
	// AddExtra entry). Pass-through values do not count.
	InjectCount int

	// BetaTokensStripped is the count of comma-separated anthropic-beta
	// tokens removed by filterAnthropicBeta (subset of StripCount).
	// Drives the `claude_ds.header.beta.stripped` counter.
	BetaTokensStripped int
}

// LoadHeaderOptsFromEnv reads PROXY_STRIP_HEADERS, PROXY_ADD_HEADERS and
// PROXY_DEBUG out of the environment and returns a populated HeaderOpts.
//
// PROXY_STRIP_HEADERS: comma OR semicolon-separated header names.
// PROXY_ADD_HEADERS:   semicolon-separated `Name: value` pairs. Entries
// without a colon are skipped (matches Python).
// PROXY_DEBUG:         truthy ("1", "true", "yes", case-insensitive)
// turns on Debug; when on, DebugLog is left nil — the caller wires it up
// to the proxy's actual logger.
func LoadHeaderOptsFromEnv() HeaderOpts {
	opts := HeaderOpts{}

	if raw := strings.TrimSpace(os.Getenv("PROXY_STRIP_HEADERS")); raw != "" {
		// Match Python: replace ';' with ',' then split on ','.
		normalised := strings.ReplaceAll(raw, ";", ",")
		for _, name := range strings.Split(normalised, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				opts.StripExtra = append(opts.StripExtra, name)
			}
		}
	}

	if raw := strings.TrimSpace(os.Getenv("PROXY_ADD_HEADERS")); raw != "" {
		for _, entry := range strings.Split(raw, ";") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			idx := strings.Index(entry, ":")
			if idx < 0 {
				continue // no colon — skip (matches Python partition fallthrough)
			}
			name := strings.TrimSpace(entry[:idx])
			value := strings.TrimSpace(entry[idx+1:])
			if name == "" {
				continue
			}
			opts.AddExtra = append(opts.AddExtra, HeaderPair{Name: name, Value: value})
		}
	}

	if raw := strings.TrimSpace(os.Getenv("PROXY_DEBUG")); raw != "" {
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "on":
			opts.Debug = true
		}
	}

	return opts
}

// BuildUpstreamHeaders applies the full header pipeline to the headers
// of an incoming request and returns the headers to send upstream.
//
// upstreamHost is the value to set Host to. bodyLen, if > 0, is the
// length of the body that will be sent — Content-Length is injected
// only when bodyLen > 0 (matches Python: `if body:`).
//
// The returned http.Header is freshly allocated. The caller may mutate
// it without affecting `in`. The HeaderStats return summarises what the
// pipeline did so callers can drive OTLP attributes / counters without
// re-walking the maps.
func BuildUpstreamHeaders(in http.Header, upstreamHost string, bodyLen int, opts HeaderOpts) (http.Header, HeaderStats) {
	out := make(http.Header, len(in)+len(opts.AddExtra)+2)
	var stats HeaderStats

	// Pre-canonicalise the configured strip list once.
	extraStrip := make(map[string]struct{}, len(opts.StripExtra))
	for _, name := range opts.StripExtra {
		extraStrip[textproto.CanonicalMIMEHeaderKey(name)] = struct{}{}
	}

	logf := func(decision, name string) {
		if !opts.Debug || opts.DebugLog == nil {
			return
		}
		// NAME ONLY — never the value. This is the redaction
		// guarantee the test suite verifies.
		opts.DebugLog(decision + " " + name)
	}

	// Walk inbound headers in stable iteration order. http.Header is a
	// map[string][]string so iteration order is unspecified; that's fine
	// for the pipeline (the response is a map too) — tests assert on
	// final contents, not log ordering.
	for name, values := range in {
		canon := textproto.CanonicalMIMEHeaderKey(name)

		switch {
		case isHopByHop(canon):
			logf("strip (hop-by-hop)", canon)
			stats.StripCount += len(values)
			continue
		case isProxyManaged(canon):
			logf("strip (proxy-managed)", canon)
			stats.StripCount += len(values)
			continue
		}
		if _, ok := extraStrip[canon]; ok {
			logf("strip (configured)", canon)
			stats.StripCount += len(values)
			continue
		}

		if canon == "Anthropic-Beta" {
			// Combine all values, comma-split, filter, rejoin.
			joined := strings.Join(values, ",")
			kept, dropped := filterAnthropicBeta(joined)
			stats.BetaTokensStripped += dropped
			stats.StripCount += dropped
			if kept == "" {
				logf("strip (all filtered) anthropic-beta", canon)
				continue
			}
			logf("rewrite (filtered beta)", canon)
			// Single-string set — the rejoined form.
			out[canon] = []string{kept}
			continue
		}

		// Pass through — preserve all values.
		logf("pass", canon)
		out[canon] = append(out[canon], values...)
	}

	// Inject mandatory proxy-side headers. Host is always re-injected.
	if upstreamHost != "" {
		out["Host"] = []string{upstreamHost}
		stats.InjectCount++
		logf("inject", "Host")
	}
	if bodyLen > 0 {
		out["Content-Length"] = []string{strconv.Itoa(bodyLen)}
		stats.InjectCount++
		logf("inject", "Content-Length")
	}

	// Inject configured extras. Order matters; duplicate names append.
	for _, p := range opts.AddExtra {
		canon := textproto.CanonicalMIMEHeaderKey(p.Name)
		out[canon] = append(out[canon], p.Value)
		stats.InjectCount++
		logf("inject (configured)", canon)
	}

	return out, stats
}

// isHopByHop reports whether canon (already canonical-MIME form) is one
// of the HTTP/1.1 hop-by-hop headers.
func isHopByHop(canon string) bool {
	_, ok := hopByHop[canon]
	return ok
}

// isProxyManaged reports whether canon is one of the headers the proxy
// always recomputes (Host, Content-Length).
func isProxyManaged(canon string) bool {
	_, ok := proxyManaged[canon]
	return ok
}

// filterAnthropicBeta splits joined on commas, drops fields whose
// lower-cased text contains any of stripAnthropicBetaSubstrings, and
// rejoins the survivors with ", " (matches Python's `", ".join(kept)`).
// Returns "" if every field was filtered, plus the count of fields
// dropped — the count drives the `claude_ds.header.beta.stripped`
// counter without ever exposing the values themselves.
func filterAnthropicBeta(joined string) (string, int) {
	fields := strings.Split(joined, ",")
	kept := make([]string, 0, len(fields))
	dropped := 0
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		lower := strings.ToLower(f)
		drop := false
		for _, bad := range stripAnthropicBetaSubstrings {
			if strings.Contains(lower, bad) {
				drop = true
				break
			}
		}
		if drop {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, ", "), dropped
}
