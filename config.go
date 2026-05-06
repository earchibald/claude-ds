// Config-file loader for the Go rewrite (CDS-12).
//
// This file is a direct port of the `_cds_validate_config_file`,
// `_cds_repair_config_file`, `_cds_read_schema`, and `_cds_migrate_config`
// functions from the Bash launcher (`./claude-ds`), plus the six OTLP
// additions documented in the design spec (Observability section). The
// goal is bit-for-bit semantic compatibility with the Bash-era config so
// every existing user file round-trips through this loader unchanged.
//
// Pipeline (mirrors the Bash launcher line-for-line):
//
//  1. Validate every non-comment, non-blank line against the regex
//     `^[a-zA-Z_][a-zA-Z0-9_]*=.+$`. On any failure: back up to
//     `config.broken.<timestamp>.bak` and rewrite, keeping only valid
//     known-key lines, with `_schema=<CURRENT_SCHEMA>` ensured.
//  2. Read `_schema`. If behind `CURRENT_SCHEMA`, back up to
//     `config.v<old>.bak` and apply migrations in order. Migration 0→1:
//     prepend `_schema=1`, strip any pre-existing `_schema=` lines.
//  3. Parse all lines into a Config struct. Unknown keys are warned to
//     stderr (per acceptance criterion 4) and preserved in `Config.Unknown`
//     so they survive a write-back round-trip.
//  4. Apply defaults for unset fields (`base_url`, `model`, `proxy_effort`,
//     `proxy_bind`, `update_check_interval`, plus OTLP defaults for
//     `otlp_service_name`, `otlp_deployment_environment`, `otlp_protocol`).
//  5. Apply OTel env-var overrides at the end, routing each value through
//     the secretref resolver (`resolveFn`) so secret refs work in env vars
//     too. Env values never reach disk — they are merged in-process only.
//
// `resolveFn` is a package-level `var` that defaults to `Resolve` (defined
// by CDS-13 in `secretref.go`). Tests overwrite it directly to avoid
// depending on a real secret resolver.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CurrentSchema is the schema version this build understands. Bump (and
// add a migration in `migrateFromV()`) on incompatible changes. Existing
// pre-versioned configs are treated as schema 0 and migrated forward.
const CurrentSchema = 1

// Defaults populated for any unset key on Load. These match the Bash
// launcher's `DEFAULT_BASE_URL` / `DEFAULT_MODEL` constants and the
// design spec's "Defaults" subsection.
const (
	defaultBaseURL                   = "https://api.deepseek.com/anthropic"
	defaultModel                     = "deepseek-v4-pro"
	defaultProxyEffort               = "off"
	defaultProxyBind                 = "127.0.0.1"
	defaultUpdateCheckInterval       = 24 // hours
	defaultOTLPServiceName           = "claude-ds-proxy"
	defaultOTLPDeploymentEnvironment = "local"
	defaultOTLPProtocol              = "http"
)

// configLineRE is the validator regex from the Bash launcher (line 864).
// Any non-comment, non-blank line that doesn't match this triggers the
// repair flow.
var configLineRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*=.+$`)

// configKeyValueRE is the parser regex used by the Bash repair function
// (line 896). The validator has already enforced the strict form; this
// version just splits the captured key from the value.
var configKeyValueRE = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)=(.*)$`)

// knownKeys is the canonical list of recognized config keys. Order is
// load-bearing: WriteConfig emits keys in this order so a freshly-written
// file is grouped predictably (schema, identity, models, capabilities,
// proxy, update, OTLP). Anything outside this list is preserved in
// `Config.Unknown` and emitted alphabetically after the known section.
var knownKeys = []string{
	"_schema",
	"api_key_ref",
	"base_url",
	"model",
	"model_opus",
	"model_sonnet",
	"model_haiku",
	"model_small_fast",
	"capabilities",
	"unlock_auto_mode",
	"proxy_effort",
	"proxy_effort_opus",
	"proxy_effort_sonnet",
	"proxy_effort_haiku",
	"proxy_effort_small_fast",
	"proxy_strip_thinking",
	"proxy_bind",
	"proxy_debug",
	"update_check_interval",
	"otlp_endpoints",
	"otlp_headers",
	"otlp_service_name",
	"otlp_deployment_environment",
	"otlp_resource_attributes",
	"otlp_protocol",
}

