// Request rewriting for /v1/messages (CDS-18).
//
// This is the Go port of `_rewrite_body` from claude-ds-proxy.py. It
// implements the body-mutation pipeline that runs after the inbound
// request is read but before the header pipeline forwards it upstream:
//
//  1. file_id → base64. Walk every `messages[].content` block; for any
//     block with `source: {type: "file", file_id: "file_..."}`, look the
//     id up in the local Files API cache (CDS-16) and replace the source
//     with the cached `{type: base64, media_type, data}` triple. Misses
//     are left intact — the upstream returns its own error rather than
//     having the proxy fabricate one.
//
//  2. Wire-model rewrite. Read the body's `model` field. If it appears in
//     the per-cfg WireModelMap (built from `cfg.Model{Opus,Sonnet,Haiku,
//     SmallFast}` when unlock_auto_mode is on), substitute the upstream
//     id. This bridges Claude-canonical spoof ids ("claude-opus-4-7", …)
//     to real DeepSeek model ids — DeepSeek's compat shim silently
//     aliases unknown claude-* ids to its cheapest model (flash), and
//     the rewrite is what prevents that downgrade.
//
//  3. Wire-model catch-all. If the model still starts with "claude-"
//     after step 2 and `cfg.Model` (the resolved upstream model — the
//     launcher's `DEFAULT_UPSTREAM_MODEL`) is non-empty, rewrite to
//     `cfg.Model`. Defense-in-depth for new spoof ids the launcher
//     hasn't seen yet.
//
// Vision routing (step 4 of the design-spec pipeline) lives in CDS-19's
// `routeVision` and is invoked from proxy.go after this function
// returns. Effort spec resolution + ApplyRegime (step 5) is invoked by
// proxy.go's `runEffort` only when vision did not route — extended
// thinking is incompatible with the vision backends, per the design
// spec.
//
// OTLP attribute schema (per docs/superpowers/specs/2026-05-06-claude-ds-otlp-observability.md):
//
//   - claude_ds.transform.file_id_to_base64 span: `claude_ds.files.lookup.count`
//     and `claude_ds.files.substitutions` ints. Never the file_ids, never
//     the bytes, never the mime types.
//   - claude_ds.transform.wire_model span: `claude_ds.wire_model.from`,
//     `claude_ds.wire_model.to`, `claude_ds.wire_model.kind` ∈
//     {map, catchall, noop}. The model values are bounded by cfg.
//   - claude_ds.transform.effort span: `claude_ds.effort.bucket`,
//     `claude_ds.effort.regime` (closed enum),
//     `claude_ds.effort.previous_value`, plus `claude_ds.model.tier` ∈
//     {opus, sonnet, haiku, small_fast, other}.
//   - Counters: claude_ds.effort.regime.applied, claude_ds.wire_model.rewrite.count,
//     claude_ds.files.lookup.count (from files.go).
//
// Redaction is enforced by construction: this file never records
// message content, tool args/results, file ids, file bytes, or raw
// model names that include user data. Counts, outcomes, bounded enums
// only.
package main

import (
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------
// Wire-model mapping
// ---------------------------------------------------------------------

// claudeCanonicalOpus / Sonnet / Haiku are the spoof ids the launcher
// exports as ANTHROPIC_DEFAULT_*_MODEL when unlock_auto_mode is on. They
// satisfy claude code's auto-mode regex gate (^claude-(opus|sonnet)-4-6
// | ^claude-opus-4-7) without exposing a real DeepSeek id to the CLI;
// the proxy un-spoofs them at the body level here.
//
// The choice of these literals is a compatibility contract with the
// launcher (`./claude-ds`, lines ~1567-1571). Update both sites if the
// spoof ids ever change.
const (
	claudeCanonicalOpus   = "claude-opus-4-7"
	claudeCanonicalSonnet = "claude-sonnet-4-6"
	claudeCanonicalHaiku  = "claude-haiku-4-5"
)

// buildWireModelMap returns the spoof-id → upstream-id map for the
// running config. Empty when no per-tier model overrides exist (and
// cfg.Model is the only upstream); callers should treat an empty map as
// "no explicit rewrites", which still lets the catch-all fire.
//
// The order of `add` calls mirrors the launcher: small_fast is added
// first, haiku second, so haiku wins on the shared spoof id (`claude-
// haiku-4-5`). Sonnet/opus map to their own spoofs. Pairs where source
// equals destination are skipped to keep the noop kind accurate.
//
// Falls back to `cfg.Model` (the resolved upstream) when a per-tier
// override is unset, matching the launcher's `${model_*:-$resolved_model}`
// substitution.
func buildWireModelMap(cfg *Config) map[string]string {
	m := make(map[string]string, 3)
	if cfg == nil {
		return m
	}
	resolved := cfg.Model
	add := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		m[from] = to
	}
	pick := func(perTier string) string {
		if perTier != "" {
			return perTier
		}
		return resolved
	}
	add(claudeCanonicalHaiku, pick(cfg.ModelSmallFast))
	add(claudeCanonicalHaiku, pick(cfg.ModelHaiku)) // haiku wins on collision
	add(claudeCanonicalSonnet, pick(cfg.ModelSonnet))
	add(claudeCanonicalOpus, pick(cfg.ModelOpus))
	return m
}

