package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// The position-manager catch-up must not rewrite trades: -only-kinds sushiswap_v3.manager is a kind the source declares,
// writes nothing the aggregates are built on (so the refresh is skipped), and is reconciled by kind (so the run records
// its dirty window). Trades, by contrast, still owe the refresh.
func TestOnlyKinds_SushiswapV3ManagerCatchUp(t *testing.T) {
	only, err := parseOnlyKinds("sushiswap_v3", "sushiswap_v3.manager", true)
	if err != nil {
		t.Fatalf("parseOnlyKinds refused the manager kind: %v", err)
	}
	r := projectedRebuildRefresh("cfg.toml", "sushiswap_v3", true, true, 61_487_383, 65_700_000, only)
	if r.feeds || r.skipped == "" {
		t.Errorf("manager-only run: feeds=%v skipped=%q, want the aggregate refresh skipped with a reason", r.feeds, r.skipped)
	}
	scope, listed, err := reconcileScopeOf(config.Config{}, "sushiswap_v3")
	if err != nil || !listed {
		t.Fatalf("reconcileScopeOf: listed=%v err=%v", listed, err)
	}
	if !rebuildNeedsDirtyWindow(scope, listed, only) {
		t.Error("manager-only -write must record a projection dirty window: sushiswap_v3_manager_events is reconciled")
	}
	trades, err := parseOnlyKinds("sushiswap_v3", "sushiswap_v3.trade", true)
	if err != nil {
		t.Fatal(err)
	}
	if r := projectedRebuildRefresh("cfg.toml", "sushiswap_v3", true, true, 61_487_383, 65_700_000, trades); !r.feeds {
		t.Error("a trade-kind run must still refresh the aggregates")
	}
}
