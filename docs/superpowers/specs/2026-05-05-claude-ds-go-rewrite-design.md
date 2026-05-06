# claude-ds Go Rewrite — Design Spec

**Date:** 2026-05-05
**Status:** approved
**Target:** Drop-in replacement for `claude-ds` in Go, implemented from this spec.

## Goal

Reimplement `claude-ds` as a single, zero-dependency Go binary that:
- Preserves the exact CLI interface, config file format, and behavior of the
  current Bash + Python implementation
- Runs the HTTP proxy in-process (goroutine) rather than as a separate child
- Is maintainable by agents — the Go stdlib and simple architecture make it
  easy for coding agents to read, modify, and extend

## Non-goals

- Redesigning the CLI, config format, or secret reference schemes
- Changing the spec language for effort mapping
- Replacing the `install.sh` script (it remains Bash, unchanged)
- Adding features beyond what's specified here

## Architecture

```
┌─────────────────────────────────────────────────────┐
│  claude-ds (single Go binary)                       │
│                                                     │
│  main goroutine:                                    │
│    parse args → load config → resolve secret        │
│    → set env vars → build proxy config              │
│    → start proxy goroutine                          │
│    → fire update-check goroutine (non-blocking)     │
│    → apply tmux branding                            │
│    → run claude as child process (os/exec)          │
│    → wait for claude to exit                        │
│    → forward exit code → os.Exit(code)              │
│                                                     │
│  proxy goroutine:                                   │
│    net/http server on 127.0.0.1:<random-port>       │
│    POST /v1/messages → rewrite body                 │
│    POST /v1/files    → mock Files API               │
│    All other         → passthrough upstream         │
│                                                     │
│  update-check goroutine (fire-and-forget):          │
│    check .last_update_check timestamp               │
│    if <24h → return silently                        │
│    GET /repos/earchibald/claude-ds/releases/latest  │
│    compare semver → if newer, prompt user           │
│    update timestamp, exit goroutine                 │
└─────────────────────────────────────────────────────┘
```

**Why `os/exec` claude instead of `syscall.Exec`:**
`syscall.Exec` replaces the entire process image — killing the proxy
goroutine. Running claude as a child keeps the proxy alive in-process.
The parent forwards claude's exit code, signals (SIGINT/SIGTERM), and
stdio. From the user's perspective this is indistinguishable from exec;
the only difference is one extra entry in the process table.

**No orphan watchdog needed.** When claude exits, the Go process exits
and the proxy goroutine is cleaned up naturally.

## Package structure

Flat layout, single Go module. Every dependency is in stdlib.

```
claude-ds/
├── main.go           # entry point, arg parsing, orchestration
├── config.go         # config load, validate, migrate, repair
├── secretref.go      # op/system/infisical resolution via os/exec
├── proxy.go          # HTTP server, handler dispatch, upstream forwarding
├── rewrite.go        # effort injection, wire-model map, file_id→base64
├── spec.go           # effort spec parser (auto, auto:level, matrix)
├── vision.go         # vision model routing, image consolidation
├── headers.go        # header pipeline (strip/add/filter)
├── files.go          # Files API mock, multipart parse, base64 cache
├── branding.go       # tmux pane/window decoration
├── doctor.go         # --doctor diagnostics
├── update.go         # self-update from GitHub releases
├── go.mod
└── go.sum
```

## Config file

Same format, same path, same semantics as current:
- Path: `$XDG_CONFIG_HOME/claude-ds/config` (default: `~/.config/claude-ds/config`)
- Format: `key=value` pairs, `#` comments, blank lines ignored
- Mode: 0600
- Schema versioned via `_schema=N` key

### Config loading sequence

1. Read file line-by-line
2. Validate: every non-comment non-blank line must match `^[a-zA-Z_][a-zA-Z0-9_]*=.+$`
3. If invalid → back up to `config.broken.<timestamp>.bak`, repair by keeping only
   valid lines with known keys, rewrite with `_schema` set to current
