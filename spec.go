// Package claudeds is the Go rewrite of the Python claude-ds proxy. This
// file is the pure-logic effort-spec parser — a direct port of the
// _parse_spec / _bucket_from_thinking / _apply_regime / _parse_map
// functions in claude-ds-proxy.py. No I/O, no globals, table-test
// friendly.
//
// A "spec" is a small DSL describing how the proxy should rewrite the
// reasoning regime on outgoing requests:
//
//	off            — passthrough, do not rewrite (returned as a nil Resolver)
//	none|high|max  — constant: same regime for every source bucket
//	auto           — mirror the source bucket onto the regime
//	auto:<level>   — like auto, but the `none` source bucket is upgraded
//	                 to <level>; high/max buckets pass through
//	<bucket>=<v>|… — matrix; per-source-bucket regime, with `=off`
//	                 entries silently dropped from the matrix
//
// The parser is case-insensitive for keywords. Whitespace around clauses
// and around `=` is tolerated.
package claudeds

import (
	"fmt"
	"strings"
)

// Bucket is a source-side reasoning regime detected from the inbound
// `thinking` block. Buckets are the *rows* of the spec matrix.
type Bucket string

const (
	// BucketNone — no `thinking` block (or thinking disabled).
	BucketNone Bucket = "none"
	// BucketHigh — thinking enabled with a sub-ultrathink budget
	// (budget_tokens < 31000, or absent / unparseable).
	BucketHigh Bucket = "high"
	// BucketMax — thinking enabled with an ultrathink-tier budget
	// (budget_tokens >= 31000).
	BucketMax Bucket = "max"
)

// Regime is the target wire regime the proxy will impose on the
// outbound request. It is the *value* a Resolver returns for a given
// Bucket.
type Regime string

const (
	// RegimeOff is the "do nothing" sentinel. A spec that yields
	// RegimeOff for a given bucket means "leave the request alone".
	// This is distinct from RegimeNone (which actively strips the
	// thinking block).
	RegimeOff Regime = ""
	// RegimeNone — strip both `thinking` and `reasoning_effort`.
	RegimeNone Regime = "none"
	// RegimeHigh — ensure `thinking: {type: enabled}`, strip
	// `reasoning_effort`.
	RegimeHigh Regime = "high"
	// RegimeMax — ensure `thinking: {type: enabled}` and
	// `reasoning_effort: "max"`.
	RegimeMax Regime = "max"
)

// Resolver maps a source Bucket to the Regime the proxy should impose.
// A nil Resolver means "passthrough — no rewrite at all"; this is what
// ParseSpec returns for empty / `off` specs and for matrix specs that
// resolve to no entries.
type Resolver func(Bucket) Regime

// validLevels is the set of regime keywords accepted in spec strings.
// Note that RegimeOff is *not* a valid keyword on its own — `off` is
// only recognized at the top level (passthrough) or as a matrix value
// (which drops the entry).
var validLevels = map[string]Regime{
	"none": RegimeNone,
	"high": RegimeHigh,
	"max":  RegimeMax,
}

// validBuckets is the set of source-bucket keywords that may appear on
// the left side of a matrix clause.
var validBuckets = map[string]Bucket{
	"none": BucketNone,
	"high": BucketHigh,
	"max":  BucketMax,
}

// ParseSpec parses a spec string into a Resolver.
//
// Returns (nil, nil) when the spec disables injection unconditionally
// (empty string or `off`, or a matrix that resolves to no entries).
// Callers should treat a nil Resolver as "skip the rewrite for this
// model".
//
// Returns a non-nil error for syntactically invalid specs (unknown
// auto-fallback level, malformed matrix clause, unknown bucket key,
// non-level matrix value).
func ParseSpec(raw string) (Resolver, error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.EqualFold(s, "off") {
		return nil, nil
	}
	low := strings.ToLower(s)

	// Constant — same level for every source bucket.
	if regime, ok := validLevels[low]; ok {
		r := regime
		return func(_ Bucket) Regime { return r }, nil
	}

	// Bare `auto` — mirror the source bucket to the regime.
	if low == "auto" {
		return func(b Bucket) Regime {
			if r, ok := validLevels[string(b)]; ok {
				return r
			}
			return RegimeOff
		}, nil
	}

	// `auto:<level>` — upgrade the `none` source bucket to <level>;
	// other buckets mirror.
	if strings.HasPrefix(low, "auto:") {
		fallback := strings.TrimSpace(low[len("auto:"):])
		fb, ok := validLevels[fallback]
		if !ok {
			return nil, fmt.Errorf(
				"auto: fallback must be one of [high max none] (got %q)",
				fallback,
			)
		}
		return func(b Bucket) Regime {
			if b == BucketNone {
				return fb
			}
			if r, ok := validLevels[string(b)]; ok {
				return r
			}
			return RegimeOff
		}, nil
	}

	// Otherwise: matrix `bucket=val|bucket=val|...`.
	matrix := make(map[Bucket]Regime, 3)
	for _, clauseRaw := range strings.Split(s, "|") {
		clause := strings.TrimSpace(clauseRaw)
		if clause == "" {
			continue
		}
		if !strings.Contains(clause, "=") {
			return nil, fmt.Errorf("matrix clause missing '=': %q", clause)
		}
		eq := strings.IndexByte(clause, '=')
		k := strings.ToLower(strings.TrimSpace(clause[:eq]))
		v := strings.ToLower(strings.TrimSpace(clause[eq+1:]))
		bucket, ok := validBuckets[k]
		if !ok {
			return nil, fmt.Errorf(
				"matrix bucket must be one of [none high max] (got %q)",
				k,
			)
		}
		if v == "off" {
			// `off` removes the bucket from the matrix entirely
			// (passthrough for this bucket).
			continue
		}
		regime, ok := validLevels[v]
		if !ok {
			return nil, fmt.Errorf(
				"matrix value must be a level or 'off' (got %q)",
				v,
			)
		}
		matrix[bucket] = regime
	}
	if len(matrix) == 0 {
		// All clauses were `off` (or none provided). Match the Python
		// behavior: an empty matrix collapses to "no resolver".
		return nil, nil
	}
	// Snapshot the map so callers can't mutate it through aliasing.
	frozen := make(map[Bucket]Regime, len(matrix))
	for k, v := range matrix {
		frozen[k] = v
	}
	return func(b Bucket) Regime {
		if r, ok := frozen[b]; ok {
			return r
		}
		return RegimeOff
	}, nil
}

