package obs_test

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// gaugeChildren collects every child series of a GaugeVec as
// "label=value,…" → value, so a test can assert over the SET of series
// a vec carries rather than over one label set it happens to name.
func gaugeChildren(t *testing.T, vec *prometheus.GaugeVec) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 512)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		key := ""
		for _, lp := range pb.GetLabel() {
			if key != "" {
				key += ","
			}
			key += lp.GetName() + "=" + lp.GetValue()
		}
		out[key] = pb.GetGauge().GetValue()
	}
	return out
}

// TestOracleStaleBudgetMultiplier_IsTheHistoricalThreshold pins the
// number that used to live in the alert expression as a literal
// (`> 10 * stellarindex_oracle_resolution_seconds`).
//
// The whole safety claim of the per-asset budget is "nothing moves for
// an asset nobody overrode". That claim rests on this constant being
// the same 10 the rule carried, so it gets its own test rather than
// riding along inside a case that would also fail for other reasons.
func TestOracleStaleBudgetMultiplier_IsTheHistoricalThreshold(t *testing.T) {
	if obs.OracleStaleBudgetMultiplier != 10 {
		t.Fatalf("OracleStaleBudgetMultiplier = %d, want 10 — the alert read "+
			"`> 10 * stellarindex_oracle_resolution_seconds` before the budget "+
			"became a gauge; changing this re-thresholds every un-overridden "+
			"oracle asset at once",
			obs.OracleStaleBudgetMultiplier)
	}
}

// TestOracleStalenessBudget_DefaultsToTenResolutions is the
// no-behaviour-change case: a source declares a resolution, and every
// asset on it gets exactly the threshold the old expression computed.
func TestOracleStalenessBudget_DefaultsToTenResolutions(t *testing.T) {
	obs.SetOracleStalenessOverrides(nil)
	obs.DeclareOracleResolution("test-default-source", 300)

	for _, asset := range []string{"crypto:XLM", "crypto:DAI", "raw:WEIRD"} {
		if got := obs.OracleStalenessBudget("test-default-source", asset); got != 3000 {
			t.Errorf("budget(%q) = %v, want 3000 (10 × 300s, the pre-#478 threshold)",
				asset, got)
		}
	}

	// The declared resolution itself must NOT move: it describes the
	// oracle's cadence, and the alert's budget is a separate number.
	if got := gaugeChildren(t, obs.OracleResolutionSeconds)["source=test-default-source"]; got != 300 {
		t.Errorf("resolution gauge = %v, want 300 — DeclareOracleResolution must "+
			"publish the declared cadence unchanged", got)
	}
}

// TestOracleStalenessBudget_OverrideAppliesToOnePairOnly is the
// blast-radius case. An override is a claim about ONE asset; it must
// not reach the asset next to it, and it must not reach the same asset
// on another source (reflector-dex's DAI is a different feed with a
// different publication rhythm from reflector-cex's).
func TestOracleStalenessBudget_OverrideAppliesToOnePairOnly(t *testing.T) {
	obs.DeclareOracleResolution("test-cex", 300)
	obs.DeclareOracleResolution("test-dex", 300)
	obs.SetOracleStalenessOverrides([]obs.OracleStalenessOverride{
		{Source: "test-cex", Asset: "crypto:DAI", BudgetSeconds: 32400},
	})
	t.Cleanup(func() { obs.SetOracleStalenessOverrides(nil) })

	cases := []struct {
		source, asset string
		want          float64
	}{
		{"test-cex", "crypto:DAI", 32400}, // overridden
		{"test-cex", "crypto:XLM", 3000},  // sibling asset, same source
		{"test-dex", "crypto:DAI", 3000},  // same asset, different source
	}
	for _, c := range cases {
		if got := obs.OracleStalenessBudget(c.source, c.asset); got != c.want {
			t.Errorf("budget(%s, %s) = %v, want %v", c.source, c.asset, got, c.want)
		}
	}
}