4. Check `_schema` against `CURRENT_SCHEMA` → if behind, back up to
   `config.v<old>.bak`, apply migrations in order
5. Populate Config struct; unknown keys emit warning and are preserved on write

### Migrations

| From | To | Transformation |
|------|----|---------------|
| 0 (unversioned) | 1 | Prepend `_schema=1`, strip any pre-existing `_schema=` lines |

### Known config keys

```
_schema, api_key_ref, base_url, model,
model_opus, model_sonnet, model_haiku, model_small_fast,
capabilities, unlock_auto_mode,
proxy_effort, proxy_effort_opus, proxy_effort_sonnet,
proxy_effort_haiku, proxy_effort_small_fast,
proxy_strip_thinking, proxy_bind, proxy_debug,
update_check_interval
```

### Config struct

```go
type Config struct {
    Schema                int
    APIKeyRef             string
    BaseURL               string
    Model                 string
    ModelOpus             string
    ModelSonnet           string
    ModelHaiku            string
    ModelSmallFast        string
    Capabilities          string
    UnlockAutoMode        bool
    ProxyEffort           string
    ProxyEffortOpus       string
    ProxyEffortSonnet     string
    ProxyEffortHaiku      string
    ProxyEffortSmallFast  string
    ProxyBind             string
    ProxyDebug            bool
    UpdateCheckInterval   int     // hours, default 24; 0 = every launch
}
```

Defaults:
- `base_url`: `https://api.deepseek.com/anthropic`
- `model`: `deepseek-v4-pro`
- `proxy_effort`: `off`
- `proxy_bind`: `127.0.0.1`
- `CURRENT_SCHEMA`: `1` (compatible with all existing configs from the Bash version)

## Secret references

Shell out to existing CLIs via `os/exec`:

| Scheme | Command |
|--------|---------|
| `op://VAULT/ITEM/FIELD` | `op read -- <ref>` |
| `system://<account>` (macOS) | `security find-generic-password -s claude-ds -a <account> -w` |
| `system://<account>` (Linux) | `secret-tool lookup service claude-ds account <account>` |
| `infisical://PROJECT/ENV/PATH#KEY` | `infisical secrets get <key> --projectId=<proj> --env=<env> --path=<path> --plain --silent` |
| bare key | returned as-is |

### Interactive flows

First-run and `--rotate-key` prompts read from `/dev/tty` (not stdin, since
stdin may be consumed by claude). Key input uses `golang.org/x/term` for
asterisk-echoed password entry. The interactive `system://` selector
enumerates keychain accounts, the `infisical://` builder walks through
project/env/path/key prompts — same UX as the current Bash implementation.

### Store/delete operations

`system://` keychain writes use:
- macOS: `security add-generic-password -U -s claude-ds -a <account> -w <secret>`
- Linux: `secret-tool store --label='claude-ds secret' service claude-ds account <account>`

Deletion uses `security delete-generic-password` / `secret-tool clear`.

## CLI interface

Exact drop-in. Same flags, same behavior:

```
claude-ds [--rotate-key | --reset-password] [--setup] [--proxy-off | --proxy-on[=<spec>]]
          [--doctor] [--no-update-check] [--help | -h] [--version | -V]
          [upgrade | update] [claude args...]
```

| Flag | Behavior |
|------|----------|
| `--rotate-key`, `--reset-password` | Interactive key rotation; preserves proxy_effort and unlock_auto_mode |
| `--setup` | Run onboarding then exit cleanly |
| `--proxy-off` | Disable proxy for this session |
| `--proxy-on[=<spec>]` | Enable proxy with spec for this session |
| `--doctor` | 6-step diagnostics checklist, then exit |
| `--no-update-check` | Skip the automatic update check on startup |
| `--version`, `-V` | Print version + `claude --version` |
| `--help`, `-h` | Print help + `claude --help`, through $PAGER |
| `upgrade`, `update` | Check GitHub releases for a newer version, download, verify, replace, then exit |

All other args forwarded to `claude` unchanged.

