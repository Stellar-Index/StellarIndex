package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

func TestLoadReader_happyPath(t *testing.T) {
	tomlBody := `
[region]
id = "r2"
name = "Ashburn"

[stellar]
network = "pubnet"

[storage]
postgres_dsn = "postgres://u:p@h/db"
`
	c, err := cfg.LoadReader(strings.NewReader(tomlBody), "test.toml")
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	if c.Region.ID != "r2" {
		t.Errorf("region.id = %q, want r2", c.Region.ID)
	}
	if c.Region.Name != "Ashburn" {
		t.Errorf("region.name = %q", c.Region.Name)
	}
	// Default home_domain survives when the file omits it.
	if c.Region.HomeDomain != "stellarindex.io" {
		t.Errorf("default home_domain not applied, got %q", c.Region.HomeDomain)
	}
	if c.Storage.PostgresDSN != "postgres://u:p@h/db" {
		t.Errorf("postgres_dsn = %q", c.Storage.PostgresDSN)
	}
	// Default ingestion.enabled_sources should persist through file parse.
	if len(c.Ingestion.EnabledSources) == 0 {
		t.Error("default enabled_sources not preserved")
	}
}

func TestLoadReader_AggregatePairsAndWindows(t *testing.T) {
	body := `
[region]
id = "r1"

[storage]
postgres_dsn = "postgres://u:p@h/db"

[aggregate]
pairs   = ["crypto:XLM/fiat:USD", "crypto:BTC/fiat:USD"]
windows = ["5m", "1h", "24h"]
`
	c, err := cfg.LoadReader(strings.NewReader(body), "test.toml")
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	if got := len(c.Aggregate.Pairs); got != 2 {
		t.Errorf("Pairs len = %d want 2", got)
	}
	pairs, perr := c.Aggregate.AggregatorPairs()
	if perr != nil {
		t.Fatalf("AggregatorPairs: %v", perr)
	}
	if len(pairs) != 2 || pairs[0].Base.Code != "XLM" || pairs[1].Base.Code != "BTC" {
		t.Errorf("AggregatorPairs result: %+v", pairs)
	}

	wins, werr := c.Aggregate.AggregatorWindows()
	if werr != nil {
		t.Fatalf("AggregatorWindows: %v", werr)
	}
	if len(wins) != 3 || wins[0].String() != "5m0s" {
		t.Errorf("AggregatorWindows result: %v", wins)
	}
}

func TestLoadReader_AggregatePairsRejectsMalformed(t *testing.T) {
	body := `
[region]
id = "r1"

[storage]
postgres_dsn = "postgres://u:p@h/db"

[aggregate]
pairs = ["not-a-real-pair-format"]
`
	_, err := cfg.LoadReader(strings.NewReader(body), "test.toml")
	if err == nil {
		t.Fatal("expected validation error for malformed pair")
	}
	if !strings.Contains(err.Error(), "aggregate.pairs") {
		t.Errorf("error should name the field: %v", err)
	}
}

func TestLoadReader_AggregateWindowsRejectsMalformed(t *testing.T) {
	body := `
[region]
id = "r1"

[storage]
postgres_dsn = "postgres://u:p@h/db"

[aggregate]
windows = ["1 fortnight"]
`
	_, err := cfg.LoadReader(strings.NewReader(body), "test.toml")
	if err == nil {
		t.Fatal("expected validation error for malformed window")
	}
	if !strings.Contains(err.Error(), "aggregate.windows") {
		t.Errorf("error should name the field: %v", err)
	}
}

func TestLoadReader_AggregateFlags(t *testing.T) {
	// Verify the new aggregator flags round-trip through TOML.
	body := `
[region]
id = "r1"

[storage]
postgres_dsn = "postgres://u:p@h/db"

[aggregate]
disable_class_filter         = true
enable_stablecoin_fiat_proxy = true
interval_seconds             = 15
max_trades_per_window        = 500
`
	c, err := cfg.LoadReader(strings.NewReader(body), "test.toml")
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	if !c.Aggregate.DisableClassFilter {
		t.Error("disable_class_filter not parsed")
	}
	if !c.Aggregate.EnableStablecoinFiatProxy {
		t.Error("enable_stablecoin_fiat_proxy not parsed")
	}
	if c.Aggregate.IntervalSeconds != 15 {
		t.Errorf("interval_seconds = %d want 15", c.Aggregate.IntervalSeconds)
	}
	if c.Aggregate.MaxTradesPerWindow != 500 {
		t.Errorf("max_trades_per_window = %d want 500", c.Aggregate.MaxTradesPerWindow)
	}
}