// knownKeysSet is the membership lookup derived from knownKeys.
var knownKeysSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(knownKeys))
	for _, k := range knownKeys {
		m[k] = struct{}{}
	}
	return m
}()

// resolveFn is the indirection point for the secretref resolver. CDS-13
// defines `Resolve(ref string) (string, error)` in `secretref.go`; the
// indirection lets tests stub the resolver without depending on a real
// implementation. Mutation is guarded by resolveFnMu so parallel tests
// (and any future async readers) don't race on the global. Production
// code reads the resolver through callResolve(...) below.
var (
	resolveFnMu sync.RWMutex
	resolveFn   = Resolve
)

// callResolve invokes the current resolveFn under a read lock so that
// concurrent reads with a parallel-test setter are race-free.
func callResolve(ref string) (string, error) {
	resolveFnMu.RLock()
	fn := resolveFn
	resolveFnMu.RUnlock()
	return fn(ref)
}

// Config is the parsed-and-validated representation of the on-disk
// config file. Field set matches the design spec's "Config struct"
// subsection plus the six OTLP additions for CDS-25.
type Config struct {
	Schema int

	APIKeyRef string

	BaseURL string
	Model   string

	ModelOpus      string
	ModelSonnet    string
	ModelHaiku     string
	ModelSmallFast string

	Capabilities   string
	UnlockAutoMode bool

	ProxyEffort          string
	ProxyEffortOpus      string
	ProxyEffortSonnet    string
	ProxyEffortHaiku     string
	ProxyEffortSmallFast string

	ProxyStripThinking string
	ProxyBind          string
	ProxyDebug         bool

	UpdateCheckInterval int

	// OTLP (CDS-25). Empty OTLPEndpoints disables export entirely.
	OTLPEndpoints             []string          // CSV in file
	OTLPHeaders               map[string]string // semicolon-separated `Name: value` in file
	OTLPServiceName           string
	OTLPDeploymentEnvironment string
	OTLPResourceAttributes    map[string]string // comma-separated `key=value` in file
	OTLPProtocol              string            // "http" | "grpc"

	// Unknown captures every non-known key found in the file. They are
	// preserved verbatim and re-emitted by WriteConfig so a future
	// claude-ds release that knows the key picks it up unchanged.
	Unknown map[string]string

	// Path is the absolute path the config was loaded from. Used by
	// WriteConfig and the migrate/repair backup paths.
	Path string
}

// LoadConfig reads, validates, repairs, migrates, and parses the config
// at `path`. If the file does not exist, an empty Config populated with
// defaults is returned (matches the Bash launcher's first-run behavior:
// the absence of a file is not an error).
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{Path: path, Unknown: map[string]string{}}

	// Missing file → defaults only. The launcher's `--setup` flow writes
	// a real file later; this lets non-interactive paths (tests, doctor
	// stubs) still get a usable Config.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		applyDefaults(cfg)
		applyEnvOverrides(cfg)
		return cfg, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat config %s: %w", path, err)
	}

	// Phase 1: validate; repair if damaged.
	damaged, err := ValidateConfigFile(path)
	if err != nil {
		return nil, err
	}
	if damaged {
		if err := RepairConfig(path); err != nil {
			return nil, err
		}
	}

	// Phase 2: migrate forward to CurrentSchema.
	if err := MigrateConfig(path); err != nil {
		return nil, err
	}

	// Phase 3: parse the now-clean file.
	if err := parseInto(path, cfg); err != nil {
		return nil, err
	}

	// Phase 4: defaults for anything the file didn't set.
	applyDefaults(cfg)

	// Phase 5: OTel env-var overrides (resolved through secretrefs).
	applyEnvOverrides(cfg)

	return cfg, nil
}

