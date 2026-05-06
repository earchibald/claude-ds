// Tests for the header pipeline. These mirror the Python proxy's
// _build_upstream_headers behaviour and exercise every code path:
// hop-by-hop strips, proxy-managed strips, configured strips,
// anthropic-beta filtering, env-driven config loading, mandatory
// injection, and — crucially — the redaction guarantee that DebugLog
// only ever receives header NAMES, never their values.
package main

import (
	"net/http"
	"sort"
	"strings"
	"testing"
)

func TestBuildUpstreamHeaders_HopByHopStripped(t *testing.T) {
	in := http.Header{
		"Connection":          {"keep-alive"},
		"Keep-Alive":          {"timeout=5"},
		"Transfer-Encoding":   {"chunked"},
		"TE":                  {"trailers"},
		"Trailer":             {"X-Foo"},
		"Upgrade":             {"websocket"},
		"Proxy-Authenticate":  {"Basic"},
		"Proxy-Authorization": {"Basic abc"},
		"X-Pass":              {"yes"},
	}
	out := BuildUpstreamHeaders(in, "api.example.com", 0, HeaderOpts{})

	for _, h := range []string{
		"Connection", "Keep-Alive", "Transfer-Encoding", "Te", "Trailer",
		"Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
	} {
		if _, ok := out[h]; ok {
			t.Errorf("hop-by-hop %q should be stripped, got %v", h, out[h])
		}
	}
	if got := out.Get("X-Pass"); got != "yes" {
		t.Errorf("X-Pass should pass through, got %q", got)
	}
}

func TestBuildUpstreamHeaders_HostAlwaysReplaced(t *testing.T) {
	in := http.Header{
		"Host": {"localhost:8000"},
	}
	out := BuildUpstreamHeaders(in, "api.anthropic.com", 0, HeaderOpts{})
	if got := out.Get("Host"); got != "api.anthropic.com" {
		t.Errorf("Host = %q, want api.anthropic.com", got)
	}
}

func TestBuildUpstreamHeaders_ContentLengthRecomputed(t *testing.T) {
	in := http.Header{
		"Content-Length": {"99"},
	}
	out := BuildUpstreamHeaders(in, "api.example.com", 42, HeaderOpts{})
	if got := out.Get("Content-Length"); got != "42" {
		t.Errorf("Content-Length = %q, want 42", got)
	}

	// bodyLen == 0 => no Content-Length injection.
	out2 := BuildUpstreamHeaders(in, "api.example.com", 0, HeaderOpts{})
	if got := out2.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length should be absent for bodyLen=0, got %q", got)
	}
}

