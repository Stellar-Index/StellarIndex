package obs

import (
	"math"
	"sync"
)

// Oracle staleness budgets: stellarindex_oracle_stale compares each (source, asset)'s age with its own budget gauge,
// because staleness is per ASSET. A per-source budget made crypto:DAI (published only on moves) breach reflector-cex's 50 min
// 11.6% of the time, and loosening it would lie about the source and loosen every other asset. State is process-global like the
// metrics: the oracle sink is reached through free functions that carry no config.

// OracleStaleBudgetMultiplier is the alert's historical `10 x resolution` threshold; changing it re-thresholds every un-overridden asset, so prefer a per-asset override.
const OracleStaleBudgetMultiplier = 10

// OracleStalenessOverride is one operator budget; Asset is the canonical metric label ("crypto:DAI", "native"), not the oracle's raw symbol.
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
	// published holds every pair with an age series, so a budget declared or overridden after the pair's last
	// update still reaches its gauge.
	published map[oracleStalenessKey]struct{}
}{
	bySource:  map[string]float64{},
	byAsset:   map[oracleStalenessKey]float64{},
	published: map[oracleStalenessKey]struct{}{},
}

// republishBudgetsLocked re-emits the budget gauge of every published pair; callers hold the write lock.
func republishBudgetsLocked() {
	for k := range oracleStaleness.published {
		OracleStalenessBudgetSeconds.WithLabelValues(k.source, k.asset).Set(budgetLocked(k.source, k.asset))
	}
}

// DeclareOracleResolution publishes a source's cadence and derives its default budget together, so no published age lacks a budget.
func DeclareOracleResolution(source string, resolutionSeconds float64) {
	OracleResolutionSeconds.WithLabelValues(source).Set(resolutionSeconds)

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.bySource[source] = OracleStaleBudgetMultiplier * resolutionSeconds
	republishBudgetsLocked()
}

// OracleHeartbeatGrace is the slack added to a heartbeat-driven source's heartbeat before a silent asset tickets.
const OracleHeartbeatGrace = 2 * 3600

// DeclareOracleHeartbeat budgets a heartbeat source at heartbeat + [OracleHeartbeatGrace]; a multiple of the heartbeat would let a dead feed hide for days.
func DeclareOracleHeartbeat(source string, heartbeatSeconds float64) {
	OracleResolutionSeconds.WithLabelValues(source).Set(heartbeatSeconds)

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.bySource[source] = heartbeatSeconds + OracleHeartbeatGrace
	republishBudgetsLocked()
}

// SetOracleStalenessOverrides REPLACES the override set and re-emits the budget of every pair already published.
func SetOracleStalenessOverrides(overrides []OracleStalenessOverride) {
	next := make(map[oracleStalenessKey]float64, len(overrides))
	for _, o := range overrides {
		next[oracleStalenessKey{source: o.Source, asset: o.Asset}] = o.BudgetSeconds
	}

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.byAsset = next
	republishBudgetsLocked()
}

// OracleStalenessBudget returns the override, else the source default, else +Inf: an undeclared source cannot alert but still emits a series.
func OracleStalenessBudget(source, asset string) float64 {
	oracleStaleness.mu.RLock()
	defer oracleStaleness.mu.RUnlock()
	return budgetLocked(source, asset)
}

func budgetLocked(source, asset string) float64 {
	if b, ok := oracleStaleness.byAsset[oracleStalenessKey{source: source, asset: asset}]; ok {
		return b
	}
	if b, ok := oracleStaleness.bySource[source]; ok {
		return b
	}
	return math.Inf(1)
}

// RecordOracleUpdate emits the age and its budget from one call, so Prometheus's label-matched comparison always has both sides.
func RecordOracleUpdate(source, asset string, updatedAtUnix float64) {
	OracleLastUpdateUnix.WithLabelValues(source, asset).Set(updatedAtUnix)

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.published[oracleStalenessKey{source: source, asset: asset}] = struct{}{}
	OracleStalenessBudgetSeconds.WithLabelValues(source, asset).Set(budgetLocked(source, asset))
}