// ValidateLine reports whether `line` is a valid `key=value` line. Blank
// lines and lines starting with `#` (optionally after whitespace) are
// considered valid (they're skipped by the validator). Anything else
// must match the strict regex.
func ValidateLine(line string) bool {
	trim := strings.TrimSpace(line)
	if trim == "" || strings.HasPrefix(trim, "#") {
		return true
	}
	return configLineRE.MatchString(line)
}

// ValidateConfigFile inspects every non-blank, non-comment line in the
// file at `path`. Returns (damaged=true, nil) if any line fails the
// validator, (false, nil) otherwise. Pure inspection — never mutates.
//
// Mirrors `_cds_validate_config_file()` in the Bash launcher.
func ValidateConfigFile(path string) (damaged bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read config %s: %w", path, err)
	}
	for lineno, raw := range strings.Split(string(data), "\n") {
		// Mirror the Bash trailing-newline behavior: an empty trailing
		// element from a final \n is silently skipped.
		if raw == "" {
			continue
		}
		if !ValidateLine(raw) {
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠ config line %d is malformed: %q\n",
				lineno+1, raw)
			damaged = true
		}
	}
	return damaged, nil
}

// RepairConfig backs up the damaged config to
// `config.broken.<timestamp>.bak`, then rewrites the file keeping only
// valid `key=value` lines whose key is in `knownKeys`. Unknown keys are
// warned and dropped during repair (matches the Bash launcher's
// "schema/version mismatch" semantics — we'd rather be conservative and
// drop a key we don't recognize than carry forward a value that may have
// been written by a *future* claude-ds with a different meaning).
//
// The schema line is forced to `_schema=<CurrentSchema>` regardless of
// what the file said (the validator may have dropped the schema line as
// malformed). Idempotent.
//
// Mirrors `_cds_repair_config_file()` in the Bash launcher.
func RepairConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}

	stamp := time.Now().Format("20060102150405")
	backup := path + ".broken." + stamp + ".bak"
	if err := copyFile(path, backup); err != nil {
		return fmt.Errorf("back up damaged config to %s: %w", backup, err)
	}
	fmt.Fprintf(os.Stderr,
		"claude-ds: ⚠ damaged config detected — backed up original to %s\n",
		backup)

	// Walk every non-comment line; keep last-write of each known key.
	type kv struct{ k, v string }
	var kept []kv
	idx := map[string]int{}
	for lineno, raw := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(raw)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		m := configKeyValueRE.FindStringSubmatch(raw)
		if m == nil {
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠   dropping malformed line %d: %q\n",
				lineno+1, raw)
			continue
		}
		k, v := m[1], m[2]
		if _, ok := knownKeysSet[k]; !ok {
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠   dropping unknown key %q (line %d) — not in this version's schema\n",
				k, lineno+1)
			continue
		}
		if i, exists := idx[k]; exists {
			kept[i].v = v
		} else {
			idx[k] = len(kept)
			kept = append(kept, kv{k, v})
		}
	}

	// Force `_schema=<CurrentSchema>`; insert at the front if missing.
	if i, exists := idx["_schema"]; exists {
		kept[i].v = strconv.Itoa(CurrentSchema)
	} else {
		kept = append([]kv{{"_schema", strconv.Itoa(CurrentSchema)}}, kept...)
	}

	// Build the canonical output and write it atomically. Header lines
	// match the Bash launcher's repair output to keep `diff` output
	// minimal for users who repair-and-grep.
	var out strings.Builder
	fmt.Fprintf(&out, "# claude-ds config — repaired %s\n",
		time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&out, "# Original (damaged) backed up at: %s\n", backup)
	out.WriteString("#\n")
	for _, p := range kept {
		fmt.Fprintf(&out, "%s=%s\n", p.k, p.v)
	}
	if err := writeFile0600(path, out.String()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"claude-ds: repaired config — preserved %d key(s); see %s for the original.\n",
		len(kept), backup)
	return nil
}

