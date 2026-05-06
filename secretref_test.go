// secretref_test.go — unit tests for the secret-reference resolver.
//
// Unit tests cover:
//   - ResolveScheme parsing for every scheme + bare-key passthrough
//   - Resolve() bare-key passthrough (no shell-out)
//   - stripQuotes / canonicalizeScheme / parseInt helpers
//   - Subprocess error formatting when the CLI binary is missing
//   - parseDumpKeychainAccounts / parseSecretToolAccounts pure parsers
//   - Infisical URI parsing edge cases (missing #, missing path, etc.)
//
// Integration tests that actually shell out to `op` / `security` /
// `infisical` are gated behind `-tags integration` so CI doesn't need
// the external CLIs installed. Run them locally with:
//
//	go test -tags=integration ./...
//
// The integration suite is intentionally minimal — we don't read
// production secrets in tests; we just verify the resolver dispatches
// to the correct binary and surfaces stderr on failure.

package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestResolveScheme(t *testing.T) {
	cases := []struct {
		in     string
		scheme string
		body   string
		ok     bool
	}{
		{"op://Vault/Item/field", "op", "Vault/Item/field", true},
		{"system://signoz", "system", "signoz", true},
		{"infisical://proj/dev/path#KEY", "infisical", "proj/dev/path#KEY", true},
		{"OP://Vault/Item/field", "op", "Vault/Item/field", true}, // case-insensitive
		{"plain-token", "", "", false},
		{"", "", "", false},
		{"://no-scheme", "", "no-scheme", true},
		{"weird:/notmatching", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			scheme, body, ok := ResolveScheme(c.in)
			if scheme != c.scheme || body != c.body || ok != c.ok {
				t.Errorf("ResolveScheme(%q) = (%q, %q, %v); want (%q, %q, %v)",
					c.in, scheme, body, ok, c.scheme, c.body, c.ok)
			}
		})
	}
}

func TestResolveBareKey(t *testing.T) {
	// Bare keys — no scheme separator — must be returned as-is, with
	// no shell-out. Includes empty string, plain ASCII, unicode, and
	// strings containing colons (just not the `://` triple).
	cases := []string{
		"",
		"plain-secret-token",
		"sk-abc123",
		"key:with:colons:but:no:scheme",
		"unicode-🔑-key",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			got, err := Resolve(c)
			if err != nil {
				t.Fatalf("Resolve(%q) returned err: %v", c, err)
			}
			if got != c {
				t.Errorf("Resolve(%q) = %q; want passthrough %q", c, got, c)
			}
		})
	}
}

func TestResolveUnknownSchemePassthrough(t *testing.T) {
	// Unknown schemes (e.g. "warden://") are treated as bare keys —
	// matches the bash library's permissive case-block fallthrough.
	in := "warden://some-item"
	got, err := Resolve(in)
	if err != nil {
		t.Fatalf("Resolve(%q) err: %v", in, err)
	}
	if got != in {
		t.Errorf("Resolve(%q) = %q; want passthrough %q", in, got, in)
	}
}

func TestStripQuotes(t *testing.T) {
	cases := []struct{ in, out string }{
		{`'foo'`, `foo`},
		{`"foo"`, `foo`},
		{`'foo`, `'foo`},     // unmatched single
		{`foo"`, `foo"`},     // unmatched double
		{`"foo'`, `"foo'`},   // mismatched pair
		{``, ``},             // empty
		{`""`, ``},           // empty quoted
		{`''`, ``},           // empty quoted
		{`a`, `a`},           // single char
		{`'a'`, `a`},         // single char quoted
		{`'foo bar baz'`, `foo bar baz`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := stripQuotes(c.in); got != c.out {
				t.Errorf("stripQuotes(%q) = %q; want %q", c.in, got, c.out)
			}
		})
	}
}

func TestCanonicalizeScheme(t *testing.T) {
	cases := []struct{ in, out string }{
		{"system", "system://"},
		{"op", "op://"},
		{"infisical", "infisical://"},
		{"system://", "system://"},        // already canonical → passthrough
		{"system://account", "system://account"}, // composite → passthrough
		{"warden", "warden"},                     // unknown → passthrough
		{"", ""},
	}
	for _, c := range cases {
		if got := canonicalizeScheme(c.in); got != c.out {
			t.Errorf("canonicalizeScheme(%q) = %q; want %q", c.in, got, c.out)
		}
	}
}

func TestParseInt(t *testing.T) {
	cases := []struct {
		in string
		n  int
		ok bool
	}{
		{"", 0, false},
		{"0", 0, true},
		{"1", 1, true},
		{"42", 42, true},
		{"100", 100, true},
		{"abc", 0, false},
		{"1a", 0, false},
		{"-1", 0, false}, // negative not allowed
		{" 1", 0, false}, // leading whitespace not stripped
	}
	for _, c := range cases {
		n, ok := parseInt(c.in)
		if n != c.n || ok != c.ok {
			t.Errorf("parseInt(%q) = (%d, %v); want (%d, %v)", c.in, n, ok, c.n, c.ok)
		}
	}
}