`--` terminates claude-ds flag parsing; everything after is forwarded.

## Environment variables

### Read by claude-ds

| Variable | Effect |
|----------|--------|
| `CLAUDE_DS_PROXY_EFFORT` | Override proxy_effort for this invocation |
| `CLAUDE_DS_PROXY_DEBUG` | Enable proxy debug logging |
| `CLAUDE_DS_NO_BRANDING` | Suppress tmux branding |
| `CLAUDE_DS_NO_UPDATE_CHECK` | Skip the automatic update check on startup (same as `--no-update-check`) |
| `GITHUB_TOKEN` | GitHub personal access token for authenticated API requests (5,000 req/hr vs 60 unauthenticated); optional |
| `XDG_CONFIG_HOME` | Config directory location |
| `PAGER` | Pager for --help output |

### Exported to claude

| Variable | Condition |
|----------|-----------|
| `ANTHROPIC_BASE_URL` | Always (points at proxy when enabled, DeepSeek directly otherwise) |
| `ANTHROPIC_AUTH_TOKEN` | Always (resolved API key) |
| `ANTHROPIC_MODEL` | Always |
| `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` | Always |
| `ANTHROPIC_SMALL_FAST_MODEL` | Always |
| `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL_NAME` / `_DESCRIPTION` | When unlock_auto_mode=true |
| `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL_SUPPORTED_CAPABILITIES` | When capabilities is set |
| `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS` | Always (unset when proxy is active) |
| `CLAUDE_DISABLE_NONSTREAMING_FALLBACK` | Always |
| `CLAUDE_DS` | Always (marker: `1`) |

## Auto-mode unlock

When `unlock_auto_mode=1`:

```
ANTHROPIC_MODEL = "claude-opus-4-7"
ANTHROPIC_DEFAULT_OPUS_MODEL = "claude-opus-4-7"
ANTHROPIC_DEFAULT_SONNET_MODEL = "claude-sonnet-4-6"
ANTHROPIC_DEFAULT_HAIKU_MODEL = "claude-haiku-4-5"
ANTHROPIC_SMALL_FAST_MODEL = "claude-haiku-4-5"
```

Picker labels are DeepSeek-branded so `/model` shows what's actually running.
Per-tier `model_*` overrides win over spoofed values.

