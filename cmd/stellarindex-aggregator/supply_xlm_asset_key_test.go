package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// TestBuildXLMRefresher_AssetKeyMatchesSupplyAssetKey pins T017/T034: the
// XLM refresher must register (and report) the "XLM" asset_key that
// supply.AssetKey produces for native — the same shape
// StaleComponentLedgersByAsset overrides and outcome-metric labels are
// keyed on — never the "native" canonical.AssetType literal. Passing
// "native" through supplyRefresherOptions silently orphans any operator
// override configured under the documented "XLM" key, because the
// Refresher's per-asset lookup is exact-match on the snapshot's
// AssetKey (always "XLM" for native — see internal/supply/xlm.go).
func TestBuildXLMRefresher_AssetKeyMatchesSupplyAssetKey(t *testing.T) {
	cfg := config.Config{}
	cfg.Stellar.Network = "pubnet"

	// store/closeTimes are only wrapped, never dereferenced, before the
	// function returns — buildXLMRefresher constructs the computer and
	// the option list without touching either.
	_, assetKey, err := buildXLMRefresher(cfg, nil, &fakeLakeTip{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildXLMRefresher: %v", err)
	}
	if assetKey != "XLM" {
		t.Fatalf("buildXLMRefresher assetKey = %q, want %q — an operator's "+
			"[supply.stale_component_ledgers_by_asset] \"XLM\" override would "+
			"silently never apply under this key", assetKey, "XLM")
	}
}