func TestLoadReader_rejectsUnknownKeys(t *testing.T) {
	// Silent typos in config are a classic deployment bug. Unknown
	// keys must be a hard error.
	body := `
[region]
id = "r1"
nonsense_field = "oops"
`
	_, err := cfg.LoadReader(strings.NewReader(body), "test.toml")
	if err == nil {
		t.Fatal("expected unknown-key error, got nil")
	}
	if !strings.Contains(err.Error(), "nonsense_field") {
		t.Errorf("error should name the offending key: %v", err)
	}
}

func TestLoad_readsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.toml")
	body := `
[region]
id = "r3"
name = "Singapore"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := cfg.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Region.ID != "r3" {
		t.Errorf("got %q", c.Region.ID)
	}
}

func TestLoad_ExampleConfigValid(t *testing.T) {
	// The checked-in configs/example.toml is the reference operators
	// copy for fresh deployments — it MUST load + validate cleanly.
	// Resolve relative to the test file: ../../configs/example.toml.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wd, "..", "..", "configs", "example.toml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example.toml not at %s: %v", path, err)
	}
	c, err := cfg.Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	// Smoke-check: region + listen came from the file, not defaults.
	if c.Region.ID == "" {
		t.Error("region.id didn't populate from file")
	}
	if c.API.ListenAddr == "" {
		t.Error("api.listen_addr didn't populate from file")
	}
}

// TestAnsibleFiatPegStanza_ValidAndComplete pins the r1 template's
// declared fiat-peg map through the REAL decoder + validator, the same
// wrapper-level protection the triangulation stanza gets in
// internal/aggregate/orchestrator: nothing else in the build parses the
// jinja template, so a typo'd issuer or ticker would otherwise surface
// only as an API that refuses to boot on r1 (or, worse, a peg that
// silently never fills). The stanza's BODY is pure TOML, but it is now
// gated behind {% if run_aggregator %} (on for r1/pubnet, off for the lean
// test nets) — so the extractor drops jinja control lines to reconstruct the
// r1 render (aggregator on → body present). Pins the two operator-approved
// entries (2026-08-24): AUDD → AUD and, transitively via the AUDD↔AUDR par
// corridor, AUDR → AUD.
func TestAnsibleFiatPegStanza_ValidAndComplete(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wd, "..", "..", "configs", "ansible", "roles",
		"archival-node", "templates", "stellarindex.toml.j2")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("ansible template not at %s: %v", path, err)
	}
	// Extract the [pricing_guard] table (incl. its sub-tables) — the
	// rest of the file carries jinja substitutions and can't round-trip
	// the decoder whole.
	var out []string
	inStanza := false
	inJinjaComment := false // inside a multi-line {# ... #} block
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip the body of a multi-line jinja comment opened below.
		if inJinjaComment {
			if strings.Contains(line, "#}") {
				inJinjaComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			inStanza = trimmed == "[pricing_guard]" ||
				strings.HasPrefix(trimmed, "[pricing_guard.")
		}
		if !inStanza {
			continue
		}
		// Drop jinja control lines ({% if/endif %}) and jinja comments
		// ({# ... #}, possibly spanning lines) so the r1-rendered body (the
		// {% if run_aggregator %} branch, on for r1) parses as TOML. Safe
		// here because the stanza has no {% else %} branch to disambiguate.
		if strings.HasPrefix(trimmed, "{%") {
			continue
		}
		if strings.HasPrefix(trimmed, "{#") {
			if !strings.Contains(trimmed, "#}") {
				inJinjaComment = true // closes on a later line
			}
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatalf("no [pricing_guard] stanza in %s", path)
	}
	c, err := cfg.LoadReader(strings.NewReader(strings.Join(out, "\n")), "stellarindex.toml.j2:pricing_guard")
	if err != nil {
		t.Fatalf("deployed pricing_guard stanza does not load: %v", err)
	}
	pegs := c.PricingGuard.FiatPeggedClassicAssets
	want := map[string]string{
		"AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU": "AUD",
		"AUDR-GAAVW6EQ4N4SHNTKBLTOBXKS6CEIMT2KZI7YQ5B37ECNVPFLBIGRKLIL": "AUD",
	}
	if !reflect.DeepEqual(pegs, want) {
		t.Errorf("deployed fiat_pegged_classic_assets = %v, want %v", pegs, want)
	}
}

func TestLoad_missingFileErrorsNice(t *testing.T) {
	_, err := cfg.Load("/absolutely/not/a/real/path.toml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "not/a/real") {
		t.Errorf("error should include the path: %v", err)
	}
}

func TestApplyEnvOverrides_CoversEveryEnvTag(t *testing.T) {
	// Drift check: every field in the config schema that declares an
	// `env:"…"` tag MUST be honoured by ApplyEnvOverrides. Without
	// this test a new secret-referencing field could ship with the
	// env override silently ignored.
	fields := cfg.Describe()
	var envVars []string
	for _, f := range fields {
		if f.Env != "" {
			envVars = append(envVars, f.Env)
		}
	}
	if len(envVars) == 0 {
		t.Fatal("schema produced zero env-tagged fields — Describe() regression?")
	}

	// Use a sentinel that can't arise from defaults so we can tell
	// whether the override landed.
	const sentinel = "_test_env_override_sentinel_"
	for _, name := range envVars {
		t.Setenv(name, sentinel+name)
	}

	c := cfg.Default()
	c.ApplyEnvOverrides()

	// Serialise the fields via reflect and check that every env-
	// tagged leaf's value starts with the sentinel.
	for _, f := range envVars {
		val := lookupFieldByEnv(&c, f, fields)
		if val == "" {
			t.Errorf("env override %s: field value is empty — ApplyEnvOverrides didn't wire this field",
				f)
			continue
		}
		if !strings.HasPrefix(val, sentinel) {
			t.Errorf("env override %s: field value %q doesn't start with sentinel — ApplyEnvOverrides ignored this env var",
				f, val)
		}
	}
}

// lookupFieldByEnv walks the config via reflect to find the field
// whose `env:` tag matches envName, then returns its stringified
// value. Supports only string leaves (which is what all env-tagged
// fields are today).
func lookupFieldByEnv(c *cfg.Config, envName string, fields []cfg.SchemaField) string {
	v := reflect.ValueOf(c).Elem()
	for _, f := range fields {
		if f.Env != envName {
			continue
		}
		return reflectStringFromPath(v, f.Path)
	}
	return ""
}

// reflectStringFromPath walks a dotted path like
// "storage.postgres_dsn" down the struct via its toml tags.
func reflectStringFromPath(root reflect.Value, path string) string {
	parts := strings.Split(path, ".")
	cur := root
	for _, p := range parts {
		cur = findFieldByTOMLTag(cur, p)
		if !cur.IsValid() {
			return ""
		}
	}
	if cur.Kind() == reflect.String {
		return cur.String()
	}
	return ""
}

func findFieldByTOMLTag(v reflect.Value, tag string) reflect.Value {
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i)
		if ft.Tag.Get("toml") == tag {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "postgres://from-env/db")
	c := cfg.Default()
	c.ApplyEnvOverrides()
	if c.Storage.PostgresDSN != "postgres://from-env/db" {
		t.Errorf("env override didn't land: %q", c.Storage.PostgresDSN)
	}

	// Unset env var → no change.
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "")
	c2 := cfg.Default()
	original := c2.Storage.PostgresDSN
	c2.ApplyEnvOverrides()
	if c2.Storage.PostgresDSN != original {
		t.Errorf("empty env should not override: %q", c2.Storage.PostgresDSN)
	}
}

// TestApplyEnvOverrides_DoesNotCorruptS3KeyNames pins A16-01
// (audit-2026-06-14): S3AccessKeyEnv / S3SecretKeyEnv hold the NAME of the env
// var carrying the credential (buildS3Client does os.Getenv on them), NOT the
// value. ApplyEnvOverrides must NOT overwrite the name with the secret value —
// doing so made os.Getenv("AKIA…")→"" and silently dropped S3 static creds.
func TestApplyEnvOverrides_DoesNotCorruptS3KeyNames(t *testing.T) {
	t.Setenv("STELLARINDEX_S3_ACCESS_KEY", "AKIAEXAMPLE")
	t.Setenv("STELLARINDEX_S3_SECRET_KEY", "supersecret")
	c := cfg.Default()
	wantAccess, wantSecret := c.Storage.S3AccessKeyEnv, c.Storage.S3SecretKeyEnv
	c.ApplyEnvOverrides()
	if c.Storage.S3AccessKeyEnv != wantAccess {
		t.Errorf("S3AccessKeyEnv was overwritten with the secret value: got %q, want the name %q",
			c.Storage.S3AccessKeyEnv, wantAccess)
	}
	if c.Storage.S3SecretKeyEnv != wantSecret {
		t.Errorf("S3SecretKeyEnv was overwritten with the secret value: got %q, want the name %q",
			c.Storage.S3SecretKeyEnv, wantSecret)
	}
}

func TestLoadWithEnv_RevalidatesAfterOverride(t *testing.T) {
	// A file that Validate() accepts, then env override with a
	// malformed DSN. Previously (Load + ApplyEnvOverrides) this got
	// past startup and errored at DB dial time. LoadWithEnv must
	// catch it as ErrInvalidConfig.
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	good := `
[region]
id = "r1"
home_domain = "stellarindex.io"