// MigrateConfig reads the file's `_schema` and walks forward to
// `CurrentSchema`. Backs up to `config.v<old>.bak` before the first
// mutating step. Migrations run in order; each `migrateFromV(N)` call
// transforms a schema-N file into a schema-(N+1) file in place.
//
// Mirrors `_cds_migrate_config()` in the Bash launcher.
func MigrateConfig(path string) error {
	from, err := readSchemaInt(path)
	if err != nil {
		return err
	}
	if from == CurrentSchema {
		return nil
	}
	if from > CurrentSchema {
		return fmt.Errorf(
			"config schema is v%d but this claude-ds only understands up to v%d. "+
				"Upgrade claude-ds, or back up + remove %s to start fresh",
			from, CurrentSchema, path)
	}
	backup := fmt.Sprintf("%s.v%d.bak", path, from)
	if err := copyFile(path, backup); err != nil {
		return fmt.Errorf("back up config to %s before migration: %w", backup, err)
	}
	for cur := from; cur < CurrentSchema; cur++ {
		if err := migrateFromV(path, cur); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr,
		"claude-ds: upgraded config from schema v%d to v%d (backup at %s).\n",
		from, CurrentSchema, backup)
	return nil
}

// migrateFromV transforms a schema-N file into a schema-(N+1) file in
// place. Each step is idempotent on a file already at the target
// schema, so re-running migrations is safe.
func migrateFromV(path string, from int) error {
	switch from {
	case 0:
		// 0→1: prepend `_schema=1`, strip any pre-existing `_schema=`
		// lines (the validator should have caught duplicates, but a
		// hand-edit could produce them).
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var out strings.Builder
		out.WriteString("_schema=1\n")
		for _, raw := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(raw, "_schema=") {
				continue
			}
			out.WriteString(raw)
			out.WriteByte('\n')
		}
		// Trim the doubled trailing newline introduced by the loop above
		// when the input file already ended with one.
		s := out.String()
		s = strings.TrimRight(s, "\n") + "\n"
		return writeFile0600(path, s)
	default:
		return fmt.Errorf("no migration registered for schema v%d → v%d", from, from+1)
	}
}

// readSchemaInt returns the integer value of the `_schema` line, or 0 if
// no such line exists (legacy, pre-versioned configs).
func readSchemaInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read config %s: %w", path, err)
	}
	for _, raw := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(raw)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if !strings.HasPrefix(raw, "_schema=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(raw, "_schema="))
		n, err := strconv.Atoi(v)
		if err != nil {
			// A malformed `_schema=` line is treated as schema 0 — the
			// validator will have caught it and routed through repair.
			return 0, nil
		}
		return n, nil
	}
	return 0, nil
}

// parseInto reads the (already validated and migrated) file and fills
// every recognized key on cfg. Unknown keys are warned and stashed on
// cfg.Unknown.
func parseInto(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	for lineno, raw := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(raw)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		m := configKeyValueRE.FindStringSubmatch(raw)
		if m == nil {
			// Should be unreachable post-validate, but be defensive: a
			// race with another writer could land us here.
			return fmt.Errorf("config %s line %d: malformed key=value", path, lineno+1)
		}
		k, v := m[1], m[2]
		if _, ok := knownKeysSet[k]; !ok {
			fmt.Fprintf(os.Stderr,
				"claude-ds: ⚠ unknown config key %q (line %d) — preserved verbatim on rewrite\n",
				k, lineno+1)
			cfg.Unknown[k] = v
			continue
		}
		assignKnown(cfg, k, v)
	}
	return nil
}

