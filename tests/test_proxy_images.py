#!/usr/bin/env python3
"""Tests for the image / Files API proxy features in claude-ds-proxy.

TDD test suite covering:
  1. Unit-level: multipart parser, file cache, source-rewriting logic.
  2. Integration-level: live proxy HTTP server handling POST /v1/files and
     POST /v1/messages with file_id blocks.
  3. Regression: existing reasoning-effort rewriting still works alongside
     the new image support.
"""

import base64
import json
import os
import sys
import threading
import time
import unittest
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

# ── ensure the proxy module is importable from the repo root ─────────────────
# The file is named with dashes ("claude-ds-proxy.py") which Python can't
# import directly; use importlib to load it by path and bind it as
# `claude_ds_proxy` in sys.modules.
_REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_PROXY_PATH = os.path.join(_REPO, "claude-ds-proxy.py")

# The proxy reads UPSTREAM_BASE_URL at import time; supply a dummy so the
# module doesn't sys.exit() during test collection.
os.environ.setdefault("UPSTREAM_BASE_URL", "http://127.0.0.1:19999")

import importlib.util as _ilu  # noqa: E402

_spec = _ilu.spec_from_file_location("claude_ds_proxy", _PROXY_PATH)
proxy = _ilu.module_from_spec(_spec)
sys.modules["claude_ds_proxy"] = proxy
_spec.loader.exec_module(proxy)


# ═══════════════════════════════════════════════════════════════════════════
# ── Helpers ─────────────────────────────────────────────────────────────────
# ═══════════════════════════════════════════════════════════════════════════

# A tiny 1×1 red PNG — deterministic bytes for tests.
_RED_PNG_B64 = (
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADklEQVQI12P4z8BQDwADhQGAWjR9awAAAABJRU5ErkJggg=="
)
_RED_PNG = base64.b64decode(_RED_PNG_B64)


def _multipart_body(filename: str, data: bytes, content_type: str = "image/png"):
    """Build a minimal multipart/form-data body and return (content_type_header, body_bytes)."""
    boundary = "testboundary123"
    lines = []
    lines.append(f"--{boundary}".encode())
    lines.append(
        f'Content-Disposition: form-data; name="file"; filename="{filename}"'.encode()
    )
    lines.append(f"Content-Type: {content_type}".encode())
    lines.append(b"")
    lines.append(data)
    lines.append(f"--{boundary}--".encode())
    body = b"\r\n".join(lines)
    ct_header = f"multipart/form-data; boundary={boundary}"
    return ct_header, body


# ═══════════════════════════════════════════════════════════════════════════
# ── Unit: multipart parser ───────────────────────────────────────────────────
# ═══════════════════════════════════════════════════════════════════════════

class TestParseMultipart(unittest.TestCase):

    def test_basic_file_part(self):
        ct, body = _multipart_body("shot.png", _RED_PNG, "image/png")
        parts = proxy._parse_multipart(ct, body)
        self.assertIsNotNone(parts)
        self.assertEqual(len(parts), 1)
        p = parts[0]
        self.assertEqual(p["filename"], "shot.png")
        self.assertEqual(p["content_type"], "image/png")
        self.assertEqual(p["data"], _RED_PNG)

    def test_non_multipart_returns_none(self):
        result = proxy._parse_multipart("application/json", b"{}")
        self.assertIsNone(result)

    def test_empty_content_type_returns_none(self):
        result = proxy._parse_multipart("", b"")
        self.assertIsNone(result)


# ═══════════════════════════════════════════════════════════════════════════
# ── Unit: file cache ─────────────────────────────────────────────────────────
# ═══════════════════════════════════════════════════════════════════════════

