package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// cmpSemver
// ---------------------------------------------------------------------

func TestCmpSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		{"1.2.3", "v1.2.3", 0},
		{"1.2.4", "1.2.3", 1},
		{"1.2.3", "1.2.4", -1},
		{"2.0.0", "1.99.99", 1},
		{"1.0", "1.0.0", 0},
		{"1", "1.0.0", 0},
		{"1.0.0-alpha", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		{"0.9.0-dev", "0.9.0", -1},
		// Garbage components → 0; "1.0.x" ↔ "1.0.0" tied numerically,
		// neither has a prerelease, so equal.
		{"1.0.x", "1.0.0", 0},
	}
	for _, c := range cases {
		got := cmpSemver(c.a, c.b)
		if got != c.want {
			t.Errorf("cmpSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------
// upgrade flow — happy path
// ---------------------------------------------------------------------

// fakeReleaseServer stands up an httptest.Server that mimics the GitHub
// API endpoints we hit. Provide the bin contents and tag; the server
// computes its own checksums.txt.
type fakeReleaseServer struct {
	server      *httptest.Server
	tag         string
	binBytes    []byte
	binName     string
	statusOnAPI int    // override status for the /releases/latest endpoint
	corruptSums bool   // serve a checksums.txt with a bad digest
	missingBin  bool   // omit the platform binary asset from the manifest
	missingSums bool   // omit checksums.txt
	gotAuth     string // last Authorization header on the API call
}

func newFakeReleaseServer(t *testing.T, tag string, binBytes []byte) *fakeReleaseServer {
	t.Helper()
	f := &fakeReleaseServer{
		tag:      tag,
		binBytes: binBytes,
		binName:  fmt.Sprintf("claude-ds-%s-%s", runtime.GOOS, runtime.GOARCH),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/earchibald/claude-ds/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		if f.statusOnAPI != 0 {
			http.Error(w, "rate limited", f.statusOnAPI)
			return
		}
		var assets []string
		if !f.missingBin {
			assets = append(assets, fmt.Sprintf(
				`{"name":%q,"browser_download_url":"%s/dl/%s"}`,
				f.binName, f.server.URL, f.binName))
		}
		if !f.missingSums {
			assets = append(assets, fmt.Sprintf(
				`{"name":"checksums.txt","browser_download_url":"%s/dl/checksums.txt"}`,
				f.server.URL))
		}
		body := fmt.Sprintf(
			`{"tag_name":%q,"assets":[%s]}`,
			f.tag, strings.Join(assets, ","))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case f.binName:
			_, _ = w.Write(f.binBytes)
		case "checksums.txt":
			h := sha256.Sum256(f.binBytes)
			digest := hex.EncodeToString(h[:])
			if f.corruptSums {
				digest = strings.Repeat("0", 64)
			}
			_, _ = fmt.Fprintf(w, "%s  %s\n", digest, f.binName)
			_, _ = fmt.Fprintf(w, "%s  some-other-asset\n", strings.Repeat("a", 64))
		default:
			http.NotFound(w, r)
		}
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// scriptBinary writes a tiny shell script with the given exit code so
// the smoke test can exercise it. The script is opaque on darwin/linux.
func scriptBinary(t *testing.T, dir string, exitCode int, schemaPrint string) []byte {
	t.Helper()
	body := "#!/bin/sh\n"
	if schemaPrint != "" {
		body += `if [ "$1" = "--print-schema" ]; then echo ` + schemaPrint + `; exit 0; fi
`
	}
	body += fmt.Sprintf("if [ \"$1\" = \"--version\" ]; then echo claude-ds 9.9.9; exit %d; fi\n", exitCode)
	body += fmt.Sprintf("exit %d\n", exitCode)
	return []byte(body)
}

// installFakeBinary writes `bin` to <dir>/claude-ds with mode 0755 and
// returns its path. Acts as the "currently installed" binary the
// updater will replace.
func installFakeBinary(t *testing.T, dir string, bin []byte) string {
	t.Helper()
	p := filepath.Join(dir, "claude-ds")
	if err := os.WriteFile(p, bin, 0o755); err != nil {
		t.Fatalf("install fake binary: %v", err)
	}
	return p
}

func TestUpgradeHappyPath(t *testing.T) {
	dir := t.TempDir()
	oldBin := scriptBinary(t, dir, 0, "")
	binPath := installFakeBinary(t, dir, oldBin)

	newBin := scriptBinary(t, dir, 0, "")
	srv := newFakeReleaseServer(t, "v0.99.0", newBin)

	c := &updateClient{
		apiBase:    srv.server.URL,
		owner:      "earchibald",
		repo:       "claude-ds",
		httpc:      srv.server.Client(),
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		currentSc:  CurrentSchema,
		current:    "0.1.0",
		execPath:   binPath,
		// no probeSchema → schema-bump branch is skipped
	}

	var stdout, stderr strings.Builder
	if err := c.upgrade(context.Background(), &stdout, &stderr); err != nil {
		t.Fatalf("upgrade: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated to v0.99.0") {
		t.Errorf("expected success message, got %q", stdout.String())
	}
	gotBytes, _ := os.ReadFile(binPath)
	if string(gotBytes) != string(newBin) {
		t.Errorf("binary contents not replaced")
	}
	if _, err := os.Stat(binPath + ".old"); !os.IsNotExist(err) {
		t.Errorf("expected %s.old to be cleaned up; err=%v", binPath, err)
	}
}

func TestUpgradeAlreadyUpToDate(t *testing.T) {
	dir := t.TempDir()
	binPath := installFakeBinary(t, dir, []byte("dummy"))
	srv := newFakeReleaseServer(t, "v0.5.0", []byte("ignored"))
	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		current: "0.5.0", execPath: binPath,
	}
	var stdout strings.Builder
	if err := c.upgrade(context.Background(), &stdout, &stdout); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !strings.Contains(stdout.String(), "already up to date") {
		t.Errorf("expected up-to-date msg, got %q", stdout.String())
	}
	if data, _ := os.ReadFile(binPath); string(data) != "dummy" {
		t.Errorf("binary should not have been touched")
	}
}

func TestUpgradeChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	binPath := installFakeBinary(t, dir, []byte("orig"))
	srv := newFakeReleaseServer(t, "v0.99.0", []byte("payload"))
	srv.corruptSums = true
	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		current: "0.1.0", execPath: binPath,
	}
	var stderr strings.Builder
	err := c.upgrade(context.Background(), &stderr, &stderr)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected checksum mismatch, got %v", err)
	}
	if data, _ := os.ReadFile(binPath); string(data) != "orig" {
		t.Errorf("binary should not have been touched on checksum mismatch")
	}
}

func TestUpgradeSmokeTestFailure(t *testing.T) {
	dir := t.TempDir()
	oldBin := scriptBinary(t, dir, 0, "")
	binPath := installFakeBinary(t, dir, oldBin)

	// New binary exits 1 on --version → smoke-test should fail.
	newBin := scriptBinary(t, dir, 1, "")
	srv := newFakeReleaseServer(t, "v0.99.0", newBin)

	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		current: "0.1.0", execPath: binPath,
	}
	var stderr strings.Builder
	err := c.upgrade(context.Background(), &stderr, &stderr)
	if err == nil {
		t.Fatal("expected smoke-test failure")
	}
	if !strings.Contains(err.Error(), "smoke test failed") {
		t.Errorf("expected smoke-test error, got %v", err)
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != string(oldBin) {
		t.Errorf("expected old binary restored after smoke-test failure")
	}
	if _, err := os.Stat(binPath + ".old"); !os.IsNotExist(err) {
		t.Errorf("expected %s.old removed after rollback; err=%v", binPath, err)
	}
}

func TestUpgrade403FromAPI(t *testing.T) {
	dir := t.TempDir()
	binPath := installFakeBinary(t, dir, []byte("orig"))
	srv := newFakeReleaseServer(t, "v0.99.0", []byte("ignored"))
	srv.statusOnAPI = http.StatusForbidden
	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		current: "0.1.0", execPath: binPath,
	}
	var stderr strings.Builder
	err := c.upgrade(context.Background(), &stderr, &stderr)
	if err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestUpgradeMissingAsset(t *testing.T) {
	dir := t.TempDir()
	binPath := installFakeBinary(t, dir, []byte("orig"))
	srv := newFakeReleaseServer(t, "v0.99.0", []byte("payload"))
	srv.missingBin = true
	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		current: "0.1.0", execPath: binPath,
	}
	var stderr strings.Builder
	err := c.upgrade(context.Background(), &stderr, &stderr)
	if err == nil {
		t.Fatal("expected error on missing asset")
	}
	if !strings.Contains(err.Error(), "no asset named") {
		t.Errorf("expected no-asset error, got %v", err)
	}
}

