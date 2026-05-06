// Files API mock — intercepts POST /v1/files locally and never forwards
// upstream. Anthropic's Files API beta is bridged in-process so DeepSeek
// (which has no Files API) can still receive image content as inline
// base64 in the subsequent /v1/messages request.
//
// Semantics (matched against claude-ds-proxy.py):
//
//  1. Multipart bodies (Content-Type: multipart/form-data; boundary=...)
//     are parsed via mime/multipart. The first part with a filename or
//     non-empty Content-Type is taken as the file payload.
//  2. Anything else with a non-empty body — typically
//     application/octet-stream — is treated as a single raw blob with
//     the request's Content-Type as the mime type (default
//     application/octet-stream).
//  3. The bytes are base64-encoded and stashed in a process-lifetime
//     cache keyed by a fresh `file_<32-hex>` ID generated from
//     crypto/rand.
//  4. The response shape mirrors the Anthropic Files API success
//     payload exactly: `{id, type, filename, mime_type, size,
//     created_at, downloadable}` with `created_at` as RFC3339.
//
// The cache is exposed to the rest of the binary (CDS-18 body rewriter)
// via LookupFile.
//
// Concurrency: the cache is a sync.RWMutex-protected map. Writers
// (filesHandler) take the write lock; readers (LookupFile) take the
// read lock. Verified with `go test -race`.
//
// OTLP observability: claude_ds.files.upload.count / upload.size /
// cache.entries / cache.bytes / lookup.count{outcome=hit|miss}. Meter
// is fetched once at init via otel.Meter("claude-ds-proxy"); until
// CDS-23 wires the global provider it's a no-op meter and instrument
// calls are zero-cost. Redaction: never record bytes, ID, or
// filename — only sizes, counts, top-level mime category, and outcome.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// fileEntry is one cached file. Lives in memory for the duration of
// the proxy process; cleared when claude exits.
type fileEntry struct {
	data      string // base64 (StdEncoding) of the original bytes
	mimeType  string
	filename  string
	size      int64
	createdAt time.Time
}

// fileCache holds all uploaded files keyed by their generated file_id.
//
// A single sync.RWMutex protects the map. Lookups are read-mostly once
// the upload lands, so RWMutex (rather than sync.Mutex) lets multiple
// /v1/messages rewrites resolve IDs concurrently without contending
// with each other.
var (
	fileCacheMu sync.RWMutex
	fileCache   = map[string]fileEntry{}
)

// multipartMaxMemory is the in-memory threshold for ParseMultipartForm.
// Above this, parts spill to disk via os.CreateTemp — but we're called
// only on /v1/files where Claude Code uploads images of ~a few MB at
// most, so 64 MB is generous and keeps the path entirely in RAM in
// practice.
const multipartMaxMemory = 64 << 20 // 64 MB

// ---------------------------------------------------------------------
// OTLP instruments
// ---------------------------------------------------------------------

var (
	filesUploadCount  metric.Int64Counter
	filesUploadSize   metric.Int64Histogram
	filesCacheEntries metric.Int64UpDownCounter
	filesCacheBytes   metric.Int64UpDownCounter
	filesLookupCount  metric.Int64Counter
)

func init() {
	// otel.Meter returns a no-op meter when no MeterProvider is wired,
	// so this init is safe even before CDS-23 lands.
	m := otel.Meter("claude-ds-proxy")

	filesUploadCount, _ = m.Int64Counter(
		"claude_ds.files.upload.count",
		metric.WithDescription("Files API uploads ingested."),
		metric.WithUnit("1"),
	)
	filesUploadSize, _ = m.Int64Histogram(
		"claude_ds.files.upload.size",
		metric.WithDescription("Per-upload size in bytes (raw, pre-base64)."),
		metric.WithUnit("By"),
	)
	// cache.entries / cache.bytes are documented as observable gauges
	// in the OTLP spec. We model them as UpDownCounters: the OTLP
	// pipeline reports the cumulative running value, which gives the
	// same dashboards (live count and live byte total) without
	// requiring a callback registration on the global provider — that
	// wiring lives in CDS-23 and would create a circular import here.
	filesCacheEntries, _ = m.Int64UpDownCounter(
		"claude_ds.files.cache.entries",
		metric.WithDescription("Live count of cached file entries."),
		metric.WithUnit("1"),
	)
	filesCacheBytes, _ = m.Int64UpDownCounter(
		"claude_ds.files.cache.bytes",
		metric.WithDescription("Sum of cached base64 payload sizes (post-encode)."),
		metric.WithUnit("By"),
	)
	filesLookupCount, _ = m.Int64Counter(
		"claude_ds.files.lookup.count",
		metric.WithDescription("LookupFile resolutions, partitioned by outcome."),
		metric.WithUnit("1"),
	)
}

// ---------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------

