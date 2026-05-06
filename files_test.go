// Tests for the Files API mock — multipart parsing, raw-binary
// fallback, cache hit/miss, mime detection, response shape, and a
// concurrent-access stress (run with `go test -race`).
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// resetFileCache clears the global cache between tests. The package
// has process-lifetime state, so tests that observe entry counts must
// start from a known-empty state.
func resetFileCache(t *testing.T) {
	t.Helper()
	fileCacheMu.Lock()
	fileCache = map[string]fileEntry{}
	fileCacheMu.Unlock()
}

// buildMultipart returns a multipart/form-data body with one file part.
func buildMultipart(t *testing.T, fieldName, filename, contentType string, data []byte) (body *bytes.Buffer, ct string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{
		fmt.Sprintf(`form-data; name=%q; filename=%q`, fieldName, filename),
	}
	if contentType != "" {
		hdr["Content-Type"] = []string{contentType}
	}
	part, err := w.CreatePart(hdr)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("part.Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("multipart.Close: %v", err)
	}
	return &buf, w.FormDataContentType()
}

// doUpload runs filesHandler against a constructed request and decodes
// the JSON response. Fatal on non-200.
func doUpload(t *testing.T, body io.Reader, contentType string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	filesHandler(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type: got %q want application/json", got)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestFilesHandler_MultipartUpload covers the canonical Claude Code
// flow: multipart/form-data with a single image part. The cache should
// pick up exactly one entry and the response should match the
// Anthropic Files API shape.
func TestFilesHandler_MultipartUpload(t *testing.T) {
	resetFileCache(t)

	payload := []byte("\x89PNG\r\n\x1a\nfake-png-bytes")
	body, ct := buildMultipart(t, "file", "screenshot.png", "image/png", payload)

	out := doUpload(t, body, ct)

	// Response shape
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "file_") || len(id) != len("file_")+32 {
		t.Errorf("id=%q expected file_<32-hex>", id)
	}
	if out["type"] != "file" {
		t.Errorf("type=%v want \"file\"", out["type"])
	}
	if out["filename"] != "screenshot.png" {
		t.Errorf("filename=%v want screenshot.png", out["filename"])
	}
	if out["mime_type"] != "image/png" {
		t.Errorf("mime_type=%v want image/png", out["mime_type"])
	}
	if out["size"].(float64) != float64(len(payload)) {
		t.Errorf("size=%v want %d", out["size"], len(payload))
	}
	if out["downloadable"] != false {
		t.Errorf("downloadable=%v want false", out["downloadable"])
	}
	if _, ok := out["created_at"].(string); !ok {
		t.Errorf("created_at missing or non-string: %v", out["created_at"])
	}

	// Cache populated with a base64 round-trip of the payload.
	fileCacheMu.RLock()
	entry, ok := fileCache[id]
	fileCacheMu.RUnlock()
	if !ok {
		t.Fatalf("cache miss for id=%s", id)
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Errorf("round-trip mismatch: got %q want %q", decoded, payload)
	}
}

// TestFilesHandler_RawBinaryUpload covers the application/octet-stream
// branch: no multipart wrapper, just a raw body. The mime type comes
// from the request header.
func TestFilesHandler_RawBinaryUpload(t *testing.T) {
	resetFileCache(t)

	payload := []byte("raw-bytes-here")
	out := doUpload(t, bytes.NewReader(payload), "application/octet-stream")

	id := out["id"].(string)
	// The handler doesn't know the original filename for raw uploads,
	// so it stamps "upload". When the declared type is octet-stream
	// and there's no extension, the resolver keeps that as the final
	// answer.
	if out["filename"] != "upload" {
		t.Errorf("filename=%v want upload", out["filename"])
	}
	if out["mime_type"] != "application/octet-stream" {
		t.Errorf("mime_type=%v want application/octet-stream", out["mime_type"])
	}
	if out["size"].(float64) != float64(len(payload)) {
		t.Errorf("size=%v want %d", out["size"], len(payload))
	}

	fileCacheMu.RLock()
	_, ok := fileCache[id]
	fileCacheMu.RUnlock()
	if !ok {
		t.Errorf("raw-binary upload missing from cache")
	}
}

// TestFilesHandler_RawNonOctet — non-multipart, non-octet-stream
// content type (e.g. image/png posted directly). The header type
// should win over the octet-stream fallback.
func TestFilesHandler_RawNonOctet(t *testing.T) {
	resetFileCache(t)
	payload := []byte("\x89PNG\r\nbytes")
	out := doUpload(t, bytes.NewReader(payload), "image/png")
	if out["mime_type"] != "image/png" {
		t.Errorf("mime_type=%v want image/png", out["mime_type"])
	}
}

// TestFilesHandler_MultipartMimeFromExtension — when a multipart part
// declares no Content-Type, the handler must fall back to extension
// detection via mime.TypeByExtension.
func TestFilesHandler_MultipartMimeFromExtension(t *testing.T) {
	resetFileCache(t)

	body, ct := buildMultipart(t, "file", "doc.json", "" /* no content-type on part */, []byte(`{"k":1}`))
	out := doUpload(t, body, ct)

	mt, _ := out["mime_type"].(string)
	// mime.TypeByExtension(".json") on most platforms yields
	// application/json; we accept any application/json variant just in
	// case the host adds a ;charset suffix that resolveMimeType
	// already strips.
	if mt != "application/json" {
		t.Errorf("mime_type=%q want application/json", mt)
	}
}

// TestLookupFile_HitMiss exercises both LookupFile branches and the
// outcome attribute path (no public way to read counters here, but the
// race detector and the boolean return are enough to validate the
// code path).
func TestLookupFile_HitMiss(t *testing.T) {
	resetFileCache(t)

	payload := []byte("hello world")
	body, ct := buildMultipart(t, "file", "hello.txt", "text/plain", payload)
	out := doUpload(t, body, ct)
	id := out["id"].(string)

	data, mimeType, ok := LookupFile(id)
	if !ok {
		t.Fatalf("LookupFile miss for fresh id=%s", id)
	}
	if mimeType != "text/plain" {
		t.Errorf("mimeType=%q want text/plain", mimeType)
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Errorf("round-trip mismatch")
	}

	if _, _, ok := LookupFile("file_deadbeef"); ok {
		t.Errorf("expected miss for unknown id")
	}
	// Empty IDs and malformed IDs should also miss cleanly.
	if _, _, ok := LookupFile(""); ok {
		t.Errorf("expected miss for empty id")
	}
}

// TestFilesHandler_EmptyBody — empty multipart parts and zero-length
// raw bodies should return 400, not poison the cache.
func TestFilesHandler_EmptyBody(t *testing.T) {
	resetFileCache(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/files", bytes.NewReader(nil))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	filesHandler(rec, req)
	if rec.Result().StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Result().StatusCode)
	}

	fileCacheMu.RLock()
	n := len(fileCache)
	fileCacheMu.RUnlock()
	if n != 0 {
		t.Errorf("cache populated on empty upload: %d entries", n)
	}
}

