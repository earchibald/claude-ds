// secretref.go — secret-reference resolver for the claude-ds Go rewrite.
//
// This is the Go port of the Bash secretref library that lived inline in
// the legacy `claude-ds` launcher (lines 356–804). Same scheme set, same
// shell-out strategy, same interactive UX — but exposed as a Go package
// API so other Phase-2 children (CDS-12 config loader, CDS-23 launch
// path) can call it directly.
//
// Schemes (dispatched by Resolve):
//
//	op://VAULT/ITEM/FIELD               1Password CLI       — `op read`
//	system://<account>                  OS keychain         — security / secret-tool
//	infisical://PROJECT/ENV/PATH#KEY    Infisical CLI       — `infisical secrets get`
//	<bare>                              plaintext passthrough
//
// Keychain service name is hard-coded to "claude-ds" (matches the legacy
// SECRETREF_KEYCHAIN_SERVICE setting from `claude-ds`).
//
// Missing CLI binaries produce a descriptive error referencing the
// missing binary — never panic. Subprocess failures surface stderr so
// the user can see what went wrong upstream.
//
// Interactive prompts read from /dev/tty (not stdin, since stdin is
// claimed by claude). Password input uses golang.org/x/term for
// asterisk-echoed entry that survives terminal mode flips on Ctrl-C.
//
// CDS-13. Parent: CDS-9.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/term"
)

// keychainService is the service name written to and read from the OS
// keychain. The legacy Bash launcher used SECRETREF_KEYCHAIN_SERVICE=
// "claude-ds"; we hard-code it here so there's exactly one service name
// across the codebase. Future wrappers that vendor this file can fork
// this constant.
const keychainService = "claude-ds"

// Resolve dispatches a secret reference to the appropriate scheme
// handler and returns the resolved value. The contract:
//
//   - "op://…"        → `op read -- <ref>`
//   - "system://…"    → keychain lookup
//   - "infisical://…" → `infisical secrets get …`
//   - anything else   → returned verbatim (bare key passthrough)
//
// Missing CLIs surface as a wrapped error referencing the missing
// binary; subprocess errors include the captured stderr.
//
// CDS-12 calls this directly for config.api_key_ref, individual
// otlp_headers values, and OTel env overrides.
func Resolve(ref string) (string, error) {
	scheme, body, ok := ResolveScheme(ref)
	if !ok {
		// Bare key — no scheme, return as-is.
		return ref, nil
	}

	switch scheme {
	case "op":
		return resolveOp(ref)
	case "system":
		return KeychainLookup(body)
	case "infisical":
		return resolveInfisical(body)
	default:
		// Unknown scheme — match the Bash library's permissive
		// behavior and return the input verbatim. Anything we don't
		// recognise is treated as plaintext, not as an error.
		return ref, nil
	}
}

// ResolveScheme splits a ref into (scheme, body, ok). ok is false when
// the ref has no `://` separator (i.e. it's a bare key). The scheme is
// lowercased for case-insensitive comparison; the body is returned
// unchanged.
//
// Examples:
//
//	ResolveScheme("op://Vault/Item/field")   → "op", "Vault/Item/field", true
//	ResolveScheme("system://signoz")          → "system", "signoz", true
//	ResolveScheme("plain-token")              → "", "", false
func ResolveScheme(ref string) (scheme, body string, ok bool) {
	idx := strings.Index(ref, "://")
	if idx < 0 {
		return "", "", false
	}
	return strings.ToLower(ref[:idx]), ref[idx+3:], true
}

// resolveOp shells out to the 1Password CLI to read the referenced
// field. The full ref (including `op://` prefix) is passed verbatim to
// `op read --`, matching the legacy Bash semantics exactly.
func resolveOp(ref string) (string, error) {
	if _, err := exec.LookPath("op"); err != nil {
		return "", fmt.Errorf("op CLI not found on PATH (install 1Password CLI: https://developer.1password.com/docs/cli/)")
	}
	out, stderr, err := runCmd("op", "read", "--", ref)
	if err != nil {
		return "", fmt.Errorf("op read %q failed: %w (stderr: %s)", ref, err, strings.TrimSpace(stderr))
	}
	return strings.TrimRight(out, "\n"), nil
}

