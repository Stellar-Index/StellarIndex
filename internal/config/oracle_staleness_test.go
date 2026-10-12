package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// TestOracleSourceNames_AreKnownSources: every name a staleness override may use must be a source
// the indexer can run (an on-chain source in KnownSources or a poller with its own
// [external.<name>] section), or an override would be accepted for a source nothing emits.
func TestOracleSourceNames_AreKnownSources(t *testing.T) {
	if len(config.OracleSourceNames) == 0 {
		t.Fatal("OracleSourceNames is empty — this check must not pass vacuously")
	}
	external := map[string]bool{}
	et := reflect.TypeFor[config.ExternalConfig]()
	for i := range et.NumField() {
		external[strings.Split(et.Field(i).Tag.Get("toml"), ",")[0]] = true
	}
	for name := range config.OracleSourceNames {
		if _, ok := config.KnownSources[name]; !ok && !external[name] {
			t.Errorf("OracleSourceNames has %q, which is neither in KnownSources nor "+
				"an [external.%s] section — an override naming it could never match "+
				"a series", name, name)
		}
	}
}

// An override must be able to reach an externally-polled oracle source such as chainlink. Without it in
// OracleSourceNames the validator refused it, so no Chainlink feed budget could be tuned.
func TestValidateStalenessOverrides_AcceptsPolledOracleSource(t *testing.T) {
	c := config.Default()
	c.Oracle.StalenessOverrides = []config.OracleStalenessOverrideConfig{{
		Source:        "chainlink",
		Asset:         "fiat:JPY",
		BudgetSeconds: 345600,
		Reason:        "FX feed: 24 h heartbeat plus the weekend close",
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected a chainlink staleness override: %v", err)
	}
}

// Codified-config gate for the shipped staleness override in the ansible template, which nothing else parses before
// deploy. A misspelled asset label ("DAI" vs "crypto:DAI") or dropped `reason` would stop the indexer starting. Pins
// reflector-cex / crypto:DAI at 32400s (9h), sized from measured gaps; change it deliberately.
func TestAnsibleOracleStalenessOverrides_ValidAndComplete(t *testing.T) {
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

	// Same extraction shape as TestAnsibleFiatPegStanza_ValidAndComplete:
	// keep the stanza body, drop jinja control lines and {# … #} comments
	// so the rendered TOML round-trips through the decoder.
	var out []string
	inStanza := false
	inJinjaComment := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if inJinjaComment {
			if strings.Contains(line, "#}") {
				inJinjaComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "{#") {
			if !strings.Contains(trimmed, "#}") {
				inJinjaComment = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "{%") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			inStanza = trimmed == "[[oracle.staleness_overrides]]"
		}
		if inStanza {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no [[oracle.staleness_overrides]] stanza in %s — the seeded "+
			"reflector-cex/crypto:DAI budget is the reason this gate exists", path)
	}

	c, err := config.LoadReader(strings.NewReader(strings.Join(out, "\n")),
		"stellarindex.toml.j2:oracle.staleness_overrides")
	if err != nil {
		t.Fatalf("deployed staleness_overrides stanza does not load: %v", err)
	}

	rows := c.Oracle.StalenessOverrides
	if len(rows) != 1 {
		t.Fatalf("deployed overrides = %d rows, want exactly 1 (reflector-cex / crypto:DAI); "+
			"every row is an alerting exception and should be added deliberately", len(rows))
	}
	got := rows[0]
	if got.Source != "reflector-cex" || got.Asset != "crypto:DAI" {
		t.Errorf("deployed override = %s/%s, want reflector-cex/crypto:DAI", got.Source, got.Asset)
	}
	if got.BudgetSeconds != 32400 {
		t.Errorf("deployed budget = %ds, want 32400 (9h — 1.3× the widest observed 7h gap)", got.BudgetSeconds)
	}
	// The default this replaces, spelled out: an override that landed
	// BELOW the source default would be a tightening dressed as a
	// widening, and the reason text would read as nonsense.
	if defaultBudget := 10 * 300; got.BudgetSeconds <= defaultBudget {
		t.Errorf("deployed budget %ds does not exceed the reflector default %ds — "+
			"this row exists to widen a peg asset's bound", got.BudgetSeconds, defaultBudget)
	}
	if len(strings.TrimSpace(got.Reason)) < 40 {
		t.Errorf("deployed reason %q is too thin — it must carry the observation "+
			"behind the number so the claim can be re-tested", got.Reason)
	}
}
