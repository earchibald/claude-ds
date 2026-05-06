package main

import (
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// ParseSpec — happy paths
// ---------------------------------------------------------------------

func TestParseSpec_PassthroughForms(t *testing.T) {
	t.Parallel()
	cases := []string{"", "off", "OFF", "  ", "  off  "}
	for _, raw := range cases {
		raw := raw
		t.Run("input="+raw, func(t *testing.T) {
			t.Parallel()
			r, err := ParseSpec(raw)
			if err != nil {
				t.Fatalf("ParseSpec(%q) returned error: %v", raw, err)
			}
			if r != nil {
				t.Fatalf("ParseSpec(%q) returned non-nil resolver; want nil for passthrough", raw)
			}
		})
	}
}

func TestParseSpec_ConstantLevels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec string
		want Regime
	}{
		{"none", RegimeNone},
		{"high", RegimeHigh},
		{"max", RegimeMax},
		{"NONE", RegimeNone},
		{"  High  ", RegimeHigh},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.spec, func(t *testing.T) {
			t.Parallel()
			r, err := ParseSpec(tc.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r == nil {
				t.Fatalf("expected resolver, got nil")
			}
			// Constant — every bucket maps to the same regime.
			for _, b := range []Bucket{BucketNone, BucketHigh, BucketMax} {
				if got := r(b); got != tc.want {
					t.Errorf("ParseSpec(%q)(bucket=%q) = %q; want %q", tc.spec, b, got, tc.want)
				}
			}
		})
	}
}

func TestParseSpec_AutoMirror(t *testing.T) {
	t.Parallel()
	r, err := ParseSpec("auto")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("expected resolver, got nil")
	}
	cases := []struct {
		bucket Bucket
		want   Regime
	}{
		{BucketNone, RegimeNone},
		{BucketHigh, RegimeHigh},
		{BucketMax, RegimeMax},
	}
	for _, tc := range cases {
		if got := r(tc.bucket); got != tc.want {
			t.Errorf("auto(bucket=%q) = %q; want %q", tc.bucket, got, tc.want)
		}
	}
	// An unknown bucket value falls through to RegimeOff.
	if got := r(Bucket("garbage")); got != RegimeOff {
		t.Errorf("auto(bucket=garbage) = %q; want %q", got, RegimeOff)
	}
}

func TestParseSpec_AutoWithFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec string
		// expected per bucket
		none, high, max Regime
	}{
		{"auto:high", RegimeHigh, RegimeHigh, RegimeMax},
		{"auto:max", RegimeMax, RegimeHigh, RegimeMax},
		// Degenerate: `auto:none` upgrades nothing — still mirrors.
		{"auto:none", RegimeNone, RegimeHigh, RegimeMax},
		// Case-insensitive.
		{"AUTO:HIGH", RegimeHigh, RegimeHigh, RegimeMax},
		// Whitespace tolerated around fallback.
		{"auto:  max  ", RegimeMax, RegimeHigh, RegimeMax},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.spec, func(t *testing.T) {
			t.Parallel()
			r, err := ParseSpec(tc.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r == nil {
				t.Fatal("expected resolver, got nil")
			}
			if got := r(BucketNone); got != tc.none {
				t.Errorf("none-bucket: got %q want %q", got, tc.none)
			}
			if got := r(BucketHigh); got != tc.high {
				t.Errorf("high-bucket: got %q want %q", got, tc.high)
			}
			if got := r(BucketMax); got != tc.max {
				t.Errorf("max-bucket: got %q want %q", got, tc.max)
			}
		})
	}
}

