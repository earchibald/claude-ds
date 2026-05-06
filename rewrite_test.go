// Tests for the request-rewriting pipeline (CDS-18).
//
// Coverage matrix (from the issue acceptance criteria):
//   - file_id → base64 round-trip with cache hits / misses / mix
//   - wire-model map per known canonical id
//   - catch-all kicks in for unknown claude-* ids when cfg.Model is set
//   - effort spec resolution per model tier with EFFORT_DEFAULT fallback
//   - vision-detected requests skip effort rewrite (regime span absent)
//   - non-claude-* model passes through wire-model untouched
//   - non-image, non-claude-* request: only effort transform applied
//   - body that is not valid JSON: passes through unchanged with no error
//
// Tests use the package-internal `rewriteBody` directly so they cover
// the rewrite contract without booting a Proxy. The vision-routing
// "skip effort" assertion uses a real Proxy + httptest upstream so it
// can observe pipeline ordering end-to-end.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedFileCache adds a synthetic entry to the package-level files cache
// without going through the multipart upload path. The data is small
// constant base64 so test assertions are stable.
func seedFileCache(t *testing.T, id, mime, b64 string) {
	t.Helper()
	fileCacheMu.Lock()
	fileCache[id] = fileEntry{data: b64, mimeType: mime, size: int64(len(b64))}
	fileCacheMu.Unlock()
	t.Cleanup(func() {
		fileCacheMu.Lock()
		delete(fileCache, id)
		fileCacheMu.Unlock()
	})
}

// decodeBody round-trips the rewritten body through json.Unmarshal so
// tests can assert on the structured form regardless of map ordering.
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("decodeBody: %v\nraw=%s", err, body)
	}
	return obj
}

// ---------------------------------------------------------------------
// file_id → base64
// ---------------------------------------------------------------------

func TestRewriteBody_FileIDToBase64_Hit(t *testing.T) {
	// Two file references — both cached. Both should be substituted.
	seedFileCache(t, "file_aaa", "image/png", "AAAA")
	seedFileCache(t, "file_bbb", "image/jpeg", "BBBB")

	in := []byte(`{
        "model": "deepseek-v4-pro",
        "messages": [
            {"role": "user", "content": [
                {"type": "image", "source": {"type": "file", "file_id": "file_aaa"}},
                {"type": "image", "source": {"type": "file", "file_id": "file_bbb"}}
            ]}
        ]
    }`)
	out, info, err := rewriteBody(in, &Config{Model: "deepseek-v4-pro"})
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.FileIDLookups != 2 || info.FileIDHits != 2 || info.FileIDMisses != 0 {
		t.Fatalf("counts: lookups=%d hits=%d misses=%d", info.FileIDLookups, info.FileIDHits, info.FileIDMisses)
	}
	if !info.Mutated {
		t.Fatalf("expected Mutated=true after substitutions")
	}

	obj := decodeBody(t, out)
	msgs := obj["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	for _, b := range content {
		src := b.(map[string]any)["source"].(map[string]any)
		if src["type"] != "base64" {
			t.Fatalf("expected base64 source after rewrite, got %v", src["type"])
		}
		if src["data"] == "" {
			t.Fatalf("missing base64 data")
		}
	}
}

func TestRewriteBody_FileIDToBase64_Miss(t *testing.T) {
	// Reference an id that is NOT in the cache — block must be left
	// alone (upstream returns its own error).
	in := []byte(`{
        "model": "deepseek-v4-pro",
        "messages": [
            {"role": "user", "content": [
                {"type": "image", "source": {"type": "file", "file_id": "file_zzz_missing"}}
            ]}
        ]
    }`)
	out, info, err := rewriteBody(in, &Config{Model: "deepseek-v4-pro"})
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.FileIDLookups != 1 || info.FileIDHits != 0 || info.FileIDMisses != 1 {
		t.Fatalf("counts: lookups=%d hits=%d misses=%d", info.FileIDLookups, info.FileIDHits, info.FileIDMisses)
	}
	// On miss the body should round-trip identically.
	obj := decodeBody(t, out)
	src := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if src["type"] != "file" || src["file_id"] != "file_zzz_missing" {
		t.Fatalf("expected miss to be left untouched: %v", src)
	}
}