// TestFilesHandler_MethodNotAllowed — only POST is mocked. GET / etc.
// must short-circuit so CDS-15's mux doesn't accidentally treat them
// as uploads.
func TestFilesHandler_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	rec := httptest.NewRecorder()
	filesHandler(rec, req)
	if rec.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", rec.Result().StatusCode)
	}
}

// TestFilesHandler_MockResponseShape pins the exact JSON keys returned,
// so a future refactor doesn't accidentally drop a field that Claude
// Code depends on. Ordering is irrelevant; presence and types matter.
func TestFilesHandler_MockResponseShape(t *testing.T) {
	resetFileCache(t)
	body, ct := buildMultipart(t, "file", "x.png", "image/png", []byte("data"))
	out := doUpload(t, body, ct)

	want := map[string]string{
		"id":           "string",
		"type":         "string",
		"filename":     "string",
		"mime_type":    "string",
		"size":         "float64", // JSON numbers decode to float64
		"created_at":   "string",
		"downloadable": "bool",
	}
	for k, kind := range want {
		v, ok := out[k]
		if !ok {
			t.Errorf("response missing key %q", k)
			continue
		}
		got := fmt.Sprintf("%T", v)
		if got != kind {
			t.Errorf("key %q: type=%s want %s", k, got, kind)
		}
	}
}