func TestParseSpec_Matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec string
		// expected per bucket; RegimeOff means "not in matrix" (passthrough).
		none, high, max Regime
	}{
		{
			name: "all-three-canonical-order",
			spec: "none=high|high=max|max=max",
			none: RegimeHigh, high: RegimeMax, max: RegimeMax,
		},
		{
			name: "reordered",
			spec: "max=high|none=max|high=none",
			none: RegimeMax, high: RegimeNone, max: RegimeHigh,
		},
		{
			name: "partial-only-none",
			spec: "none=high",
			none: RegimeHigh, high: RegimeOff, max: RegimeOff,
		},
		{
			name: "off-drops-clause",
			spec: "none=high|high=off|max=max",
			none: RegimeHigh, high: RegimeOff, max: RegimeMax,
		},
		{
			name: "case-and-whitespace",
			spec: "  None = High |  HIGH=max  ",
			none: RegimeHigh, high: RegimeMax, max: RegimeOff,
		},
		{
			name: "trailing-empty-clauses",
			spec: "none=high||",
			none: RegimeHigh, high: RegimeOff, max: RegimeOff,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := ParseSpec(tc.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r == nil {
				t.Fatal("expected resolver, got nil")
			}
			if got := r(BucketNone); got != tc.none {
				t.Errorf("none-bucket: got %q want %q", got, tc.none)
			}
			if got := r(BucketHigh); got != tc.high {
				t.Errorf("high-bucket: got %q want %q", got, tc.high)
			}
			if got := r(BucketMax); got != tc.max {
				t.Errorf("max-bucket: got %q want %q", got, tc.max)
			}
		})
	}
}

func TestParseSpec_MatrixAllOff_ReturnsNil(t *testing.T) {
	t.Parallel()
	// Every clause is an `off` — Python collapses this to `None`
	// (passthrough). We mirror that.
	r, err := ParseSpec("none=off|high=off|max=off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r != nil {
		t.Fatal("expected nil resolver for all-off matrix; got non-nil")
	}
}

// ---------------------------------------------------------------------
// ParseSpec — error paths
// ---------------------------------------------------------------------

func TestParseSpec_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		spec        string
		wantSubstr  string // substring that must appear in the error
	}{
		{
			name:       "auto-fallback-unknown",
			spec:       "auto:medium",
			wantSubstr: "auto: fallback",
		},
		{
			name:       "auto-fallback-empty",
			spec:       "auto:",
			wantSubstr: "auto: fallback",
		},
		{
			name:       "matrix-clause-no-equals",
			spec:       "none|high=max",
			wantSubstr: "missing '='",
		},
		{
			name:       "matrix-bucket-unknown",
			spec:       "low=high",
			wantSubstr: "matrix bucket",
		},
		{
			name:       "matrix-value-unknown",
			spec:       "none=medium",
			wantSubstr: "matrix value",
		},
		{
			name:       "matrix-mixed-good-and-bad",
			spec:       "none=high|max=ultra",
			wantSubstr: "matrix value",
		},
		{
			name:       "garbage-keyword",
			spec:       "elevenses",
			wantSubstr: "missing '='", // reaches matrix branch, fails on no '='
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := ParseSpec(tc.spec)
			if err == nil {
				t.Fatalf("expected error for %q; got resolver=%v", tc.spec, r)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// ---------------------------------------------------------------------
// BucketFromThinking
// ---------------------------------------------------------------------

func TestBucketFromThinking(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body map[string]any
		want Bucket
	}{
		{
			name: "absent",
			body: map[string]any{},
			want: BucketNone,
		},
		{
			name: "nil-thinking",
			body: map[string]any{"thinking": nil},
			want: BucketNone,
		},
		{
			name: "non-map-thinking",
			body: map[string]any{"thinking": "enabled"},
			want: BucketNone,
		},
		{
			name: "type-disabled",
			body: map[string]any{"thinking": map[string]any{"type": "disabled"}},
			want: BucketNone,
		},
		{
			name: "type-missing",
			body: map[string]any{"thinking": map[string]any{"budget_tokens": 50000}},
			want: BucketNone,
		},
		{
			name: "enabled-no-budget",
			// Python: budget_tokens defaults to 0 → 0 < 31000 → "high".
			body: map[string]any{"thinking": map[string]any{"type": "enabled"}},
			want: BucketHigh,
		},
		{
			name: "enabled-budget-zero",
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 0}},
			want: BucketHigh,
		},
		{
			name: "enabled-budget-low",
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 5000}},
			want: BucketHigh,
		},
		{
			name: "enabled-budget-just-below-threshold",
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 30999}},
			want: BucketHigh,
		},
		{
			name: "enabled-budget-at-threshold",
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 31000}},
			want: BucketMax,
		},
		{
			name: "enabled-budget-above-threshold",
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 64000}},
			want: BucketMax,
		},
		{
			name: "enabled-budget-float-decoded-from-json",
			// json.Unmarshal decodes numbers as float64.
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(31000)}},
			want: BucketMax,
		},
		{
			name: "enabled-budget-string-numeric",
			// Python int("31000") would parse cleanly → max.
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": "31000"}},
			want: BucketMax,
		},
		{
			name: "enabled-budget-string-nonnumeric",
			// Python's int("nope") raises → except branch returns "high".
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": "nope"}},
			want: BucketHigh,
		},
		{
			name: "enabled-budget-nil",
			// Python: int(thinking.get("budget_tokens", 0) or 0) → int(0) → 0 → high.
			body: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": nil}},
			want: BucketHigh,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BucketFromThinking(tc.body); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// ApplyRegime
// ---------------------------------------------------------------------

func TestApplyRegime_None(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"model":            "claude-opus-4-7",
		"thinking":         map[string]any{"type": "enabled", "budget_tokens": 12345},
		"reasoning_effort": "high",
		"messages":         []any{},
	}
	ApplyRegime(body, RegimeNone)
	if _, ok := body["thinking"]; ok {
		t.Errorf("RegimeNone should strip 'thinking'; still present: %v", body["thinking"])
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Errorf("RegimeNone should strip 'reasoning_effort'; still present: %v", body["reasoning_effort"])
	}
	// Unrelated fields preserved.
	if body["model"] != "claude-opus-4-7" {
		t.Errorf("RegimeNone should not touch unrelated fields; model=%v", body["model"])
	}
}

