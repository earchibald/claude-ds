// Self-update from GitHub releases (CDS-22).
//
// Implements `claude-ds upgrade` / `claude-ds update` and the optional
// startup update-check goroutine. The flow is intentionally narrow and
// stdlib-only: GitHub releases JSON, SHA256 verification of the matching
// platform asset, in-place replacement with `<path>.old` rollback, and a
// smoke test of the new binary before we keep it.
//
// Public surface used elsewhere in the package:
//
//	runUpgrade()           — body of the upgrade/update subcommand
//	startupUpdateCheck(*Config) — fire-and-forget startup check
//	cmpSemver(a, b)        — strict semver comparison helper
//
// Everything else is package-private and exists for testability:
//   - updateClient bundles the HTTP client, owner/repo, and the
//     "current binary" knobs so tests can point at httptest.Server.
//   - schemaProber is a hook that lets tests stub out reading the new
//     binary's compiled-in CURRENT_SCHEMA.
//   - schemaBumpDecider is a hook for the [b]ackup/[o]verwrite/[x]exit
//     prompt so tests don't need an interactive TTY.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// ---------------------------------------------------------------------
// Public entry points
// ---------------------------------------------------------------------

// runUpgrade is the body of the `upgrade` / `update` subcommand. It
// returns a process exit code so main.go can `return runUpgrade()` from
// the dispatcher.
func runUpgrade() int {
	c := defaultUpdateClient()
	if err := c.upgrade(context.Background(), os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade: %v\n", err)
		return 1
	}
	return 0
}

// startupUpdateCheck spawns the fire-and-forget startup update goroutine.
// It returns immediately. Caller is responsible for honouring
// --no-update-check before calling this; we additionally honour the
// CLAUDE_DS_NO_UPDATE_CHECK env var and the TTY/cache gates inline.
//
// Per spec, on user-accepted update we run the upgrade flow inline and
// os.Exit(0). On any failure or "no" we silently update the cache file
// and let main proceed.
func startupUpdateCheck(cfg *Config) {
	if os.Getenv("CLAUDE_DS_NO_UPDATE_CHECK") == "1" {
		return
	}
	// TTY gate — never prompt headless callers.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return
	}
	intervalHours := defaultUpdateCheckInterval
	if cfg != nil && cfg.UpdateCheckInterval > 0 {
		intervalHours = cfg.UpdateCheckInterval
	}
	cachePath := updateCheckCachePath()
	if cacheFresh(cachePath, time.Duration(intervalHours)*time.Hour) {
		return
	}

	// Goroutine + 3s context — main's normal launch path proceeds in
	// parallel and may exec claude before this returns. That's fine: an
	// in-flight prompt that misses the user's window simply gets lost.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		c := defaultUpdateClient()
		tag, status, err := c.fetchLatestTag(ctx)
		if err != nil {
			// 403 / 429 → silent skip + cache so we don't hammer.
			if status == http.StatusForbidden || status == http.StatusTooManyRequests {
				_ = writeCache(cachePath, time.Now())
			}
			return
		}
		if cmpSemver(tag, VERSION) <= 0 {
			_ = writeCache(cachePath, time.Now())
			return
		}

		fmt.Fprintf(os.Stderr,
			"New version %s available (running %s). Update now? [Y/n] ",
			tag, VERSION)
		ans := readYesNo()
		if !ans {
			_ = writeCache(cachePath, time.Now())
			return
		}
		// Accepted → run upgrade inline, then exit so the user
		// re-launches against the new binary.
		exitCode := runUpgrade()
		os.Exit(exitCode)
	}()
}

// ---------------------------------------------------------------------
// Semver compare
// ---------------------------------------------------------------------

// cmpSemver returns -1, 0, or +1 comparing two semver-ish strings.
// Strict-enough for our purposes: leading "v" stripped, missing
// components treated as 0, prerelease (`-rc1`) sorts BELOW the same
// numeric core (per semver §11). Non-numeric components inside the
// numeric triple are treated as 0 so `0.9.0-dev` sorts below `0.9.0`.
func cmpSemver(a, b string) int {
	an, ap := splitSemver(a)
	bn, bp := splitSemver(b)
	for i := 0; i < 3; i++ {
		if an[i] < bn[i] {
			return -1
		}
		if an[i] > bn[i] {
			return 1
		}
	}
	// Numeric cores tied. Per semver: "1.0.0-alpha" < "1.0.0".
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "" && bp != "":
		return 1
	case ap != "" && bp == "":
		return -1
	}
	// Both have prereleases; compare lexicographically. Coarse but
	// sufficient — we don't need full §11 dotted-identifier rules.
	if ap < bp {
		return -1
	}
	if ap > bp {
		return 1
	}
	return 0
}