// TestSetOracleStalenessOverrides_Replaces pins install-not-accumulate
// semantics: deleting a row from the config and restarting must delete
// the override. A register that only ever grew would make a retired
// exception permanent and invisible.
func TestSetOracleStalenessOverrides_Replaces(t *testing.T) {
	obs.DeclareOracleResolution("test-replace", 300)
	obs.SetOracleStalenessOverrides([]obs.OracleStalenessOverride{
		{Source: "test-replace", Asset: "crypto:DAI", BudgetSeconds: 32400},
	})
	if got := obs.OracleStalenessBudget("test-replace", "crypto:DAI"); got != 32400 {
		t.Fatalf("budget after install = %v, want 32400", got)
	}

	obs.SetOracleStalenessOverrides(nil)
	if got := obs.OracleStalenessBudget("test-replace", "crypto:DAI"); got != 3000 {
		t.Errorf("budget after removing the row = %v, want the source default 3000", got)
	}
}

// TestOracleStalenessBudget_UndeclaredSourceCannotAlert documents the
// fallback, which is chosen to be bit-for-bit what the old rule did.
//
// The pre-#478 expression joined against
// stellarindex_oracle_resolution_seconds, so a source that never
// declared a resolution had no right-hand side and the alert could not
// fire for it at all. +Inf reproduces that silence (`age > +Inf` is
// never true) while still emitting a series, so the gap is visible
// instead of being an absent row nobody looks for.
func TestOracleStalenessBudget_UndeclaredSourceCannotAlert(t *testing.T) {
	obs.SetOracleStalenessOverrides(nil)
	got := obs.OracleStalenessBudget("test-never-declared", "crypto:XLM")
	if !math.IsInf(got, 1) {
		t.Fatalf("budget for an undeclared source = %v, want +Inf — a finite "+
			"fallback would make an alert fire that could not fire before", got)
	}
}

// TestRecordOracleUpdate_EveryAgeHasABudget is the "no asset silently
// loses its alert" guard, and it is deliberately written over the
// REGISTRY rather than over one call.
//
// stellarindex_oracle_stale is a bare vector-to-vector comparison:
// Prometheus evaluates it only where both sides carry identical
// labels, so an asset with an age and no budget is an asset with no
// alert — silently, with the rule looking perfectly healthy. This
// asserts the invariant across every child either vec holds by the
// time it runs, which is what catches a future writer that sets
// OracleLastUpdateUnix directly instead of going through
// RecordOracleUpdate.
func TestRecordOracleUpdate_EveryAgeHasABudget(t *testing.T) {
	obs.DeclareOracleResolution("test-pairing", 300)
	obs.SetOracleStalenessOverrides([]obs.OracleStalenessOverride{
		{Source: "test-pairing", Asset: "crypto:DAI", BudgetSeconds: 32400},
	})
	t.Cleanup(func() { obs.SetOracleStalenessOverrides(nil) })

	obs.RecordOracleUpdate("test-pairing", "crypto:XLM", 1_700_000_000)
	obs.RecordOracleUpdate("test-pairing", "crypto:DAI", 1_700_000_001)

	ages := gaugeChildren(t, obs.OracleLastUpdateUnix)
	budgets := gaugeChildren(t, obs.OracleStalenessBudgetSeconds)

	if len(ages) == 0 {
		t.Fatal("no last_update_unix children collected — this check must not pass vacuously")
	}
	for labels := range ages {
		if _, ok := budgets[labels]; !ok {
			t.Errorf("last_update_unix{%s} has no matching staleness_budget_seconds "+
				"series — that asset cannot alert at all, because the rule compares "+
				"the two on identical label sets", labels)
		}
	}

	if got := budgets["asset=crypto:XLM,source=test-pairing"]; got != 3000 {
		t.Errorf("budget gauge for the un-overridden asset = %v, want 3000", got)
	}
	if got := budgets["asset=crypto:DAI,source=test-pairing"]; got != 32400 {
		t.Errorf("budget gauge for the overridden asset = %v, want 32400", got)
	}
}
