// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// perAssetStaleReader is a marketsStaleReader stub keyed on the
// asset_id string each fan-out leg is called with, so a test can make
// one leg fail while another succeeds.
type perAssetStaleReader struct {
	rows map[string][]Market
	errs map[string]error
}

func (r *perAssetStaleReader) DistinctPairsExt(context.Context, string, int, timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (r *perAssetStaleReader) SourceMarkets(context.Context, string, string, int, timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (r *perAssetStaleReader) AssetMarkets(context.Context, string, string, int, timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (r *perAssetStaleReader) AllPools(context.Context, timescale.PoolsFilter, string, int, timescale.MarketsOrder) ([]Pool, string, error) {
	return nil, "", nil
}

func (r *perAssetStaleReader) PairMarket(context.Context, canonical.Asset, canonical.Asset) (Market, bool, error) {
	return Market{}, false, nil
}

func (r *perAssetStaleReader) GetPairsVolumeHistory24hBatch(context.Context, [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return nil, nil
}

func (r *perAssetStaleReader) FirstTradeBatch(context.Context, [][2]string) (map[string]time.Time, error) {
	return nil, nil
}

func (r *perAssetStaleReader) DistinctPairsExtAt(context.Context, string, int, timescale.MarketsOrder) ([]Market, string, time.Time, bool, error) {
	return nil, "", time.Time{}, false, nil
}

func (r *perAssetStaleReader) SourceMarketsAt(context.Context, string, string, int, timescale.MarketsOrder) ([]Market, string, time.Time, bool, error) {
	return nil, "", time.Time{}, false, nil
}

func (r *perAssetStaleReader) AssetMarketsAt(_ context.Context, asset string, _ string, _ int, _ timescale.MarketsOrder) ([]Market, string, time.Time, bool, error) {
	if err, ok := r.errs[asset]; ok {
		return nil, "", time.Time{}, false, err
	}
	return r.rows[asset], "", time.Now(), false, nil
}

// TestFanOutAssetMarketsSurfacesPartialFailure is the CA2-A04-correct-6
// regression: when one leg of an asset_id fan-out errors and another
// succeeds, the merged response must say so via `stale`, not silently
// report a partial view as fresh and complete.
func TestFanOutAssetMarketsSurfacesPartialFailure(t *testing.T) {
	reader := &perAssetStaleReader{
		rows: map[string][]Market{
			"native": {{Base: "native", Quote: "fiat:USD", TradeCount24h: 5}},
		},
		errs: map[string]error{
			"crypto:XLM": errors.New("leg deadline exceeded"),
		},
	}
	s := quietServer()

	merged, _, stale, err := s.fanOutAssetMarkets(context.Background(), reader,
		[]string{"native", "crypto:XLM"}, 100, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatalf("fanOutAssetMarkets returned error %v, want nil (partial success)", err)
	}
	if len(merged) != 1 {
		t.Fatalf("merged rows = %d, want 1 (the surviving leg)", len(merged))
	}
	if !stale {
		t.Fatal("stale = false with a failed leg — a dropped leg must taint the " +
			"composite so the caller can tell the view is partial, not report a " +
			"partial market list as complete")
	}
}

// TestFanOutAssetMarketsSortsByRequestedOrder is the order_by half of
// CA2-A04-correct-6: the fan-out merge must sort by the caller's
// order_by, not a hardcoded trade-count comparator.
func TestFanOutAssetMarketsSortsByRequestedOrder(t *testing.T) {
	hiVol := "500.00"
	loVol := "10.00"
	reader := &perAssetStaleReader{
		rows: map[string][]Market{
			"native": {
				// Highest trade count but LOWEST volume: under order_by=pair
				// this must sort by base/quote, not trade count or volume.
				{Base: "zzz", Quote: "aaa", TradeCount24h: 100, Volume24hUSD: &loVol},
				{Base: "aaa", Quote: "zzz", TradeCount24h: 1, Volume24hUSD: &hiVol},
			},
		},
	}
	s := quietServer()

	merged, _, _, err := s.fanOutAssetMarkets(context.Background(), reader,
		[]string{"native"}, 100, timescale.MarketsOrderPair)
	if err != nil {
		t.Fatalf("fanOutAssetMarkets: %v", err)
	}
	if len(merged) != 2 || merged[0].Base != "aaa" || merged[1].Base != "zzz" {
		t.Fatalf("order_by=pair merged order = %v, want base ascending (aaa, zzz) — "+
			"the hardcoded trade-count sort must not override order_by", merged)
	}

	merged, _, _, err = s.fanOutAssetMarkets(context.Background(), reader,
		[]string{"native"}, 100, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatalf("fanOutAssetMarkets: %v", err)
	}
	if len(merged) != 2 || merged[0].Base != "aaa" || merged[1].Base != "zzz" {
		t.Fatalf("order_by=volume_24h_usd_desc merged order = %v, want highest volume "+
			"(aaa/zzz, 500.00) first, not highest trade count", merged)
	}
}