// TestFilesHandler_ConcurrentUploadsAndLookups stress-tests the
// RWMutex: many goroutines uploading and many reading concurrently.
// Run with `go test -race` to validate.
func TestFilesHandler_ConcurrentUploadsAndLookups(t *testing.T) {
	t.Parallel()
	resetFileCache(t)

	const writers = 16
	const readers = 16
	const perWriter = 8

	// Channel of minted IDs so readers see hits as well as misses.
	ids := make(chan string, writers*perWriter)

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				payload := []byte(fmt.Sprintf("worker-%d-blob-%d", w, i))
				body, ct := buildMultipart(t, "file",
					fmt.Sprintf("w%d-i%d.bin", w, i),
					"application/octet-stream", payload)
				req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
				req.Header.Set("Content-Type", ct)
				rec := httptest.NewRecorder()
				filesHandler(rec, req)
				if rec.Result().StatusCode != 200 {
					t.Errorf("writer %d-%d: status=%d", w, i, rec.Result().StatusCode)
					continue
				}
				var out map[string]any
				if err := json.NewDecoder(rec.Result().Body).Decode(&out); err != nil {
					t.Errorf("decode: %v", err)
					continue
				}
				ids <- out["id"].(string)
			}
		}(w)
	}

	// Readers spin until they've seen enough hits, then bail. They
	// also sprinkle in known-miss lookups to exercise both branches.
	for r := 0; r < readers; r++ {
		go func(r int) {
			defer wg.Done()
			seen := 0
			for seen < perWriter {
				select {
				case id := <-ids:
					_, _, ok := LookupFile(id)
					if !ok {
						t.Errorf("reader %d: cache miss for fresh id=%s", r, id)
					}
					// Re-publish so other readers also see it.
					select {
					case ids <- id:
					default:
					}
					seen++
				default:
					_, _, _ = LookupFile("file_doesnotexist")
				}
			}
		}(r)
	}

	wg.Wait()
	close(ids)

	fileCacheMu.RLock()
	n := len(fileCache)
	fileCacheMu.RUnlock()
	if n != writers*perWriter {
		t.Errorf("cache size=%d want %d", n, writers*perWriter)
	}
}

// TestResolveMimeType_Fallbacks exercises the helper's branch table
// directly. Pure unit test, no HTTP machinery.
func TestResolveMimeType_Fallbacks(t *testing.T) {
	cases := []struct {
		declared string
		filename string
		want     string
	}{
		{"image/png", "x.png", "image/png"},
		{"image/png; charset=utf-8", "x.png", "image/png"}, // strip params
		{"", "x.png", "image/png"},                         // extension lookup
		{"application/octet-stream", "x.png", "image/png"}, // override generic
		{"", "noext", "application/octet-stream"},          // ultimate fallback
		{"", "", "application/octet-stream"},
	}
	for _, tc := range cases {
		got := resolveMimeType(tc.declared, tc.filename)
		if got != tc.want {
			t.Errorf("resolveMimeType(%q, %q) = %q, want %q",
				tc.declared, tc.filename, got, tc.want)
		}
	}
}

// TestTopLevelMediaType pins the OTLP attribute helper.
func TestTopLevelMediaType(t *testing.T) {
	cases := map[string]string{
		"image/png":                "image",
		"application/octet-stream": "application",
		"text/plain":               "text",
		"":                         "unknown",
		"weirdvalue":               "weirdvalue",
	}
	for in, want := range cases {
		if got := topLevelMediaType(in); got != want {
			t.Errorf("topLevelMediaType(%q) = %q want %q", in, got, want)
		}
	}
}