func TestBuildUpstreamHeaders_AnthropicBeta(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // Get() result; "" means absent
	}{
		{"only files-api", "files-api", ""},
		{"files-api with version", "files-api/v1", ""},
		{"files-api in middle", "foo,files-api/v1,bar", "foo, bar"},
		{"no match passes through", "prompt-caching-2024-07-31", "prompt-caching-2024-07-31"},
		{"multi-keep", "foo, bar , baz", "foo, bar, baz"},
		{"all-filtered drops header", "files-api,files-api-v2", ""},
		{"case-insensitive substring", "Files-API/v1,keep", "keep"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := BuildUpstreamHeaders(
				http.Header{"Anthropic-Beta": {tc.in}},
				"h",
				0,
				HeaderOpts{},
			)
			got := out.Get("Anthropic-Beta")
			if got != tc.want {
				t.Errorf("anthropic-beta in=%q got=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildUpstreamHeaders_AnthropicBeta_MultiValueHeader(t *testing.T) {
	// http.Header allows multiple values per key. The pipeline must
	// flatten them with commas before filtering.
	in := http.Header{
		"Anthropic-Beta": {"files-api/v1", "prompt-caching"},
	}
	out := BuildUpstreamHeaders(in, "h", 0, HeaderOpts{})
	if got := out.Get("Anthropic-Beta"); got != "prompt-caching" {
		t.Errorf("multi-value beta got=%q want=prompt-caching", got)
	}
}

func TestBuildUpstreamHeaders_StripExtra(t *testing.T) {
	in := http.Header{
		"X-Drop":      {"yes"},
		"X-Also-Drop": {"yes"},
		"X-Keep":      {"please"},
	}
	out := BuildUpstreamHeaders(in, "h", 0, HeaderOpts{
		StripExtra: []string{"x-drop", "X-ALSO-DROP"}, // case-insensitive
	})
	if _, ok := out["X-Drop"]; ok {
		t.Error("X-Drop should be stripped")
	}
	if _, ok := out["X-Also-Drop"]; ok {
		t.Error("X-Also-Drop should be stripped")
	}
	if got := out.Get("X-Keep"); got != "please" {
		t.Errorf("X-Keep got=%q want=please", got)
	}
}

func TestBuildUpstreamHeaders_AddExtra(t *testing.T) {
	in := http.Header{
		"X-Existing": {"a"},
	}
	out := BuildUpstreamHeaders(in, "h", 0, HeaderOpts{
		AddExtra: []HeaderPair{
			{Name: "X-New", Value: "v1"},
			{Name: "X-Existing", Value: "b"}, // appends
			{Name: "X-Multi", Value: "m1"},
			{Name: "X-Multi", Value: "m2"},
		},
	})
	if got := out.Get("X-New"); got != "v1" {
		t.Errorf("X-New got=%q want=v1", got)
	}
	existing := out["X-Existing"]
	if len(existing) != 2 || existing[0] != "a" || existing[1] != "b" {
		t.Errorf("X-Existing got=%v want=[a b]", existing)
	}
	multi := out["X-Multi"]
	if len(multi) != 2 || multi[0] != "m1" || multi[1] != "m2" {
		t.Errorf("X-Multi got=%v want=[m1 m2]", multi)
	}
}

func TestLoadHeaderOptsFromEnv_StripHeaders(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"X-Foo", []string{"X-Foo"}},
		{"X-Foo,X-Bar", []string{"X-Foo", "X-Bar"}},
		{"X-Foo;X-Bar", []string{"X-Foo", "X-Bar"}},
		{"X-Foo, X-Bar ; X-Baz", []string{"X-Foo", "X-Bar", "X-Baz"}},
		{",,X-Only,,", []string{"X-Only"}},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("PROXY_STRIP_HEADERS", tc.raw)
			t.Setenv("PROXY_ADD_HEADERS", "")
			t.Setenv("PROXY_DEBUG", "")
			opts := LoadHeaderOptsFromEnv()
			if !equalSlice(opts.StripExtra, tc.want) {
				t.Errorf("StripExtra got=%v want=%v", opts.StripExtra, tc.want)
			}
		})
	}
}

func TestLoadHeaderOptsFromEnv_AddHeaders(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []HeaderPair
	}{
		{"empty", "", nil},
		{"single", "X-Foo: bar", []HeaderPair{{"X-Foo", "bar"}}},
		{"two", "X-Foo: bar; X-Baz: qux", []HeaderPair{{"X-Foo", "bar"}, {"X-Baz", "qux"}}},
		{"value with colon", "Authorization: Bearer abc:def",
			[]HeaderPair{{"Authorization", "Bearer abc:def"}}},
		{"empty value tolerated", "X-Empty:", []HeaderPair{{"X-Empty", ""}}},
		{"no colon entry skipped", "X-Foo: bar; nope; X-Baz: qux",
			[]HeaderPair{{"X-Foo", "bar"}, {"X-Baz", "qux"}}},
		{"empty name skipped", "X-Foo: bar; : nope; X-Baz: qux",
			[]HeaderPair{{"X-Foo", "bar"}, {"X-Baz", "qux"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROXY_STRIP_HEADERS", "")
			t.Setenv("PROXY_ADD_HEADERS", tc.raw)
			t.Setenv("PROXY_DEBUG", "")
			opts := LoadHeaderOptsFromEnv()
			if !equalPairs(opts.AddExtra, tc.want) {
				t.Errorf("AddExtra got=%v want=%v", opts.AddExtra, tc.want)
			}
		})
	}
}

func TestLoadHeaderOptsFromEnv_Debug(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"no":    false,
		"1":     true,
		"true":  true,
		"True":  true,
		"YES":   true,
		"on":    true,
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("PROXY_STRIP_HEADERS", "")
			t.Setenv("PROXY_ADD_HEADERS", "")
			t.Setenv("PROXY_DEBUG", raw)
			if got := LoadHeaderOptsFromEnv().Debug; got != want {
				t.Errorf("Debug for %q got=%v want=%v", raw, got, want)
			}
		})
	}
}