// BucketFromThinking inspects an Anthropic-style request body and
// returns the source bucket that classifies its `thinking` block.
//
// Behavior matches the Python _bucket_from_thinking exactly:
//   - missing or non-map `thinking` → BucketNone
//   - `thinking.type` != "enabled" → BucketNone
//   - enabled, `budget_tokens` >= 31000 → BucketMax
//   - enabled, otherwise (including absent / non-numeric / zero / negative
//     budget_tokens) → BucketHigh
//
// In particular, an enabled `thinking` block with a non-integer
// budget_tokens (e.g. a string) classifies as BucketHigh, not
// BucketNone — this preserves the Python try/except fallback.
func BucketFromThinking(body map[string]any) Bucket {
	raw, ok := body["thinking"]
	if !ok {
		return BucketNone
	}
	thinking, ok := raw.(map[string]any)
	if !ok {
		return BucketNone
	}
	if t, _ := thinking["type"].(string); t != "enabled" {
		return BucketNone
	}
	n, ok := budgetTokens(thinking["budget_tokens"])
	if !ok {
		// Unparseable budget — Python returns "high" via the
		// except branch.
		return BucketHigh
	}
	if n >= 31000 {
		return BucketMax
	}
	return BucketHigh
}

// budgetTokens coerces a JSON-decoded budget_tokens value into an int.
// JSON numbers decode as float64 by default; spec tests and live
// callers may also pass int / int64 / json.Number-style strings of
// digits. Anything else returns ok=false so the caller can fall back
// to the "high" classification (matching the Python except clause).
func budgetTokens(v any) (int, bool) {
	switch x := v.(type) {
	case nil:
		// Absent or explicit null → Python's `or 0` collapses to 0.
		return 0, true
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float32:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		// Python's int(x) accepts a decimal string. We're conservative
		// and only accept clean integer strings; anything else trips
		// the except branch (returns BucketHigh).
		if x == "" {
			return 0, true
		}
		var n int
		_, err := fmt.Sscanf(x, "%d", &n)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// ApplyRegime mutates `body` in place to match the target Regime.
//
//   - RegimeOff: no-op (callers should normally short-circuit before
//     calling, but we tolerate it).
//   - RegimeNone: strip both `thinking` and `reasoning_effort`.
//   - RegimeHigh: ensure `thinking: {type: enabled}` (preserving any
//     caller-supplied budget_tokens), strip `reasoning_effort`.
//   - RegimeMax: ensure `thinking: {type: enabled}` (preserving any
//     caller-supplied budget_tokens), set `reasoning_effort: "max"`.
//
// The function is idempotent — applying the same regime twice yields
// the same body as applying it once.
func ApplyRegime(body map[string]any, regime Regime) {
	if body == nil {
		return
	}
	switch regime {
	case RegimeOff:
		return
	case RegimeNone:
		delete(body, "thinking")
		delete(body, "reasoning_effort")
		return
	case RegimeHigh, RegimeMax:
		// Preserve budget_tokens if the caller supplied one.
		newThinking := map[string]any{"type": "enabled"}
		if existing, ok := body["thinking"].(map[string]any); ok {
			if budget, ok := existing["budget_tokens"]; ok {
				newThinking["budget_tokens"] = budget
			}
		}
		body["thinking"] = newThinking
		if regime == RegimeHigh {
			delete(body, "reasoning_effort")
		} else {
			body["reasoning_effort"] = "max"
		}
	}
}

// ParseMap parses a `model=spec;model=spec;...` string into a
// per-model resolver table.
//
// Empty input returns an empty (non-nil) map. Clauses without `=` or
// with an empty model name are silently skipped, matching the Python
// _parse_map. Per-clause spec parsing errors propagate.
func ParseMap(s string) (map[string]Resolver, error) {
	out := make(map[string]Resolver)
	if s == "" {
		return out, nil
	}
	for _, pairRaw := range strings.Split(s, ";") {
		pair := strings.TrimSpace(pairRaw)
		if pair == "" || !strings.Contains(pair, "=") {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		model := strings.TrimSpace(pair[:eq])
		rawSpec := pair[eq+1:]
		if model == "" {
			continue
		}
		resolver, err := ParseSpec(rawSpec)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", model, err)
		}
		out[model] = resolver
	}
	return out, nil
}