// defaultUpstreamModel returns the catch-all target for unrecognised
// claude-* ids. Mirrors the launcher's `DEFAULT_UPSTREAM_MODEL=$resolved_model`.
// Empty cfg.Model disables the catch-all (defensive — the launcher
// always sets it, but config.go applies defaultModel on Load).
func defaultUpstreamModel(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Model
}

// ---------------------------------------------------------------------
// Effort spec resolution
// ---------------------------------------------------------------------

// modelTier classifies an upstream model id into a bounded enum. Used
// only as an OTLP span / counter attribute — `other` is the backstop
// for anything not matching a configured tier. Never includes raw model
// values that could leak.
//
// Tiers are derived from the cfg.Model{Opus,Sonnet,Haiku,SmallFast}
// fields. Collisions (multiple tiers sharing the same wire id) resolve
// in opus > sonnet > haiku > small_fast priority order, matching the
// launcher's last-write-wins ordering for EFFORT_MAP.
func modelTier(cfg *Config, upstream string) string {
	if cfg == nil || upstream == "" {
		return "other"
	}
	switch upstream {
	case cfg.ModelOpus:
		return "opus"
	case cfg.ModelSonnet:
		return "sonnet"
	case cfg.ModelHaiku:
		return "haiku"
	case cfg.ModelSmallFast:
		return "small_fast"
	}
	// Resolved-model fallback: when a per-tier override is unset, the
	// launcher's wire-model map points the spoof at cfg.Model. We don't
	// invent a tier in that case — `other` is the honest answer.
	return "other"
}

// effortSpecForModel returns the effort spec to apply for the given
// upstream model id, falling back to cfg.ProxyEffort (EFFORT_DEFAULT)
// when no per-tier override applies. An empty return value means
// "passthrough — do not rewrite this request".
//
// The lookup mirrors the launcher's EFFORT_MAP construction: tiers are
// added in priority order (small_fast → haiku → sonnet → opus) and
// later writes win on wire-id collision. Any spec that resolves to the
// literal "off" (case-insensitive) is treated as "drop from the map";
// the global default still applies if it's non-off.
func effortSpecForModel(cfg *Config, upstream string) string {
	if cfg == nil {
		return ""
	}
	pick := func(perTier string) (string, bool) {
		if perTier == "" {
			return "", false
		}
		if strings.EqualFold(strings.TrimSpace(perTier), "off") {
			return "", true // explicit override to off — passthrough
		}
		return perTier, true
	}
	// Build the map keyed by wire id. Order matches the launcher.
	pairs := []struct {
		wire string
		spec string
	}{
		{cfg.ModelSmallFast, cfg.ProxyEffortSmallFast},
		{cfg.ModelHaiku, cfg.ProxyEffortHaiku},
		{cfg.ModelSonnet, cfg.ProxyEffortSonnet},
		{cfg.ModelOpus, cfg.ProxyEffortOpus},
	}
	emap := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if p.wire == "" {
			continue
		}
		v, ok := pick(p.spec)
		if !ok {
			continue
		}
		emap[p.wire] = v
	}
	if v, ok := emap[upstream]; ok {
		// Empty here means "explicit off" — passthrough for this model.
		return v
	}
	return cfg.ProxyEffort
}

// ---------------------------------------------------------------------
// Image detection
// ---------------------------------------------------------------------

// hasImageContent reports whether the request body contains any image
// content blocks (direct or nested inside tool_result blocks). Mirrors
// the Python `_has_image_content`. Used by CDS-19 to decide whether to
// route to the vision model — the function is exposed here so unit
// tests can assert it without booting a proxy.
//
// Non-JSON bodies and missing/empty `messages` arrays return false.
func hasImageContent(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return false
	}
	return messagesContainImage(msgs)
}