func splitSemver(s string) ([3]int, string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	pre := ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.SplitN(s, ".", 3)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			n = 0
		}
		out[i] = n
	}
	return out, pre
}

// ---------------------------------------------------------------------
// Update client
// ---------------------------------------------------------------------

// updateClient bundles every dependency the update flow has on the
// outside world. Tests build a stripped-down client pointed at an
// httptest.Server; production callers use defaultUpdateClient().
type updateClient struct {
	apiBase   string // e.g. https://api.github.com (no trailing slash)
	owner     string
	repo      string
	httpc     *http.Client
	goos      string
	goarch    string
	currentSc int    // CurrentSchema of the running binary
	current   string // running version (VERSION)
	// Hooks (non-nil in tests).
	probeSchema schemaProber
	decideBump  schemaBumpDecider
	// execPath, if non-empty, overrides os.Executable() — used by tests
	// so we replace a sandbox binary instead of the test runner.
	execPath string
	// readYesNo overrides the interactive y/n prompt for tests.
	readAccept func() bool
}

type schemaProber func(ctx context.Context, binPath string) (int, error)
type schemaBumpDecider func(oldSchema, newSchema int) bumpChoice

type bumpChoice int

const (
	bumpBackup bumpChoice = iota
	bumpOverwrite
	bumpExit
)

func defaultUpdateClient() *updateClient {
	return &updateClient{
		apiBase: "https://api.github.com",
		owner:   "earchibald",
		repo:    "claude-ds",
		httpc: &http.Client{
			Timeout: 10 * time.Second,
		},
		goos:        runtime.GOOS,
		goarch:      runtime.GOARCH,
		currentSc:   CurrentSchema,
		current:     VERSION,
		probeSchema: probeSchemaViaPrintSchema,
		decideBump:  promptSchemaBump,
		readAccept:  readYesNo,
	}
}

// ---------------------------------------------------------------------
// GitHub release API
// ---------------------------------------------------------------------

// release is the subset of the GitHub releases JSON we consume.
type release struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// fetchLatestRelease GETs /repos/<owner>/<repo>/releases/latest.
// Returns the release plus the HTTP status code so callers can branch
// on 403/429.
func (c *updateClient) fetchLatestRelease(ctx context.Context) (*release, int, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.apiBase, c.owner, c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a small prefix for the error message.
		var b strings.Builder
		_, _ = io.Copy(&b, io.LimitReader(resp.Body, 256))
		return nil, resp.StatusCode, fmt.Errorf("github releases API: %s: %s",
			resp.Status, strings.TrimSpace(b.String()))
	}
	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode release JSON: %w", err)
	}
	return &r, resp.StatusCode, nil
}

// fetchLatestTag is the lightweight wrapper the startup check uses.
// Returns (tag, statusCode, err).
func (c *updateClient) fetchLatestTag(ctx context.Context) (string, int, error) {
	r, status, err := c.fetchLatestRelease(ctx)
	if err != nil {
		return "", status, err
	}
	return r.TagName, status, nil
}

// ---------------------------------------------------------------------
// Upgrade flow
// ---------------------------------------------------------------------