// assignKnown writes a single known key/value pair into cfg. Type
// coercion (bool, int, list, map) lives here, so the rest of the loader
// stays string-clean.
func assignKnown(cfg *Config, k, v string) {
	switch k {
	case "_schema":
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Schema = n
		}
	case "api_key_ref":
		cfg.APIKeyRef = v
	case "base_url":
		cfg.BaseURL = v
	case "model":
		cfg.Model = v
	case "model_opus":
		cfg.ModelOpus = v
	case "model_sonnet":
		cfg.ModelSonnet = v
	case "model_haiku":
		cfg.ModelHaiku = v
	case "model_small_fast":
		cfg.ModelSmallFast = v
	case "capabilities":
		cfg.Capabilities = v
	case "unlock_auto_mode":
		cfg.UnlockAutoMode = parseBool(v)
	case "proxy_effort":
		cfg.ProxyEffort = v
	case "proxy_effort_opus":
		cfg.ProxyEffortOpus = v
	case "proxy_effort_sonnet":
		cfg.ProxyEffortSonnet = v
	case "proxy_effort_haiku":
		cfg.ProxyEffortHaiku = v
	case "proxy_effort_small_fast":
		cfg.ProxyEffortSmallFast = v
	case "proxy_strip_thinking":
		cfg.ProxyStripThinking = v
	case "proxy_bind":
		cfg.ProxyBind = v
	case "proxy_debug":
		cfg.ProxyDebug = parseBool(v)
	case "update_check_interval":
		if n, err := strconv.Atoi(v); err == nil {
			cfg.UpdateCheckInterval = n
		}
	case "otlp_endpoints":
		cfg.OTLPEndpoints = splitCSV(v)
	case "otlp_headers":
		cfg.OTLPHeaders = parseHeadersFile(v)
	case "otlp_service_name":
		cfg.OTLPServiceName = v
	case "otlp_deployment_environment":
		cfg.OTLPDeploymentEnvironment = v
	case "otlp_resource_attributes":
		cfg.OTLPResourceAttributes = parseKVList(v, ",")
	case "otlp_protocol":
		cfg.OTLPProtocol = v
	}
}

// parseBool accepts the same truthy spellings as the Bash launcher's
// `case` arms (1/true/yes/on). Anything else is false.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// splitCSV splits a comma-separated list, trims whitespace, and drops
// empty entries.
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseHeadersFile parses the file-format `otlp_headers` value:
// semicolon-separated `Name: value` pairs.
func parseHeadersFile(v string) map[string]string {
	return parseKVPairs(v, ";", ":")
}

// parseKVList parses a `sep`-separated list of `key=value` pairs.
func parseKVList(v, sep string) map[string]string {
	return parseKVPairs(v, sep, "=")
}

// parseKVPairs is the workhorse for both `otlp_headers` and
// `otlp_resource_attributes`: split on `pairSep`, then on `kvSep`. Empty
// keys or values are silently dropped.
func parseKVPairs(v, pairSep, kvSep string) map[string]string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(v, pairSep) {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.Index(pair, kvSep)
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(pair[:i])
		val := strings.TrimSpace(pair[i+1:])
		if k == "" || val == "" {
			continue
		}
		out[k] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyDefaults populates any unset key with the spec-defined default.
// Schema is *not* defaulted here — readSchemaInt returns 0 for a missing
// line, and that's the right value for the Schema field on a freshly
// migrated file (the migrate step writes _schema=1 for us).
func applyDefaults(cfg *Config) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Model == "" {
		cfg.Model = defaultModel
	}
	if cfg.ProxyEffort == "" {
		cfg.ProxyEffort = defaultProxyEffort
	}
	if cfg.ProxyBind == "" {
		cfg.ProxyBind = defaultProxyBind
	}
	if cfg.UpdateCheckInterval == 0 {
		cfg.UpdateCheckInterval = defaultUpdateCheckInterval
	}
	if cfg.OTLPServiceName == "" {
		cfg.OTLPServiceName = defaultOTLPServiceName
	}
	if cfg.OTLPDeploymentEnvironment == "" {
		cfg.OTLPDeploymentEnvironment = defaultOTLPDeploymentEnvironment
	}
	if cfg.OTLPProtocol == "" {
		cfg.OTLPProtocol = defaultOTLPProtocol
	}
	if cfg.Unknown == nil {
		cfg.Unknown = map[string]string{}
	}
}

