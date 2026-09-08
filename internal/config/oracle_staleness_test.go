package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// TestValidate_OracleStalenessOverrideAccepted is the shape an operator
// writes when an asset is legitimately slow (issue #478).
func TestValidate_OracleStalenessOverrideAccepted(t *testing.T) {
	c := config.Default()
	c.Oracle.StalenessOverrides = []config.OracleStalenessOverrideConfig{{
		Source:        "reflector-cex",
		Asset:         "crypto:DAI",
		BudgetSeconds: 32400,
		Reason:        "peg asset; publishes only on movement, observed gaps to 7h",
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid staleness override should pass: %v", err)
	}
}

// TestValidate_RejectsBadOracleStalenessOverride covers every way a
// per-asset budget can be written down and silently do nothing.
//
// The override's only job is to change one alert's threshold, and each
// row below changes NOTHING while looking like it does: a source
// nothing emits, an asset spelled differently from the metric's label,
// a budget that would ticket on every evaluation, an unexplained
// claim, or two rows fighting over one pair. None of them produce a
// runtime error — the alert just keeps firing on its old schedule
// while the config reads as if it were handled — so they have to fail
// at startup.
func TestValidate_RejectsBadOracleStalenessOverride(t *testing.T) {
	base := config.OracleStalenessOverrideConfig{
		Source:        "reflector-cex",
		Asset:         "crypto:DAI",
		BudgetSeconds: 32400,
		Reason:        "peg asset; publishes only on movement",
	}
	// with returns a copy of base with one field bent out of shape.
	with := func(mutate func(*config.OracleStalenessOverrideConfig)) config.OracleStalenessOverrideConfig {
		row := base
		mutate(&row)
		return row
	}

	cases := []struct {
		name string
		rows []config.OracleStalenessOverrideConfig
		want string
	}{
		{
			name: "source is not an oracle",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Source = "soroswap" }),
			},
			want: "is not an oracle source",
		},
		{
			name: "source typo",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Source = "reflector_cex" }),
			},
			want: "is not an oracle source",
		},
		{
			// The metric's asset label is canonical.Asset.String(), so a
			// bare oracle symbol keys a series that never exists.
			name: "bare symbol instead of canonical asset",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Asset = "DAI" }),
			},
			want: "not a canonical asset identifier",
		},
		{
			// "XLM" parses — to the native asset, which stringifies back
			// as "native". An override written this way would look
			// correct and match nothing.
			name: "asset alias that does not round-trip",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Asset = "XLM" }),
			},
			want: "is an alias for",
		},
		{
			name: "zero budget",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.BudgetSeconds = 0 }),
			},
			want: "budget_seconds must be > 0",
		},
		{
			name: "negative budget",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.BudgetSeconds = -1 }),
			},
			want: "budget_seconds must be > 0",
		},
		{
			name: "no stated reason",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Reason = "   " }),
			},
			want: "reason is required",
		},
		{
			name: "two budgets for one pair",
			rows: []config.OracleStalenessOverrideConfig{
				base,
				with(func(r *config.OracleStalenessOverrideConfig) { r.BudgetSeconds = 3600 }),
			},
			want: "duplicates",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Default()
			c.Oracle.StalenessOverrides = tc.rows
			err := c.Validate()
			if err == nil {
				t.Fatal("expected rejection — this row would silently match no series")
			}
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Errorf("error = %v, want it to wrap ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestOracleSourceNames_AreKnownSources keeps the oracle subset honest:
// every name a staleness override may use must also be a source the
// indexer can actually run. A name that drifted out of KnownSources
// would accept an override for a source nothing emits.
func TestOracleSourceNames_AreKnownSources(t *testing.T) {
	if len(config.OracleSourceNames) == 0 {
		t.Fatal("OracleSourceNames is empty — this check must not pass vacuously")
	}
	for name := range config.OracleSourceNames {
		if _, ok := config.KnownSources[name]; !ok {
			t.Errorf("OracleSourceNames has %q, which is not in KnownSources — an "+
				"override naming it could never match a series", name)
		}
	}
}

// TestAnsibleOracleStalenessOverrides_ValidAndComplete is the codified-
// config gate for the one shipped override (#478).
//
// The override lives in the deployed ansible template, which is plain
// TOML behind jinja control lines. Nothing else parses it before a
// deploy, so a mis-spelled asset label ("DAI" for "crypto:DAI") or a
// dropped `reason` would surface as an indexer that refuses to start on
// r1 — config.Validate rejects both, correctly, but at the worst
// possible moment. This extracts the stanza the r1 render produces
// (aggregator on) and puts it through the same loader.
//
// It also pins the seeded row itself: reflector-cex / crypto:DAI at
// 32400s (9h), sized from measured gaps to 7h against the 50-minute
// source default. Changing the number should be a deliberate edit here
// too, with the new observation written into `reason`.
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