// messagesContainImage walks a decoded messages array. Split out so
// rewriteBody can re-use it on the already-decoded body without paying
// for a second Unmarshal.
func messagesContainImage(messages []any) bool {
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "image":
				return true
			case "tool_result":
				inner, ok := block["content"].([]any)
				if !ok {
					continue
				}
				for _, nb := range inner {
					nblock, ok := nb.(map[string]any)
					if !ok {
						continue
					}
					if nblock["type"] == "image" {
						return true
					}
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------
// File-source rewriting
// ---------------------------------------------------------------------

// rewriteFileSources walks every content block and substitutes any
// `source.type == "file"` block with the cached inline base64 source.
// Mutates `messages` in place. Returns (lookups, hits) — `lookups` is
// the count of file_id references encountered, `hits` is the subset
// that were resolved.
//
// Misses are left intact: leaving the bad block lets the upstream
// generate a useful error message instead of the proxy fabricating
// one. The CDS-25 dashboards alert on the resulting lookup-miss
// counter.
func rewriteFileSources(messages []any) (lookups, hits int) {
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			src, ok := block["source"].(map[string]any)
			if !ok {
				continue
			}
			if src["type"] != "file" {
				continue
			}
			fileID, _ := src["file_id"].(string)
			if fileID == "" {
				// Malformed block — count as a lookup so the count
				// matches the dashboard's "file_id references seen"
				// semantics, but don't try to resolve.
				lookups++
				continue
			}
			lookups++
			data, mimeType, ok := LookupFile(fileID)
			if !ok {
				continue
			}
			block["source"] = map[string]any{
				"type":       "base64",
				"media_type": mimeType,
				"data":       data,
			}
			hits++
		}
	}
	return lookups, hits
}

// ---------------------------------------------------------------------
// Pipeline entrypoint
// ---------------------------------------------------------------------

// rewriteBody is the real implementation that replaces the Phase-3
// stub in proxy.go. The signature matches what proxy.go's `runRewrite`
// call site expects — `(body, cfg) → (body2, info, err)` — and the
// returned RewriteInfo populates the OTLP span / counter attributes
// the dashboard contract requires.
//
// Pipeline (steps 1, 2, 3 of the request-routing diagram in the design
// spec — vision routing and effort rewrite are subsequent steps in
// proxy.go):
//
//  1. file_id → base64 substitution (rewriteFileSources)
//  2. wire-model map lookup (buildWireModelMap)
//  3. wire-model catch-all (defaultUpstreamModel) for any unhandled
//     `claude-*` id
//
// Non-JSON bodies pass through untouched with a noop info — matches
// the Python `try/except json.loads` fallback. Empty bodies likewise.
//
// Mutation tracking: info.Mutated is true when any of the three steps
// changed the body. The caller (proxy.go) reflects this in the
// `claude_ds.transform.mutated` attribute on each child span.
func rewriteBody(body []byte, cfg *Config) ([]byte, RewriteInfo, error) {
	info := RewriteInfo{WireModelKind: "noop"}
	if len(body) == 0 || !looksLikeJSON(body) {
		return body, info, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		// Non-JSON / malformed JSON — passthrough (mirrors Python).
		return body, info, nil
	}

	// 1. file_id → base64.
	if msgs, ok := obj["messages"].([]any); ok {
		lookups, hits := rewriteFileSources(msgs)
		info.FileIDLookups = lookups
		info.FileIDHits = hits
		info.FileIDMisses = lookups - hits
	}

	// 2 + 3. wire-model rewrite.
	cur, _ := obj["model"].(string)
	info.ModelRequested = cur
	info.ModelUpstream = cur
	wireMap := buildWireModelMap(cfg)
	if mapped, ok := wireMap[cur]; ok && mapped != cur {
		obj["model"] = mapped
		info.ModelUpstream = mapped
		info.WireModelKind = "map"
	} else if strings.HasPrefix(cur, "claude-") {
		if def := defaultUpstreamModel(cfg); def != "" && def != cur {
			obj["model"] = def
			info.ModelUpstream = def
			info.WireModelKind = "catchall"
		}
	}

	mutated := info.FileIDHits > 0 || info.WireModelKind != "noop"
	info.Mutated = mutated
	if !mutated {
		// No structural change — preserve the input bytes byte-for-byte
		// so any unrelated whitespace / key ordering survives untouched.
		return body, info, nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		// Re-marshal failure is unrecoverable but extremely unlikely
		// for a body we just unmarshalled successfully. Pass the
		// original bytes through and surface the error so the caller
		// records it on the transform span.
		return body, info, err
	}
	return out, info, nil
}
