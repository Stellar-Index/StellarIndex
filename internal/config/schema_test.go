package config_test

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

func TestDescribe_everyFieldHasDoc(t *testing.T) {
	// Describe() panics if any leaf field is missing `doc:`. Running
	// it successfully is the whole invariant.
	fields := cfg.Describe()
	if len(fields) == 0 {
		t.Fatal("Describe returned no fields")
	}
	for _, f := range fields {
		if f.Doc == "" {
			t.Errorf("field %q has empty doc", f.Path)
		}
		if !strings.Contains(f.Path, ".") {
			t.Errorf("field %q has no dot — top-level section missing", f.Path)
		}
	}
}

func TestEmitMarkdown_hasGeneratedBanner(t *testing.T) {
	var buf bytes.Buffer
	if err := cfg.EmitMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	if !strings.Contains(firstLine, "GENERATED FILE") {
		t.Fatalf("line 1 = %q, want the generated-file banner", firstLine)
	}
	// Every top-level section should appear as a heading.
	for _, s := range []string{"[region]", "[stellar]", "[storage]", "[ingestion]", "[aggregate]", "[api]", "[obs]"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing section %s", s)
		}
	}
}

func TestDefault_isValidShape(t *testing.T) {
	c := cfg.Default()
	if c.Region.ID == "" {
		t.Error("Region.ID empty in defaults")
	}
	if c.Stellar.Network == "" {
		t.Error("Stellar.Network empty in defaults")
	}
	if len(c.Stellar.RPCEndpoints) == 0 {
		t.Error("Stellar.RPCEndpoints empty in defaults")
	}
	if c.Aggregate.MinUSDVolume <= 0 {
		t.Error("Aggregate.MinUSDVolume must be positive")
	}
	if c.Obs.TraceSample < 0 || c.Obs.TraceSample > 1 {
		t.Errorf("Obs.TraceSample out of [0,1]: %f", c.Obs.TraceSample)
	}
}

// TestDefaultPricingGuard_MatchesPricingguardConstants pins the config
// defaults — Default() always supplies them, so they are the floors every
// deployment runs — to the pricingguard constants carrying the rationale.
// Checked on the effective policy so units are covered too.
func TestDefaultPricingGuard_MatchesPricingguardConstants(t *testing.T) {
	pg := cfg.Default().PricingGuard
	assertDefaultSubstancePolicy(t, "Default()", pg)
}

// assertDefaultSubstancePolicy asserts pg maps to exactly the pricingguard
// DefaultSubstance* floors.
func assertDefaultSubstancePolicy(t *testing.T, label string, pg cfg.PricingGuardConfig) {
	t.Helper()
	pol := pricingguard.SubstancePolicyFromValues(
		pg.SubstanceMinVolumeUSD, pg.SubstanceMinBuckets,
		pg.SubstanceMinSpanMinutes, pg.SubstanceWindowHours)
	if pol.MinVolumeUSD == nil || pol.MinVolumeUSD.Cmp(big.NewRat(pricingguard.DefaultSubstanceMinVolumeUSD, 1)) != 0 {
		t.Errorf("%s: substance_min_volume_usd = %v, want pricingguard.DefaultSubstanceMinVolumeUSD = %d",
			label, pg.SubstanceMinVolumeUSD, pricingguard.DefaultSubstanceMinVolumeUSD)
	}
	if pol.MinBuckets != pricingguard.DefaultSubstanceMinBuckets {
		t.Errorf("%s: substance_min_buckets = %d, want pricingguard.DefaultSubstanceMinBuckets = %d",
			label, pol.MinBuckets, pricingguard.DefaultSubstanceMinBuckets)
	}
	if pol.MinSpan != pricingguard.DefaultSubstanceMinSpan {
		t.Errorf("%s: substance_min_span_minutes = %v, want pricingguard.DefaultSubstanceMinSpan = %v",
			label, pol.MinSpan, pricingguard.DefaultSubstanceMinSpan)
	}
	if pol.Window != pricingguard.DefaultSubstanceWindow {
		t.Errorf("%s: substance_window_hours = %v, want pricingguard.DefaultSubstanceWindow = %v",
			label, pol.Window, pricingguard.DefaultSubstanceWindow)
	}
}

// TestPricingGuard_RejectsUnsatisfiableOrOverflowingSubstance: a span floor
// at or above the window, a bucket floor above the window's minute count, or
// a window that wraps time.Duration must fail the load — the first two
// withhold every pair, the last serves every pair unguarded, all silently.
func TestPricingGuard_RejectsUnsatisfiableOrOverflowingSubstance(t *testing.T) {
	reject := map[string]string{
		"span equals default window":   "substance_min_span_minutes = 1440",
		"span above default window":    "substance_min_span_minutes = 2000",
		"span equals explicit window":  "substance_min_span_minutes = 120\nsubstance_window_hours = 2",
		"default span vs short window": "substance_window_hours = 6",
		"buckets above window":         "substance_min_buckets = 1441",
		"window wraps Duration":        "substance_window_hours = 3000000",
		"window above cap":             "substance_window_hours = 9601",
	}
	for name, body := range reject {
		t.Run(name, func(t *testing.T) {
			_, err := cfg.LoadReader(strings.NewReader("[pricing_guard]\n"+body+"\n"), "t.toml")
			if !errors.Is(err, cfg.ErrInvalidConfig) {
				t.Fatalf("%q loaded without ErrInvalidConfig: err = %v", body, err)
			}
			if !strings.Contains(err.Error(), "pricing_guard: substance_") {
				t.Errorf("error does not name the substance keys: %v", err)
			}
		})
	}
	accept := map[string]string{
		"defaults":                 "",
		"span just under window":   "substance_min_span_minutes = 1439",
		"buckets at window":        "substance_min_buckets = 1440",
		"window at cap":            "substance_window_hours = 9600",
		"short window, short span": "substance_min_span_minutes = 60\nsubstance_window_hours = 2",
	}
	for name, body := range accept {
		t.Run(name, func(t *testing.T) {
			if _, err := cfg.LoadReader(strings.NewReader("[pricing_guard]\n"+body+"\n"), "t.toml"); err != nil {
				t.Fatalf("%q rejected: %v", body, err)
			}
		})
	}
}
