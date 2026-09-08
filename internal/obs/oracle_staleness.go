package obs

import (
	"math"
	"sync"
)

// Oracle staleness budgets (issue #478).
//
// `stellarindex_oracle_stale` asks one question per (source, asset):
// has this pair gone longer without a publication than it is allowed
// to? The allowance used to be computed inside the alert expression as
// `10 * stellarindex_oracle_resolution_seconds` — a per-SOURCE number,
// because resolution is declared per source.
//
// Staleness, though, is a per-ASSET property. `reflector-cex` declares
// a 300 s resolution, so every asset on it got a 3000 s (50 min)
// budget; `crypto:DAI` is a peg asset that Reflector publishes only
// when it moves, so it ran 7-hour gaps and breached that budget 11.6%
// of the time with nothing broken anywhere. The fix is a budget that
// can vary per asset — not a looser resolution, which would be a lie
// about the source's cadence and would loosen every OTHER asset on it.
//
// So the budget is now its own gauge on the same label set as
// last_update_unix, and the alert is a plain comparison. This file
// owns the policy behind that gauge:
//
//   - [DeclareOracleResolution] publishes a source's resolution and
//     derives its DEFAULT budget (multiplier × resolution) — the exact
//     number the alert used to compute inline, so nothing moves for an
//     asset nobody overrode.
//   - [SetOracleStalenessOverrides] installs the operator's
//     per-(source, asset) exceptions from `[[oracle.staleness_overrides]]`.
//   - [RecordOracleUpdate] emits the age and the budget together.
//
// The state is process-global for the same reason the metrics are:
// the emission point (internal/pipeline's oracle sink) is reached
// through a chain of free functions that carry no config, and both the
// declaration and the emission happen in one binary — the indexer
// declares at dispatcher-build time, long before the first event is
// persisted.

// OracleStaleBudgetMultiplier is how many declared resolutions an
// oracle source may miss before stellarindex_oracle_stale tickets.
//
// This constant IS the alert's historical threshold: the rule read
// `> 10 * stellarindex_oracle_resolution_seconds` until the budget
// became a gauge. Changing it re-thresholds every oracle asset that
// has no explicit override, which is a fleet-wide alerting change —
// prefer a per-asset override for a single misbehaving pair.
const OracleStaleBudgetMultiplier = 10

// OracleStalenessOverride is one operator-declared budget for a single
// (source, asset) pair. Asset is the CANONICAL asset string exactly as
// it appears in the metric's `asset` label ("crypto:DAI", "native",
// "raw:XAU"), not the oracle's raw symbol — the sink labels series
// with canonical.Asset.String().
type OracleStalenessOverride struct {
	Source        string
	Asset         string
	BudgetSeconds float64
}

type oracleStalenessKey struct{ source, asset string }

var oracleStaleness = struct {
	mu sync.RWMutex
	// bySource holds each source's default budget, derived from its
	// declared resolution.
	bySource map[string]float64
	// byAsset holds operator overrides, which win over the default.
	byAsset map[oracleStalenessKey]float64
}{
	bySource: map[string]float64{},
	byAsset:  map[oracleStalenessKey]float64{},
}

// DeclareOracleResolution publishes `source`'s declared publication
// cadence and derives its default staleness budget.
//
// Call it where a source's decoder is constructed (see
// pipeline.BuildDispatcher). Both effects must happen together: a
// source whose resolution is published but whose budget is not would
// emit ages that nothing can judge.
func DeclareOracleResolution(source string, resolutionSeconds float64) {
	OracleResolutionSeconds.WithLabelValues(source).Set(resolutionSeconds)

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.bySource[source] = OracleStaleBudgetMultiplier * resolutionSeconds
}

// SetOracleStalenessOverrides REPLACES the operator override set — it
// is a whole-policy install from config, not an accumulating register,
// so removing a row from the config removes the override on restart.
//
// Call it before the first oracle update is persisted. An override
// installed later only reaches the gauge on that pair's next
// publication, which for a slow asset is exactly the wait the override
// exists to tolerate.
func SetOracleStalenessOverrides(overrides []OracleStalenessOverride) {
	next := make(map[oracleStalenessKey]float64, len(overrides))
	for _, o := range overrides {
		next[oracleStalenessKey{source: o.Source, asset: o.Asset}] = o.BudgetSeconds
	}

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.byAsset = next
}

// OracleStalenessBudget reports the staleness budget for one
// (source, asset): the operator override if there is one, else the
// source's default (multiplier × declared resolution).
//
// A source that never declared a resolution yields +Inf, which
// reproduces the pre-#478 behaviour exactly: the old expression joined
// against stellarindex_oracle_resolution_seconds, so a source with no
// resolution series had no right-hand side and could not alert at all.
// +Inf keeps that silence while still emitting a series, so the gap is
// visible on a dashboard instead of being an absent row nobody
// notices. Every oracle source the dispatcher can enable declares one
// (pinned by pipeline's TestBuildDispatcher_DeclaresBudgetForEveryOracleSource).
func OracleStalenessBudget(source, asset string) float64 {
	oracleStaleness.mu.RLock()
	defer oracleStaleness.mu.RUnlock()

	if b, ok := oracleStaleness.byAsset[oracleStalenessKey{source: source, asset: asset}]; ok {
		return b
	}
	if b, ok := oracleStaleness.bySource[source]; ok {
		return b
	}
	return math.Inf(1)
}

// RecordOracleUpdate publishes one oracle observation: the timestamp
// of the update AND the staleness budget that timestamp will be judged
// against, on the same label set.
//
// This pairing is the point. stellarindex_oracle_stale is a bare
// vector-to-vector comparison, which Prometheus evaluates only where
// both sides carry identical labels; emitting the two gauges from one
// call is what makes "every asset with an age has a budget" a property
// of the code rather than a convention someone has to remember.
func RecordOracleUpdate(source, asset string, updatedAtUnix float64) {
	OracleLastUpdateUnix.WithLabelValues(source, asset).Set(updatedAtUnix)
	OracleStalenessBudgetSeconds.WithLabelValues(source, asset).
		Set(OracleStalenessBudget(source, asset))
}