func TestUpgradeGitHubTokenAuthHeader(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token-xyz")
	dir := t.TempDir()
	binPath := installFakeBinary(t, dir, []byte("orig"))
	srv := newFakeReleaseServer(t, "v0.5.0", []byte("ignored"))
	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		current: "0.5.0", execPath: binPath,
	}
	var stdout strings.Builder
	_ = c.upgrade(context.Background(), &stdout, &stdout)
	if !strings.Contains(srv.gotAuth, "Bearer test-token-xyz") {
		t.Errorf("expected Authorization header, got %q", srv.gotAuth)
	}
}

// ---------------------------------------------------------------------
// Schema bump prompt
// ---------------------------------------------------------------------

func TestUpgradeSchemaBumpExit(t *testing.T) {
	dir := t.TempDir()
	oldBin := scriptBinary(t, dir, 0, "")
	binPath := installFakeBinary(t, dir, oldBin)

	// New binary advertises schema=2 via --print-schema.
	newBin := scriptBinary(t, dir, 0, "2")
	srv := newFakeReleaseServer(t, "v0.99.0", newBin)

	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		currentSc: 1, current: "0.1.0", execPath: binPath,
		probeSchema: probeSchemaViaPrintSchema,
		decideBump:  func(o, n int) bumpChoice { return bumpExit },
	}
	var stderr strings.Builder
	err := c.upgrade(context.Background(), &stderr, &stderr)
	if err == nil {
		t.Fatal("expected exit error on schema bump")
	}
	if !strings.Contains(err.Error(), "aborted by user") {
		t.Errorf("expected aborted-by-user, got %v", err)
	}
	if data, _ := os.ReadFile(binPath); string(data) != string(oldBin) {
		t.Error("binary should not have been replaced after [x]exit")
	}
}

