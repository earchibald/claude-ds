#!/usr/bin/env python3
# claude-ds-proxy — request-rewriting proxy that:
#   1. Translates Anthropic `thinking.budget_tokens` into DeepSeek's
#      reasoning regime on outgoing /v1/messages bodies.
#   2. Mocks the Anthropic Files API (POST /v1/files) so that Claude Code
#      can upload images; the proxy caches the binary as base64 and rewrites
#      any `source.type == "file"` blocks in subsequent /v1/messages to
#      the inline base64 format that DeepSeek expects.
#
# Spawned by the `claude-ds` shell wrapper. Stdlib-only (Python 3.8+).
#
# ── Reasoning-effort translation ────────────────────────────────────────────
#
# DeepSeek's compat shim recognises three reasoning regimes:
#   "none"   — `thinking` block ABSENT, no `reasoning_effort`. No reasoning.
#   "high"   — `thinking: {"type": "enabled"}` PRESENT, `reasoning_effort`
#              absent (or =high — same wire effect). DeepSeek's default
#              reasoning depth.
#   "max"    — `thinking` PRESENT *and* `reasoning_effort=max`. Maximum
#              reasoning depth. (Other Anthropic levels — `low`, `medium`,
#              `minimal`, `xhigh` — are not real DeepSeek regimes; this
#              proxy collapses them onto the three above.)
#
# So the proxy applies a *transformation* (not just an injection):
#   level=none → strip `thinking`, strip `reasoning_effort`
#   level=high → ensure `thinking: {type: enabled}` (with the original
#                budget_tokens preserved if present), strip `reasoning_effort`
#   level=max  → ensure `thinking: {type: enabled}`, set `reasoning_effort=max`
#
# Claude → DeepSeek mapping (proxy_effort=auto):
#   Claude off (no thinking)     → none (no reasoning tokens)
#   Claude low/medium/high       → high (DeepSeek default reasoning)
#   Claude extra-high / max      → max  (DeepSeek maximum reasoning)
#   small_fast tier (always)     → none (strip thinking — saves tokens)
#
# ── Image / Files API bridging ──────────────────────────────────────────────
#
# When Claude Code attaches an image it:
#   1. POSTs the binary to POST /v1/files (with anthropic-beta: files-api-*)
#   2. Receives a `file_id` (e.g. "file_abc123")
#   3. Sends subsequent /v1/messages with content blocks:
#        {"type": "image", "source": {"type": "file", "file_id": "file_abc123"}}
#
# DeepSeek does not implement the Files API and expects inline base64:
#        {"type": "image", "source": {"type": "base64",
#                                     "media_type": "image/png",
#                                     "data": "<base64>"}}
#
# The proxy bridges this by:
#   • Intercepting POST /v1/files — parsing the multipart body, converting
#     the file to base64, caching it keyed by a generated file_id, and
#     returning a mock Anthropic Files API success response.
#   • Intercepting POST /v1/messages — scanning every content block across
#     every message for `source.type == "file"`, looking up the cached
#     base64, and rewriting the source block to the inline base64 format
#     before forwarding to DeepSeek.
#   • Stripping `anthropic-beta: files-api-*` from outgoing headers to
#     DeepSeek so the upstream doesn't reject the request.
#
# ── Configuration (env vars set by the wrapper) ─────────────────────────────
#
#   UPSTREAM_BASE_URL   required. e.g. https://api.deepseek.com/anthropic
#   PROXY_BIND          default 127.0.0.1
#   PROXY_PORT          default 0 (kernel-assigned; actual port printed to
#                       stdout as the first line so the parent can read it)
#   EFFORT_DEFAULT      spec applied to any model not named in EFFORT_MAP.
#                       see "Spec language" below. empty = passthrough.
#   EFFORT_MAP          per-wire-model overrides. semicolon-separated
#                       `<model>=<spec>` pairs.
#                       e.g. "claude-opus-4-7=auto;claude-sonnet-4-6=high"
#   PROXY_DEBUG         "1" to log rewrites/requests to stderr.
#   VISION_MODEL        model to use when the request contains images (base64
#                       or file-source blocks).  default: deepseek-chat
#                       set to "" to disable auto-routing.
#
# ── Spec language (value side of EFFORT_MAP / EFFORT_DEFAULT) ───────────────
#
#   off            no transformation — pass the body through unchanged.
#                  (empty string is treated the same.)
#   none           force the "no reasoning" regime (strip thinking block).
#   high           force the "default reasoning" regime (thinking enabled,
#                  no reasoning_effort).
#   max            force the "maximum reasoning" regime (thinking enabled
#                  + reasoning_effort=max).
#   auto           derive the regime from claude's `thinking` block:
#                    no/disabled thinking          → "none"
#                    thinking enabled, budget <31k → "high"
#                    thinking enabled, budget ≥31k → "max"
#                  (Thresholds align with Claude Code's canned levels:
#                  `think` ≈ 4k, `think hard` ≈ 10k, `think harder` ≈ 20k,
#                  `ultrathink` ≈ 31999. ultrathink → max; everything else
#                  with thinking → high.)
#   auto:<level>   like auto, but use <level> as the fallback when claude
#                  sends no thinking block. e.g. `auto:high` ensures
#                  thinking is always on; `auto:none` is identical to bare
#                  `auto`.
#   none=<v>|high=<v>|max=<v>
#                  full per-source-bucket matrix. each clause is optional;
#                  unlisted source buckets pass through unchanged.
#                  e.g. `none=high|high=high|max=max` ≡ `auto:high`.
#
# Resolution order on every POST /v1/messages:
#   1. spec = EFFORT_MAP[model] if present, else EFFORT_DEFAULT
#   2. if spec is empty/`off` → forward unchanged
#   3. otherwise resolve spec against the source bucket → target regime
#   4. apply the regime's transformation to the body
#
# Note: the proxy intentionally OVERRIDES any caller-supplied
# `reasoning_effort`, because Claude Code's `/effort low` / `/effort medium`
# / `/effort xhigh` send levels DeepSeek doesn't recognise — passing them
# through unchanged would cause silent 400s or quiet downgrades. To opt
# out of the override, set the spec to `off` for that model.

