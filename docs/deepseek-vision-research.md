# DeepSeek Vision Integration — Research Findings

> **TL;DR**: DeepSeek's Anthropic-compatible API only processes images in the
> **last (most recent) user turn**.  Bridging Claude Code's Files API to DeepSeek
> requires three things working together: (1) mock the Files API endpoint, (2) rewrite
> `file_id` references to inline base64 before forwarding, and (3) consolidate all
> images into the last user turn and strip tool machinery.  Without all three, images
> are silently ignored.

---

## Background

Claude Code (the CLI) has two paths for getting images into a conversation:

| Path | When used | Wire format |
|------|-----------|-------------|
| **Files API** (`POST /v1/files`) | Clipboard paste / drag-and-drop | Multipart upload → returns `file_id`; subsequent messages reference `{"type":"file","file_id":"..."}` |
| **Read tool** | `/read <path>` or agent calls `Read(file_path=...)` | Base64 blob is placed inside a `tool_result` content block in the user turn |

DeepSeek's Anthropic-compatible endpoint (`api.deepseek.com/anthropic`) supports
**neither** of these formats natively.  It expects:

```json
{
  "type": "image",
  "source": {
    "type": "base64",
    "media_type": "image/png",
    "data": "<BASE64_STRING>"
  }
}
```

…at the **top level** of the content array in the **last user turn**.

---

## Attempt log — what we tried and why it failed

### Attempt 0 — do nothing (baseline failure)
*Symptom*: "The screenshot didn't come through as a supported attachment."  
*Root cause*: Claude Code tried to POST `/v1/files`.  DeepSeek's endpoint 404'd.
Claude Code fell back to the Read tool.  The proxy didn't touch the resulting
`tool_result` block.  DeepSeek received an unrecognised content shape and ignored it.

---

### Attempt 1 — mock the Files API endpoint
*Change*: Intercept `POST /v1/files`, parse the multipart body, store binary as base64
in a process-scoped `_FILE_CACHE`, return a fake Anthropic Files API response
(`{id, object, bytes, created_at, filename, purpose}`).

*Result*: Upload succeeded.  But the subsequent `POST /v1/messages` contained
`{"type":"file","file_id":"fake_xyz"}` which DeepSeek still didn't understand.

*Learning*: Must also rewrite file references in message bodies.

---

### Attempt 2 — rewrite file_id → base64 in /v1/messages
*Change*: Added `_rewrite_file_sources()` pass in `_rewrite_body()`.  Walks every
content block in `messages`; when it finds `source.type == "file"`, looks up the cached
base64 and replaces the block with the inline base64 format.

*Result*: In a **single-turn** conversation it worked.  In **multi-turn** sessions
(which is every real conversation after the first exchange) DeepSeek still said
"I cannot see the image."

*Learning*: Something about multi-turn conversations breaks vision.  Started
investigating what DeepSeek actually needs.

---

### Attempt 3 — direct DeepSeek API probing (isolation test)
To separate proxy problems from DeepSeek constraints, we tested the DeepSeek API directly
with `curl` and raw Python `http.client` calls, bypassing the proxy entirely.

**Test matrix:**

| Scenario | Result |
|----------|--------|
| Single-turn, image in only message | ✅ Worked |
| Multi-turn, image in **first** turn | ❌ "I cannot see the image" |
| Multi-turn, image in **middle** turn | ❌ Ignored |
| Multi-turn, image in **last** turn | ✅ Worked |
| Multi-turn, image in last turn + `tool_use` blocks present | ❌ Failed |
| Multi-turn, image in last turn, no `tool_use`/`tools` | ✅ Worked |
| `tool_result`-wrapped image at top of last turn | ❌ Ignored |
| `tool_result` unwrapped, image at top of last turn | ✅ Worked |

**Key empirical findings:**
1. **DeepSeek only processes images in the last user turn.**  Earlier turns are text-only
   for DeepSeek regardless of what the spec says.