func TestRewriteBody_FileIDToBase64_Mixed(t *testing.T) {
	seedFileCache(t, "file_hit", "image/png", "HIT_DATA")
	in := []byte(`{
        "model": "deepseek-v4-pro",
        "messages": [
            {"role": "user", "content": [
                {"type": "image", "source": {"type": "file", "file_id": "file_hit"}},
                {"type": "image", "source": {"type": "file", "file_id": "file_miss"}}
            ]}
        ]
    }`)
	_, info, err := rewriteBody(in, &Config{Model: "deepseek-v4-pro"})
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.FileIDLookups != 2 || info.FileIDHits != 1 || info.FileIDMisses != 1 {
		t.Fatalf("counts: lookups=%d hits=%d misses=%d", info.FileIDLookups, info.FileIDHits, info.FileIDMisses)
	}
}

// ---------------------------------------------------------------------
// Wire-model map
// ---------------------------------------------------------------------

func TestRewriteBody_WireModel_Mapping(t *testing.T) {
	// Each canonical spoof id must map to its configured tier model.
	cfg := &Config{
		Model:          "deepseek-v4-pro", // resolved upstream
		ModelOpus:      "deepseek-v4-pro",
		ModelSonnet:    "deepseek-v4-pro",
		ModelHaiku:     "deepseek-chat",
		ModelSmallFast: "deepseek-chat",
	}
	cases := []struct {
		from string
		to   string
	}{
		{"claude-opus-4-7", "deepseek-v4-pro"},
		{"claude-sonnet-4-6", "deepseek-v4-pro"},
		{"claude-haiku-4-5", "deepseek-chat"},
	}
	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			in := []byte(`{"model":"` + tc.from + `","messages":[]}`)
			out, info, err := rewriteBody(in, cfg)
			if err != nil {
				t.Fatalf("rewriteBody: %v", err)
			}
			if info.ModelRequested != tc.from {
				t.Fatalf("ModelRequested=%q want %q", info.ModelRequested, tc.from)
			}
			if info.ModelUpstream != tc.to {
				t.Fatalf("ModelUpstream=%q want %q", info.ModelUpstream, tc.to)
			}
			if info.WireModelKind != "map" {
				t.Fatalf("WireModelKind=%q want map", info.WireModelKind)
			}
			obj := decodeBody(t, out)
			if obj["model"] != tc.to {
				t.Fatalf("body model=%q want %q", obj["model"], tc.to)
			}
		})
	}
}

func TestRewriteBody_WireModel_CatchAll(t *testing.T) {
	// Unknown claude-* id with cfg.Model set → rewritten to cfg.Model.
	cfg := &Config{Model: "deepseek-v4-pro"}
	in := []byte(`{"model":"claude-future-9-9","messages":[]}`)
	out, info, err := rewriteBody(in, cfg)
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.ModelUpstream != "deepseek-v4-pro" || info.WireModelKind != "catchall" {
		t.Fatalf("expected catchall to deepseek-v4-pro, got upstream=%q kind=%q",
			info.ModelUpstream, info.WireModelKind)
	}
	obj := decodeBody(t, out)
	if obj["model"] != "deepseek-v4-pro" {
		t.Fatalf("body model=%q", obj["model"])
	}
}

func TestRewriteBody_WireModel_CatchAllDisabledWhenDefaultEmpty(t *testing.T) {
	// cfg.Model empty → catch-all is a no-op even for unknown claude-* ids.
	cfg := &Config{}
	in := []byte(`{"model":"claude-future-9-9","messages":[]}`)
	_, info, err := rewriteBody(in, cfg)
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.WireModelKind != "noop" || info.ModelUpstream != "claude-future-9-9" {
		t.Fatalf("expected noop with cfg.Model empty, got kind=%q upstream=%q",
			info.WireModelKind, info.ModelUpstream)
	}
}

