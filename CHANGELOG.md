# Changelog

All notable changes to claude-ds are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.8.1] — 2026-05-05

### Changed

- **Installer auto-resolves latest GitHub Release.** The installer now queries
  the GitHub Releases API for the latest tag before downloading, so the REAME
  README no longer pins a version number that would rot on every release. The
  `CDS_INSTALL_REF` env var still overrides to any tag, branch, or SHA.
- **Installer compares installed version before prompting.** When a
  `claude-ds` binary already exists at the install path, the installer
  extracts its version and compares it against the resolved release tag.
  Default is N for same or later versions already installed, Y for upgrades.

### Added

- GitHub release CI workflow (merged from CDS-7). Tag pushes trigger automatic
  release creation with notes extracted from CHANGELOG.md.
- CHANGELOG.md following [Keep a Changelog](https://keepachangelog.com).

## [0.8.0] — 2026-05-05

### Fixed

- **Critical: `unlock_auto_mode=1` was routing requests to DeepSeek's cheapest
  model.** When auto-mode was enabled, `claude-ds` spoofed Claude-canonical
  model IDs (`claude-opus-4-7`, `claude-sonnet-4-6`, etc.) to satisfy Claude
  Code's permission-classifier regex gate. DeepSeek does not recognise these
  IDs — and instead of rejecting them, it **silently aliases unknown model
  IDs to `deepseek-flash`**, its cheapest and least capable model. Every
  request was being downgraded. Tool calls, reasoning, complex prompts — all
  hitting flash instead of the configured tier. This was invisible to the
  user: no error, no warning, just worse results.

### Added

- **WIRE_MODEL_MAP — model ID rewrite on the wire.** The fix: a semicolon-
  separated `from=to` map (`claude-opus-4-7=deepseek-v4-pro;...`) that
  rewrites the request body's `model` field back to the real DeepSeek ID
  before forwarding upstream. The spoofed ID still satisfies Claude Code's
  auto-mode gate; the rewrite undoes the spoof at the wire level so DeepSeek
  sees the model the user actually configured. Per-tier config keys
  `model_opus`, `model_sonnet`, `model_haiku`, and `model_small_fast` feed
  the map automatically.

### Changed

- **Effort map keys now reference post-rewrite (wire-level) model IDs.** The
  `EFFORT_MAP` env var now uses the real upstream model IDs as keys, matching
  the post-`WIRE_MODEL_MAP` value rather than the spoofed Claude-canonical ID.
  This ensures effort routing operates on the actual model that reaches
  DeepSeek.

## [0.7.4] — 2026-05-05

### Fixed

- **Vision normalization — images injected into last user turn.** DeepSeek's
  Anthropic-compatible endpoint only processes images in the most recent user
  turn. In multi-turn conversations images in earlier turns were silently
  ignored. The new `_normalize_for_vision()` function removes the `tools` and
  `tool_choice` keys from the request, extracts images from all earlier turns
  (including those nested in `tool_result` blocks), replaces them with
  placeholder text, and prepends them into the last user turn with images
  unwrapped from `tool_result` content. Assistant `tool_use` blocks are
  converted to plain text descriptions.
- **Image processing when the `Read` tool loads images from disk.** Images
  landing inside a `tool_result` block in the last user turn were ignored by
  DeepSeek. The normalization now unwraps `tool_result` content in the final
  turn so images are visible.
- **Vision processing failure with `tool_use`/`tool_result` blocks.** The
  presence of `tool_use`/`tool_result` blocks and the top-level `tools` key
  caused DeepSeek to fail vision processing even when images were in the
  correct position. Now stripped before forwarding.

### Changed

- Unified vision normalization: replaces two previous functions
  (`_hoist_images_to_first_turn`, `_normalize_tool_turns_for_vision`) with a
  single `_normalize_for_vision()` that consolidates all images into the last
  user turn.

## [0.7.3] — 2026-04-20

### Fixed

- **`DISABLE_EXPERIMENTAL_BETAS` blocking the Files API.** `claude-ds` exported
  `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` unconditionally, which told Claude
  Code to skip the Anthropic Files API entirely. Image attachments never reached
  `POST /v1/files` and fell back to the filesystem `Read` tool, bypassing the
  proxy. Now unset when the proxy starts successfully.
- **Stale system-level proxy detection.** An older `claude-ds-proxy.py`
  (pre-image-proxy) at `/usr/local/bin/` caused silent failures after upgrade.
  Added startup warning comparing launched proxy against system paths.
- **Stale proxy warning fires once per path.** Deduplicated warning messages so
  each stale path is reported only once per session.
- **Install script sets proxy mode 755.** The `install.sh` now ensures
  `claude-ds-proxy.py` is executable.

### Changed

- **Structured header pipeline.** Replaced ad-hoc header logic with
  `_build_upstream_headers()`: centralised strip/add tables, per-field
  `anthropic-beta` filtering, `PROXY_STRIP_HEADERS` and `PROXY_ADD_HEADERS`
  env-var configuration, and full DEBUG-mode header dump.

## [0.7.2] — 2026-04-15

### Fixed

- **Effort rewrite injecting `thinking` into vision-routed requests.** When
  `proxy_effort=auto:high` was configured, the effort-rewrite pass injected
  `thinking:{type:"enabled"}` into every request — even after routing to
  `deepseek-chat` for vision. The vision model does not support extended
  thinking with image inputs and responded with "I cannot see this image".
  Fixed by setting a `routed_to_vision` flag that short-circuits the effort
  rewrite pass.

## [0.7.1] — 2026-04-12

### Fixed

- **System prompt told the model it couldn't process images.** The wrapper
  injected a system prompt stating `Your underlying model is deepseek-v4-pro`,
  a text-only model. The model deduced it couldn't process images and told
  users so, even though the proxy already transparently routed image requests
  to `deepseek-chat` (vision-capable). Added an explicit note that the proxy
  handles the Files API and base64 rewriting and routes to `deepseek-chat`.

## [0.7.0] — 2026-04-10

### Added

- **Image proxy — Files API mock.** Intercepts `POST /v1/files` uploads,
  caches the decoded base64 payload in memory, and returns a mock
  Anthropic-compatible Files API response. No upstream forwarding needed.
- **`file_id` → base64 rewrite.** Scans all message content blocks for
  `source.type == "file"` references and substitutes them with inline base64
  data from the cache.
- **Vision model routing.** Requests containing images are automatically routed
  to `deepseek-chat` (vision-capable) instead of the text-only default model.
- **Thread-safe in-memory file cache.** Uses `threading.Lock`-protected dict
  scoped to the proxy lifetime — no disk writes or external state.
- **Full test suite.** 19 unit and integration tests covering multipart parsing,
  file cache, source rewriting, and live proxy + mock upstream pipeline.
- **`tests/` package.** `__init__.py` makes the tests directory importable.

## [0.6.0] — 2026-04-05

### Added

- **Auto-mode setup recommendation.** The installer now recommends and configures
  auto mode during onboarding, with `proxy_effort=auto:high` as the default.
- **Stale config detection.** Setup detects and reports stale configurations
  and offers to regenerate them.
- **Config schema migration.** Auto-migrates configs from older schema versions
  with `.v<old>.bak` backups.

### Changed

- Auto-mode defaults to on for new installs.
- `REPO_BASE` made overridable via environment variable.

## [0.5.0] — 2026-04-02

### Added

- `.worktrees/` directory to `.gitignore` for git worktree support.

## [0.4.0] — 2026-03-28

### Added

- **Single `curl | bash` installer.** One-command installation with branch-aware
  `CDS_INSTALL_REF` support and interactive sudo handling.
- **Writable ancestor resolution.** The installer walks up from the install
  directory to find the first writable ancestor, avoiding permission errors.

## [0.3.0] — 2026-03-25

### Added

- Initial release of `claude-ds` — wrapper that routes `claude` through
  DeepSeek's Anthropic-compatible API.
- Config file management with schema versioning and 1Password secret ref
  support.
- Proxy mode: transparency rewrites requests/responses between Claude Code
  and DeepSeek API.
- Effort proxy: translates Claude `/effort` levels to DeepSeek
  `reasoning_effort`.
- System health diagnostics via `--doctor`.
- Self-healing config with automatic schema migration.
- Tmux pane branding.

[Unreleased]: https://github.com/earchibald/claude-ds/compare/v0.8.1...HEAD
[0.8.1]: https://github.com/earchibald/claude-ds/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/earchibald/claude-ds/compare/v0.7.4...v0.8.0
[0.7.4]: https://github.com/earchibald/claude-ds/compare/v0.7.3...v0.7.4
[0.7.3]: https://github.com/earchibald/claude-ds/compare/v0.7.2...v0.7.3
[0.7.2]: https://github.com/earchibald/claude-ds/compare/v0.7.1...v0.7.2
[0.7.1]: https://github.com/earchibald/claude-ds/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/earchibald/claude-ds/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/earchibald/claude-ds/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/earchibald/claude-ds/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/earchibald/claude-ds/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/earchibald/claude-ds/releases/tag/v0.3.0