class TestFileCache(unittest.TestCase):

    def setUp(self):
        # Start each test with a clean cache.
        with proxy._FILE_CACHE_LOCK:
            proxy._FILE_CACHE.clear()

    def test_store_and_lookup(self):
        fid = proxy._store_file(_RED_PNG, "test.png", "image/png")
        self.assertTrue(fid.startswith("file_"))
        cached = proxy._lookup_file(fid)
        self.assertIsNotNone(cached)
        self.assertEqual(cached["media_type"], "image/png")
        self.assertEqual(cached["filename"], "test.png")
        self.assertEqual(base64.b64decode(cached["data"]), _RED_PNG)

    def test_lookup_missing_returns_none(self):
        self.assertIsNone(proxy._lookup_file("file_doesnotexist"))

    def test_media_type_guessed_from_filename(self):
        fid = proxy._store_file(_RED_PNG, "image.jpeg", "")
        cached = proxy._lookup_file(fid)
        self.assertEqual(cached["media_type"], "image/jpeg")

    def test_unique_ids(self):
        ids = {proxy._store_file(b"x", "a.png", "image/png") for _ in range(100)}
        self.assertEqual(len(ids), 100)

    def test_size_stored(self):
        fid = proxy._store_file(_RED_PNG, "t.png", "image/png")
        self.assertEqual(proxy._lookup_file(fid)["size"], len(_RED_PNG))


# ═══════════════════════════════════════════════════════════════════════════
# ── Unit: rewrite_file_sources ───────────────────────────────────────────────
# ═══════════════════════════════════════════════════════════════════════════

class TestRewriteFileSources(unittest.TestCase):

    def setUp(self):
        with proxy._FILE_CACHE_LOCK:
            proxy._FILE_CACHE.clear()

    def _msg_with_file_block(self, file_id):
        return {
            "role": "user",
            "content": [
                {"type": "image", "source": {"type": "file", "file_id": file_id}},
                {"type": "text", "text": "what do you see?"},
            ],
        }

    def test_single_substitution(self):
        fid = proxy._store_file(_RED_PNG, "s.png", "image/png")
        msgs = [self._msg_with_file_block(fid)]
        n = proxy._rewrite_file_sources(msgs)
        self.assertEqual(n, 1)
        src = msgs[0]["content"][0]["source"]
        self.assertEqual(src["type"], "base64")
        self.assertEqual(src["media_type"], "image/png")
        self.assertEqual(base64.b64decode(src["data"]), _RED_PNG)

    def test_multi_turn_all_swapped(self):
        fid = proxy._store_file(_RED_PNG, "s.png", "image/png")
        msgs = [self._msg_with_file_block(fid), self._msg_with_file_block(fid)]
        n = proxy._rewrite_file_sources(msgs)
        self.assertEqual(n, 2)

    def test_unknown_file_id_left_unchanged(self):
        msgs = [self._msg_with_file_block("file_unknown")]
        n = proxy._rewrite_file_sources(msgs)
        self.assertEqual(n, 0)
        src = msgs[0]["content"][0]["source"]
        self.assertEqual(src["type"], "file")
        self.assertEqual(src["file_id"], "file_unknown")

    def test_no_file_blocks_unchanged(self):
        msgs = [{"role": "user", "content": [{"type": "text", "text": "hello"}]}]
        n = proxy._rewrite_file_sources(msgs)
        self.assertEqual(n, 0)

    def test_string_content_skipped(self):
        msgs = [{"role": "user", "content": "plain text"}]
        n = proxy._rewrite_file_sources(msgs)
        self.assertEqual(n, 0)


# ═══════════════════════════════════════════════════════════════════════════
# ── Integration: live proxy with mock upstream ───────────────────────────────
# ═══════════════════════════════════════════════════════════════════════════

class _UpstreamCapture(BaseHTTPRequestHandler):
    """Tiny upstream mock that records the last request and returns {}."""

    last_request: dict = {}  # class-level; tests read this
    _lock = threading.Lock()

    def log_message(self, *a):
        pass

    def do_POST(self):
        clen = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(clen) if clen else b""
        with _UpstreamCapture._lock:
            _UpstreamCapture.last_request = {
                "path": self.path,
                "headers": dict(self.headers),
                "body": body,
            }
        resp = json.dumps({"id": "msg_test", "type": "message", "content": []}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)


def _reload_proxy():
    """Re-execute the proxy module in place so env-var changes take effect."""
    _spec.loader.exec_module(proxy)


def _start_server(handler_class, host="127.0.0.1", port=0):
    srv = HTTPServer((host, port), handler_class)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv, srv.server_address[1]