import base64
import email.parser
import http.client
import json
import mimetypes
import os
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit


# ---------- Files API in-memory cache ---------------------------------------
# Maps file_id → {"data": <base64_str>, "media_type": <str>, "filename": <str>}
# Protected by _FILE_CACHE_LOCK for thread safety.
_FILE_CACHE: dict = {}
_FILE_CACHE_LOCK = threading.Lock()


def _store_file(data: bytes, filename: str, media_type: str) -> str:
    """Cache `data` as base64 and return a generated file_id."""
    file_id = "file_" + uuid.uuid4().hex[:24]
    b64 = base64.b64encode(data).decode("ascii")
    if not media_type:
        media_type, _ = mimetypes.guess_type(filename) or ("application/octet-stream", None)
        media_type = media_type or "application/octet-stream"
    with _FILE_CACHE_LOCK:
        _FILE_CACHE[file_id] = {
            "data": b64,
            "media_type": media_type,
            "filename": filename,
            "size": len(data),
        }
    return file_id


def _lookup_file(file_id: str):
    """Return cached file dict for file_id, or None."""
    with _FILE_CACHE_LOCK:
        return _FILE_CACHE.get(file_id)


# ---------- multipart parser ------------------------------------------------

def _parse_multipart(content_type: str, body: bytes):
    """Parse a multipart/form-data body.

    Returns a list of dicts, each with keys: name, filename, content_type, data.
    Returns None when the content-type is not multipart/form-data or parsing fails.
    """
    ct = content_type or ""
    if "multipart/form-data" not in ct:
        return None
    # email.parser wants a full MIME message; synthesise one.
    raw = b"Content-Type: " + ct.encode("latin-1") + b"\r\n\r\n" + body
    try:
        msg = email.parser.BytesParser().parsebytes(raw)
    except Exception:
        return None
    if not msg.is_multipart():
        return None
    parts = []
    for part in msg.get_payload():
        cd = part.get("Content-Disposition", "")
        disp_params = dict(
            kv.strip().split("=", 1)
            for kv in cd.split(";")[1:]
            if "=" in kv
        )
        name = disp_params.get("name", "").strip('"')
        filename = disp_params.get("filename", "").strip('"')
        pct = part.get_content_type() or ""
        data = part.get_payload(decode=True) or b""
        parts.append({"name": name, "filename": filename, "content_type": pct, "data": data})
    return parts