// filesHandler is the HTTP handler for POST /v1/files. CDS-15 registers
// it on the proxy mux. It never forwards to upstream: the file is
// stored locally and a mock Anthropic Files API response is returned,
// so the upstream model can later be fed inline base64 by CDS-18.
func filesHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, filename, mimeType, err := readUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) == 0 {
		http.Error(w, "empty file upload", http.StatusBadRequest)
		return
	}

	// Resolve the mime type. Order matches Python:
	//   1. explicit part/header content-type (sans parameters)
	//   2. extension lookup via mime.TypeByExtension
	//   3. application/octet-stream fallback
	mimeType = resolveMimeType(mimeType, filename)

	id, err := newFileID()
	if err != nil {
		http.Error(w, "id generation failed", http.StatusInternalServerError)
		return
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	size := int64(len(data))
	now := time.Now().UTC()

	entry := fileEntry{
		data:      encoded,
		mimeType:  mimeType,
		filename:  filename,
		size:      size,
		createdAt: now,
	}

	fileCacheMu.Lock()
	fileCache[id] = entry
	fileCacheMu.Unlock()

	// OTLP — sizes/counts only. Never the bytes, the ID, or the filename.
	mediaTypeAttr := attribute.String("claude_ds.files.media_type", topLevelMediaType(mimeType))
	filesUploadCount.Add(ctx, 1, metric.WithAttributes(mediaTypeAttr))
	filesUploadSize.Record(ctx, size, metric.WithAttributes(mediaTypeAttr))
	filesCacheEntries.Add(ctx, 1)
	filesCacheBytes.Add(ctx, int64(len(encoded)))

	resp := map[string]any{
		"id":           id,
		"type":         "file",
		"filename":     filename,
		"mime_type":    mimeType,
		"size":         size,
		"created_at":   now.Format(time.RFC3339),
		"downloadable": false,
	}

	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// LookupFile resolves a `file_<hex>` ID to its cached base64 data and
// detected mime type. CDS-18 calls this when rewriting /v1/messages
// bodies that reference uploaded files.
//
// A miss is a real bug: it means the body referenced a file_id we
// never minted. The OTLP outcome attribute lets the dashboard alert
// on it.
func LookupFile(id string) (data string, mimeType string, ok bool) {
	fileCacheMu.RLock()
	entry, ok := fileCache[id]
	fileCacheMu.RUnlock()

	outcome := "miss"
	if ok {
		outcome = "hit"
		data = entry.data
		mimeType = entry.mimeType
	}
	filesLookupCount.Add(
		context.Background(),
		1,
		metric.WithAttributes(attribute.String("claude_ds.files.lookup.outcome", outcome)),
	)
	return data, mimeType, ok
}

// ---------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------

// readUpload extracts (bytes, filename, mimeType) from the request,
// branching on Content-Type:
//   - multipart/form-data → first file part
//   - anything else with a body → raw blob
//
// mimeType returned here is the *raw* declared type from the part or
// the request header. The caller layers extension fallback on top via
// resolveMimeType.
func readUpload(r *http.Request) (data []byte, filename, mimeType string, err error) {
	contentType := r.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(contentType)

	// Multipart fast path — has a boundary and parses cleanly.
	if strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
		return readMultipart(r, params["boundary"])
	}

	// Raw-binary fallback (application/octet-stream, image/png, etc.,
	// or no Content-Type at all). Read the whole body.
	body, rerr := io.ReadAll(r.Body)
	if rerr != nil {
		return nil, "", "", rerr
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return body, "upload", mediaType, nil
}

// readMultipart walks the parts in order and returns the first part
// that looks like a file (has a filename or a non-empty Content-Type
// header). Falls back to the very first part if nothing matches.
func readMultipart(r *http.Request, boundary string) ([]byte, string, string, error) {
	mr := multipart.NewReader(r.Body, boundary)
	form, err := mr.ReadForm(multipartMaxMemory)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = form.RemoveAll() }()

	// File parts (those with a filename) live in form.File. Iterate in
	// a stable-ish order — Go's map iteration is randomised, but the
	// inner slice preserves submission order, and the canonical
	// Anthropic flow only sends a single file part anyway.
	for _, headers := range form.File {
		if len(headers) == 0 {
			continue
		}
		fh := headers[0]
		f, oerr := fh.Open()
		if oerr != nil {
			return nil, "", "", oerr
		}
		body, rerr := io.ReadAll(f)
		_ = f.Close()
		if rerr != nil {
			return nil, "", "", rerr
		}

		filename := fh.Filename
		if filename == "" {
			filename = "upload"
		}
		ct := fh.Header.Get("Content-Type")
		return body, filename, ct, nil
	}

	// No file parts — try a value part with a non-empty body. Mirrors
	// the Python fallback where `name="file"` arrives as a plain value.
	for _, vals := range form.Value {
		for _, v := range vals {
			if v == "" {
				continue
			}
			return []byte(v), "upload", "", nil
		}
	}

	return nil, "", "", errors.New("multipart body had no file or value parts")
}

// resolveMimeType applies the fallback chain documented above and
// guarantees a non-empty result.
func resolveMimeType(declared, filename string) string {
	declared = strings.TrimSpace(strings.SplitN(declared, ";", 2)[0])
	if declared != "" && declared != "application/octet-stream" {
		return declared
	}
	if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
		if guessed := mime.TypeByExtension(ext); guessed != "" {
			return strings.SplitN(guessed, ";", 2)[0]
		}
	}
	if declared != "" {
		return declared
	}
	return "application/octet-stream"
}

// newFileID returns a fresh `file_<32-hex>` identifier. 16 bytes of
// crypto/rand entropy → 128 bits, comfortably collision-resistant for
// the lifetime of a process.
func newFileID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "file_" + hex.EncodeToString(buf[:]), nil
}

// topLevelMediaType returns the part before the slash ("image",
// "application", "text", ...) for the OTLP attribute. Matches the
// observability spec which only carries the top-level category — the
// subtype could leak content-discriminating info on small fleets.
func topLevelMediaType(mt string) string {
	if i := strings.Index(mt, "/"); i > 0 {
		return mt[:i]
	}
	if mt == "" {
		return "unknown"
	}
	return mt
}