// applyEnvOverrides honors the standard OTel env vars at load time. Each
// value is routed through `resolveFn` so secret refs work in env vars
// too — the resolved value lives only in the Config's in-memory maps and
// never reaches stdout/stderr/disk. Per the OTel spec, env-var headers
// are *comma*-separated `name=value` pairs (distinct from the
// semicolon-separated file format).
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		// Resolve secret refs (a `op://...` endpoint is unusual but
		// permitted; mostly this is a no-op passthrough for plain URLs).
		if resolved, err := callResolve(v); err == nil && resolved != "" {
			cfg.OTLPEndpoints = appendUnique(cfg.OTLPEndpoints, resolved)
		}
	}
	if v := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); v != "" {
		merged := cfg.OTLPHeaders
		if merged == nil {
			merged = map[string]string{}
		}
		for hk, hv := range parseKVList(v, ",") {
			if resolved, err := callResolve(hv); err == nil && resolved != "" {
				merged[hk] = resolved
			}
		}
		cfg.OTLPHeaders = merged
	}
	if v := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); v != "" {
		merged := cfg.OTLPResourceAttributes
		if merged == nil {
			merged = map[string]string{}
		}
		for rk, rv := range parseKVList(v, ",") {
			// Resource attribute values are not typically secret, but
			// we still route them through resolveFn so a user *can*
			// stash one in 1Password if they wish (e.g. tenant IDs).
			if resolved, err := callResolve(rv); err == nil && resolved != "" {
				merged[rk] = resolved
			}
		}
		cfg.OTLPResourceAttributes = merged
	}
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// WriteConfig serializes cfg to disk at cfg.Path (or `path` if cfg.Path
// is empty), mode 0600, atomic-truncate. Output ordering: a header
// comment, then known keys in `knownKeys` declaration order (omitting
// any whose value is the zero value or matches the default), then
// unknown keys sorted alphabetically.
//
// Skipping default-equal values keeps the file small and humane — a
// brand-new install with no overrides writes the schema line and
// nothing else.
func WriteConfig(path string, cfg *Config) error {
	if path == "" {
		path = cfg.Path
	}
	if path == "" {
		return fmt.Errorf("WriteConfig: no path provided and cfg.Path is empty")
	}
	if cfg.Schema == 0 {
		cfg.Schema = CurrentSchema
	}

	var out strings.Builder
	fmt.Fprintf(&out, "# claude-ds config — written %s\n",
		time.Now().Format("2006-01-02 15:04:05"))
	out.WriteString("# Schema: ")
	out.WriteString(strconv.Itoa(cfg.Schema))
	out.WriteString("\n#\n")

	for _, k := range knownKeys {
		v, ok := serializeKnown(cfg, k)
		if !ok {
			continue
		}
		fmt.Fprintf(&out, "%s=%s\n", k, v)
	}

	// Unknown keys, sorted for deterministic output.
	if len(cfg.Unknown) > 0 {
		keys := make([]string, 0, len(cfg.Unknown))
		for k := range cfg.Unknown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&out, "%s=%s\n", k, cfg.Unknown[k])
		}
	}

	// Make sure the parent directory exists at 0700; the launcher's
	// install path creates this, but tests using a temp dir need it too.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return writeFile0600(path, out.String())
}