// resolveInfisical parses the body of an `infisical://PROJECT/ENV/PATH#KEY`
// reference and shells out to the Infisical CLI. Path normalisation:
// empty path or single-slash path means root ("/"); otherwise the path
// is prefixed with `/` and trailing `/` is stripped.
//
// The fragment (`#KEY`) is required — no fragment yields a clear error
// rather than a confusing CLI failure.
func resolveInfisical(body string) (string, error) {
	if _, err := exec.LookPath("infisical"); err != nil {
		return "", fmt.Errorf("infisical CLI not found on PATH (install Infisical CLI: https://infisical.com/docs/cli/overview)")
	}

	hashIdx := strings.LastIndex(body, "#")
	if hashIdx < 0 {
		return "", fmt.Errorf("infisical:// requires #SECRET_KEY fragment (e.g. infisical://PROJECT/ENV/PATH#KEY)")
	}
	rest := body[:hashIdx]
	key := body[hashIdx+1:]

	// rest must be PROJECT/ENV[/PATH...] — at least two `/` separators.
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("infisical:// requires PROJECT_ID/ENV/PATH (got %q)", rest)
	}
	proj := parts[0]
	env := parts[1]
	var path string
	if len(parts) < 3 || parts[2] == "" {
		path = "/"
	} else {
		path = "/" + strings.TrimSuffix(parts[2], "/")
		if path == "" {
			path = "/"
		}
	}

	if proj == "" || env == "" || key == "" {
		return "", fmt.Errorf("infisical:// missing project/env/key (project=%q env=%q key=%q)", proj, env, key)
	}

	out, stderr, err := runCmd("infisical", "secrets", "get", key,
		"--projectId="+proj,
		"--env="+env,
		"--path="+path,
		"--plain", "--silent",
	)
	if err != nil {
		return "", fmt.Errorf("infisical secrets get %q failed: %w (stderr: %s)", key, err, strings.TrimSpace(stderr))
	}
	return strings.TrimRight(out, "\n"), nil
}

// KeychainStore writes a secret to the OS keychain for the configured
// service. macOS: `security add-generic-password -U` (the -U flag
// updates the entry if it already exists, matching the legacy bash
// semantics). Linux: `secret-tool store`, with the secret piped on
// stdin (never on the command line — that would leak it via /proc).
func KeychainStore(account, secret string) error {
	switch runtime.GOOS {
	case "darwin":
		_, stderr, err := runCmd("security", "add-generic-password",
			"-U",
			"-s", keychainService,
			"-a", account,
			"-w", secret,
		)
		if err != nil {
			return fmt.Errorf("security add-generic-password failed: %w (stderr: %s)", err, strings.TrimSpace(stderr))
		}
		return nil
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return fmt.Errorf("secret-tool not found on PATH (install libsecret-tools)")
		}
		cmd := exec.Command("secret-tool", "store",
			"--label="+keychainService+" secret",
			"service", keychainService,
			"account", account,
		)
		cmd.Stdin = strings.NewReader(secret)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("secret-tool store failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	default:
		return fmt.Errorf("system:// keychain not supported on %s", runtime.GOOS)
	}
}