func (c *updateClient) upgrade(ctx context.Context, stdout, stderr io.Writer) error {
	rel, _, err := c.fetchLatestRelease(ctx)
	if err != nil {
		return err
	}
	if rel.TagName == "" {
		return errors.New("github releases API: missing tag_name")
	}
	if cmpSemver(rel.TagName, c.current) <= 0 {
		fmt.Fprintf(stdout, "already up to date (%s)\n", c.current)
		return nil
	}

	binAsset, sumAsset, err := c.resolveAssets(rel)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "claude-ds-update-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	binTmp := filepath.Join(tmpDir, binAsset.Name)
	sumTmp := filepath.Join(tmpDir, sumAsset.Name)

	binSum, err := c.downloadFile(ctx, binAsset.BrowserDownloadURL, binTmp, true)
	if err != nil {
		return fmt.Errorf("download %s: %w", binAsset.Name, err)
	}
	if _, err := c.downloadFile(ctx, sumAsset.BrowserDownloadURL, sumTmp, false); err != nil {
		return fmt.Errorf("download %s: %w", sumAsset.Name, err)
	}

	wantSum, err := lookupChecksum(sumTmp, binAsset.Name)
	if err != nil {
		return err
	}
	if !strings.EqualFold(wantSum, binSum) {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s",
			binAsset.Name, binSum, wantSum)
	}

	binPath, err := c.resolveBinaryPath()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}

	// Make the temp file executable so we can smoke-test it pre-replace.
	if err := os.Chmod(binTmp, 0o755); err != nil {
		return fmt.Errorf("chmod tmp: %w", err)
	}

	// Schema-bump detection: probe new binary BEFORE replacement.
	if c.probeSchema != nil {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		newSchema, perr := c.probeSchema(probeCtx, binTmp)
		cancel()
		if perr == nil && newSchema != 0 && newSchema != c.currentSc {
			choice := bumpBackup
			if c.decideBump != nil {
				choice = c.decideBump(c.currentSc, newSchema)
			}
			switch choice {
			case bumpExit:
				return fmt.Errorf("schema bump %d → %d: aborted by user",
					c.currentSc, newSchema)
			case bumpBackup:
				if err := backupConfigForSchema(c.currentSc); err != nil {
					fmt.Fprintf(stderr, "warning: backup config: %v\n", err)
				}
			case bumpOverwrite:
				// no-op
			}
		}
	}

	backupPath := binPath + ".old"
	if err := copyFileMode(binPath, backupPath, 0o755); err != nil {
		return fmt.Errorf("backup %s: %w", binPath, err)
	}

	if err := copyFileMode(binTmp, binPath, 0o755); err != nil {
		// Couldn't replace; nothing to roll back.
		_ = os.Remove(backupPath)
		return fmt.Errorf("replace %s: %w", binPath, err)
	}

	if err := smokeTest(binPath); err != nil {
		// Restore the old binary.
		if rerr := copyFileMode(backupPath, binPath, 0o755); rerr != nil {
			return fmt.Errorf("smoke test failed (%v); rollback also failed: %v", err, rerr)
		}
		_ = os.Remove(backupPath)
		return fmt.Errorf("smoke test failed: %w", err)
	}

	_ = os.Remove(backupPath)
	fmt.Fprintf(stdout, "updated to %s\n", rel.TagName)
	return nil
}

func (c *updateClient) resolveAssets(rel *release) (*releaseAsset, *releaseAsset, error) {
	want := fmt.Sprintf("claude-ds-%s-%s", c.goos, c.goarch)
	var binA, sumA *releaseAsset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		switch a.Name {
		case want:
			binA = a
		case "checksums.txt":
			sumA = a
		}
	}
	if binA == nil {
		return nil, nil, fmt.Errorf("no asset named %q in release %s", want, rel.TagName)
	}
	if sumA == nil {
		return nil, nil, fmt.Errorf("no checksums.txt in release %s", rel.TagName)
	}
	return binA, sumA, nil
}

// downloadFile streams `url` to `path`. If hashIt is true, the body is
// teed through a SHA256 hasher and the hex digest is returned.
func (c *updateClient) downloadFile(ctx context.Context, url, path string, hashIt bool) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		// Assets on public repos don't strictly need this, but it's
		// cheap and keeps rate-limit accounting consistent.
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s for %s", resp.Status, url)
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sum string
	if hashIt {
		h := sha256.New()
		w := io.MultiWriter(f, h)
		if _, err := io.Copy(w, resp.Body); err != nil {
			return "", err
		}
		sum = hex.EncodeToString(h.Sum(nil))
	} else {
		if _, err := io.Copy(f, resp.Body); err != nil {
			return "", err
		}
	}
	return sum, nil
}

// lookupChecksum parses a `sha256sum`-style file and returns the digest
// associated with `name`. Lines look like:
//
//	<sha256>  <filename>
//
// (two spaces). Some tools emit a single space; we tolerate both.
func lookupChecksum(path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Split on whitespace; first field = digest, last field = name.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Some tools prefix the filename with `*` (binary mode).
		fname := strings.TrimPrefix(fields[len(fields)-1], "*")
		if fname == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum for %q not found in %s", name, filepath.Base(path))
}