// serializeKnown returns the on-disk string form of the named key, plus
// `ok=false` for keys whose current value is the zero/default and
// shouldn't be emitted. The schema line is always emitted.
func serializeKnown(cfg *Config, k string) (string, bool) {
	switch k {
	case "_schema":
		return strconv.Itoa(cfg.Schema), true
	case "api_key_ref":
		return cfg.APIKeyRef, cfg.APIKeyRef != ""
	case "base_url":
		return cfg.BaseURL, cfg.BaseURL != "" && cfg.BaseURL != defaultBaseURL
	case "model":
		return cfg.Model, cfg.Model != "" && cfg.Model != defaultModel
	case "model_opus":
		return cfg.ModelOpus, cfg.ModelOpus != ""
	case "model_sonnet":
		return cfg.ModelSonnet, cfg.ModelSonnet != ""
	case "model_haiku":
		return cfg.ModelHaiku, cfg.ModelHaiku != ""
	case "model_small_fast":
		return cfg.ModelSmallFast, cfg.ModelSmallFast != ""
	case "capabilities":
		return cfg.Capabilities, cfg.Capabilities != ""
	case "unlock_auto_mode":
		if cfg.UnlockAutoMode {
			return "1", true
		}
		return "", false
	case "proxy_effort":
		return cfg.ProxyEffort, cfg.ProxyEffort != "" && cfg.ProxyEffort != defaultProxyEffort
	case "proxy_effort_opus":
		return cfg.ProxyEffortOpus, cfg.ProxyEffortOpus != ""
	case "proxy_effort_sonnet":
		return cfg.ProxyEffortSonnet, cfg.ProxyEffortSonnet != ""
	case "proxy_effort_haiku":
		return cfg.ProxyEffortHaiku, cfg.ProxyEffortHaiku != ""
	case "proxy_effort_small_fast":
		return cfg.ProxyEffortSmallFast, cfg.ProxyEffortSmallFast != ""
	case "proxy_strip_thinking":
		return cfg.ProxyStripThinking, cfg.ProxyStripThinking != ""
	case "proxy_bind":
		return cfg.ProxyBind, cfg.ProxyBind != "" && cfg.ProxyBind != defaultProxyBind
	case "proxy_debug":
		if cfg.ProxyDebug {
			return "1", true
		}
		return "", false
	case "update_check_interval":
		if cfg.UpdateCheckInterval != 0 && cfg.UpdateCheckInterval != defaultUpdateCheckInterval {
			return strconv.Itoa(cfg.UpdateCheckInterval), true
		}
		return "", false
	case "otlp_endpoints":
		if len(cfg.OTLPEndpoints) == 0 {
			return "", false
		}
		return strings.Join(cfg.OTLPEndpoints, ","), true
	case "otlp_headers":
		if len(cfg.OTLPHeaders) == 0 {
			return "", false
		}
		return joinKVPairs(cfg.OTLPHeaders, "; ", ": "), true
	case "otlp_service_name":
		return cfg.OTLPServiceName, cfg.OTLPServiceName != "" && cfg.OTLPServiceName != defaultOTLPServiceName
	case "otlp_deployment_environment":
		return cfg.OTLPDeploymentEnvironment, cfg.OTLPDeploymentEnvironment != "" && cfg.OTLPDeploymentEnvironment != defaultOTLPDeploymentEnvironment
	case "otlp_resource_attributes":
		if len(cfg.OTLPResourceAttributes) == 0 {
			return "", false
		}
		return joinKVPairs(cfg.OTLPResourceAttributes, ", ", "="), true
	case "otlp_protocol":
		return cfg.OTLPProtocol, cfg.OTLPProtocol != "" && cfg.OTLPProtocol != defaultOTLPProtocol
	}
	return "", false
}

// joinKVPairs reassembles a map into a string for on-disk storage,
// sorted by key for stable output.
func joinKVPairs(m map[string]string, pairSep, kvSep string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+kvSep+m[k])
	}
	return strings.Join(parts, pairSep)
}

// copyFile preserves mode and contents — used for the .bak files.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

// writeFile0600 satisfies the spec's I/O contract: O_CREATE|O_WRONLY|
// O_TRUNC, mode 0600. We avoid os.WriteFile here because the spec
// explicitly names the open-flag set, and we want the code to read the
// same way auditors will read the spec.
func writeFile0600(path, contents string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(contents); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Defensive chmod in case umask or an existing file with a wider
	// mode masked our 0600 on creation (umask only restricts; it
	// doesn't promote 0644 to 0600 on an existing file).
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