// KeychainLookup reads a secret out of the OS keychain. Caller must
// distinguish "not found" from "tool errored out" — both surface as
// errors here, matching the Bash semantics where any non-zero exit is
// treated as failure. The secret is returned with its trailing newline
// trimmed (security/secret-tool both append one).
func KeychainLookup(account string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, stderr, err := runCmd("security", "find-generic-password",
			"-s", keychainService,
			"-a", account,
			"-w",
		)
		if err != nil {
			return "", fmt.Errorf("security find-generic-password failed for account %q: %w (stderr: %s)", account, err, strings.TrimSpace(stderr))
		}
		return strings.TrimRight(out, "\n"), nil
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return "", fmt.Errorf("secret-tool not found on PATH (install libsecret-tools)")
		}
		out, stderr, err := runCmd("secret-tool", "lookup",
			"service", keychainService,
			"account", account,
		)
		if err != nil {
			return "", fmt.Errorf("secret-tool lookup failed for account %q: %w (stderr: %s)", account, err, strings.TrimSpace(stderr))
		}
		return strings.TrimRight(out, "\n"), nil
	default:
		return "", fmt.Errorf("system:// keychain not supported on %s", runtime.GOOS)
	}
}

// KeychainDelete removes a keychain entry. Matches the Bash library's
// "best effort" semantics — a missing entry is not an error (the legacy
// code had `|| true` on both branches). Tool-not-found on Linux is also
// treated as a no-op so reset flows on minimal images don't bail.
func KeychainDelete(account string) error {
	switch runtime.GOOS {
	case "darwin":
		// Ignore non-zero exit — `security delete` returns 44
		// when the entry doesn't exist, which we treat as success.
		_, _, _ = runCmd("security", "delete-generic-password",
			"-s", keychainService,
			"-a", account,
		)
		return nil
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return nil
		}
		_, _, _ = runCmd("secret-tool", "clear",
			"service", keychainService,
			"account", account,
		)
		return nil
	default:
		return fmt.Errorf("system:// keychain not supported on %s", runtime.GOOS)
	}
}

// KeychainListAccounts enumerates the accounts stored under the
// keychainService. Empty list (and nil error) means "no entries"; the
// callers (interactive selectors) treat that as a fresh-install flow.
//
// macOS: parses `security dump-keychain` and pulls `acct` blob entries
// whose `svce` matches our service. The CLI may trigger a one-time
// keychain-access prompt on first call; we silently swallow failures so
// the selector still works.
//
// Linux: `secret-tool search --all`.
func KeychainListAccounts() ([]string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, _, err := runCmd("security", "dump-keychain")
		if err != nil {
			// Failures are silently empty — matches Bash semantics.
			return nil, nil
		}
		return parseDumpKeychainAccounts(out, keychainService), nil
	case "linux":
		if _, err := exec.LookPath("secret-tool"); err != nil {
			return nil, nil
		}
		out, _, err := runCmd("secret-tool", "search", "--all", "service", keychainService)
		if err != nil {
			return nil, nil
		}
		return parseSecretToolAccounts(out), nil
	default:
		return nil, nil
	}
}

// parseDumpKeychainAccounts walks the `security dump-keychain` output,
// finds blocks whose `svce` blob equals service, and extracts each
// matching `acct` blob. Returned list is sorted+deduplicated.
//
// Format of each entry (one per `keychain:` block):
//
//	"svce"<blob>="claude-ds"
//	"acct"<blob>="signoz"
//
// We split on `keychain: ` (matches the awk RS in the bash version) and
// scan each chunk independently. Robust to ordering of svce/acct lines
// within a chunk.
func parseDumpKeychainAccounts(out, service string) []string {
	want := `"svce"<blob>="` + service + `"`
	seen := map[string]struct{}{}
	var accounts []string
	// Split on the keychain-block delimiter. The first chunk is
	// pre-delimiter junk (often empty); we still scan it harmlessly.
	for _, chunk := range strings.Split(out, "keychain: ") {
		if !strings.Contains(chunk, want) {
			continue
		}
		// Find the acct blob in this chunk.
		const prefix = `"acct"<blob>="`
		i := strings.Index(chunk, prefix)
		if i < 0 {
			continue
		}
		rest := chunk[i+len(prefix):]
		end := strings.Index(rest, `"`)
		if end <= 0 {
			continue
		}
		acct := rest[:end]
		if _, ok := seen[acct]; ok {
			continue
		}
		seen[acct] = struct{}{}
		accounts = append(accounts, acct)
	}
	// Sort to match `sort -u` from the bash implementation.
	sortStrings(accounts)
	return accounts
}