2. **`tool_use` / `tool_result` / top-level `tools` key breaks vision** even when the
   image is correctly positioned.  DeepSeek's Anthropic-compat shim appears to switch
   into a "function calling" mode that suppresses vision processing.
3. **`tool_result`-wrapped images are ignored** even in the last turn; the image must
   be at the top level of the content array.

---

### Attempt 4 — hoist images to first turn (wrong direction)
*Change*: Added `_hoist_images_to_first_turn()`.  Collected images from all turns and
prepended them to the **first** user turn.

*Result*: Consistently failed.  This was the **wrong direction** — we had the constraint
backwards.  DeepSeek needs the image in the **last** turn, not the first.

*Learning*: Confirmed by re-running the isolation test.  Removed this function.

---

### Attempt 5 — normalize tool blocks separately
*Change*: Added `_normalize_tool_turns_for_vision()` alongside the hoist function:
stripped `tools`/`tool_choice`, flattened assistant `tool_use` blocks to text,
replaced `tool_result` blocks with their non-image content.

*Result*: Helped with the tool-machinery issue in isolation but still failed in
combination because images were still in the wrong turn.

---

### Attempt 6 — inject images into last turn (unified approach) ✅
*Change*: Replaced both functions with a single unified `_normalize_for_vision()`:

1. Remove top-level `tools` and `tool_choice` keys from the request.
2. Scan all **non-last** user turns: extract image blocks (including those nested
   inside `tool_result` content arrays), replace them with a `[image — in current turn]`
   text placeholder, keep all other content.
3. Scan the **last** user turn: extract images from `tool_result` wrappers (the Read-tool
   path), flatten `tool_result` blocks to their non-image text content.
4. Prepend all collected images (from earlier turns + unwrapped from last-turn
   tool_results) to the front of the last user turn's content array.
5. Convert all assistant `tool_use` blocks to plain-text descriptions
   (`[used <name> tool: <args>]`).

*Result*: ✅ **Working end-to-end** across both image paths (Files API and Read tool)
in single-turn and multi-turn conversations.

---

### Side-quest: CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS
During investigation we discovered that `claude-ds` was exporting
`CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` unconditionally.  This flag tells Claude Code
to skip the Files API entirely — so images pasted from clipboard *never reached the proxy
at all*.  Claude Code fell back to the Read-tool path exclusively.

Fix: unset the flag immediately after the proxy starts successfully.  The proxy already
strips the `anthropic-beta: files-api-*` header before forwarding to DeepSeek, so
upstream never sees the unsupported header.

---

### Side-quest: stale proxy processes
On one occasion Claude Code was routing through an old, pre-image-proxy version of
`claude-ds-proxy.py` that happened to be root-owned at `/usr/local/bin/`.  The new proxy
at `~/.local/bin/` was never invoked.  Added startup detection in `claude-ds` to warn
if a diverged proxy exists at a system path.

---

### Side-quest: effort rewrite breaking vision
The effort-rewriting pass (Pass 2 in `_rewrite_body`) injected `thinking:{type:enabled}`
into every request when `proxy_effort=auto:high`.  After Pass 1b routed the request to
`deepseek-v4-flash` (vision model), Pass 2 added thinking parameters.  `deepseek-v4-flash`
does not support extended thinking simultaneously with image inputs.  Fix: set a
`routed_to_vision` flag in Pass 1b and early-return before Pass 2.

---

## Architecture of the final solution