[stellar]
network = "pubnet"
rpc_endpoints = ["http://rpc:8000"]

[storage]
postgres_dsn = "postgres://valid@host/db"
`
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	// Sanity: file validates.
	if _, err := cfg.LoadWithEnv(path); err != nil {
		t.Fatalf("clean env should load: %v", err)
	}

	// Env override with a malformed DSN.
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "mysql://not-a-postgres-url")
	_, err := cfg.LoadWithEnv(path)
	if err == nil {
		t.Fatal("bad env-var DSN must be rejected by LoadWithEnv")
	}
	if !strings.Contains(err.Error(), "postgres_dsn") {
		t.Errorf("err should name the offending field: %v", err)
	}
}

// TestApplyEnvOverrides_ReturnsOverriddenFieldPaths — CFG-01
// (audit-2026-07-23). Asserts the corrected value: ApplyEnvOverrides
// returns exactly the config-path of each field an env var actually
// replaced, and nothing for vars that were unset/empty — the data
// LoadWithEnv logs so an operator can see WHICH fields the
// environment silently won over the TOML file.
func TestApplyEnvOverrides_ReturnsOverriddenFieldPaths(t *testing.T) {
	t.Setenv("STELLARINDEX_POSTGRES_DSN", "postgres://from-env/db")
	t.Setenv("STELLARINDEX_REDIS_PASSWORD", "s3cr3t")
	// Deliberately left unset: STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD,
	// EXCHANGERATESAPI_KEY, etc. — must NOT appear in the result.

	c := cfg.Default()
	got := c.ApplyEnvOverrides()

	want := []string{"storage.postgres_dsn", "storage.redis_password_env"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyEnvOverrides() = %v, want %v", got, want)
	}
}

// TestApplyEnvOverrides_NoOverridesReturnsEmpty confirms the common
// case (no relevant env vars set) returns an empty/nil list rather
// than a slice of empty strings or similar placeholder noise.
func TestApplyEnvOverrides_NoOverridesReturnsEmpty(t *testing.T) {
	for _, name := range []string{
		"STELLARINDEX_POSTGRES_DSN", "STELLARINDEX_REDIS_PASSWORD",
		"STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD", "EXCHANGERATESAPI_KEY",
		"POLYGON_API_KEY", "COINMARKETCAP_API_KEY", "CRYPTOCOMPARE_API_KEY",
		"COINGECKO_API_KEY", "CHAINLINK_RPC_URL",
	} {
		t.Setenv(name, "")
	}
	c := cfg.Default()
	if got := c.ApplyEnvOverrides(); len(got) != 0 {
		t.Errorf("ApplyEnvOverrides() with nothing set = %v, want empty", got)
	}
}

// TestLoadWithEnv_LogsOverriddenFieldsWithoutValues — CFG-01
// (audit-2026-07-23). LoadWithEnv must log which fields an env
// override touched, and must NEVER log the field's value (most
// overridden fields are secrets by construction).
func TestLoadWithEnv_LogsOverriddenFieldsWithoutValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.toml")
	good := `
[region]
id = "r1"
home_domain = "stellarindex.io"

[stellar]
network = "pubnet"
rpc_endpoints = ["http://rpc:8000"]

[storage]
postgres_dsn = "postgres://valid@host/db"
`
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	const secretValue = "postgres://SECRET-CREDS-MUST-NOT-BE-LOGGED@evil-host/db"
	t.Setenv("STELLARINDEX_POSTGRES_DSN", secretValue)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := cfg.LoadWithEnv(path); err != nil {
		t.Fatalf("LoadWithEnv: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "storage.postgres_dsn") {
		t.Errorf("expected the overridden field path in the log output, got: %s", out)
	}
	if strings.Contains(out, secretValue) {
		t.Errorf("secret value leaked into the log output: %s", out)
	}
}