# ---------- spec parsing ----------------------------------------------------

# DeepSeek-recognised reasoning regimes. These are the only values a
# spec can resolve to (plus the implicit "passthrough" for `off`).
_LEVELS = {"none", "high", "max"}
# Source-bucket keys (rows of the auto-mapping matrix). The auto-bucket
# function emits one of these for each request based on claude's
# `thinking` block.
_BUCKETS = ("none", "high", "max")


def _parse_spec(raw: str):
    """Parse a spec string into a callable: bucket -> effort_or_none.

    Returns None when the spec disables injection unconditionally."""
    s = (raw or "").strip()
    if not s or s.lower() == "off":
        return None
    s_low = s.lower()
    if s_low in _LEVELS:
        # Constant — same level for every source bucket.
        def _const(bucket, _s=s_low):
            _ = bucket
            return _s
        return _const
    if s_low == "auto":
        # bare `auto` mirrors the source bucket directly: 'none' → strip,
        # 'high' → enable thinking, 'max' → enable thinking + max effort.
        return lambda bucket: bucket if bucket in _LEVELS else None
    if s_low.startswith("auto:"):
        # `auto:<level>` upgrades the no-thinking case ('none' bucket) to
        # the named level, while leaving high/max buckets at their bucket
        # value. Useful for "always at least thinking" (`auto:high`) or
        # "always full reasoning" (`auto:max`).
        fallback = s_low.split(":", 1)[1].strip()
        if fallback not in _LEVELS:
            raise ValueError(f"auto: fallback must be one of {sorted(_LEVELS)} (got {fallback!r})")
        def _auto_with_fallback(bucket, _f=fallback):
            if bucket == "none":
                return _f
            return bucket if bucket in _LEVELS else None
        return _auto_with_fallback
    # Otherwise, expect a `key=val|key=val|...` matrix.
    matrix = {}
    for clause in s.split("|"):
        clause = clause.strip()
        if not clause:
            continue
        if "=" not in clause:
            raise ValueError(f"matrix clause missing '=': {clause!r}")
        k, v = clause.split("=", 1)
        k, v = k.strip().lower(), v.strip().lower()
        if k not in _BUCKETS:
            raise ValueError(f"matrix bucket must be one of {list(_BUCKETS)} (got {k!r})")
        if v not in _LEVELS and v != "off":
            raise ValueError(f"matrix value must be a level or 'off' (got {v!r})")
        if v != "off":
            matrix[k] = v
    if not matrix:
        return None
    return lambda bucket, _m=matrix: _m.get(bucket)


def _parse_map(spec: str) -> dict:
    """Parse `model=spec;model=spec;...` into {model: resolver}."""
    out = {}
    if not spec:
        return out
    for pair in spec.split(";"):
        pair = pair.strip()
        if not pair or "=" not in pair:
            continue
        model, raw_spec = pair.split("=", 1)
        model = model.strip()
        if not model:
            continue
        out[model] = _parse_spec(raw_spec)
    return out


# ---------- environment / config -------------------------------------------

UPSTREAM = os.environ.get("UPSTREAM_BASE_URL", "")
if not UPSTREAM:
    print("claude-ds-proxy: UPSTREAM_BASE_URL is required", file=sys.stderr)
    sys.exit(2)

try:
    EFFORT_RESOLVERS = _parse_map(os.environ.get("EFFORT_MAP", ""))
    DEFAULT_RESOLVER = _parse_spec(os.environ.get("EFFORT_DEFAULT", ""))
except ValueError as e:
    print(f"claude-ds-proxy: bad effort spec: {e}", file=sys.stderr)
    sys.exit(2)

DEBUG = os.environ.get("PROXY_DEBUG", "") == "1"
VISION_MODEL = os.environ.get("VISION_MODEL", "deepseek-chat")

