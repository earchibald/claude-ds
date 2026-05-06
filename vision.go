// Vision routing for /v1/messages (CDS-19).
//
// This is the Go port of `_normalize_for_vision` from claude-ds-proxy.py
// (and the surrounding `if VISION_MODEL and _has_image_content(messages)`
// gate in `_rewrite_body`). It is invoked by proxy.go's `runVision`
// wrapper *after* CDS-18's rewriteBody has settled the wire-model id and
// *before* the effort regime is applied — proxy.go skips the effort
// rewrite entirely when this function returns Routed=true, because
// DeepSeek's vision backends do not support extended thinking.
//
// Pipeline (mirrors the Python source line-for-line):
//
//  1. JSON-decode the body. Non-JSON or non-object → passthrough with
//     Routed=false. No error returned: `_rewrite_body` in Python catches
//     the json.loads exception and passes the body through, so the proxy
//     never refuses a request just because it's not JSON.
//
//  2. Gate on cfg.VisionModel and hasImageContent. Empty VisionModel
//     disables routing entirely (matches `VISION_MODEL=''` in Python:
//     line 776 of claude-ds-proxy.py); no images means there's nothing
//     to consolidate. Either short-circuit returns the input bytes
//     untouched.
//
//  3. Override the `model` field on the request body to cfg.VisionModel.
//
//  4. Strip the top-level `tools` and `tool_choice` keys — DeepSeek
//     rejects Anthropic-only tool schemas (Python line 640).
//
//  5. Walk every message:
//
//      a. Assistant turns: convert each `tool_use` block to a plain-text
//         description `[used <name> tool: k1='v1', k2='v2', ...]`. The
//         Python source uses `repr()` on each value via the `!r` format
//         specifier; we match that with a Go-side equivalent that wraps
//         strings in single quotes and renders other types via fmt.Sprintf
//         with %v. Blocks that aren't `tool_use` are preserved verbatim.
//
//      b. User turns that aren't the last user turn: collect any image
//         blocks (direct or nested in `tool_result.content`), replace
//         each with `[image — in current turn]`, and flatten any text
//         blocks out of `tool_result` wrappers.
//
//      c. The last user turn: collect images (direct or nested in
//         `tool_result.content`), flatten any text out of `tool_result`,
//         and prepend the collected images so DeepSeek processes them.
//
//  6. Re-serialize the body and return Routed=true with the counts the
//     OTLP transform span and counter need.
//
// OTLP attribute schema (per docs/superpowers/specs/2026-05-06-claude-ds-otlp-observability.md):
//
//   - claude_ds.transform.vision_route span: routed (bool), images.count
//     (the consolidated total), tool_use.converted_count, tools_dropped /
//     tool_choice_dropped (bool), claude_ds.model.from / .to (bounded
//     by cfg).
//   - Counter claude_ds.vision.route.count{model.to, routed} — fired by
//     proxy.go's `runVision` from the VisionInfo we return.
//
// Redaction is enforced by construction: this file never records image
// bytes, image URLs, tool argument values, message text, file ids, or
// any other user data on the wire. Only counts, booleans, and the model
// name (a closed enum bounded by cfg.VisionModel) reach OTLP.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

// imagePlaceholder is the text block that replaces an image block in
// non-last user turns. Matches Python `_normalize_for_vision` line 675.
const imagePlaceholder = "[image — in current turn]"

