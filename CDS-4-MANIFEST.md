# CDS-4 Change Manifest

Tracks every change made to the system as part of
[CDS-4 — proxy: handle images and configure claude to use proxy for images POST](README.md).

---

## Files modified

### `claude-ds-proxy.py`

**New imports added** (stdlib-only, no new dependencies):

| Import | Reason |
|--------|--------|
| `base64` | Encode uploaded file bytes to base64 for the cache and for the rewritten source blocks. |
| `email.parser` | Parse `multipart/form-data` bodies — stdlib's `email` package provides a spec-compliant MIME parser. |
| `mimetypes` | Guess `media_type` from filenames when the upload doesn't specify a content-type. |
| `uuid` | Generate unique, unpredictable `file_id` values for the in-memory cache. |

**New module-level state:**

| Symbol | Purpose |
|--------|---------|
| `_FILE_CACHE: dict` | In-memory store — maps `file_id → {data, media_type, filename, size}`. |
| `_FILE_CACHE_LOCK: threading.Lock` | Protects `_FILE_CACHE` for thread-safe concurrent access. |

**New functions:**

| Function | Purpose |
|----------|---------|
| `_store_file(data, filename, media_type) → file_id` | Encode `data` to base64, store in cache, return generated id. |
| `_lookup_file(file_id) → dict \| None` | Thread-safe cache lookup. |
| `_parse_multipart(content_type, body) → list \| None` | Parse `multipart/form-data` using `email.parser.BytesParser`. |
| `_is_files_upload(method, path) → bool` | Detect `POST /v1/files` requests. |
| `_rewrite_file_sources(messages) → int` | Scan every content block in all messages; swap `source.type == "file"` blocks to inline base64. Returns substitution count. |

**Modified functions:**

| Function | Change |
|----------|--------|
| `_rewrite_body(body)` | Added Pass 1 before the effort pass: calls `_rewrite_file_sources` when the body contains a `messages` array. |
| `Proxy._handle(self)` | Added Files API interception: delegates `POST /v1/files` to `_handle_files_upload`. Strips `anthropic-beta: files-api-*` header from outgoing forwarded requests. |

**New methods on `Proxy`:**

| Method | Purpose |
|--------|---------|
| `_handle_files_upload(self, body)` | Parse multipart or raw-binary upload, call `_store_file`, return mock Anthropic Files API JSON response. Does NOT forward to upstream. |

---

## Files created

| File | Purpose |
|------|---------|
| `tests/__init__.py` | Makes `tests/` a Python package so `python3 -m unittest tests.test_proxy_images` resolves correctly. |
| `tests/test_proxy_images.py` | Full TDD test suite — 19 tests across 4 classes covering unit (multipart parser, file cache, source rewriting) and integration (live proxy + mock upstream). |
| `CDS-4-MANIFEST.md` | This file. |

---

## No system-level changes

All additions are:
- **In-process Python** — no new services, daemons, or system tools.
- **Stdlib-only** — `base64`, `email`, `mimetypes`, `uuid` are Python standard library. Zero new packages.
- **In-memory cache** — no disk writes, no databases, no external state.
- **Scoped to the proxy lifetime** — the cache lives as long as the proxy process (one claude-ds session). Images uploaded in one session are not persisted across sessions, matching Claude Code's existing behaviour.

---

## Test run (passing)

```
Ran 19 tests in 1.023s

OK
```

---

## Post-merge fix — v0.7.1 (commit 97de0b9, PR #6)

**Problem discovered in production**: The system prompt told the model
`Your underlying model is deepseek-v4-pro`. Since `deepseek-v4-pro` is
text-only, the model deduced from training knowledge that it couldn't
process images and told users so — even though the proxy was already
transparently routing image requests to `deepseek-chat` (vision-capable).

**File changed**: `claude-ds` (wrapper script)

| Change | Detail |
|--------|--------|
| System prompt — image support note added | Explicitly informs the model it CAN process images; that the proxy handles Files API / base64 rewriting and routes to `deepseek-chat`. Overrides incorrect self-assessment from model training. |
| Version bump | `0.7.0` → `0.7.1` |

