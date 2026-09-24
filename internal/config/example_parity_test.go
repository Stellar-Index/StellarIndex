package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestExampleTOMLDocumentsSchemaFields is a regression guard for
// RLT-104: configs/example.toml is hand-maintained beside
// internal/config/config.go's struct tags and had drifted — whole
// config surfaces (anomaly, api.sep10, api.dashboard, metadata,
// price_alerts, signup_reaper, oracle.band/redstone/soroswap, and a
// scatter of individual knobs) were undocumented, so an operator
// reading the example file had no way to discover them.
//
// This pins the specific fields RLT-104 found missing so the drift
// cannot silently return. It is a targeted census, not a full
// schema walk (config.Describe() also emits container/map-key rows
// that have no single literal key to search for).
func TestExampleTOMLDocumentsSchemaFields(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wd, "..", "..", "configs", "example.toml")
	body, err := os.ReadFile(path) //nolint:gosec // fixed repo-relative path
	if err != nil {
		t.Skipf("example.toml not at %s: %v", path, err)
	}
	text := string(body)

	// Leaf key names RLT-104 found entirely absent from
	// configs/example.toml (active or commented) — a superset also
	// serves as the marker that the surrounding section/table exists.
	leaves := []string{
		"confidence_max_freeze", "extension_minutes", "initial_hold_minutes",
		"max_extensions", "source_count_max_freeze",
		"uncorroborated_initial_hold_minutes", "unfreeze_buckets",
		"unfreeze_confidence_min", "unfreeze_z_score_max", "z_score_min_freeze",
		"allow_credentials", "auth_backend", "code_secret_env", "cookie_domain",
		"cookie_secure", "email_from", "magic_link_ttl_minutes",
		"resend_api_key_env", "session_ttl_days", "failed_auth_rate_limit_per_min",
		"prometheus_url", "request_timeout", "challenge_ttl", "web_auth_domain",
		"serving_statement_timeout", "single_instance", "tls_cert_probe_hosts",
		"issuer_home_domains", "standard_reference_contract", "adapter_contract",
		"factory_contract", "seed_rpc_endpoint",
		"divergence_min_interval_seconds", "triangulations",
		"background_statement_timeout", "clickhouse_serving_password_env",
		"max_supply_overrides", "stale_component_ledgers_by_asset",
		"max_concurrent_streams", "max_streams_per_ip", "max_tip_producers",
		"max_tip_producers_per_caller", "feed_map", "demo_api_key",
		"persist_per_source", "disable_substance_gate", "fx_cross_max_age_hours",
		"substance_min_buckets", "substance_min_span_minutes",
		"substance_min_volume_usd", "substance_window_hours",
	}
	// Section headers that RLT-104 found had no [section] stanza at
	// all — a leaf-name-only search can false-pass these (e.g.
	// "enabled" is common to every section) so the header itself is
	// the load-bearing marker.
	sections := []string{
		"[anomaly]", "[anomaly.phase2]", "[api.sep10]", "[api.dashboard]",
		"[metadata]", "[price_alerts]", "[signup_reaper]",
		"[oracle.band]", "[oracle.redstone]", "[oracle.soroswap]",
	}

	for _, leaf := range leaves {
		// A leaf is documented either as `leaf = …` (active or
		// commented) or as a `[…leaf]` / `[[…leaf]]` table header
		// (map/slice-of-struct fields, e.g. [metadata.issuer_home_domains]).
		q := regexp.QuoteMeta(leaf)
		re := regexp.MustCompile(`(?m)^\s*#?\s*` + q + `\s*=|\[[^\]]*\b` + q + `\b[^\]]*\]`)
		if !re.MatchString(text) {
			t.Errorf("configs/example.toml: field %q not documented (RLT-104)", leaf)
		}
	}
	for _, section := range sections {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(section)).MatchString(text) {
			t.Errorf("configs/example.toml: section %q missing (RLT-104)", section)
		}
	}
}