func TestApplyRegime_High_PreservesBudget(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"thinking":         map[string]any{"type": "enabled", "budget_tokens": 8000},
		"reasoning_effort": "low",
	}
	ApplyRegime(body, RegimeHigh)
	want := map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 8000},
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("RegimeHigh: got %#v, want %#v", body, want)
	}
}

func TestApplyRegime_High_NoExistingThinking(t *testing.T) {
	t.Parallel()
	body := map[string]any{"reasoning_effort": "medium"}
	ApplyRegime(body, RegimeHigh)
	want := map[string]any{
		"thinking": map[string]any{"type": "enabled"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("RegimeHigh w/o existing thinking: got %#v, want %#v", body, want)
	}
}

func TestApplyRegime_Max(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 64000},
	}
	ApplyRegime(body, RegimeMax)
	want := map[string]any{
		"thinking":         map[string]any{"type": "enabled", "budget_tokens": 64000},
		"reasoning_effort": "max",
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("RegimeMax: got %#v, want %#v", body, want)
	}
}

func TestApplyRegime_Max_NoExistingThinking(t *testing.T) {
	t.Parallel()
	body := map[string]any{}
	ApplyRegime(body, RegimeMax)
	want := map[string]any{
		"thinking":         map[string]any{"type": "enabled"},
		"reasoning_effort": "max",
	}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("RegimeMax fresh body: got %#v, want %#v", body, want)
	}
}

func TestApplyRegime_Off_NoOp(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"thinking":         map[string]any{"type": "enabled", "budget_tokens": 12345},
		"reasoning_effort": "high",
		"model":            "claude-opus-4-7",
	}
	clone := deepCloneMap(body)
	ApplyRegime(body, RegimeOff)
	if !reflect.DeepEqual(body, clone) {
		t.Errorf("RegimeOff should be a no-op; got %#v", body)
	}
}

func TestApplyRegime_NilBody_NoPanic(t *testing.T) {
	t.Parallel()
	// Should not panic.
	ApplyRegime(nil, RegimeMax)
}

func TestApplyRegime_Idempotent(t *testing.T) {
	t.Parallel()
	regimes := []Regime{RegimeNone, RegimeHigh, RegimeMax}
	for _, r := range regimes {
		r := r
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			body := map[string]any{
				"thinking":         map[string]any{"type": "enabled", "budget_tokens": 7777},
				"reasoning_effort": "low",
			}
			ApplyRegime(body, r)
			snapshot := deepCloneMap(body)
			ApplyRegime(body, r)
			if !reflect.DeepEqual(body, snapshot) {
				t.Errorf("regime %q is not idempotent\nfirst:  %#v\nsecond: %#v",
					r, snapshot, body)
			}
		})
	}
}

// ---------------------------------------------------------------------
// ParseMap
// ---------------------------------------------------------------------