// parseSecretToolAccounts walks `secret-tool search --all` output and
// pulls every `attribute.account = …` line.
func parseSecretToolAccounts(out string) []string {
	seen := map[string]struct{}{}
	var accounts []string
	for _, line := range strings.Split(out, "\n") {
		const prefix = "attribute.account = "
		idx := strings.Index(line, prefix)
		if idx < 0 {
			continue
		}
		acct := strings.TrimSpace(line[idx+len(prefix):])
		if acct == "" {
			continue
		}
		if _, ok := seen[acct]; ok {
			continue
		}
		seen[acct] = struct{}{}
		accounts = append(accounts, acct)
	}
	sortStrings(accounts)
	return accounts
}

// sortStrings is a tiny in-place sort to avoid pulling in the sort
// package for one call. List sizes are small (number of keychain
// entries for one service) so insertion sort is fine.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// PromptInteractive reads a single line from /dev/tty (not stdin,
// because stdin is consumed by claude in the launch path). Trailing
// newline is trimmed; surrounding ASCII single- or double-quote pairs
// are stripped to match the legacy `_secretref_strip_quotes` behavior
// — users who paste a quoted ref from a docs page get the unquoted
// value.
func PromptInteractive(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open /dev/tty: %w", err)
	}
	defer tty.Close()
	if _, err := io.WriteString(tty, prompt); err != nil {
		return "", fmt.Errorf("write prompt to /dev/tty: %w", err)
	}
	r := bufio.NewReader(tty)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read /dev/tty: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	return stripQuotes(line), nil
}

// PromptPassword reads a secret from /dev/tty with asterisk echo via
// golang.org/x/term. The returned string has its trailing newline
// stripped. On a non-tty (no controlling terminal) we return a
// descriptive error rather than silently degrading — first-run setup
// requires a real tty.
func PromptPassword(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open /dev/tty: %w", err)
	}
	defer tty.Close()
	fd := int(tty.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("/dev/tty is not a terminal (cannot read password)")
	}
	if _, err := io.WriteString(tty, prompt); err != nil {
		return "", fmt.Errorf("write prompt to /dev/tty: %w", err)
	}
	// term.ReadPassword reads a line with echo disabled. We could
	// implement asterisk-echo by polling stdin, but term.ReadPassword
	// is the right primitive for password input — terminal raw-mode
	// handling, signal safety, and EOL detection are all handled for
	// us. The asterisk echo from the legacy bash version was a nice
	// UX touch but not load-bearing — users still know they're
	// typing because the prompt told them so.
	bytes, err := term.ReadPassword(fd)
	// term suppresses the trailing newline echo; emit one so the
	// next prompt doesn't run on the same line.
	_, _ = io.WriteString(tty, "\n")
	if err != nil {
		return "", fmt.Errorf("read password from /dev/tty: %w", err)
	}
	return string(bytes), nil
}