// resolveBinaryPath returns the on-disk path of the running binary,
// resolving symlinks so we replace the real file (not the symlink).
// Tests can override via updateClient.execPath.
func (c *updateClient) resolveBinaryPath() (string, error) {
	if c.execPath != "" {
		return c.execPath, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		// Fall back to the unresolved path; some sandboxed envs return
		// ENOENT on EvalSymlinks for self.
		return p, nil
	}
	return resolved, nil
}

// copyFile copies src to dst with the given mode. Atomic-ish: writes to
// a tempfile in the destination directory and renames into place. Falls
// back to a stream copy if cross-fs.
func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".claude-ds-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpName)
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		cleanup()
		return err
	}
	return nil
}

// smokeTest runs `<bin> --version` with a 2-second timeout and returns
// nil iff exit code is 0.
func smokeTest(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// probeSchemaViaPrintSchema runs `<bin> --print-schema` and parses the
// integer schema number on stdout. Returns 0 (and no error) if the new
// binary doesn't recognize --print-schema — an older or future build
// without the flag is treated as "schema unknown, skip the prompt".
func probeSchemaViaPrintSchema(ctx context.Context, bin string) (int, error) {
	cmd := exec.CommandContext(ctx, bin, "--print-schema")
	out, err := cmd.Output()
	if err != nil {
		// Unknown flag → exit non-zero. Treat as "no info".
		return 0, nil
	}
	s := strings.TrimSpace(string(out))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// promptSchemaBump asks the user [b]ackup / [o]verwrite / [x]exit on
// stderr/stdin. Default ([Enter]) is backup.
func promptSchemaBump(oldSchema, newSchema int) bumpChoice {
	fmt.Fprintf(os.Stderr,
		"Config schema changed (v%d → v%d). Your config will be auto-migrated on next launch.\n",
		oldSchema, newSchema)
	fmt.Fprintln(os.Stderr, "  [b] backup config  (default) — write config.v"+strconv.Itoa(oldSchema)+".bak, let auto-migration run")
	fmt.Fprintln(os.Stderr, "  [o] overwrite      — keep config as-is (stale keys may be dropped)")
	fmt.Fprintln(os.Stderr, "  [x] exit           — abort the update")
	fmt.Fprint(os.Stderr, "[B/o/x]: ")

	var ans string
	_, _ = fmt.Fscanln(os.Stdin, &ans)
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "o", "overwrite":
		return bumpOverwrite
	case "x", "exit":
		return bumpExit
	default:
		return bumpBackup
	}
}

// backupConfigForSchema copies $XDG_CONFIG_HOME/claude-ds/config to
// config.v<old>.bak. Best-effort: if no config file exists we just
// return nil (nothing to back up).
func backupConfigForSchema(oldSchema int) error {
	src := userConfigPath()
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dst := fmt.Sprintf("%s.v%d.bak", src, oldSchema)
	return copyFileMode(src, dst, 0o600)
}

// ---------------------------------------------------------------------
// Cache + paths
// ---------------------------------------------------------------------

func userConfigDir() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "claude-ds")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "claude-ds")
}

func userConfigPath() string {
	return filepath.Join(userConfigDir(), "config")
}

func updateCheckCachePath() string {
	return filepath.Join(userConfigDir(), ".last_update_check")
}

func cacheFresh(path string, maxAge time.Duration) bool {
	if maxAge == 0 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return false
	}
	last := time.Unix(n, 0)
	return time.Since(last) < maxAge
}

func writeCache(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(t.Unix(), 10)), 0o600)
}

// ---------------------------------------------------------------------
// y/n prompt
// ---------------------------------------------------------------------

// readYesNo reads a single y/n answer from /dev/tty (so it works even
// if stdin is being consumed by another part of the launch). Default
// (Enter) is yes.
func readYesNo() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		// No TTY — startup gate should already have caught this; treat
		// as "no" to be safe.
		return false
	}
	defer tty.Close()
	buf := make([]byte, 16)
	n, _ := tty.Read(buf)
	ans := strings.ToLower(strings.TrimSpace(string(buf[:n])))
	if ans == "" || strings.HasPrefix(ans, "y") {
		return true
	}
	return false
}