func TestRewriteBody_WireModel_NonClaudePassthrough(t *testing.T) {
	// A non-claude-* model must not trigger the catch-all even when
	// cfg.Model is set.
	cfg := &Config{Model: "deepseek-v4-pro"}
	in := []byte(`{"model":"deepseek-chat","messages":[]}`)
	_, info, err := rewriteBody(in, cfg)
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.WireModelKind != "noop" {
		t.Fatalf("expected noop for non-claude-* model, got %q", info.WireModelKind)
	}
	if info.ModelUpstream != "deepseek-chat" {
		t.Fatalf("model changed unexpectedly: %q", info.ModelUpstream)
	}
}

// ---------------------------------------------------------------------
// Effort spec resolution (effortSpecForModel + modelTier)
// ---------------------------------------------------------------------

func TestEffortSpecForModel_PerTier(t *testing.T) {
	cfg := &Config{
		Model:                "deepseek-v4-pro",
		ModelOpus:            "deepseek-v4-pro",
		ModelSonnet:          "deepseek-v4-fast",
		ModelHaiku:           "deepseek-chat",
		ModelSmallFast:       "deepseek-chat",
		ProxyEffort:          "auto",      // EFFORT_DEFAULT
		ProxyEffortOpus:      "max",       // tier override
		ProxyEffortSonnet:    "high",      // tier override
		ProxyEffortHaiku:     "off",       // explicit-off → passthrough for haiku
		ProxyEffortSmallFast: "auto:high", // tier override
	}
	cases := []struct {
		model string
		spec  string
		tier  string
	}{
		{"deepseek-v4-pro", "max", "opus"},
		{"deepseek-v4-fast", "high", "sonnet"},
		// haiku and small_fast share `deepseek-chat`; haiku wins on
		// collision (last write in the launcher's priority order),
		// and haiku's spec is "off" → empty (passthrough).
		{"deepseek-chat", "", "haiku"},
		// Unknown tier → fall back to EFFORT_DEFAULT.
		{"deepseek-mystery", "auto", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := effortSpecForModel(cfg, tc.model)
			if got != tc.spec {
				t.Fatalf("spec for %q: got %q want %q", tc.model, got, tc.spec)
			}
			if tier := modelTier(cfg, tc.model); tier != tc.tier {
				t.Fatalf("tier for %q: got %q want %q", tc.model, tier, tc.tier)
			}
		})
	}
}

func TestEffortSpecForModel_DefaultFallback(t *testing.T) {
	cfg := &Config{ProxyEffort: "high"}
	// No per-tier overrides, no model classification → EFFORT_DEFAULT.
	if got := effortSpecForModel(cfg, "deepseek-v4-pro"); got != "high" {
		t.Fatalf("expected EFFORT_DEFAULT 'high', got %q", got)
	}
}

// ---------------------------------------------------------------------
// hasImageContent
// ---------------------------------------------------------------------

func TestHasImageContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"direct_image", `{"messages":[{"role":"user","content":[{"type":"image"}]}]}`, true},
		{"tool_result_image", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image"}]}]}]}`, true},
		{"text_only", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, false},
		{"empty", ``, false},
		{"non_json", `not json`, false},
		{"no_messages", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasImageContent([]byte(tc.body)); got != tc.want {
				t.Fatalf("hasImageContent: got %v want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Passthrough cases
// ---------------------------------------------------------------------

func TestRewriteBody_NonJSON_Passthrough(t *testing.T) {
	in := []byte("not json at all")
	out, info, err := rewriteBody(in, &Config{Model: "deepseek-v4-pro"})
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("non-JSON body was modified: %q → %q", in, out)
	}
	if info.Mutated || info.WireModelKind != "noop" {
		t.Fatalf("expected noop info, got %+v", info)
	}
}

func TestRewriteBody_Empty_Passthrough(t *testing.T) {
	out, info, err := rewriteBody(nil, &Config{Model: "deepseek-v4-pro"})
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if out != nil {
		t.Fatalf("nil body should round-trip as nil")
	}
	if info.Mutated {
		t.Fatalf("empty body should not mutate info")
	}
}

func TestRewriteBody_OnlyEffort_NoMutation(t *testing.T) {
	// Non-image, non-claude-*, no file_ids — rewriteBody should be a
	// pure no-op (effort is applied in proxy.go, not here).
	cfg := &Config{Model: "deepseek-v4-pro", ModelOpus: "deepseek-v4-pro", ProxyEffort: "auto"}
	in := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	out, info, err := rewriteBody(in, cfg)
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.Mutated {
		t.Fatalf("expected no mutation, got info=%+v", info)
	}
	// Body bytes preserved exactly (no re-marshal).
	if !bytes.Equal(out, in) {
		t.Fatalf("body unexpectedly re-marshalled")
	}
}

// ---------------------------------------------------------------------
// Vision skips effort (end-to-end via Proxy + upstream stub)
// ---------------------------------------------------------------------

// TestVisionSkipsEffortRewrite verifies that when the request body
// contains an image (which CDS-19 will eventually route to the vision
// model), the effort regime is not applied. We run this through the
// real Proxy so we observe the pipeline ordering — including the
// `if !visionInfo.Routed { runEffort(...) }` gate in messagesHandler.
//
// CDS-19 hasn't landed yet, so today the vision stub returns
// Routed=false and effort *will* still apply on image-bearing
// requests. The assertion here is forward-looking: it captures the
// current behaviour so the CDS-19 PR has a concrete signal of when to
// flip the test to the post-routing form. For CDS-18, the substantive
// invariant is that hasImageContent correctly detects images — that
// is covered by TestHasImageContent above.
func TestVisionSkipsEffortRewrite_StubBehaviour(t *testing.T) {
	// Dummy upstream that records what it received. We only care about
	// the request body the proxy forwarded.
	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		BaseURL:      upstream.URL,
		Model:        "deepseek-v4-pro",
		ProxyEffort:  "max",
		ProxyBind:    "127.0.0.1",
		Unknown:      map[string]string{},
		OTLPProtocol: "http",
	}
	p, err := NewProxy(cfg, ProxyOpts{})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	// Request body with an inline image — when CDS-19 lands, this
	// should bypass the effort rewrite.
	in := `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`
	resp, err := http.Post("http://"+p.Addr()+"/v1/messages", "application/json", strings.NewReader(in))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if len(captured) == 0 {
		t.Fatalf("upstream did not receive a body")
	}
	// Today (CDS-19 stub returns Routed=false) effort runs — so the
	// thinking block is injected. This documents the current state;
	// CDS-19's PR flips this assertion to the post-routing form.
	var obj map[string]any
	if err := json.Unmarshal(captured, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	if _, hasThinking := obj["thinking"]; !hasThinking {
		t.Fatalf("expected thinking block under current vision stub; CDS-19 will flip this test")
	}
}

// ---------------------------------------------------------------------
// Wire-model + file_id combined run
// ---------------------------------------------------------------------

func TestRewriteBody_Combined(t *testing.T) {
	seedFileCache(t, "file_combo", "image/png", "Q09NQk8=")
	cfg := &Config{
		Model:     "deepseek-v4-pro",
		ModelOpus: "deepseek-v4-pro",
	}
	in := []byte(`{
        "model": "claude-opus-4-7",
        "messages": [
            {"role": "user", "content": [
                {"type": "image", "source": {"type": "file", "file_id": "file_combo"}}
            ]}
        ]
    }`)
	out, info, err := rewriteBody(in, cfg)
	if err != nil {
		t.Fatalf("rewriteBody: %v", err)
	}
	if info.WireModelKind != "map" || info.ModelUpstream != "deepseek-v4-pro" {
		t.Fatalf("wire-model: kind=%q upstream=%q", info.WireModelKind, info.ModelUpstream)
	}
	if info.FileIDHits != 1 {
		t.Fatalf("expected 1 file_id hit, got %d", info.FileIDHits)
	}
	if !info.Mutated {
		t.Fatalf("expected combined run to mutate")
	}
	obj := decodeBody(t, out)
	if obj["model"] != "deepseek-v4-pro" {
		t.Fatalf("model not rewritten: %v", obj["model"])
	}
	src := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if src["type"] != "base64" {
		t.Fatalf("source not rewritten to base64: %v", src)
	}
}