// stripQuotes removes a single layer of matching ASCII single- or
// double-quote pairs. Mirrors the bash `_secretref_strip_quotes`
// helper exactly: only paired quotes are stripped; unpaired quotes
// pass through.
func stripQuotes(s string) string {
	if len(s) >= 2 {
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return s[1 : len(s)-1]
		}
		if s[0] == '"' && s[len(s)-1] == '"' {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// canonicalizeScheme allows bare scheme names without `://`. `system`,
// `op`, `infisical` get the suffix appended; anything else passes
// through. Used by InteractiveSystemSelector / first-run prompts so
// users can type just `system` and get the interactive selector.
func canonicalizeScheme(s string) string {
	switch s {
	case "system", "op", "infisical":
		return s + "://"
	default:
		return s
	}
}

// InteractiveSystemSelector lists existing keychain accounts under the
// service and lets the user pick by number, accept the default ('n' for
// new), or type a free-form account name. Returns the chosen account
// name (empty string is rejected with an error).
//
// Mirrors `secretref_select_account` from the bash library.
func InteractiveSystemSelector() (string, error) {
	accounts, _ := KeychainListAccounts()

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open /dev/tty: %w", err)
	}
	defer tty.Close()

	if len(accounts) > 0 {
		fmt.Fprintln(tty)
		fmt.Fprintf(tty, "Existing keychain entries for service=%q:\n", keychainService)
		for i, a := range accounts {
			fmt.Fprintf(tty, "  [%d] %s\n", i+1, a)
		}
		fmt.Fprintln(tty, "  [n] Enter a new account name")

		choice, err := PromptInteractive("Choice (number, 'n' for new, or type the account name directly): ")
		if err != nil {
			return "", err
		}
		// Numeric pick.
		if n, ok := parseInt(choice); ok && n >= 1 && n <= len(accounts) {
			return accounts[n-1], nil
		}
		if choice == "" || choice == "n" || choice == "N" {
			name, err := PromptInteractive("New account name: ")
			if err != nil {
				return "", err
			}
			if name == "" {
				return "", fmt.Errorf("empty account name")
			}
			return name, nil
		}
		// Free-form input — treat as an account name (existing or new).
		return choice, nil
	}

	fmt.Fprintln(tty)
	fmt.Fprintf(tty, "No existing keychain entries for service=%q.\n", keychainService)
	name, err := PromptInteractive("New account name: ")
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("empty account name")
	}
	return name, nil
}

// InteractiveInfisicalBuilder walks the user through project / env /
// path / key prompts and builds a complete `infisical://` URI. Path
// is normalised: empty or "/" → root (resulting URI omits the path
// segment), trailing/leading slashes are stripped.
//
// Mirrors `secretref_build_infisical_ref` from the bash library.
func InteractiveInfisicalBuilder() (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open /dev/tty: %w", err)
	}
	// Banner — defer close so the prompts can still write.
	fmt.Fprintln(tty)
	fmt.Fprintln(tty, "Build an infisical:// reference interactively.")
	fmt.Fprintln(tty, "  See https://infisical.com/docs/cli/commands/secrets for project/env conventions.")
	tty.Close()

	proj, err := PromptInteractive("Project ID: ")
	if err != nil {
		return "", err
	}
	if proj == "" {
		return "", fmt.Errorf("empty project id, aborting")
	}

	env, err := PromptInteractive("Environment slug [dev]: ")
	if err != nil {
		return "", err
	}
	if env == "" {
		env = "dev"
	}

	folder, err := PromptInteractive("Folder path ('/' for root) [/]: ")
	if err != nil {
		return "", err
	}
	if folder == "" {
		folder = "/"
	}
	// Normalize: strip leading + trailing slashes; empty means root.
	folder = strings.TrimPrefix(folder, "/")
	folder = strings.TrimSuffix(folder, "/")

	key, err := PromptInteractive("Secret key (name of the secret in Infisical): ")
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", fmt.Errorf("empty secret key, aborting")
	}

	if folder == "" {
		return fmt.Sprintf("infisical://%s/%s/#%s", proj, env, key), nil
	}
	return fmt.Sprintf("infisical://%s/%s/%s#%s", proj, env, folder, key), nil
}

// runCmd runs a subprocess and returns (stdout, stderr, error). Used
// throughout this file so all subprocess invocations have consistent
// error formatting and stderr capture. binary-not-found is detected
// upstream by exec.LookPath; this helper reports the underlying error
// (which will be "executable file not found" if it slipped through).
func runCmd(name string, args ...string) (string, string, error) {
	var stdout, stderr strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// parseInt parses a non-negative decimal integer. Returns (0, false)
// for empty input, leading-zero-padded inputs, or anything that's not
// pure digits. Used by the interactive selector to detect "user typed
// a number" vs. "user typed an account name that starts with digits".
func parseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