The proxy's `WIRE_MODEL_MAP` rewrites these spoofed IDs back to real DeepSeek
model IDs in the request body before forwarding upstream (so DeepSeek's compat
shim doesn't silently alias unknown claude-* IDs to flash).

`DEFAULT_UPSTREAM_MODEL` is set to `resolved_model` (the configured upstream
model) and passed to the proxy. It serves as a **catch-all**: any `claude-*` ID
that is not found in `WIRE_MODEL_MAP` — new model generations, internal code
paths, future spoofed IDs — is silently rewritten to `DEFAULT_UPSTREAM_MODEL`
instead of being forwarded as-is (which would cause DeepSeek to alias it to
flash). This is defense-in-depth on top of the explicit WIRE_MODEL_MAP entries.

### Proxy-disabled flash-downgrade warning

When `unlock_auto_mode=1` and the proxy cannot start (python3 missing, curl
missing, download failure, or start timeout), **all three** disable paths must
print a prominent warning before continuing:

```
⚠ UNLOCK_AUTO_MODE IS ON — spoofed claude-* model IDs will NOT be rewritten.
⚠ DeepSeek will silently alias them to deepseek-flash (cheapest model).
⚠ Fix: ensure python3 is installed and claude-ds-proxy.py is reachable.
```

In the Go rewrite this is a `warnFlashDowngrade()` helper called from every
path that sets `proxyNeeded = false` after the proxy-start attempt. A 2-second
sleep after the warning ensures the user reads it before the TUI clears the
screen.

## Reasoning-effort proxy

### Lifecycle

The proxy is an `net/http.Server` running in a background goroutine. It binds
to `127.0.0.1:0` (kernel-assigned port). The port is read from the listener
after startup. `ANTHROPIC_BASE_URL` is set to `http://127.0.0.1:<port>` before
launching claude.

When all effort specs resolve to `off` (default), the proxy is not started
and `ANTHROPIC_BASE_URL` points directly at DeepSeek.

### Request routing

```
POST /v1/messages
  → header pipeline
  → parse JSON body (rewriting is unconditional — no Content-Type gate needed;
    /v1/messages is always JSON per the Anthropic API spec; _rewrite_body
    safely passes through non-JSON bodies)
  → file_id → base64 rewrite (cached uploads)
  → wire-model map rewrite (explicit WIRE_MODEL_MAP entries)
  → wire-model catch-all: if model still starts with "claude-" and
    DEFAULT_UPSTREAM_MODEL is set, rewrite to DEFAULT_UPSTREAM_MODEL
  → image detection → if yes: vision routing, skip effort rewrite
  → effort spec resolution → apply regime
  → re-serialize, forward upstream, stream response back

POST /v1/files (multipart/form-data)
  → parse multipart body
  → extract file, detect media type
  → base64-encode, cache by generated file_id
  → return mock Anthropic Files API JSON response

All other requests
  → passthrough (forward headers + body unchanged)
```

### Effort spec resolution

1. Look up model ID in per-model effort map → use that spec
2. Fall back to `EFFORT_DEFAULT`
3. If spec is empty or `off` → passthrough
4. Otherwise, resolve spec against source bucket → target regime
5. Apply regime transformation

### Spec language

Preserved verbatim from current implementation:

| Value | Behavior |
|-------|----------|
| `off` | Passthrough |
| `none` | Force no reasoning (strip thinking + reasoning_effort) |
| `high` | Force default reasoning (thinking enabled, no reasoning_effort) |
| `max` | Force maximum reasoning (thinking enabled + reasoning_effort=max) |
| `auto` | Mirror source bucket to regime |
| `auto:<level>` | Like auto, upgrade none-bucket to <level> |
| `none=<v>\|high=<v>\|max=<v>` | Per-source-bucket matrix |

### Source bucket detection

From the incoming `thinking` block:
- No thinking block or `type != "enabled"` → `none`
- Thinking enabled, `budget_tokens < 31000` → `high`
- Thinking enabled, `budget_tokens >= 31000` → `max`

### Regime transformations

- `none`: Remove `thinking` and `reasoning_effort` keys
- `high`: Set `thinking: {type: enabled}` (preserve budget_tokens if present), remove `reasoning_effort`
- `max`: Set `thinking: {type: enabled}`, set `reasoning_effort: "max"`

### Header pipeline

**Always stripped:**
- HTTP/1.1 hop-by-hop headers (Connection, Keep-Alive, Transfer-Encoding, etc.)
- `Host` (replaced with upstream host)
- `Content-Length` (recomputed after body rewriting)
- `files-api` values from `anthropic-beta` header

**Config-driven:**
- `PROXY_STRIP_HEADERS` — comma/semicolon-separated header names to strip
- `PROXY_ADD_HEADERS` — semicolon-separated `Name: value` pairs to inject

### Vision routing

When a request contains image content blocks (direct `type: image` or nested in
`tool_result` blocks) and `VISION_MODEL` is set (default: `deepseek-chat`):

1. Override `model` in the request body to `VISION_MODEL`
2. Consolidate all images into the last user turn (DeepSeek only processes images there)
3. Convert `tool_use` blocks to plain text descriptions
4. Strip `tools` and `tool_choice` keys
5. Skip effort rewrite (vision models don't support extended thinking)

### Files API mock

`POST /v1/files` is handled locally without forwarding upstream:

1. Parse multipart/form-data body
2. Extract file binary, detect media type (from Content-Type or filename extension)
3. Base64-encode, store in `sync.RWMutex`-protected map keyed by generated `file_<hex>`
4. Return mock response: `{id, type: "file", filename, mime_type, size, created_at, downloadable: false}`

Images are rewritten from `source.type: "file"` to `source.type: "base64"` with
inline data in subsequent `/v1/messages` requests.

### Debug logging

When `PROXY_DEBUG=1`: log every header decision (pass/strip/rewrite/inject),
every effort regime application, every file upload, and every image rewrite
to stderr.

## Observability — OpenTelemetry / OTLP

The Go rewrite ships first-class OpenTelemetry instrumentation: traces,
metrics, and logs flow over OTLP to **0..N** configured endpoints. The
canonical local target is the home-cluster SigNoz instance
(`http://signoz.local:30318` for OTLP/HTTP); 0 endpoints disables export
entirely (the SDK still runs in-process so spans/metrics are available
to in-process consumers, but nothing is shipped out).

Detailed metric inventory, span graph, attribute schema, redaction
rules, failure-mode → signal map, and SigNoz views live in
`DOCS/2026-05-06-claude-ds-otlp-observability.md`. This section captures
the spec-level decisions only.

### Dependencies

The observability stack is the only third-party dependency the rewrite
adds beyond stdlib + `golang.org/x/term`. Vendored at build time so the
binary stays statically linked and self-contained:

```
go.opentelemetry.io/otel
go.opentelemetry.io/otel/sdk
go.opentelemetry.io/otel/sdk/metric
go.opentelemetry.io/otel/sdk/log
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp
go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp
go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
```

Binary size grows from ~5 MB to ~15 MB. Still one static binary, still
no system libraries linked. gRPC variants are deliberately out — one
fewer transport, easier `tcpdump` debugging on loopback.

### Config keys (live in CDS-12)

```
otlp_endpoints              # CSV of OTLP/HTTP base URLs; default empty (disabled)
otlp_headers                # semicolon-separated Name: value pairs, secretref-resolvable
otlp_service_name           # default "claude-ds-proxy"
otlp_deployment_environment # default "local"; forced to "doctor" by --doctor / CLAUDE_DS_DOCTOR=1
otlp_resource_attributes    # comma-separated key=value; merged into the SDK Resource
otlp_protocol               # "http" (default) | "grpc"
```

`otlp_endpoints` accepts multiple comma-separated values — each becomes
an independent batch span/metric/log processor pair fanned out from a
single set of provider instances. A single export failure to one
endpoint must not block the others.

`otlp_headers` values flow through the existing `secretref` resolver.
`signoz-access-token=op://home-kubernetes/SigNoz access token (home cluster)/credential`
resolves at startup, the resolved value is held only in the exporter's
header map, and never reaches stdout/stderr/log files. Standard OTel
env vars `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`,
and `OTEL_RESOURCE_ATTRIBUTES` override the config-file values, also
via the secretref resolver.

### Provider lifecycle (lives in CDS-9 / CDS-23)

`main.go` constructs three providers at startup, **before** the proxy
goroutine launches: `MeterProvider`, `TracerProvider`, `LoggerProvider`.
Each is built from the same OTLP exporter set and shares one Resource.
Providers are registered globally (`otel.SetTracerProvider(...)` etc.)
so the proxy and any future component pick them up via
`otel.Meter("...")` / `otel.Tracer("...")` without explicit wiring.

Shutdown is paired with `defer` in `main()`:

```go
ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
defer cancel()
_ = tracerProvider.Shutdown(ctx)
_ = meterProvider.Shutdown(ctx)
_ = loggerProvider.Shutdown(ctx)
```

The 1-second budget is the price of guaranteed last-batch delivery. If
the timeout fires, the trailing data is dropped and the parent exits;
exit-code forwarding from claude is not affected. Empty
`otlp_endpoints` short-circuits exporter construction so the deferred
shutdowns are no-ops.

### Proxy instrumentation (lives in CDS-15)

The proxy uses `otelhttp.NewHandler` for the inbound side and
`otelhttp.NewTransport` for the upstream `http.Client`, which yields
the standard HTTP semconv attributes and the `http.server.*` /
`http.client.*` metric set for free. The transform pipeline, streaming
relay, and Files API mock add `claude_ds.*` spans, attributes, and
metrics on top — see the observability doc for the full inventory.

Sampling is `ParentBased(AlwaysOn)` at 100% — single-user, low QPS,
debug value > storage cost. `PROXY_DEBUG=1` raises the log level to
`DEBUG`; spans are recorded unconditionally.

### Redaction

Bodies, message content, tool args, tool results, file bytes,
`Authorization` header values, and `x-api-key` header values are NEVER
recorded as span attributes, metric attributes, or log fields. Header
*names* are fine; values are not. Counts, sizes, and mutation flags
that summarise the payload are fine. The implementer's checklist for
this rule lives in the observability doc.

### Doctor integration

`--doctor` adds an OTLP-reachability check: for each configured
endpoint, attempt a 2-second exporter construction + force-flush of an
empty batch; report ✓/✗. Silently skipped (counted as ✓) when
`otlp_endpoints` is empty.

## Tmux branding

When `$TMUX` and `$TMUX_PANE` are set and `CLAUDE_DS_NO_BRANDING` is unset:

1. Disable `automatic-rename` on the window (save original)
2. Prefix window name with `🐋 `
3. Set pane border to top with heavy lines, indigo (#4D6BFE) color
4. Set pane border format to: `🐋 DEEPSEEK` badge + model info + auto-mode status

Cleanup runs via `defer`:

```go
defer func() {
    tmuxRenumberWindow(windowID, originalName)
    tmuxSetWindowOption(windowID, "automatic-rename", originalAutoRename)
    if paneCount > 1 {
        tmuxUnsetWindowOption(windowID, "pane-border-status")
        tmuxUnsetWindowOption(windowID, "pane-border-lines")
        tmuxUnsetWindowOption(windowID, "pane-border-style")
        tmuxUnsetWindowOption(windowID, "pane-active-border-style")
        tmuxUnsetWindowOption(windowID, "pane-border-format")
    }
}()
```

All tmux operations shell out via `os/exec` to the `tmux` binary.

## Self-update from GitHub releases

The Go rewrite adds a built-in self-updater. No more `curl | bash` — the
binary checks GitHub Releases, downloads the correct asset, verifies it,
and replaces itself in-place. The `install.sh` script remains for initial
installation only.

### CLI subcommands

```
claude-ds upgrade           # check for newer release, download, verify, replace, exit
claude-ds update            # alias for upgrade
```

These bypass `install.sh` entirely and work directly with the GitHub API.
They exit 0 on success, non-zero on failure.

### Startup update check

On every normal launch (not `--doctor`, `--setup`, `--version`, `--help`,
`upgrade`, or `update`), a background goroutine checks for updates with
strict etiquette:

1. **TTY gate:** If stdin is not a terminal (`!term.IsTerminal(int(os.Stdin.Fd()))`),
   skip silently. Headless/pipe mode shouldn't block or prompt.
2. **Flag/env gate:** If `--no-update-check` or `CLAUDE_DS_NO_UPDATE_CHECK=1`,
   skip silently.
3. **Rate-limit cache:** Read `~/.config/claude-ds/.last_update_check`. If the
   timestamp is within 24 hours, skip silently. The file contains a single
   Unix epoch integer.
4. **Fire-and-forget:** Launch a goroutine with a 3-second timeout. If the
   GitHub API doesn't respond in time, the goroutine exits and the launch
   proceeds normally.
5. **Version comparison:** If the latest release tag is strictly greater than
   the running version (semver comparison), print a one-line notice and prompt:
   `New version <tag> available (running <current>). Update now? [Y/n]`
   - If yes → run the update flow inline, then exit (user re-launches)
   - If no → update `.last_update_check` timestamp, proceed with launch
6. **Rate-limit handling:** On HTTP 403 or 429 from the GitHub API, silently
   skip the check and update `.last_update_check` to avoid immediate retries.
   If `GITHUB_TOKEN` is set, use it for `Authorization: Bearer <token>` to
   get 5,000 req/hr instead of 60.

### Release asset naming

The updater maps `runtime.GOOS` + `runtime.GOARCH` to the correct asset:

| OS | Arch | Asset |
|----|------|-------|
| darwin | amd64 | `claude-ds-darwin-amd64` |
| darwin | arm64 | `claude-ds-darwin-arm64` |
| linux | amd64 | `claude-ds-linux-amd64` |
| linux | arm64 | `claude-ds-linux-arm64` |

This convention must be baked into the GitHub release workflow (`release.yml`)
so every release publishes these four assets plus `checksums.txt`.

### Update flow

When `claude-ds upgrade` is invoked (or the startup check is accepted):

1. **Fetch release metadata:** `GET /repos/earchibald/claude-ds/releases/latest`
   (with `Accept: application/vnd.github+json`). Parse the JSON response to
   extract the tag name and asset download URLs.
2. **Semver compare:** If the latest tag is not strictly greater than the
   running version, print "already up to date" and exit 0.
3. **Resolve asset URL:** Find the asset matching `claude-ds-{GOOS}-{GOARCH}`
   and the `checksums.txt` asset.
4. **Download both** to a temp directory (`os.MkdirTemp`). Compute SHA256 of
   the binary asset.
5. **Verify checksum:** Parse `checksums.txt` (lines are `<sha256>  <filename>`),
   confirm the binary's SHA256 matches. If not, delete temp files, exit with error.
6. **Resolve binary path:** Use `os.Executable()` then `filepath.EvalSymlinks`
   to find the real on-disk path (handles `~/.local/bin/claude-ds` →
   `/opt/claude-ds/claude-ds` symlinks).
7. **Backup:** Copy the current binary to `<path>.old` in the same directory.
8. **Replace:** Write the new binary to `<path>` with mode 0755.
9. **Smoke test:** Run `<path> --version` (2-second timeout). Capture exit code.
   - If exit code != 0 or the process is killed by a signal → restore `<path>.old`
     to `<path>`, print a warning, remove the temp download, exit with error.
   - If success → remove `<path>.old`, print "updated to <tag>", exit 0.
10. **Cleanup** temp directory on all paths.

### Config schema bump handling

When `CURRENT_SCHEMA` has changed between the running version and the new
version, the updater pauses before replacing the binary:

```
Config schema changed (v1 → v2). Your config will be auto-migrated on next launch.
  [b] backup config  (default) — write config.v1.bak, let auto-migration run
  [o] overwrite      — keep config as-is (stale keys may be dropped)
  [x] exit           — abort the update
```

- **backup** copies `config` to `config.v<old>.bak`, then proceeds with the
  binary replacement. On next launch, the config loader auto-migrates the
  config forward.
- **overwrite** proceeds without backing up. The config loader may drop
  unknown keys from the old schema on next launch.
- **exit** aborts the update entirely. No files changed.

If no schema bump (same `CURRENT_SCHEMA`), the update is fully non-interactive
— no prompts, just download, verify, replace.

### Rollback on failure

The smoke-test (`claude-ds --version` on the new binary) catches:
- Truncated/corrupted downloads (checksum verification catches most, but
  this is defense in depth)
- Missing symbols from a non-static build (`CGO_ENABLED=1` without the right
  libc)
- Go runtime version mismatch on very old kernels

If the smoke test fails, the old binary is restored from `<path>.old` and
the user's install is unchanged. The temp download is deleted. An error
message with the failure reason is printed to stderr.

### Binary self-location

```go
func executablePath() (string, error) {
    p, err := os.Executable()
    if err != nil {
        return "", err
    }
    return filepath.EvalSymlinks(p)
}
```

`os.Executable()` returns the path used to launch the process. For symlinked
installs (`~/.local/bin/claude-ds` → `/opt/claude-ds/claude-ds`),
`filepath.EvalSymlinks` resolves to the real filesystem path so the updater
replaces the actual binary, not the symlink.

### Release workflow convention

The GitHub Actions workflow (`release.yml`) must publish:
- `claude-ds-darwin-amd64`
- `claude-ds-darwin-arm64`
- `claude-ds-linux-amd64`
- `claude-ds-linux-arm64`
- `checksums.txt` (generated via `sha256sum claude-ds-* > checksums.txt`)

### Config file addition

```
# Update check interval in hours. 0 = check every launch. Default 24.
# update_check_interval=24
```

## Doctor diagnostics

`--doctor` runs 6 checks (down from 7 in the Bash version — python3 is no longer needed):

1. **claude on PATH** — `os/exec.LookPath("claude")`
2. **Config readable** — file exists, parses, schema version check
3. **Secret resolves** — `secretref.Resolve(config.APIKeyRef)` succeeds
4. **API key live** — POST to `{base_url}/v1/messages` with max_tokens=1, 5s timeout
5. **Proxy present** — always ✓ (compiled in)
6. **Tier collision lint** — same logic as current: group tiers by wire ID, warn on collisions

Prints ✓/✗ with actionable next steps. Exits 0.

## First-run onboarding

When config file doesn't exist:

1. Prompt for secret reference (with `system://` selector and `infisical://` builder helpers)
2. Liveness-check the resolved key against DeepSeek
3. On failure, re-ask up to 3 times
4. Prompt for proxy choice (a/m/c/s with spec language reference)
5. Prompt for auto-mode unlock (Y/n)
6. Write config file with defaults and user choices

`--rotate-key` preserves existing `proxy_effort` and `unlock_auto_mode` values.

## System prompt injection

Same pseudo-skill injected via `claude --append-system-prompt` describing:
- claude-ds wrapper context
- Config file location and format
- Secret reference schemes
- Auto-mode unlock mechanism
- Reasoning proxy behavior
- Self-healing features
- Diagnostics commands

Content matches the current implementation; only the resolved model name
and auto-mode status are dynamic.

## Signal handling

```go
cmd := exec.Command("claude", args...)
cmd.Stdin = os.Stdin
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr

// Forward signals to claude child
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
go func() {
    for sig := range sigCh {
        if cmd.Process != nil {
            cmd.Process.Signal(sig)
        }
    }
}()

cmd.Run()
os.Exit(cmd.ProcessState.ExitCode())
```

## What disappears from current implementation

| Current code | Reason gone |
|---|---|
| Orphan watchdog (~25 lines Python) | Proxy is goroutine, process exits with claude |
| Lazy proxy download (~50 lines Bash) | Proxy compiled into binary |
| Python3 presence check (~15 lines Bash) | No Python dependency |
| Curl presence check (~15 lines Bash) | Only needed by installer |
| Symlink resolution for proxy (~20 lines Bash) | Proxy not a separate file |
| Stale proxy detection (~25 lines Bash) | No separate proxy to go stale |
| mkfifo port-passing (~15 lines Bash) | Direct access to listener.Addr() |
| EXIT/INT/TERM trap (~25 lines Bash) | defer + signal.Notify |
| Secretref as copy-paste library block | Go package, importable |

## Implementation notes for agents

- Use only Go stdlib + `golang.org/x/term` (for password input; `x/term` is vendored at build time by `go mod vendor`, producing a fully static binary with zero runtime dependencies)
- `go.mod` module path: `github.com/earchibald/claude-ds`
- Go 1.22+ (for `net/http.ServeMux` pattern matching if desired)
- Build: `CGO_ENABLED=0 go build -ldflags="-s -w"` for static binary
- The proxy's upstream forwarding must handle SSE streaming (use `http.Client` with `DisableCompression`, stream `resp.Body` directly)
- Config file writes must use `os.O_CREATE|os.O_WRONLY|os.O_TRUNC` with mode 0600
- All `os/exec` calls to external CLIs (`op`, `security`, `tmux`, etc.) must handle the binary-not-found case gracefully
- The Files API cache needs a `sync.RWMutex` for concurrent read/write access
