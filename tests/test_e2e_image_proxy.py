"""
End-to-end integration test: full proxy pipeline against the live DeepSeek API.

Requires:
  - macOS keychain entry  service=claude-ds  account=deepseek  (or DEEPSEEK_API_KEY env)
  - Network access to https://api.deepseek.com

Run:
  cd .worktrees/cds-4 && python3 -m unittest tests.test_e2e_image_proxy -v
"""
import http.client
import json
import os
import struct
import subprocess
import sys
import threading
import time
import unittest
import zlib

# ── Helpers ──────────────────────────────────────────────────────────────────

PROXY_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEEPSEEK_UPSTREAM = "https://api.deepseek.com/anthropic"
VISION_MODEL = "deepseek-chat"  # maps to deepseek-v4-flash; supports vision


def _get_api_key():
    """Return the DeepSeek API key from env or macOS keychain."""
    if k := os.environ.get("DEEPSEEK_API_KEY"):
        return k
    try:
        return subprocess.check_output(
            ["security", "find-generic-password", "-s", "claude-ds",
             "-a", "deepseek", "-w"],
            text=True, stderr=subprocess.DEVNULL,
        ).strip()
    except Exception:
        return None


def _make_png(width=400, height=80, top_rgb=(220, 20, 20), bot_rgb=(20, 20, 180)):
    """Create a simple two-tone PNG (top color / bottom color) in pure stdlib."""
    rows = []
    for y in range(height):
        rgb = top_rgb if y < height // 2 else bot_rgb
        rows.append(b"\x00" + bytes(rgb) * width)
    raw = b"".join(rows)
    compressed = zlib.compress(raw, 6)

    def chunk(name, data):
        c = name + data
        return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c) & 0xFFFFFFFF)

    png = b"\x89PNG\r\n\x1a\n"
    png += chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
    png += chunk(b"IDAT", compressed)
    png += chunk(b"IEND", b"")
    return png


def _multipart_body(image_bytes, boundary=b"testboundary"):
    return (
        b"--" + boundary + b"\r\n"
        b'Content-Disposition: form-data; name="file"; filename="test.png"\r\n'
        b"Content-Type: image/png\r\n\r\n"
        + image_bytes
        + b"\r\n--" + boundary + b"--\r\n"
    )


# ── Test ─────────────────────────────────────────────────────────────────────

class TestE2EImageProxy(unittest.TestCase):

    @classmethod
    def setUpClass(cls):
        cls.api_key = _get_api_key()
        if not cls.api_key:
            raise unittest.SkipTest("No DeepSeek API key available")

        cls.image_bytes = _make_png()
        env = os.environ.copy()
        env["UPSTREAM_BASE_URL"] = DEEPSEEK_UPSTREAM
        env["PROXY_DEBUG"] = "1"

        cls.proxy_proc = subprocess.Popen(
            [sys.executable, "claude-ds-proxy.py"],
            cwd=PROXY_DIR,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        cls.proxy_port = int(cls.proxy_proc.stdout.readline().strip())

        cls.proxy_log = []
        def _drain(stream):
            for line in stream:
                cls.proxy_log.append(line.decode(errors="replace").rstrip())
        threading.Thread(target=_drain, args=(cls.proxy_proc.stdout,), daemon=True).start()
        threading.Thread(target=_drain, args=(cls.proxy_proc.stderr,), daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        if hasattr(cls, "proxy_proc"):
            cls.proxy_proc.terminate()
            cls.proxy_proc.wait(timeout=5)

    def _conn(self):
        return http.client.HTTPConnection("127.0.0.1", self.proxy_port, timeout=60)

    def _base_headers(self):
        return {
            "x-api-key": self.api_key,
            "anthropic-version": "2023-06-01",
        }

    # ── Individual test steps (ordered) ──────────────────────────────────────

    def test_1_upload_returns_file_id(self):
        body = _multipart_body(self.image_bytes)
        headers = {**self._base_headers(),
                   "Content-Type": "multipart/form-data; boundary=testboundary",
                   "anthropic-beta": "files-api-2025-04-14"}
        conn = self._conn()
        conn.request("POST", "/v1/files", body, headers)
        r = conn.getresponse()
        resp = json.loads(r.read())
        self.assertEqual(r.status, 200, msg=resp)
        self.assertIn("id", resp)
        self.assertTrue(resp["id"].startswith("file_"))
        self.assertEqual(resp["mime_type"], "image/png")
        type(self).file_id = resp["id"]   # share across tests

    def test_2_message_with_file_id_reaches_deepseek(self):
        self.assertTrue(hasattr(type(self), "file_id"),
                        "test_1 must run first to set file_id")
        payload = {
            "model": VISION_MODEL,
            "max_tokens": 80,
            "messages": [{"role": "user", "content": [
                {"type": "image",
                 "source": {"type": "file", "file_id": type(self).file_id}},
                {"type": "text", "text": "Describe what you see. Be concise."}
            ]}],
        }
        headers = {**self._base_headers(),
                   "Content-Type": "application/json",
                   "anthropic-beta": "files-api-2025-04-14"}
        conn = self._conn()
        conn.request("POST", "/v1/messages", json.dumps(payload).encode(), headers)
        r = conn.getresponse()
        resp = json.loads(r.read())
        self.assertEqual(r.status, 200, msg=resp)
        texts = [b["text"] for b in resp.get("content", []) if b.get("type") == "text"]
        self.assertTrue(texts, "Expected at least one text block in response")
        # DeepSeek processed the image and returned a non-empty description
        self.assertTrue(any(t.strip() for t in texts))

    def test_3_proxy_log_shows_rewrite(self):
        """Confirm debug log recorded the file_id→base64 swap and header strip."""
        file_id = getattr(type(self), "file_id", None)
        if file_id is None:
            self.skipTest("test_1 did not run")
        log_text = "\n".join(self.proxy_log)
        self.assertIn(f"swapped file_id='{file_id}'", log_text,
                      "Proxy log should show file_id swap")
        self.assertIn("stripped header anthropic-beta", log_text,
                      "Proxy log should show header stripping")


if __name__ == "__main__":
    unittest.main(verbosity=2)