```
Claude Code                 claude-ds-proxy           DeepSeek API
     │                            │                        │
     │  POST /v1/files            │                        │
     │  (multipart PNG)           │                        │
     │ ─────────────────────────► │                        │
     │                            │  store in _FILE_CACHE  │
     │  200 {"id":"file_xyz"}     │  (base64 + mime)       │
     │ ◄───────────────────────── │                        │
     │                            │                        │
     │  POST /v1/messages         │                        │
     │  [..., {type:file,         │                        │
     │          file_id:xyz}]     │                        │
     │ ─────────────────────────► │                        │
     │                            │  _rewrite_file_sources │
     │                            │  (file_id → base64)    │
     │                            │                        │
     │                            │  _normalize_for_vision │
     │                            │  - strip tools/tool_   │
     │                            │    choice              │
     │                            │  - extract images from │
     │                            │    all turns           │
     │                            │  - inject into last    │
     │                            │    user turn           │
     │                            │                        │
     │                            │  POST /v1/messages     │
     │                            │  (model=deepseek-chat) │
     │                            │ ──────────────────────►│
     │                            │                        │  vision
     │  200 (vision response)     │  200                   │  response
     │ ◄───────────────────────── │ ◄──────────────────────│
```

---

## Key constraints — DeepSeek Anthropic-compat endpoint

These were confirmed empirically (May 2026, `deepseek-v4-flash` / `deepseek-chat`):

| Constraint | Detail |
|-----------|--------|
| Vision model | `deepseek-chat` (deepseek-v4-flash). The primary text model `deepseek-v4-pro` is text-only. |
| Image turn | Must be in the **last** user turn only. Images in any earlier turn are silently ignored. |
| Image position | Must be **top-level** in the content array. Images nested in `tool_result` blocks are ignored. |
| Tool machinery | The presence of `tools`, `tool_choice`, or `tool_use`/`tool_result` blocks in the conversation disables vision processing. These must be stripped. |
| Extended thinking | `deepseek-v4-flash` does not support `thinking` + vision simultaneously. Must skip the effort-rewrite pass for vision-routed requests. |
| Files API | Not supported natively. Must be mocked by the proxy. |
| Inline base64 | Supported. Use `{"type":"base64","media_type":"image/png","data":"..."}`. |
| Image URL | Reportedly supported but not tested in this work. |

---

## Environment variables (proxy configuration)

| Variable | Default | Purpose |
|----------|---------|---------|
| `VISION_MODEL` | `deepseek-chat` | Model to swap to when images are detected. Set to `""` to disable vision routing. |
| `UPSTREAM_BASE_URL` | (required) | Base URL for the upstream API. |
| `PROXY_DEBUG` | `0` | Enable verbose per-request header and body logging. |
| `PROXY_STRIP_HEADERS` | `""` | Comma-separated extra headers to strip before forwarding. |
| `PROXY_ADD_HEADERS` | `""` | `Key:Value,...` headers to inject on every upstream request. |
| `EFFORT_DEFAULT` | `""` | Default reasoning effort (e.g. `auto:high`). |

---

## Test coverage

All logic has unit and integration tests in `tests/test_proxy_images.py`.

| Class | What it tests |
|-------|---------------|
| `TestFileCache` | `_store_file` / `_lookup_file` — storage, MIME guessing, uniqueness |
| `TestParseMultipart` | `_parse_multipart` — boundary parsing, non-multipart pass-through |
| `TestRewriteFileSources` | `_rewrite_file_sources` — single/multi-turn substitution, unknown id pass-through |
| `TestProxyIntegration` | Live proxy + mock upstream — upload, rewrite, header stripping, model routing, effort interaction |
| `TestEffortMapping` | Effort-rewrite pass — reasoning budget translation |
| `TestNormalizeForVision` | `_normalize_for_vision` — all 6 normalization scenarios + Files API full pipeline |

Run: `python3 -m unittest tests.test_proxy_images -v`

---

## Version history (CDS-4 scope)

| Version | Key change |
|---------|-----------|
| 0.7.0 | Initial Files API mock + `_rewrite_file_sources` |
| 0.7.1 | System prompt: tell model it CAN see images via proxy |
| 0.7.2 | Skip effort-rewrite pass for vision-routed requests |
| 0.7.3 | Unset `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS` after proxy starts; structured header pipeline |
| 0.7.4 | `_normalize_for_vision`: inject images into last user turn, strip tool machinery |
