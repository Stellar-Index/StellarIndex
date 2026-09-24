package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
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

// TestExampleTOMLDocumentsEverySchemaKey walks every key config.Describe()
// emits and requires configs/example.toml to show it, active or commented,
// under its own [section], so a new field cannot ship undocumented.
func TestExampleTOMLDocumentsEverySchemaKey(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "configs", "example.toml"))
	if err != nil {
		t.Fatalf("read example.toml: %v", err)
	}
	doc := parseExampleKeys(string(body))
	var missing []string
	for _, f := range config.Describe() {
		if !doc.documents(f.Path) {
			missing = append(missing, f.Path)
		}
	}
	if len(missing) > 0 {
		t.Errorf("configs/example.toml has no active or commented entry for %d schema keys:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// exampleKeys indexes example.toml by section. Headers and `key =` lines
// count whether active or commented out (`# key = …`).
type exampleKeys struct {
	sections map[string]bool
	keys     map[string]bool // "section.key"
}

var (
	exampleHeaderRE = regexp.MustCompile(`^\[\[?\s*([^\[\]\s]+?)\s*\]\]?\s*(#.*)?$`)
	exampleKeyRE    = regexp.MustCompile(`^([A-Za-z0-9_]+)\s*=`)
)

func parseExampleKeys(text string) exampleKeys {
	ek := exampleKeys{sections: map[string]bool{}, keys: map[string]bool{}}
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "#"))
		if m := exampleHeaderRE.FindStringSubmatch(line); m != nil {
			section = strings.ReplaceAll(m[1], `"`, "")
			ek.sections[section] = true
			continue
		}
		if m := exampleKeyRE.FindStringSubmatch(line); m != nil {
			ek.keys[section+"."+m[1]] = true
		}
	}
	return ek
}

// documents reports whether path is shown. Map and slice-of-struct element
// fields are satisfied by their container: the element keys are
// operator-chosen, so there is no literal key to search for.
func (ek exampleKeys) documents(path string) bool {
	if i := strings.Index(path, ".<key>"); i >= 0 {
		path = path[:i]
	}
	if i := strings.Index(path, "[]"); i >= 0 {
		path = path[:i]
	}
	if ek.sections[path] || ek.keys[path] {
		return true
	}
	for s := range ek.sections {
		if strings.HasPrefix(s, path+".") {
			return true
		}
	}
	return false
}

// TestAnsibleRendersDivergenceSupplyToggle pins the mechanism behind the
// docs' claim that the archival-node role renders divergence.supply.enabled
// from stellarindex_divergence_supply_enabled: the r1 template must carry
// the stanza, read that variable, and load for both renders.
func TestAnsibleRendersDivergenceSupplyToggle(t *testing.T) {
	const toggleVar = "stellarindex_divergence_supply_enabled"
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "ansible", "roles",
		"archival-node", "templates", "stellarindex.toml.j2"))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	stanza := tomlStanza(string(raw), "[divergence.supply]")
	if stanza == "" {
		t.Fatalf("stellarindex.toml.j2 renders no [divergence.supply] stanza, so no inventory variable can arm the supply cross-check")
	}
	if !strings.Contains(stanza, toggleVar) {
		t.Fatalf("[divergence.supply] stanza does not read %s:\n%s", toggleVar, stanza)
	}
	jinja := regexp.MustCompile(`\{\{.*?\}\}`)
	for _, want := range []bool{true, false} {
		rendered := jinja.ReplaceAllString(stanza, strconv.FormatBool(want))
		c, err := config.LoadReader(strings.NewReader(rendered), "stellarindex.toml.j2:divergence.supply")
		if err != nil {
			t.Fatalf("rendered %v stanza does not load: %v", want, err)
		}
		if c.Divergence.Supply.Enabled != want {
			t.Errorf("%s=%v rendered divergence.supply.enabled=%v", toggleVar, want, c.Divergence.Supply.Enabled)
		}
	}

	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "example.toml"))
	if err != nil {
		t.Fatalf("read example.toml: %v", err)
	}
	if !strings.Contains(string(example), toggleVar) {
		t.Errorf("configs/example.toml does not name %s as the way to arm [divergence.supply]", toggleVar)
	}
	for _, f := range config.Describe() {
		if f.Path == "divergence.supply.enabled" && !strings.Contains(f.Doc, toggleVar) {
			t.Errorf("divergence.supply.enabled doc does not name %s: %q", toggleVar, f.Doc)
		}
	}
}

// tomlStanza returns header's lines up to the next table header, or "".
func tomlStanza(text, header string) string {
	var out []string
	in := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			in = trimmed == header
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