func TestUpgradeSchemaBumpBackup(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	cfgPath := filepath.Join(configDir, "claude-ds", "config")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("_schema=1\nfoo=bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	oldBin := scriptBinary(t, dir, 0, "")
	binPath := installFakeBinary(t, dir, oldBin)
	newBin := scriptBinary(t, dir, 0, "2")
	srv := newFakeReleaseServer(t, "v0.99.0", newBin)

	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
		goos:    runtime.GOOS, goarch: runtime.GOARCH,
		currentSc: 1, current: "0.1.0", execPath: binPath,
		probeSchema: probeSchemaViaPrintSchema,
		decideBump:  func(o, n int) bumpChoice { return bumpBackup },
	}
	var out strings.Builder
	if err := c.upgrade(context.Background(), &out, &out); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if _, err := os.Stat(cfgPath + ".v1.bak"); err != nil {
		t.Errorf("expected config backup file: %v", err)
	}
}

// ---------------------------------------------------------------------
// startupUpdateCheck gates
// ---------------------------------------------------------------------

func TestStartupUpdateCheck_NoTTY(t *testing.T) {
	// In `go test`, stdin is typically not a TTY — IsTerminal returns
	// false. We just need to ensure the call returns immediately and
	// doesn't panic.
	startupUpdateCheck(&Config{UpdateCheckInterval: 24})
}

func TestStartupUpdateCheck_EnvGate(t *testing.T) {
	t.Setenv("CLAUDE_DS_NO_UPDATE_CHECK", "1")
	startupUpdateCheck(&Config{UpdateCheckInterval: 24})
}

func TestCacheFresh(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache")
	if cacheFresh(p, 24*time.Hour) {
		t.Error("missing cache must not be fresh")
	}
	if err := writeCache(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !cacheFresh(p, 24*time.Hour) {
		t.Error("cache should be fresh immediately after write")
	}
	// Stale: rewrite with timestamp from 25h ago.
	if err := writeCache(p, time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if cacheFresh(p, 24*time.Hour) {
		t.Error("cache should be stale after 25h")
	}
}

func TestLookupChecksum(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "checksums.txt")
	body := strings.Join([]string{
		"# comment",
		"",
		"abc123  claude-ds-darwin-arm64",
		"def456 *claude-ds-linux-amd64", // binary-mode prefix
		"ghi789  some-other-asset",
	}, "\n")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, want string }{
		{"claude-ds-darwin-arm64", "abc123"},
		{"claude-ds-linux-amd64", "def456"},
	} {
		got, err := lookupChecksum(p, c.name)
		if err != nil {
			t.Errorf("lookupChecksum(%q): %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("lookupChecksum(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if _, err := lookupChecksum(p, "missing"); err == nil {
		t.Error("expected error for missing asset")
	}
}

// ---------------------------------------------------------------------
// fetchLatestTag — 403 surfaces statusCode
// ---------------------------------------------------------------------

func TestFetchLatestTag_403(t *testing.T) {
	srv := newFakeReleaseServer(t, "v9.9.9", []byte{})
	srv.statusOnAPI = http.StatusForbidden
	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
	}
	_, status, err := c.fetchLatestTag(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
}

func TestFetchLatestTag_429(t *testing.T) {
	srv := newFakeReleaseServer(t, "v9.9.9", []byte{})
	srv.statusOnAPI = http.StatusTooManyRequests
	c := &updateClient{
		apiBase: srv.server.URL, owner: "earchibald", repo: "claude-ds",
		httpc:   srv.server.Client(),
	}
	_, status, err := c.fetchLatestTag(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", status)
	}
}
