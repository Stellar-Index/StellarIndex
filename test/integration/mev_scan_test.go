//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"reflect"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const mevIntegrationAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func openMEVStore(t *testing.T, ctx context.Context) *timescale.Store {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn) //nolint:contextcheck // golang-migrate takes no context
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestStorage_MEVTradeScanKeepsNewest: with more on-chain trades in the
// window than the cap, the MEV trade scan returns the NEWEST `limit`
// rows, ascending. An oldest-first LIMIT returned the same stale head
// every tick and never reached a burst's tail.
func TestStorage_MEVTradeScanKeepsNewest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	usdc, err := c.NewClassicAsset("USDC", mevIntegrationAccount)
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	ts := time.Now().UTC().Truncate(time.Second)
	for nonce := 1; nonce <= 5; nonce++ {
		tr := mkIntegrationTrade("soroswap", nonce, ts, pair, 1_000_000_000, 25_000_000)
		tr.Taker = mevIntegrationAccount
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade(%d): %v", nonce, err)
		}
	}

	trades, usd, err := store.TradesForArbScan(ctx, ts.Add(-time.Hour), 3)
	if err != nil {
		t.Fatalf("TradesForArbScan: %v", err)
	}
	if len(trades) != 3 || len(usd) != 3 {
		t.Fatalf("got %d trades / %d usd, want 3 / 3", len(trades), len(usd))
	}
	for i, want := range []uint32{50_000_003, 50_000_004, 50_000_005} {
		if trades[i].Ledger != want {
			t.Errorf("trades[%d].Ledger = %d, want %d (newest 3, ascending)", i, trades[i].Ledger, want)
		}
	}
}

// TestStorage_MEVFillScanCarriesPositionAssets: the cascade correlator
// keys its oracle evidence on the filled position's own reserves, which
// the scan reads out of the fill's bid + lot jsonb as a distinct, sorted
// asset list (a fill with no bid/lot yields an empty list).
func TestStorage_MEVFillScanCarriesPositionAssets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	const (
		xlmSAC  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
		usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		pool    = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
		txA     = "4444444444444444444444444444444444444444444444444444444444444444"
		txB     = "5555555555555555555555555555555555555555555555555555555555555555"
	)
	xlm, err := c.NewSorobanAsset(xlmSAC)
	if err != nil {
		t.Fatalf("xlm: %v", err)
	}
	usdcC, err := c.NewSorobanAsset(usdcSAC)
	if err != nil {
		t.Fatalf("usdc: %v", err)
	}
	ts := time.Now().UTC().Truncate(time.Second)
	amt := func(asset c.Asset) blend.AssetAmount {
		return blend.AssetAmount{Asset: asset, Amount: big.NewInt(1_000)}
	}

	withData := blend.FillAuctionEvent{
		Pool: pool, AuctionType: 0, User: mevIntegrationAccount, Filler: mevIntegrationAccount,
		FillPercent: big.NewInt(100),
		Data: blend.AuctionData{
			Bid: []blend.AssetAmount{amt(usdcC)},
			Lot: []blend.AssetAmount{amt(xlm), amt(usdcC)},
		},
		Ledger: 52_500_001, TxHash: txA, Timestamp: ts,
	}
	bare := withData
	bare.Data = blend.AuctionData{}
	bare.Ledger, bare.TxHash = 52_500_002, txB
	for _, e := range []blend.FillAuctionEvent{withData, bare} {
		if err := store.InsertBlendFillAuction(ctx, e); err != nil {
			t.Fatalf("InsertBlendFillAuction(%d): %v", e.Ledger, err)
		}
	}

	fills, err := store.BlendFillsForMEVScan(ctx, ts.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("BlendFillsForMEVScan: %v", err)
	}
	if len(fills) != 2 {
		t.Fatalf("got %d fills, want 2: %+v", len(fills), fills)
	}
	if want := []string{xlmSAC, usdcSAC}; !reflect.DeepEqual(fills[0].Assets, want) {
		t.Errorf("fill assets = %v, want the distinct sorted bid+lot assets %v", fills[0].Assets, want)
	}
	if len(fills[1].Assets) != 0 {
		t.Errorf("bare fill assets = %v, want empty", fills[1].Assets)
	}
}

// TestStorage_MEVEventAssetColumns: a pair-scoped event lands its primary
// asset/quote in mev_events.asset_id / quote_id (the per-asset history
// index's key), a cross-asset event stores NULL there, and profit_usd
// stays NULL for both — no detector estimates attacker profit.
func TestStorage_MEVEventAssetColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	const txA = "6666666666666666666666666666666666666666666666666666666666666666"
	ts := time.Now().UTC().Truncate(time.Second)
	events := []domain.MEVStoredEvent{
		{
			Kind: "sandwich", DetectedAtLedger: 52_600_001, Timestamp: ts,
			AssetID: "native", QuoteID: "fiat:USD",
			TxHashes: []string{txA}, Accounts: []string{mevIntegrationAccount},
			DedupKey: "sandwich:it:1", DetailJSON: []byte(`{"notional_usd":"1234.56"}`),
		},
		{
			Kind: "arbitrage", DetectedAtLedger: 52_600_002, Timestamp: ts.Add(time.Second),
			TxHashes: []string{txA}, Accounts: []string{mevIntegrationAccount},
			DedupKey: "arbitrage:it:2", DetailJSON: []byte(`{}`),
		},
	}
	for _, e := range events {
		if ok, err := store.InsertMEVEvent(ctx, e); err != nil || !ok {
			t.Fatalf("InsertMEVEvent(%s) = %v, %v", e.Kind, ok, err)
		}
	}

	rows, err := store.ListMEVEvents(ctx, "", 10)
	if err != nil {
		t.Fatalf("ListMEVEvents: %v", err)
	}
	got := map[string]timescale.MEVEventRow{}
	for _, r := range rows {
		got[r.Kind] = r
	}
	if s := got["sandwich"]; s.AssetID != "native" || s.QuoteID != "fiat:USD" || s.ProfitUSD != "" {
		t.Errorf("sandwich row asset/quote/profit = %q/%q/%q, want native/fiat:USD/NULL", s.AssetID, s.QuoteID, s.ProfitUSD)
	}
	if a := got["arbitrage"]; a.AssetID != "" || a.QuoteID != "" || a.ProfitUSD != "" {
		t.Errorf("arbitrage row asset/quote/profit = %q/%q/%q, want all NULL", a.AssetID, a.QuoteID, a.ProfitUSD)
	}

	var indexed int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM mev_events WHERE asset_id = 'native'`).Scan(&indexed); err != nil {
		t.Fatalf("per-asset count: %v", err)
	}
	if indexed != 1 {
		t.Errorf("per-asset lookup found %d events for native, want 1", indexed)
	}
}
