package pipeline

import (
	"math"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
)

// oracleSourceFixture describes how to enable one oracle source and
// what cadence it declares.
type oracleSourceFixture struct {
	oracle             config.OracleConfig
	resolutionSeconds  float64
	enabledSourceNames []string
}

// oracleSourceFixtures maps every source name config.OracleSourceNames
// accepts in a staleness override to the config that enables it.
//
// Keyed off that map on purpose: an oracle source added there but not
// wired into BuildDispatcher's case table fails
// TestBuildDispatcher_DeclaresBudgetForEveryOracleSource with
// "unhandled source" rather than shipping an asset population whose
// alert can never fire.
func oracleSourceFixtures() map[string]oracleSourceFixture {
	return map[string]oracleSourceFixture{
		reflector.SourceDEX: {
			oracle:             config.OracleConfig{Reflector: config.ReflectorOracleConfig{DEXContract: realisticCStrkey}},
			resolutionSeconds:  reflector.DefaultResolutionSeconds,
			enabledSourceNames: []string{reflector.SourceDEX},
		},
		reflector.SourceCEX: {
			oracle:             config.OracleConfig{Reflector: config.ReflectorOracleConfig{CEXContract: realisticCStrkey}},
			resolutionSeconds:  reflector.DefaultResolutionSeconds,
			enabledSourceNames: []string{reflector.SourceCEX},
		},
		reflector.SourceFX: {
			oracle:             config.OracleConfig{Reflector: config.ReflectorOracleConfig{FXContract: realisticCStrkey}},
			resolutionSeconds:  reflector.DefaultResolutionSeconds,
			enabledSourceNames: []string{reflector.SourceFX},
		},
		redstone.SourceName: {
			oracle:             config.OracleConfig{Redstone: config.RedstoneOracleConfig{AdapterContract: realisticCStrkey}},
			resolutionSeconds:  redstone.DefaultResolutionSeconds,
			enabledSourceNames: []string{redstone.SourceName},
		},
		band.SourceName: {
			oracle:             config.OracleConfig{Band: config.BandOracleConfig{StandardReferenceContract: realisticCStrkey}},
			resolutionSeconds:  band.DefaultResolutionSeconds,
			enabledSourceNames: []string{band.SourceName},
		},
	}
}

// TestBuildDispatcher_DeclaresBudgetForEveryOracleSource is the
// no-silent-loss guard on the wiring side.
//
// obs.RecordOracleUpdate always emits a budget series, but a source
// that never declared its resolution gets +Inf — a series that exists
// and can never ticket. That is the correct fallback (it reproduces
// the pre-#478 behaviour of a rule with no right-hand side) and
// exactly the wrong thing to ship for an enabled source. So
// every oracle source the dispatcher can enable must come out of
// BuildDispatcher with a FINITE budget, equal to the same
// 10 × resolution the alert used to compute inline.
func TestBuildDispatcher_DeclaresBudgetForEveryOracleSource(t *testing.T) {
	fixtures := oracleSourceFixtures()
	for source := range config.OracleSourceNames {
		fx, ok := fixtures[source]
		if !ok {
			t.Errorf("unhandled source %q: config.OracleSourceNames accepts staleness "+
				"overrides for it, so this test needs a fixture that enables it — "+
				"and BuildDispatcher needs an obs.DeclareOracleResolution call, or "+
				"every asset on it publishes a +Inf budget and can never ticket", source)
			continue
		}

		t.Run(source, func(t *testing.T) {
			if _, err := BuildDispatcher(fx.enabledSourceNames, fx.oracle, nil); err != nil {
				t.Fatalf("BuildDispatcher(%v): %v", fx.enabledSourceNames, err)
			}

			got := obs.OracleStalenessBudget(source, "crypto:XLM")
			if math.IsInf(got, 1) {
				t.Fatalf("budget for %q is +Inf — BuildDispatcher did not declare its "+
					"resolution, so stellarindex_oracle_stale can never fire for any "+
					"of its assets", source)
			}
			if want := obs.OracleStaleBudgetMultiplier * fx.resolutionSeconds; got != want {
				t.Errorf("budget for %q = %v, want %v (%d × %v declared resolution — "+
					"the pre-#478 threshold)",
					source, got, want, obs.OracleStaleBudgetMultiplier, fx.resolutionSeconds)
			}
		})
	}
}

// TestBuildDispatcher_InstallsConfiguredOverrides proves the operator's
// `[[oracle.staleness_overrides]]` rows actually reach the gauge, and
// that they are installed even for a source this replica does not run
// (the rows are keyed by (source, asset); one that matches nothing is
// inert, which is cheaper than reasoning about replica topology).
func TestBuildDispatcher_InstallsConfiguredOverrides(t *testing.T) {
	oracle := config.OracleConfig{
		Reflector: config.ReflectorOracleConfig{CEXContract: realisticCStrkey},
		StalenessOverrides: []config.OracleStalenessOverrideConfig{
			{
				Source:        reflector.SourceCEX,
				Asset:         "crypto:DAI",
				BudgetSeconds: 32400,
				Reason:        "peg asset — publishes only on movement",
			},
		},
	}
	if _, err := BuildDispatcher([]string{reflector.SourceCEX}, oracle, nil); err != nil {
		t.Fatalf("BuildDispatcher: %v", err)
	}
	t.Cleanup(func() { obs.SetOracleStalenessOverrides(nil) })

	if got := obs.OracleStalenessBudget(reflector.SourceCEX, "crypto:DAI"); got != 32400 {
		t.Errorf("overridden budget = %v, want 32400", got)
	}
	// The sibling asset on the same source keeps the default — an
	// override widens one pair, not the feed.
	want := obs.OracleStaleBudgetMultiplier * float64(reflector.DefaultResolutionSeconds)
	if got := obs.OracleStalenessBudget(reflector.SourceCEX, "crypto:XLM"); got != want {
		t.Errorf("sibling budget = %v, want %v (unchanged default)", got, want)
	}
}

// TestBuildDispatcher_NoOverridesLeavesEveryAssetOnTheDefault is the
// headline no-behaviour-change assertion for the wiring: with an empty
// override list — the shipped default — every oracle asset's budget is
// the number the alert expression computed before the budget existed.
func TestBuildDispatcher_NoOverridesLeavesEveryAssetOnTheDefault(t *testing.T) {
	oracle := config.OracleConfig{
		Reflector: config.ReflectorOracleConfig{CEXContract: realisticCStrkey},
	}
	if _, err := BuildDispatcher([]string{reflector.SourceCEX}, oracle, nil); err != nil {
		t.Fatalf("BuildDispatcher: %v", err)
	}

	want := obs.OracleStaleBudgetMultiplier * float64(reflector.DefaultResolutionSeconds)
	for _, asset := range []string{"crypto:XLM", "crypto:DAI", "crypto:USDC", "raw:UNMAPPED"} {
		if got := obs.OracleStalenessBudget(reflector.SourceCEX, asset); got != want {
			t.Errorf("budget(%s, %s) = %v, want %v", reflector.SourceCEX, asset, got, want)
		}
	}
}
