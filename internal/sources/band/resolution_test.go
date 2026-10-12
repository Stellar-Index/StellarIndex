package band

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// DefaultResolutionSeconds is published as
// `stellarindex_oracle_resolution_seconds{source="band"}` and seeds every band asset's default
// staleness budget at 10× it (obs.DeclareOracleResolution), the number
// `stellarindex_oracle_stale` compares against. So the constant IS this source's alert
// threshold for every asset without a per-asset override; a wrong value does not fail a build,
// it yields an alert that is permanently firing or permanently blind.
//
// It must be Band's relayer publication interval (hourly), not a poll-cadence recommendation.
// These tests pin that RELATIONSHIP, not the number: asserting `== 3600` would pass just as
// happily on a future value that is wrong in the same way.

// staleAlertMultiplier mirrors obs.OracleStaleBudgetMultiplier (the 10x in the budget gauge the oracle-stale rule
// reads); TestStaleAlertMultiplierMatchesTheShippedBudget pins the mirror.
const staleAlertMultiplier = 10

// TestStaleAlertMultiplierMatchesTheShippedBudget keeps this file reasoning about the threshold the fleet applies; if
// the default multiplier moves, the cadence assertions below must move with it.
func TestStaleAlertMultiplierMatchesTheShippedBudget(t *testing.T) {
	if staleAlertMultiplier != obs.OracleStaleBudgetMultiplier {
		t.Fatalf("staleAlertMultiplier = %d but obs.OracleStaleBudgetMultiplier = %d — "+
			"the cadence checks below would be pinning a threshold nothing applies",
			staleAlertMultiplier, obs.OracleStaleBudgetMultiplier)
	}
}

// measuredRelayIntervalSeconds is Band's observed mainnet cadence: 24 changes of
// stellarindex_oracle_last_update_unix{source="band"} per 24h for crypto:USDC and crypto:XLM,
// i.e. hourly.
const measuredRelayIntervalSeconds = 3600

// TestStaleThresholdIsLooserThanBandsActualCadence: the stale alert must not fire while Band
// is behaving normally, i.e. the threshold must exceed the publication cadence.
func TestStaleThresholdIsLooserThanBandsActualCadence(t *testing.T) {
	threshold := staleAlertMultiplier * DefaultResolutionSeconds
	if threshold <= measuredRelayIntervalSeconds {
		t.Fatalf("oracle-stale threshold for band is %ds, but Band relays every "+
			"%ds — the alert fires during NORMAL operation and carries no "+
			"information. DefaultResolutionSeconds must describe the observed "+
			"publication interval, not a poll-cadence recommendation.",
			threshold, measuredRelayIntervalSeconds)
	}
}

// TestResolutionIsNotAbsurdlyLoose is the upper bound: inflating a threshold to silence an alert is always wrong, so
// the constant must stay near the real cadence for a genuine Band outage to be detected in reasonable time.
func TestResolutionIsNotAbsurdlyLoose(t *testing.T) {
	if DefaultResolutionSeconds > 4*measuredRelayIntervalSeconds {
		t.Errorf("DefaultResolutionSeconds = %d is more than 4× Band's measured "+
			"%ds cadence. That is silencing, not fixing: at 10× it would take "+
			"%.1f hours to notice Band had stopped.",
			DefaultResolutionSeconds, measuredRelayIntervalSeconds,
			float64(staleAlertMultiplier*DefaultResolutionSeconds)/3600)
	}
}

// TestResolutionIsPositive: a zero or negative resolution makes the threshold non-positive, so `time() - last_update >
// 0` is always true; the gauge is set at dispatcher registration (pipeline.BuildDispatcher), so nothing else would
// catch it.
func TestResolutionIsPositive(t *testing.T) {
	if DefaultResolutionSeconds <= 0 {
		t.Fatalf("DefaultResolutionSeconds = %d; a non-positive resolution makes "+
			"the stale alert fire permanently for every asset", DefaultResolutionSeconds)
	}
}