func TestResolveOpMissingBinary(t *testing.T) {
	// Set PATH to a directory containing no `op` binary so the
	// LookPath check fails. We use t.Setenv so the change is
	// auto-reverted at end of test.
	t.Setenv("PATH", t.TempDir())
	_, err := Resolve("op://Private/Item/field")
	if err == nil {
		t.Fatal("expected error when op CLI is missing, got nil")
	}
	if !strings.Contains(err.Error(), "op CLI not found") {
		t.Errorf("error %q does not mention missing op CLI", err.Error())
	}
}

func TestResolveInfisicalMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Resolve("infisical://proj/dev/path#KEY")
	if err == nil {
		t.Fatal("expected error when infisical CLI is missing, got nil")
	}
	if !strings.Contains(err.Error(), "infisical CLI not found") {
		t.Errorf("error %q does not mention missing infisical CLI", err.Error())
	}
}

func TestResolveInfisicalMissingFragment(t *testing.T) {
	// We need PATH to find infisical, otherwise the missing-binary
	// check fires first and we never reach the fragment check.
	// Workaround: stub a fake `infisical` binary in a temp dir.
	tmp := t.TempDir()
	stub := tmp + "/infisical"
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", tmp)

	_, err := Resolve("infisical://proj/dev/path-without-fragment")
	if err == nil {
		t.Fatal("expected error for missing fragment, got nil")
	}
	if !strings.Contains(err.Error(), "#SECRET_KEY") {
		t.Errorf("error %q does not mention #SECRET_KEY", err.Error())
	}
}

func TestResolveInfisicalMissingProjectEnv(t *testing.T) {
	tmp := t.TempDir()
	stub := tmp + "/infisical"
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", tmp)

	_, err := Resolve("infisical://only-one-segment#KEY")
	if err == nil {
		t.Fatal("expected error for missing project/env, got nil")
	}
	if !strings.Contains(err.Error(), "PROJECT_ID/ENV/PATH") {
		t.Errorf("error %q does not mention PROJECT_ID/ENV/PATH", err.Error())
	}
}

func TestResolveSystemMissingSecretToolLinux(t *testing.T) {
	// Only meaningful on Linux. On macOS, KeychainLookup uses
	// `security` (always present) and reports a "not found" error,
	// not a missing-binary error.
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test (macOS uses /usr/bin/security which is always present)")
	}
	t.Setenv("PATH", t.TempDir())
	_, err := Resolve("system://nonexistent")
	if err == nil {
		t.Fatal("expected error when secret-tool is missing, got nil")
	}
	if !strings.Contains(err.Error(), "secret-tool not found") {
		t.Errorf("error %q does not mention missing secret-tool", err.Error())
	}
}

func TestParseDumpKeychainAccounts(t *testing.T) {
	// Synthetic dump-keychain output with two matching entries
	// (claude-ds service) and one non-matching entry (other-service).
	out := `keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="signoz"
    "svce"<blob>="claude-ds"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="other-account"
    "svce"<blob>="other-service"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="api-key-prod"
    "svce"<blob>="claude-ds"
keychain: "/Users/x/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="signoz"
    "svce"<blob>="claude-ds"
`
	got := parseDumpKeychainAccounts(out, "claude-ds")
	want := []string{"api-key-prod", "signoz"}
	if !equalStrings(got, want) {
		t.Errorf("parseDumpKeychainAccounts = %v; want %v", got, want)
	}
}

func TestParseDumpKeychainAccountsEmpty(t *testing.T) {
	if got := parseDumpKeychainAccounts("", "claude-ds"); len(got) != 0 {
		t.Errorf("parseDumpKeychainAccounts on empty = %v; want []", got)
	}
}

func TestParseSecretToolAccounts(t *testing.T) {
	out := `[/org/freedesktop/secrets/collection/login/1]
label = claude-ds secret
secret = (not shown)
created = 2026-05-06 12:34:56
modified = 2026-05-06 12:34:56
schema = org.freedesktop.Secret.Generic
attribute.account = signoz
attribute.service = claude-ds
[/org/freedesktop/secrets/collection/login/2]
attribute.account = api-key-prod
attribute.service = claude-ds
[/org/freedesktop/secrets/collection/login/3]
attribute.account = signoz
attribute.service = claude-ds
`
	got := parseSecretToolAccounts(out)
	want := []string{"api-key-prod", "signoz"}
	if !equalStrings(got, want) {
		t.Errorf("parseSecretToolAccounts = %v; want %v", got, want)
	}
}

func TestSortStrings(t *testing.T) {
	xs := []string{"c", "a", "b", "a"}
	sortStrings(xs)
	want := []string{"a", "a", "b", "c"}
	if !equalStrings(xs, want) {
		t.Errorf("sortStrings = %v; want %v", xs, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