_up = urlsplit(UPSTREAM)
UP_SCHEME = _up.scheme
UP_HOST = _up.hostname or ""
UP_PORT = _up.port or (443 if UP_SCHEME == "https" else 80)
UP_PATH_PREFIX = (_up.path or "").rstrip("/")


def _log(*args):
    if DEBUG:
        print("[claude-ds-proxy]", *args, file=sys.stderr, flush=True)


# ---------- request rewriting ----------------------------------------------

_HOP_BY_HOP = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
}


def _should_inject(method: str, path: str) -> bool:
    if method != "POST":
        return False
    bare = path.split("?", 1)[0].rstrip("/")
    return bare.endswith("/v1/messages")


def _is_files_upload(method: str, path: str) -> bool:
    if method != "POST":
        return False
    bare = path.split("?", 1)[0].rstrip("/")
    return bare.endswith("/v1/files")


def _bucket_from_thinking(thinking) -> str:
    """Return one of 'none' | 'high' | 'max' for a thinking block.

    DeepSeek only distinguishes three reasoning regimes, so the bucket
    space matches: no thinking → 'none'; thinking enabled with any
    sub-ultrathink budget → 'high'; thinking enabled with ultrathink-tier
    budget → 'max'."""
    if not isinstance(thinking, dict) or thinking.get("type") != "enabled":
        return "none"
    try:
        n = int(thinking.get("budget_tokens", 0) or 0)
    except (TypeError, ValueError):
        return "high"
    return "max" if n >= 31000 else "high"


def _apply_regime(obj: dict, regime: str) -> None:
    """Mutate `obj` in place to match the target DeepSeek regime.

    none — strip both `thinking` and `reasoning_effort` so DeepSeek
           returns a plain non-reasoning response.
    high — ensure `thinking: {type: enabled}` (preserving any
           caller-supplied budget_tokens), strip `reasoning_effort`
           since DeepSeek treats omission and `=high` as the same wire
           effect and Anthropic-style values like `low`/`medium` would
           otherwise be rejected.
    max  — ensure `thinking: {type: enabled}`, set `reasoning_effort=max`."""
    if regime == "none":
        obj.pop("thinking", None)
        obj.pop("reasoning_effort", None)
        return
    # Both `high` and `max` need thinking enabled. Preserve an existing
    # budget_tokens if present; otherwise omit (DeepSeek handles unbounded).
    existing = obj.get("thinking")
    new_thinking: dict = {"type": "enabled"}
    if isinstance(existing, dict) and "budget_tokens" in existing:
        new_thinking["budget_tokens"] = existing["budget_tokens"]
    obj["thinking"] = new_thinking
    if regime == "high":
        obj.pop("reasoning_effort", None)
    elif regime == "max":
        obj["reasoning_effort"] = "max"


def _rewrite_file_sources(messages: list) -> int:
    """Scan every content block in `messages` and replace any
    `source.type == "file"` block with the cached inline base64 source.

    Returns the number of substitutions made.  Mutates `messages` in place.
    """
    subs = 0
    for msg in messages:
        content = msg.get("content")
        if isinstance(content, str):
            continue
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict):
                continue
            src = block.get("source")
            if not isinstance(src, dict):
                continue
            if src.get("type") != "file":
                continue
            file_id = src.get("file_id", "")
            cached = _lookup_file(file_id)
            if cached is None:
                _log(f"file_id={file_id!r} not in cache — leaving as-is")
                continue
            block["source"] = {
                "type": "base64",
                "media_type": cached["media_type"],
                "data": cached["data"],
            }
            subs += 1
            _log(f"swapped file_id={file_id!r} → base64 ({cached['media_type']}, {cached['size']}B)")
    return subs


def _has_image_content(messages: list) -> bool:
    """Return True if any content block in `messages` is an image."""
    for msg in messages:
        content = msg.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict):
                continue
            if block.get("type") != "image":
                continue
            return True
    return False


