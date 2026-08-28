//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestStorage_OracleRawRowsReadBack is the reader half of the oracle
// capture-totality design (PR-1). Every oracle_updates reader
// re-parses the asset column with canonical.ParseAsset; on origin/main
// `raw:NOTACOIN` fell into the classic `<code>:<issuer>` split and the
// keyed readers returned an error (a 500 at the API), while
// LatestOracleStreams silently dropped the row. With the
// canonical.AssetOracleRaw variant the row round-trips through
// InsertOracleUpdate → SQL → ParseAsset on every reader, and stays
// invisible to readers keyed on a MAPPED asset (safe by keying).
func TestStorage_OracleRawRowsReadBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, _ := c.NewFiatAsset("USD")
	raw, err := c.NewOracleRawAsset("NOTACOIN")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	rawFeed, err := c.NewOracleRawAsset("SolvBTC.BBN_FUNDAMENTAL/USD")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	price, _ := new(big.Int).SetString("12420000000000", 10)
	ts := time.Now().UTC().Truncate(time.Second)

	seeds := []c.OracleUpdate{
		// A mapped row and a raw row from the SAME source + event — the
		// shape PR-2's decoders will emit for a mixed vector.
		{
			Source: "reflector-cex", Ledger: 52_430_001,
			TxHash:  "1111111111111111111111111111111111111111111111111111111111111111",
			OpIndex: 0, Timestamp: ts,
			Asset: c.NativeAsset(), Quote: usd,
			Price: c.NewAmount(price), Decimals: 14,
		},
		{
			Source: "reflector-cex", Ledger: 52_430_001,
			TxHash:  "1111111111111111111111111111111111111111111111111111111111111111",
			OpIndex: 1, Timestamp: ts,
			Asset: raw, Quote: usd,
			Price: c.NewAmount(price), Decimals: 14,
		},
		// A RedStone-shaped feed_id with `.`, `_`, `/` in the symbol.
		{
			Source: "redstone", Ledger: 52_430_002,
			TxHash:  "2222222222222222222222222222222222222222222222222222222222222222",
			OpIndex: 0, Timestamp: ts,
			Asset: rawFeed, Quote: usd,
			Price: c.NewAmount(price), Decimals: 8,
		},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate(%s): %v", u.Asset, err)
		}
	}

	// LatestOracleUpdatesForAssets keyed on the raw asset returns it,
	// parsed back to the raw variant, code intact.
	got, err := store.LatestOracleUpdatesForAssets(ctx, []c.Asset{raw}, "")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets(raw): %v", err)
	}
	if len(got) != 1 || !got[0].Asset.Equal(raw) || got[0].OpIndex != 1 {
		t.Fatalf("LatestOracleUpdatesForAssets(raw) = %+v, want the one raw:NOTACOIN row at op_index 1", got)
	}
	if got[0].Asset.IsMapped() {
		t.Error("raw row read back as mapped")
	}

	// Keyed on the mapped asset it is NOT returned — safe by keying.
	got, err = store.LatestOracleUpdatesForAssets(ctx, []c.Asset{c.NativeAsset()}, "")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets(native): %v", err)
	}
	if len(got) != 1 || !got[0].Asset.Equal(c.NativeAsset()) {
		t.Fatalf("LatestOracleUpdatesForAssets(native) = %+v, want only the native row", got)
	}

	// LatestOracleObservation (the divergence seam) with a raw key.
	obs, err := store.LatestOracleObservation(ctx, "redstone",
		[]string{rawFeed.String()}, []string{usd.String()})
	if err != nil {
		t.Fatalf("LatestOracleObservation(raw feed): %v", err)
	}
	if obs == nil || !obs.Asset.Equal(rawFeed) {
		t.Fatalf("LatestOracleObservation(raw feed) = %+v, want raw:SolvBTC.BBN_FUNDAMENTAL/USD", obs)
	}
	if obs.Asset.Code != "SolvBTC.BBN_FUNDAMENTAL/USD" {
		t.Errorf("raw code not verbatim: %q", obs.Asset.Code)
	}

	// LatestAggregatorPricesForPair with a raw base.
	agg, err := store.LatestAggregatorPricesForPair(ctx, raw, usd, []string{"reflector-cex"})
	if err != nil {
		t.Fatalf("LatestAggregatorPricesForPair(raw): %v", err)
	}
	if len(agg) != 1 || !agg[0].Asset.Equal(raw) {
		t.Fatalf("LatestAggregatorPricesForPair(raw) = %+v, want the raw row", agg)
	}

	// LatestOracleUpdateForAsset (single-key, ErrNotFound shape).
	one, err := store.LatestOracleUpdateForAsset(ctx, "reflector-cex", raw)
	if err != nil {
		t.Fatalf("LatestOracleUpdateForAsset(raw): %v", err)
	}
	if !one.Asset.Equal(raw) {
		t.Errorf("LatestOracleUpdateForAsset(raw) = %+v", one)
	}

	// LatestOracleStreams is unkeyed: on main the raw rows were dropped
	// by its parse-failure `continue`; totality means they are listed.
	streams, err := store.LatestOracleStreams(ctx)
	if err != nil {
		t.Fatalf("LatestOracleStreams: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range streams {
		seen[u.Source+"|"+u.Asset.String()] = true
	}
	for _, want := range []string{
		"reflector-cex|native",
		"reflector-cex|raw:NOTACOIN",
		"redstone|raw:SolvBTC.BBN_FUNDAMENTAL/USD",
	} {
		if !seen[want] {
			t.Errorf("LatestOracleStreams missing %s (got %v)", want, seen)
		}
	}
}
