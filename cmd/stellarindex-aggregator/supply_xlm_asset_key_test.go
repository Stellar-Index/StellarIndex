package main

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
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

// TestSupplyRefresherOptions_PerAssetOverrideReachesGate drives each
// accepted operator spelling of a [supply.stale_component_ledgers_by_asset]
// key through supplyRefresherOptions into a real Refresher whose
// snapshot carries the supply.AssetKey the production computer stamps.
// A 1190-ledger component lag exceeds the 1000-ledger global default,
// so the tick is OK only if the 5000-ledger per-asset override landed.
func TestSupplyRefresherOptions_PerAssetOverrideReachesGate(t *testing.T) {
	const phoIssuerAccount = "GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	const sep41Contract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	pho, err := canonical.NewClassicAsset("PHO", phoIssuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	sep41, err := canonical.NewSorobanAsset(sep41Contract)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		configKey string
		asset     canonical.Asset
	}{
		{"PHO-" + phoIssuerAccount, pho},
		{"PHO:" + phoIssuerAccount, pho},
		{"native", canonical.NativeAsset()},
		{"XLM", canonical.NativeAsset()},
		{sep41Contract, sep41},
	}
	for _, tc := range cases {
		t.Run(tc.configKey, func(t *testing.T) {
			assetKey, err := supply.AssetKey(tc.asset)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{}
			cfg.Supply.StaleComponentLedgersByAsset = map[string]uint32{tc.configKey: 5000}
			opts, err := supplyRefresherOptions(cfg, assetKey)
			if err != nil {
				t.Fatalf("supplyRefresherOptions: %v", err)
			}
			r := supply.NewRefresher(
				stubSupplyLedgers{ledger: 50_001_500, observedAt: time.Unix(1_770_000_000, 0).UTC()},
				stubSupplyComputer{out: supply.Supply{
					AssetKey:           assetKey,
					TotalSupply:        big.NewInt(1_000_000),
					CirculatingSupply:  big.NewInt(900_000),
					Basis:              supply.BasisXLMSDFReserveExclusion,
					MinComponentLedger: 50_000_310,
				}},
				&stubSupplyInserter{},
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				opts...,
			)
			if out := r.Tick(context.Background()); out.Kind != supply.OutcomeKindOK {
				t.Fatalf("config key %q, snapshot key %q: tick kind = %s, want %s — the "+
					"per-asset override never reached the gate (err=%v)",
					tc.configKey, assetKey, out.Kind, supply.OutcomeKindOK, out.Err)
			}
		})
	}
}

// A key that can never match must fail refresher construction, not
// silently leave the global threshold in force.
func TestSupplyRefresherOptions_RejectsUnresolvableKey(t *testing.T) {
	cfg := config.Config{}
	cfg.Supply.StaleComponentLedgersByAsset = map[string]uint32{"PHO_NOT_A_KEY": 5000}
	if _, err := supplyRefresherOptions(cfg, "XLM"); err == nil {
		t.Fatal("supplyRefresherOptions accepted an unparseable key; want error")
	}
}
