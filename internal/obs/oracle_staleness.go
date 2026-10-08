package obs

import (
	"math"
	"sync"
)

// Oracle staleness budgets: stellarindex_oracle_stale compares each (source, asset)'s age
// with its own budget gauge, because staleness is per ASSET. A per-source budget made
// crypto:DAI (published only on moves) breach reflector-cex's 50 min 11.6% of the time,
// and loosening the resolution would lie about the source and loosen every other asset.
// State is process-global like the metrics: the oracle sink is reached through free
// functions that carry no config.

// OracleStaleBudgetMultiplier is the alert's historical `10 × resolution` threshold;
// changing it re-thresholds every un-overridden asset, so prefer a per-asset override.
const OracleStaleBudgetMultiplier = 10

// OracleStalenessOverride is one operator budget; Asset is the canonical metric label
// ("crypto:DAI", "native"), not the oracle's raw symbol.
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

// DeclareOracleResolution publishes a source's cadence and derives its default budget
// together, so no published age lacks a budget to be judged by.
func DeclareOracleResolution(source string, resolutionSeconds float64) {
	OracleResolutionSeconds.WithLabelValues(source).Set(resolutionSeconds)

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.bySource[source] = OracleStaleBudgetMultiplier * resolutionSeconds
}

// OracleHeartbeatGrace is the slack added to a heartbeat-driven source's
// heartbeat before a silent asset tickets.
const OracleHeartbeatGrace = 2 * 3600

// DeclareOracleHeartbeat budgets a heartbeat source at heartbeat + [OracleHeartbeatGrace];
// a multiple of the heartbeat would let a dead feed hide for days.
func DeclareOracleHeartbeat(source string, heartbeatSeconds float64) {
	OracleResolutionSeconds.WithLabelValues(source).Set(heartbeatSeconds)

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.bySource[source] = heartbeatSeconds + OracleHeartbeatGrace
}

// SetOracleStalenessOverrides REPLACES the override set (removing a config row removes it
// on restart). Call before the first update; a later install reaches a pair only on its
// next publication.
func SetOracleStalenessOverrides(overrides []OracleStalenessOverride) {
	next := make(map[oracleStalenessKey]float64, len(overrides))
	for _, o := range overrides {
		next[oracleStalenessKey{source: o.Source, asset: o.Asset}] = o.BudgetSeconds
	}

	oracleStaleness.mu.Lock()
	defer oracleStaleness.mu.Unlock()
	oracleStaleness.byAsset = next
}

// OracleStalenessBudget returns the override, else the source default, else +Inf: an
// undeclared source cannot alert but still emits a visible series. Every source declares
// one (TestBuildDispatcher_DeclaresBudgetForEveryOracleSource and its external twin).
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

// RecordOracleUpdate emits the age and its budget from one call, so Prometheus's
// label-matched comparison always has both sides.
func RecordOracleUpdate(source, asset string, updatedAtUnix float64) {
	OracleLastUpdateUnix.WithLabelValues(source, asset).Set(updatedAtUnix)
	OracleStalenessBudgetSeconds.WithLabelValues(source, asset).
		Set(OracleStalenessBudget(source, asset))
}