def _rewrite_body(body: bytes) -> bytes:
    try:
        obj = json.loads(body)
    except Exception as e:
        _log(f"body not JSON, passing through ({e})")
        return body
    if not isinstance(obj, dict):
        return body

    # Pass 1: rewrite file_id → base64 for any cached uploaded files.
    messages = obj.get("messages")
    if isinstance(messages, list):
        subs = _rewrite_file_sources(messages)
        if subs:
            _log(f"rewrote {subs} file-source block(s) in messages")

    # Pass 1b: if the request contains images and VISION_MODEL is set,
    # transparently swap the model so it routes to a vision-capable backend.
    if VISION_MODEL and isinstance(messages, list) and _has_image_content(messages):
        original_model = obj.get("model", "")
        if original_model != VISION_MODEL:
            obj["model"] = VISION_MODEL
            _log(f"image detected — overriding model {original_model!r} → {VISION_MODEL!r}")

    # Pass 2: apply reasoning-effort transformation.
    model = obj.get("model", "") or ""
    resolver = EFFORT_RESOLVERS.get(model, DEFAULT_RESOLVER)
    if resolver is None:
        _log(f"no effort spec applies to model={model!r}; passing through")
        return json.dumps(obj, ensure_ascii=False).encode("utf-8") if messages else body

    bucket = _bucket_from_thinking(obj.get("thinking"))
    regime = resolver(bucket)
    if not regime:
        _log(f"spec yielded no regime for model={model!r} bucket={bucket!r}; passing through")
        return json.dumps(obj, ensure_ascii=False).encode("utf-8") if messages else body

    incoming_re = obj.get("reasoning_effort", "<absent>")
    _apply_regime(obj, regime)
    _log(
        f"applied regime={regime!r} model={model!r} source-bucket={bucket!r} "
        f"(was reasoning_effort={incoming_re!r})"
    )
    return json.dumps(obj, ensure_ascii=False).encode("utf-8")


# ---------- HTTP server -----------------------------------------------------