func TestParseMap_Empty(t *testing.T) {
	t.Parallel()
	m, err := ParseMap("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Error("ParseMap(\"\") should return an empty (non-nil) map")
	}
	if len(m) != 0 {
		t.Errorf("ParseMap(\"\") got %d entries; want 0", len(m))
	}
}

func TestParseMap_Basic(t *testing.T) {
	t.Parallel()
	// Acceptance-criterion #2 from the issue.
	m, err := ParseMap("claude-opus-4-7=auto;claude-sonnet-4-6=high")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(m), m)
	}
	// claude-opus-4-7 → auto resolver: maps each bucket to itself.
	r1, ok := m["claude-opus-4-7"]
	if !ok || r1 == nil {
		t.Fatalf("missing or nil resolver for claude-opus-4-7")
	}
	if got := r1(BucketHigh); got != RegimeHigh {
		t.Errorf("claude-opus-4-7(auto)(high) = %q; want high", got)
	}
	if got := r1(BucketMax); got != RegimeMax {
		t.Errorf("claude-opus-4-7(auto)(max) = %q; want max", got)
	}
	if got := r1(BucketNone); got != RegimeNone {
		t.Errorf("claude-opus-4-7(auto)(none) = %q; want none", got)
	}
	// claude-sonnet-4-6 → constant high.
	r2, ok := m["claude-sonnet-4-6"]
	if !ok || r2 == nil {
		t.Fatalf("missing or nil resolver for claude-sonnet-4-6")
	}
	for _, b := range []Bucket{BucketNone, BucketHigh, BucketMax} {
		if got := r2(b); got != RegimeHigh {
			t.Errorf("claude-sonnet-4-6(high)(%q) = %q; want high", b, got)
		}
	}
}

func TestParseMap_OffYieldsNilResolver(t *testing.T) {
	t.Parallel()
	m, err := ParseMap("model-x=off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, ok := m["model-x"]
	if !ok {
		t.Fatal("model-x not in map")
	}
	if r != nil {
		t.Errorf("expected nil resolver for off-spec; got %v", r)
	}
}

func TestParseMap_SilentSkips(t *testing.T) {
	t.Parallel()
	// Empty clauses, no-equals clauses, empty model name — all silently dropped.
	m, err := ParseMap(";;model-a=high;=max;no-equals;;model-b=none")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := m["model-a"]; !ok {
		t.Error("model-a missing")
	}
	if _, ok := m["model-b"]; !ok {
		t.Error("model-b missing")
	}
	if len(m) != 2 {
		t.Errorf("expected 2 entries (silent skips), got %d: %v", len(m), m)
	}
}

func TestParseMap_PropagatesParseError(t *testing.T) {
	t.Parallel()
	_, err := ParseMap("model-a=high;model-b=auto:medium")
	if err == nil {
		t.Fatal("expected error from invalid auto: fallback")
	}
	if !strings.Contains(err.Error(), "model-b") {
		t.Errorf("error should reference offending model %q: %s", "model-b", err.Error())
	}
	if !strings.Contains(err.Error(), "auto: fallback") {
		t.Errorf("error should reference parse cause: %s", err.Error())
	}
}

func TestParseMap_AcceptsMatrixSpec(t *testing.T) {
	t.Parallel()
	m, err := ParseMap("alpha=none=high|high=max;beta=auto:max")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r, ok := m["alpha"]; !ok || r == nil {
		t.Fatal("alpha missing or nil")
	} else {
		if got := r(BucketNone); got != RegimeHigh {
			t.Errorf("alpha(none)=%q want high", got)
		}
		if got := r(BucketMax); got != RegimeOff {
			t.Errorf("alpha(max)=%q want off", got)
		}
	}
	if r, ok := m["beta"]; !ok || r == nil {
		t.Fatal("beta missing or nil")
	} else {
		if got := r(BucketNone); got != RegimeMax {
			t.Errorf("beta(none)=%q want max", got)
		}
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

// deepCloneMap is a shallow-recursive copy sufficient for the small
// JSON-shaped maps used in these tests.
func deepCloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		switch x := v.(type) {
		case map[string]any:
			out[k] = deepCloneMap(x)
		default:
			out[k] = v
		}
	}
	return out
}