---

## Post-merge fix — v0.7.2 (commit b284408, PR #7)

**Problem discovered in production**: When `proxy_effort=auto:high` is configured
(standard config), Pass 2 of `_rewrite_body` injected `thinking:{type:"enabled"}`
into every request. After Pass 1b had already routed the request to `deepseek-chat`
for vision, Pass 2 would still run and add thinking parameters. `deepseek-chat`
(deepseek-v4-flash) does not support extended thinking simultaneously with image
inputs — it responded "I cannot see this image" / "The image isn't rendering on
my end" instead of processing the image.

**Root cause**: `routed_to_vision` flag was not set, so the effort-rewrite pass had
no way to know it should be skipped.

**Files changed**: `claude-ds-proxy.py`, `tests/test_proxy_images.py`, `claude-ds`

| Change | Detail |
|--------|--------|
| `routed_to_vision` flag in `_rewrite_body` | Set to `True` after Pass 1b overrides the model to `VISION_MODEL`. |
| Early return before Pass 2 | `if routed_to_vision: return body` — prevents thinking injection for vision routes. |
| Regression test added | `test_effort_not_applied_to_vision_route`: EFFORT_DEFAULT=auto:high + image request → upstream body must NOT contain `thinking`. |
| Version bump | `0.7.1` → `0.7.2` |

**Verified**: Live test with EFFORT_DEFAULT=auto:high, image upload, DeepSeek vision response confirmed.

---

## Stale proxy detection — (commit 2b99188, PR #8)

**Problem discovered**: An older `claude-ds-proxy.py` (pre-image-proxy) was root-owned
at `/usr/local/bin/claude-ds-proxy.py`. Sessions started from that location ran the old
proxy silently, causing images to fail even after v0.7.2 was installed to `~/.local/bin/`.

**Files changed**: `claude-ds`, `install.sh`

| Change | Detail |
|--------|--------|
| `claude-ds` startup warning | Compares proxy being launched against `/usr/local/bin/` and `/opt/homebrew/bin/`; warns with exact `sudo cp` fix command if stale version found. |
| `install.sh` stale sync | After installing, scans for diverged proxy files in system paths and offers to sync with sudo (interactive). Prints exact command on failure. |

**Remaining manual step**: Run once to sync root-owned stale proxy:
```bash
sudo cp ~/.local/bin/claude-ds-proxy.py /usr/local/bin/claude-ds-proxy.py
```
New sessions (started via `claude-ds`) are unaffected — `which claude-ds` resolves to `~/.local/bin/claude-ds` (v0.7.2).

---

## Root cause fix — DISABLE_EXPERIMENTAL_BETAS blocking Files API — v0.7.3

**Problem discovered**: `claude-ds` exported `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`
unconditionally (line 1601). This flag tells Claude Code to skip the Anthropic Files API
entirely, so image attachments never reached `POST /v1/files`. Claude Code fell back to
reading image files via the filesystem `Read` tool, completely bypassing the proxy.

**Files changed**: `claude-ds`, `claude-ds-proxy.py`

| Change | Detail |
|--------|--------|
| `claude-ds`: unset beta flag when proxy is active | After the proxy starts successfully, `unset CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS` so Claude Code uses the Files API. The proxy intercepts uploads and strips the `files-api` beta header before forwarding to DeepSeek. |
| `claude-ds-proxy.py`: structured header pipeline | Replaced ad-hoc inline header logic with `_build_upstream_headers()`: centralised strip/add tables, per-field `anthropic-beta` filtering, `PROXY_STRIP_HEADERS` and `PROXY_ADD_HEADERS` env-var config, full DEBUG-mode header dump (incoming + outgoing, per-decision). |
| Version bump | `0.7.2` → `0.7.3` |

**E2E verified**: `POST /v1/files` → proxy caches base64 → `POST /v1/messages` with `file_id`
→ proxy rewrites to inline base64 → DeepSeek (`deepseek-v4-flash`) responds with visual
description of the image. All 22 unit tests pass.