func TestBuildUpstreamHeaders_DebugRedactsValues(t *testing.T) {
	// The redaction guarantee: when Debug is on, DebugLog must never
	// see a header VALUE, only a NAME. We verify by feeding deliberately
	// secret-looking values and asserting no log line contains any of
	// them.
	const (
		apiKey      = "sk-ant-DEADBEEFSECRET"
		bearerToken = "Bearer SUPER-SECRET-TOKEN"
		oddBeta     = "files-api/v1,prompt-caching"
		stripVal    = "STRIPPED-SECRET-PLEASE-DO-NOT-LOG"
		passVal     = "PASS-SECRET-DO-NOT-LOG"
		hostHeader  = "evil.example.com:8080"
		injectVal   = "INJECTED-SECRET-DO-NOT-LOG"
	)
	in := http.Header{
		"X-Api-Key":      {apiKey},
		"Authorization":  {bearerToken},
		"Anthropic-Beta": {oddBeta},
		"X-Drop":         {stripVal},
		"X-Pass":         {passVal},
		"Connection":     {"keep-alive"},
		"Host":           {hostHeader},
	}
	var lines []string
	opts := HeaderOpts{
		StripExtra: []string{"X-Drop"},
		AddExtra:   []HeaderPair{{"X-Inject", injectVal}},
		Debug:      true,
		DebugLog:   func(s string) { lines = append(lines, s) },
	}
	BuildUpstreamHeaders(in, "api.upstream.test", 12, opts)

	if len(lines) == 0 {
		t.Fatal("Debug=true but DebugLog was never called")
	}

	forbidden := []string{
		apiKey, bearerToken, oddBeta, stripVal, passVal, hostHeader,
		injectVal, "DEADBEEF", "SUPER-SECRET",
		"keep-alive",       // hop-by-hop value
		"prompt-caching",   // surviving beta value
		"files-api",        // dropped beta value
		"api.upstream.test", // even the injected Host value
		"12",                // Content-Length value
	}
	for _, line := range lines {
		for _, secret := range forbidden {
			if strings.Contains(line, secret) {
				t.Errorf("debug line %q leaked value %q — redaction violated", line, secret)
			}
		}
	}

	// And positive: every header NAME we touched should appear in some line.
	wantNames := []string{
		"X-Api-Key", "Authorization", "Anthropic-Beta", "X-Drop", "X-Pass",
		"Connection", "Host", "X-Inject", "Content-Length",
	}
	joined := strings.Join(lines, "\n")
	for _, n := range wantNames {
		if !strings.Contains(joined, n) {
			t.Errorf("debug log missing decision for %q in:\n%s", n, joined)
		}
	}
}

func TestBuildUpstreamHeaders_DebugNilLoggerIsSafe(t *testing.T) {
	in := http.Header{"X-Foo": {"bar"}}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Debug=true + nil DebugLog should not panic, got %v", r)
		}
	}()
	BuildUpstreamHeaders(in, "h", 0, HeaderOpts{Debug: true})
}

func TestBuildUpstreamHeaders_PreservesMultiValueHeaders(t *testing.T) {
	in := http.Header{
		"X-Multi": {"a", "b", "c"},
	}
	out := BuildUpstreamHeaders(in, "h", 0, HeaderOpts{})
	got := append([]string{}, out["X-Multi"]...)
	sort.Strings(got)
	if !equalSlice(got, []string{"a", "b", "c"}) {
		t.Errorf("multi-value got=%v want=[a b c]", got)
	}
}

func TestBuildUpstreamHeaders_DoesNotMutateInput(t *testing.T) {
	in := http.Header{
		"Host":           {"original.host"},
		"X-Pass":         {"yes"},
		"Anthropic-Beta": {"files-api,prompt-caching"},
	}
	BuildUpstreamHeaders(in, "upstream.host", 5, HeaderOpts{
		StripExtra: []string{"X-Pass"},
		AddExtra:   []HeaderPair{{"X-Added", "v"}},
	})
	if got := in.Get("Host"); got != "original.host" {
		t.Errorf("input Host mutated, got %q", got)
	}
	if got := in.Get("X-Pass"); got != "yes" {
		t.Errorf("input X-Pass mutated, got %q", got)
	}
	if got := in.Get("Anthropic-Beta"); got != "files-api,prompt-caching" {
		t.Errorf("input Anthropic-Beta mutated, got %q", got)
	}
	if _, ok := in["X-Added"]; ok {
		t.Error("input gained X-Added — pipeline mutated caller's header map")
	}
}

// --- helpers ----------------------------------------------------------------

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalPairs(a, b []HeaderPair) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