class Proxy(BaseHTTPRequestHandler):
    # HTTP/1.1 + Connection: close on responses gives us length-by-EOF
    # framing — the simplest way to forward streaming SSE bodies without
    # re-encoding chunked transfer.
    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):  # noqa: A002 — match base sig
        if DEBUG:
            super().log_message(format, *args)

    def _handle(self):
        clen = self.headers.get("Content-Length")
        body = b""
        if clen:
            try:
                body = self.rfile.read(int(clen))
            except Exception as e:
                self.send_error(400, f"failed to read request body: {e}")
                return

        # ── Files API upload interception ──────────────────────────────────
        # Handle POST /v1/files: store the file locally, return a mock
        # Anthropic Files API response, do NOT forward to upstream.
        if _is_files_upload(self.command, self.path):
            self._handle_files_upload(body)
            return

        # ── /v1/messages rewriting ─────────────────────────────────────────
        if _should_inject(self.command, self.path) and body:
            ctype = (self.headers.get("Content-Type") or "").lower()
            if "json" in ctype:
                body = _rewrite_body(body)

        fwd_headers = []
        for h, v in self.headers.items():
            if h.lower() in _HOP_BY_HOP:
                continue
            if h.lower() in ("host", "content-length"):
                continue
            # Strip the Files-API beta header — DeepSeek rejects it.
            if h.lower() == "anthropic-beta" and "files-api" in v.lower():
                _log(f"stripped header {h}: {v}")
                continue
            fwd_headers.append((h, v))
        fwd_headers.append(("Host", UP_HOST))
        if body:
            fwd_headers.append(("Content-Length", str(len(body))))

        target_path = UP_PATH_PREFIX + self.path

        if UP_SCHEME == "https":
            conn = http.client.HTTPSConnection(UP_HOST, UP_PORT, timeout=600)
        else:
            conn = http.client.HTTPConnection(UP_HOST, UP_PORT, timeout=600)

        try:
            conn.putrequest(self.command, target_path, skip_host=True, skip_accept_encoding=True)
            for h, v in fwd_headers:
                conn.putheader(h, v)
            conn.endheaders()
            if body:
                conn.send(body)
            resp = conn.getresponse()
        except Exception as e:
            _log(f"upstream request failed: {e}")
            try:
                self.send_error(502, f"upstream error: {e}")
            except Exception:
                pass
            conn.close()
            return

        try:
            self.send_response(resp.status, resp.reason)
            for h, v in resp.getheaders():
                hl = h.lower()
                if hl in _HOP_BY_HOP:
                    continue
                if hl == "content-length":
                    continue
                self.send_header(h, v)
            self.send_header("Connection", "close")
            self.end_headers()

            while True:
                chunk = resp.read(8192)
                if not chunk:
                    break
                try:
                    self.wfile.write(chunk)
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    _log("client closed connection mid-stream")
                    break
        finally:
            try:
                conn.close()
            except Exception:
                pass

    def _handle_files_upload(self, body: bytes):
        """Mock POST /v1/files: cache the file as base64, return a fake
        Anthropic Files API response without forwarding to upstream."""
        ctype = self.headers.get("Content-Type") or ""
        filename = "upload"
        media_type = ""
        file_data = b""

        if "multipart/form-data" in ctype:
            parts = _parse_multipart(ctype, body)
            if parts:
                # Claude Code sends the file in the first non-text-only part,
                # usually with name="file".
                file_part = next(
                    (p for p in parts if p.get("filename") or p.get("content_type")),
                    parts[0],
                )
                file_data = file_part["data"]
                filename = file_part["filename"] or file_part["name"] or "upload"
                media_type = file_part["content_type"] or ""
            else:
                # Fallback: treat the whole body as raw file data.
                file_data = body
        elif body:
            # Raw binary (application/octet-stream or similar).
            file_data = body
            # Try to detect media type from content-type header.
            if ";" in ctype:
                media_type = ctype.split(";")[0].strip()
            else:
                media_type = ctype.strip()

        if not file_data:
            self.send_error(400, "empty file upload")
            return

        # Guess media type from filename if not already known.
        if not media_type and filename:
            guessed, _ = mimetypes.guess_type(filename)
            media_type = guessed or "application/octet-stream"
        media_type = media_type or "application/octet-stream"

        file_id = _store_file(file_data, filename, media_type)
        _log(f"files-api: stored file_id={file_id!r} filename={filename!r} "
             f"media_type={media_type!r} size={len(file_data)}")

        resp_body = json.dumps({
            "id": file_id,
            "type": "file",
            "filename": filename,
            "mime_type": media_type,
            "size": len(file_data),
            "created_at": int(time.time()),
            "downloadable": False,
        }, ensure_ascii=False).encode("utf-8")

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp_body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(resp_body)

    do_GET = _handle
    do_POST = _handle
    do_PUT = _handle
    do_DELETE = _handle
    do_PATCH = _handle
    do_OPTIONS = _handle
    do_HEAD = _handle


def _watch_orphan():
    """Background watchdog: exit when the parent process dies. The wrapper
    `exec`s claude after spawning us, so our parent stays alive for the
    duration of the claude session. When claude exits, we get reparented
    to PID 1 (init/launchd) — that's our cue to shut down."""
    initial_ppid = os.getppid()
    while True:
        time.sleep(2)
        ppid = os.getppid()
        if ppid != initial_ppid and ppid == 1:
            _log("parent died (reparented to init); exiting")
            os._exit(0)


def main():
    threading.Thread(target=_watch_orphan, daemon=True).start()
    bind = os.environ.get("PROXY_BIND", "127.0.0.1")
    port = int(os.environ.get("PROXY_PORT", "0") or "0")
    server = ThreadingHTTPServer((bind, port), Proxy)
    actual_port = server.server_address[1]
    # First line of stdout is the bound port — the parent wrapper reads
    # it synchronously to know we're ready and which port to point
    # ANTHROPIC_BASE_URL at.
    print(actual_port, flush=True)
    _log(f"listening on {bind}:{actual_port}, upstream={UPSTREAM}")
    _log(f"per-model resolvers: {sorted(EFFORT_RESOLVERS)}, default-set={DEFAULT_RESOLVER is not None}")
    _log(f"vision-model routing: {VISION_MODEL!r} (set VISION_MODEL='' to disable)")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