// routeVision is the real implementation that replaces the Phase-3 stub
// formerly in proxy.go. The signature matches what proxy.go's runVision
// expects: `(body, cfg) → (out, info, err)`.
//
// Errors only surface for catastrophic re-marshal failures of a body we
// just successfully unmarshalled — practically unreachable, but plumbed
// so the caller can record the failure on the transform span instead of
// silently corrupting the request.
func routeVision(body []byte, cfg *Config) ([]byte, VisionInfo, error) {
	info := VisionInfo{}
	if len(body) == 0 || !looksLikeJSON(body) {
		return body, info, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		// Non-JSON / malformed JSON — passthrough (mirrors Python
		// `_rewrite_body`'s try/except json.loads).
		return body, info, nil
	}
	msgsRaw, _ := obj["messages"].([]any)
	imageCount := countImagesInMessages(msgsRaw)
	if imageCount == 0 {
		// No images in the request — nothing for vision routing to do.
		// Counter / span fire with Routed=false, Disabled=false,
		// ImageCount=0 so SigNoz sees the "no image" baseline.
		return body, info, nil
	}

	// Past this point, the body has at least one image. Always populate
	// the counts so SigNoz can tell "would-have-routed but disabled"
	// apart from "no image at all".
	info.ImageCount = imageCount
	info.ImagesCollected = imageCount

	if cfg == nil || cfg.VisionModel == "" {
		// Routing disabled at config level — record Disabled so the
		// span / counter can distinguish this from the no-image case,
		// and leave the body untouched.
		info.Disabled = true
		return body, info, nil
	}

	// We're going to route. Capture the from-model now so the caller
	// can record both sides on the span.
	from, _ := obj["model"].(string)
	info.ModelFrom = from
	info.ModelTo = cfg.VisionModel

	// Step 3: override the model. Even if the upstream id already
	// matches VISION_MODEL, recording Routed=true is correct — the
	// pipeline still consolidated images and stripped tool keys.
	obj["model"] = cfg.VisionModel

	// Step 4: drop tools / tool_choice. Track whether they were
	// present so the span attributes can record the booleans.
	if _, ok := obj["tools"]; ok {
		info.ToolsDropped = true
		delete(obj, "tools")
	}
	if _, ok := obj["tool_choice"]; ok {
		info.ToolChoiceDropped = true
		delete(obj, "tool_choice")
	}

	// Step 5: walk messages. Find the last user turn first — Python
	// uses a reverse-iter `next(...)` over the index range.
	lastUserIdx := -1
	for i := len(msgsRaw) - 1; i >= 0; i-- {
		msg, ok := msgsRaw[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx == -1 {
		// No user turn at all — Python returns 0 here. We've still
		// dropped tools and overridden the model, so re-serialize and
		// report Routed=true. Image collection is a no-op.
		out, err := json.Marshal(obj)
		if err != nil {
			return body, info, err
		}
		info.Routed = true
		return out, info, nil
	}

	var imagesCollected []any
	for i, raw := range msgsRaw {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content, _ := msg["content"].([]any)
		switch {
		case role == "assistant":
			if content == nil {
				continue
			}
			newContent := make([]any, 0, len(content))
			for _, b := range content {
				block, ok := b.(map[string]any)
				if !ok {
					newContent = append(newContent, b)
					continue
				}
				if block["type"] == "tool_use" {
					name, _ := block["name"].(string)
					if name == "" {
						name = "tool"
					}
					inp, _ := block["input"].(map[string]any)
					info.ToolUseConverted++
					newContent = append(newContent, map[string]any{
						"type": "text",
						"text": formatToolUseLabel(name, inp),
					})
				} else {
					newContent = append(newContent, b)
				}
			}
			msg["content"] = newContent

		case role == "user" && i != lastUserIdx:
			if content == nil {
				continue
			}
			newContent := make([]any, 0, len(content))
			for _, b := range content {
				block, ok := b.(map[string]any)
				if !ok {
					newContent = append(newContent, b)
					continue
				}
				switch block["type"] {
				case "image":
					imagesCollected = append(imagesCollected, block)
					newContent = append(newContent, map[string]any{
						"type": "text",
						"text": imagePlaceholder,
					})
				case "tool_result":
					inner, _ := block["content"].([]any)
					if inner != nil {
						for _, nb := range inner {
							nblock, ok := nb.(map[string]any)
							if !ok {
								continue
							}
							switch nblock["type"] {
							case "image":
								imagesCollected = append(imagesCollected, nblock)
							case "text":
								newContent = append(newContent, nblock)
							}
						}
					} else if s, ok := block["content"].(string); ok {
						newContent = append(newContent, map[string]any{
							"type": "text",
							"text": s,
						})
					}
				default:
					newContent = append(newContent, b)
				}
			}
			msg["content"] = newContent

		case role == "user" && i == lastUserIdx:
			if content == nil {
				continue
			}
			newContent := make([]any, 0, len(content))
			for _, b := range content {
				block, ok := b.(map[string]any)
				if !ok {
					newContent = append(newContent, b)
					continue
				}
				switch block["type"] {
				case "image":
					imagesCollected = append(imagesCollected, block)
				case "tool_result":
					inner, _ := block["content"].([]any)
					if inner != nil {
						for _, nb := range inner {
							nblock, ok := nb.(map[string]any)
							if !ok {
								continue
							}
							switch nblock["type"] {
							case "image":
								imagesCollected = append(imagesCollected, nblock)
							case "text":
								newContent = append(newContent, nblock)
							}
						}
					} else if s, ok := block["content"].(string); ok {
						newContent = append(newContent, map[string]any{
							"type": "text",
							"text": s,
						})
					}
				default:
					newContent = append(newContent, b)
				}
			}
			msg["content"] = newContent
		}
	}

	info.ImageCount = len(imagesCollected)

	// Step 6: prepend collected images to last user turn. Python
	// (line 728): `last_msg["content"] = images_collected + last_content`.
	if len(imagesCollected) > 0 {
		lastMsg, _ := msgsRaw[lastUserIdx].(map[string]any)
		var lastContent []any
		switch v := lastMsg["content"].(type) {
		case []any:
			lastContent = v
		case string:
			lastContent = []any{map[string]any{"type": "text", "text": v}}
		}
		merged := make([]any, 0, len(imagesCollected)+len(lastContent))
		merged = append(merged, imagesCollected...)
		merged = append(merged, lastContent...)
		lastMsg["content"] = merged
	}

	// For symmetry with the design spec: ImagesCollected is also kept
	// (proxy.go's runVision currently reads ImagesCollected for the
	// span attribute).
	info.ImagesCollected = info.ImageCount
	info.Routed = true

	out, err := json.Marshal(obj)
	if err != nil {
		// Re-marshal failure is unrecoverable but extremely unlikely
		// for a body we just unmarshalled. Return the original bytes
		// + the error so the caller records it on the span.
		return body, info, err
	}
	return out, info, nil
}

// formatToolUseLabel renders a tool_use block as a plain-text label.
// Matches the Python format string exactly:
//
//	[used <name> tool[: k='v', k2='v2', ...]]
//
// Python uses `f"{k}={v!r}"` which calls `repr()` on each value. For
// strings that produces single-quoted Python repr; for numbers / bools
// it produces the literal value. We approximate the common cases:
//
//   - string → wrapped in single quotes (Python's `'foo'`)
//   - number / bool / null → fmt.Sprintf("%v", v)
//   - other (slices, maps) → JSON encoding (best-effort; the Python
//     source produces something like `[1, 2, 3]` for a list — we use
//     the JSON form which is close enough for the diagnostic label).
//
// Map iteration is sorted by key so the label is deterministic for
// tests and span comparison; Python's dict iteration is insertion-order
// since 3.7, but we don't have access to the original key order here
// (Go's map is unordered) so a sorted form is the only stable answer.
func formatToolUseLabel(name string, inp map[string]any) string {
	if len(inp) == 0 {
		return fmt.Sprintf("[used %s tool]", name)
	}
	keys := make([]string, 0, len(inp))
	for k := range inp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+pythonRepr(inp[k]))
	}
	desc := parts[0]
	for i := 1; i < len(parts); i++ {
		desc += ", " + parts[i]
	}
	return fmt.Sprintf("[used %s tool: %s]", name, desc)
}

// pythonRepr approximates Python's `repr()` for the value types that
// can appear inside a JSON-decoded `tool_use.input` object. Strings
// get single-quoted (Python's preferred quote when the value contains
// no embedded single quote). Other types use Go's default formatting.
func pythonRepr(v any) string {
	switch t := v.(type) {
	case string:
		// Python repr prefers single quotes unless the string contains
		// a single quote (then double). Approximate that rule.
		hasSingle := false
		for _, c := range t {
			if c == '\'' {
				hasSingle = true
				break
			}
		}
		if hasSingle {
			return fmt.Sprintf("%q", t) // double-quoted
		}
		return "'" + t + "'"
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		// JSON numbers come out as float64; render integers without
		// the trailing `.0` to match common Python repr expectations.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		// Slices, maps, etc. — JSON form is the best compromise.
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}