class TestProxyIntegration(unittest.TestCase):

    @classmethod
    def setUpClass(cls):
        # Start mock upstream.
        cls.upstream_srv, cls.upstream_port = _start_server(_UpstreamCapture)
        upstream_url = f"http://127.0.0.1:{cls.upstream_port}"

        # Point the proxy at our mock upstream and reload.
        os.environ["UPSTREAM_BASE_URL"] = upstream_url
        os.environ["PROXY_DEBUG"] = "1"
        os.environ.pop("EFFORT_MAP", None)
        os.environ.pop("EFFORT_DEFAULT", None)

        _reload_proxy()

        cls.proxy_srv, cls.proxy_port = _start_server(
            lambda *a, **kw: proxy.Proxy(*a, **kw)
        )
        cls.proxy_url = f"http://127.0.0.1:{cls.proxy_port}"

    @classmethod
    def tearDownClass(cls):
        cls.proxy_srv.shutdown()
        cls.upstream_srv.shutdown()

    def setUp(self):
        with proxy._FILE_CACHE_LOCK:
            proxy._FILE_CACHE.clear()
        _UpstreamCapture.last_request = {}

    # ── Files API upload ──────────────────────────────────────────────────

    def test_files_upload_returns_200_with_file_id(self):
        ct, body = _multipart_body("shot.png", _RED_PNG, "image/png")
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/files",
            data=body,
            headers={"Content-Type": ct, "Content-Length": str(len(body))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            self.assertEqual(resp.status, 200)
            data = json.loads(resp.read())
        self.assertIn("id", data)
        self.assertTrue(data["id"].startswith("file_"))
        self.assertEqual(data["filename"], "shot.png")
        self.assertEqual(data["mime_type"], "image/png")
        # Verify NOT forwarded to upstream.
        self.assertEqual(_UpstreamCapture.last_request, {})

    def test_files_upload_cached_for_later_use(self):
        ct, body = _multipart_body("img.png", _RED_PNG, "image/png")
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/files",
            data=body,
            headers={"Content-Type": ct, "Content-Length": str(len(body))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            data = json.loads(resp.read())
        fid = data["id"]
        cached = proxy._lookup_file(fid)
        self.assertIsNotNone(cached)
        self.assertEqual(base64.b64decode(cached["data"]), _RED_PNG)

    # ── /v1/messages file_id rewriting ────────────────────────────────────

    def _upload_and_get_fid(self, png=_RED_PNG):
        ct, body = _multipart_body("x.png", png, "image/png")
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/files",
            data=body,
            headers={"Content-Type": ct, "Content-Length": str(len(body))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            return json.loads(resp.read())["id"]

    def test_messages_file_block_rewritten_to_base64(self):
        fid = self._upload_and_get_fid()
        payload = json.dumps({
            "model": "claude-sonnet-4-6",
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "image", "source": {"type": "file", "file_id": fid}},
                        {"type": "text", "text": "describe the image"},
                    ],
                }
            ],
        }).encode()
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/messages",
            data=payload,
            headers={"Content-Type": "application/json", "Content-Length": str(len(payload))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            resp.read()
        upstream_body = json.loads(_UpstreamCapture.last_request["body"])
        src = upstream_body["messages"][0]["content"][0]["source"]
        self.assertEqual(src["type"], "base64")
        self.assertEqual(src["media_type"], "image/png")
        self.assertEqual(base64.b64decode(src["data"]), _RED_PNG)

    def test_messages_multi_turn_all_rewritten(self):
        fid = self._upload_and_get_fid()
        payload = json.dumps({
            "model": "claude-sonnet-4-6",
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "image", "source": {"type": "file", "file_id": fid}},
                    ],
                },
                {
                    "role": "assistant",
                    "content": [{"type": "text", "text": "I see a red square."}],
                },
                {
                    "role": "user",
                    "content": [
                        {"type": "image", "source": {"type": "file", "file_id": fid}},
                        {"type": "text", "text": "and now?"},
                    ],
                },
            ],
        }).encode()
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/messages",
            data=payload,
            headers={"Content-Type": "application/json", "Content-Length": str(len(payload))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            resp.read()
        upstream_msgs = json.loads(_UpstreamCapture.last_request["body"])["messages"]
        # _normalize_for_vision consolidates images into the last user turn.
        # turn 0 (non-last) should have a placeholder text, not an image.
        turn0_types = [b.get("type") for b in upstream_msgs[0]["content"]]
        self.assertNotIn("image", turn0_types, "images should be moved out of turn 0")
        # turn 2 (last user turn) should have both images (hoisted + original).
        turn2_images = [
            b for b in upstream_msgs[2]["content"] if b.get("type") == "image"
        ]
        self.assertEqual(len(turn2_images), 2, "both images should be in last turn")
        for img in turn2_images:
            self.assertEqual(img["source"]["type"], "base64")

    def test_files_api_beta_header_stripped(self):
        fid = self._upload_and_get_fid()
        payload = json.dumps({
            "model": "claude-sonnet-4-6",
            "messages": [{"role": "user", "content": "hi"}],
        }).encode()
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/messages",
            data=payload,
            headers={
                "Content-Type": "application/json",
                "Content-Length": str(len(payload)),
                "anthropic-beta": "files-api-2025-04-14",
            },
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            resp.read()
        up_headers = _UpstreamCapture.last_request.get("headers", {})
        for h, v in up_headers.items():
            if h.lower() == "anthropic-beta":
                self.assertNotIn("files-api", v.lower(),
                                 "files-api beta header should be stripped")

    # ── Regression: reasoning-effort rewriting still works ────────────────

    def test_vision_model_routing_when_image_present(self):
        """Model should be overridden to VISION_MODEL when the request has images."""
        os.environ["VISION_MODEL"] = "deepseek-chat"
        _reload_proxy()
        fid = self._upload_and_get_fid()
        payload = json.dumps({
            "model": "deepseek-v4-pro",
            "messages": [{"role": "user", "content": [
                {"type": "image", "source": {"type": "file", "file_id": fid}},
                {"type": "text", "text": "describe it"},
            ]}],
        }).encode()
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/messages",
            data=payload,
            headers={"Content-Type": "application/json", "Content-Length": str(len(payload))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            resp.read()
        upstream_body = json.loads(_UpstreamCapture.last_request["body"])
        self.assertEqual(upstream_body["model"], "deepseek-chat")

        os.environ.pop("VISION_MODEL", None)
        _reload_proxy()

    def test_vision_model_not_swapped_for_text_only(self):
        """Model must NOT be overridden when the request has no image blocks."""
        os.environ["VISION_MODEL"] = "deepseek-chat"
        _reload_proxy()
        payload = json.dumps({
            "model": "deepseek-v4-pro",
            "messages": [{"role": "user", "content": "hello, text only"}],
        }).encode()
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/messages",
            data=payload,
            headers={"Content-Type": "application/json", "Content-Length": str(len(payload))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            resp.read()
        upstream_body = json.loads(_UpstreamCapture.last_request["body"])
        self.assertEqual(upstream_body["model"], "deepseek-v4-pro")

        os.environ.pop("VISION_MODEL", None)
        _reload_proxy()

    def test_effort_not_applied_to_vision_route(self):
        """When routed to vision model, effort rewriting must be skipped.

        deepseek-chat (v4-flash) does not support extended thinking; injecting
        thinking params causes it to respond 'I cannot see this image.'
        """
        os.environ["VISION_MODEL"] = "deepseek-chat"
        os.environ["EFFORT_DEFAULT"] = "auto:high"
        _reload_proxy()
        fid = self._upload_and_get_fid()
        payload = json.dumps({
            "model": "deepseek-v4-pro",
            "messages": [{"role": "user", "content": [
                {"type": "image", "source": {"type": "file", "file_id": fid}},
                {"type": "text", "text": "describe it"},
            ]}],
        }).encode()
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/messages",
            data=payload,
            headers={"Content-Type": "application/json", "Content-Length": str(len(payload))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            resp.read()
        upstream_body = json.loads(_UpstreamCapture.last_request["body"])
        # Model must be overridden to vision model
        self.assertEqual(upstream_body["model"], "deepseek-chat")
        # Effort rewrite must NOT have added thinking
        self.assertNotIn("thinking", upstream_body,
                         "thinking must not be injected for vision routes")
        self.assertNotIn("reasoning_effort", upstream_body,
                         "reasoning_effort must not be injected for vision routes")

        os.environ.pop("VISION_MODEL", None)
        os.environ.pop("EFFORT_DEFAULT", None)
        _reload_proxy()

    # ── Regression: reasoning-effort rewriting still works ────────────────

    def test_effort_rewriting_unaffected(self):
        """Effort rewriting should still apply even when no files are involved."""
        os.environ["EFFORT_DEFAULT"] = "auto"
        _reload_proxy()

        payload = json.dumps({
            "model": "claude-sonnet-4-6",
            "thinking": {"type": "enabled", "budget_tokens": 5000},
            "messages": [{"role": "user", "content": "hello"}],
        }).encode()
        req = urllib.request.Request(
            f"{self.proxy_url}/v1/messages",
            data=payload,
            headers={"Content-Type": "application/json", "Content-Length": str(len(payload))},
            method="POST",
        )
        with urllib.request.urlopen(req) as resp:
            resp.read()
        upstream_body = json.loads(_UpstreamCapture.last_request["body"])
        # budget=5000 → bucket=high, auto→high regime: thinking enabled, no reasoning_effort.
        self.assertEqual(upstream_body.get("thinking", {}).get("type"), "enabled")
        self.assertNotIn("reasoning_effort", upstream_body)

        os.environ.pop("EFFORT_DEFAULT", None)
        _reload_proxy()


# ══════════════════════════════════════════════════════════════════════════════
# ── Unit tests for _normalize_for_vision ─────────────────────────────────────
# ══════════════════════════════════════════════════════════════════════════════

class TestNormalizeForVision(unittest.TestCase):
    """Unit tests for the _normalize_for_vision() function.

    Verifies that images are always consolidated into the last user turn,
    tool_use / tool_result blocks are flattened, and the top-level ``tools``
    and ``tool_choice`` keys are removed.
    """

    def _img(self) -> dict:
        return {
            "type": "image",
            "source": {"type": "base64", "media_type": "image/png", "data": _RED_PNG_B64},
        }

    def _text(self, text: str) -> dict:
        return {"type": "text", "text": text}

    def _normalize(self, obj: dict) -> dict:
        import copy
        obj_copy = copy.deepcopy(obj)
        proxy._normalize_for_vision(obj_copy, obj_copy["messages"])
        return obj_copy

    def test_single_turn_image_preserved(self):
        """Image already in last (only) turn stays there, image first."""
        obj = {
            "messages": [
                {"role": "user", "content": [self._img(), self._text("What is this?")]},
            ]
        }
        result = self._normalize(obj)
        content = result["messages"][0]["content"]
        self.assertEqual(content[0]["type"], "image")
        self.assertTrue(any(b["type"] == "text" for b in content))

    def test_image_injected_into_last_user_turn(self):
        """Image from an earlier user turn is moved to the last user turn."""
        obj = {
            "messages": [
                {"role": "user", "content": [self._img(), self._text("Hi")]},
                {"role": "assistant", "content": "Hello!"},
                {"role": "user", "content": [self._text("Describe the image.")]},
            ]
        }
        result = self._normalize(obj)
        first_content = result["messages"][0]["content"]
        self.assertFalse(any(b.get("type") == "image" for b in first_content))
        last_content = result["messages"][2]["content"]
        self.assertTrue(any(b.get("type") == "image" for b in last_content))
        self.assertEqual(last_content[0]["type"], "image")

    def test_tool_result_image_extracted_in_last_turn(self):
        """Image nested in tool_result in the last user turn is unwrapped."""
        obj = {
            "tools": [{"name": "Read", "description": "read file"}],
            "messages": [
                {"role": "user", "content": "What is this?"},
                {
                    "role": "assistant",
                    "content": [
                        {"type": "tool_use", "id": "t1", "name": "Read", "input": {"file_path": "/tmp/img.png"}},
                    ],
                },
                {
                    "role": "user",
                    "content": [
                        {"type": "tool_result", "tool_use_id": "t1", "content": [self._img()]},
                    ],
                },
            ],
        }
        result = self._normalize(obj)
        self.assertNotIn("tools", result)
        asst_content = result["messages"][1]["content"]
        self.assertEqual(asst_content[0]["type"], "text")
        self.assertIn("Read", asst_content[0]["text"])
        last_content = result["messages"][2]["content"]
        self.assertEqual(last_content[0]["type"], "image")
        self.assertFalse(any(b.get("type") == "tool_result" for b in last_content))

    def test_tool_result_image_in_earlier_turn_moves_to_last(self):
        """Image in a tool_result in an earlier turn is moved to the final turn."""
        obj = {
            "messages": [
                {"role": "user", "content": "What is this?"},
                {
                    "role": "assistant",
                    "content": [
                        {"type": "tool_use", "id": "t1", "name": "Read", "input": {"file_path": "/tmp/img.png"}},
                    ],
                },
                {
                    "role": "user",
                    "content": [
                        {"type": "tool_result", "tool_use_id": "t1", "content": [self._img()]},
                    ],
                },
                {"role": "assistant", "content": "I read the image."},
                {"role": "user", "content": [self._text("Describe it.")]},
            ],
        }
        result = self._normalize(obj)
        mid_content = result["messages"][2]["content"]
        self.assertFalse(any(b.get("type") == "image" for b in mid_content))
        self.assertFalse(any(b.get("type") == "tool_result" for b in mid_content))
        last_content = result["messages"][4]["content"]
        self.assertEqual(last_content[0]["type"], "image")

    def test_multiple_images_all_in_last_turn(self):
        """Multiple images from different turns all end up in the last turn."""
        obj = {
            "messages": [
                {"role": "user", "content": [self._img(), self._text("First.")]},
                {"role": "assistant", "content": "Got it."},
                {"role": "user", "content": [self._img(), self._text("Second.")]},
                {"role": "assistant", "content": "OK."},
                {"role": "user", "content": [self._text("Compare them.")]},
            ]
        }
        result = self._normalize(obj)
        last_content = result["messages"][4]["content"]
        image_blocks = [b for b in last_content if b.get("type") == "image"]
        self.assertEqual(len(image_blocks), 2)

    def test_tools_and_tool_choice_stripped(self):
        """Top-level tools and tool_choice keys are removed."""
        obj = {
            "tools": [{"name": "Foo"}],
            "tool_choice": {"type": "auto"},
            "messages": [
                {"role": "user", "content": [self._img(), self._text("Hi")]},
            ],
        }
        result = self._normalize(obj)
        self.assertNotIn("tools", result)
        self.assertNotIn("tool_choice", result)

    def test_no_images_returns_zero(self):
        """When there are no images, the function returns 0."""
        obj = {
            "messages": [
                {"role": "user", "content": "Hello"},
                {"role": "assistant", "content": "Hi"},
                {"role": "user", "content": "How are you?"},
            ]
        }
        import copy
        original = copy.deepcopy(obj)
        count = proxy._normalize_for_vision(obj, obj["messages"])
        self.assertEqual(count, 0)
        self.assertEqual(obj["messages"], original["messages"])

    def test_files_api_path_full_pipeline(self):
        """Files-API path: file_id rewrite + vision normalize delivers image to last turn."""
        fake_id = "file_testvision"
        with proxy._FILE_CACHE_LOCK:
            proxy._FILE_CACHE[fake_id] = {
                "data": _RED_PNG_B64,
                "media_type": "image/png",
                "filename": "test.png",
                "size": len(_RED_PNG_B64),
            }
        os.environ["VISION_MODEL"] = "deepseek-chat"
        _reload_proxy()
        with proxy._FILE_CACHE_LOCK:
            proxy._FILE_CACHE[fake_id] = {
                "data": _RED_PNG_B64,
                "media_type": "image/png",
                "filename": "test.png",
                "size": len(_RED_PNG_B64),
            }

        body = json.dumps({
            "model": "claude-opus-4-7",
            "max_tokens": 10,
            "messages": [
                {"role": "user", "content": "Hello"},
                {"role": "assistant", "content": "Hi!"},
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "Look at this:"},
                        {"type": "image", "source": {"type": "file", "file_id": fake_id}},
                    ],
                },
            ],
        }).encode()

        result_bytes = proxy._rewrite_body(body)
        os.environ.pop("VISION_MODEL", None)
        _reload_proxy()

        result = json.loads(result_bytes)
        self.assertEqual(result["model"], "deepseek-chat")
        last_content = result["messages"][-1].get("content", [])
        image_blocks = [b for b in last_content if isinstance(b, dict) and b.get("type") == "image"]
        self.assertEqual(len(image_blocks), 1)
        self.assertEqual(image_blocks[0]["source"]["data"], _RED_PNG_B64)


if __name__ == "__main__":
    unittest.main(verbosity=2)
