// Package main: claude-ds — DeepSeek wrapper for Claude Code.
//
// Single source of truth for the running binary's version. All version
// checks (--version, self-update semver compare, doctor diagnostics) MUST
// import this constant rather than hard-coding a string.
package main

// VERSION is the semantic version of this build. Keep in sync with
// CHANGELOG.md and the git tag used by .github/workflows/release.yml.
const VERSION = "0.9.0-dev"
