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

// TestStorage_OracleRawRowsConsumers is the consumer half of the oracle
// capture-totality design against real Timescale: with a mapped row and
// raw rows in the same window, (1) the MEV scan — the only unkeyed
// consumer, feeding the liquidation_cascade correlator — returns the
// mapped row only; (2) the source bespoke page counts the raw feeds
// (totality) and surfaces them as the "Unmapped feeds" KPI; (3) the
// unfiltered streams read still lists the raw row (the API boundary,
// not storage, applies include_unmapped).
func TestStorage_OracleRawRowsConsumers(t *testing.T) {
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
	raw2, err := c.NewOracleRawAsset("ALSONOTACOIN")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	price, _ := new(big.Int).SetString("12420000000000", 10)
	ts := time.Now().UTC().Truncate(time.Second)
	const tx = "3333333333333333333333333333333333333333333333333333333333333333"

	seeds := []c.OracleUpdate{
		{Source: "reflector-cex", Ledger: 52_430_010, TxHash: tx, OpIndex: 0, Timestamp: ts, Asset: c.NativeAsset(), Quote: usd, Price: c.NewAmount(price), Decimals: 14},
		{Source: "reflector-cex", Ledger: 52_430_010, TxHash: tx, OpIndex: 1, Timestamp: ts, Asset: raw, Quote: usd, Price: c.NewAmount(price), Decimals: 14},
		{Source: "reflector-cex", Ledger: 52_430_010, TxHash: tx, OpIndex: 2, Timestamp: ts, Asset: raw2, Quote: usd, Price: c.NewAmount(price), Decimals: 14},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate(%s): %v", u.Asset, err)
		}
	}

	// (1) MEV scan: raw rows excluded in SQL.
	refs, err := store.OracleUpdatesForMEVScan(ctx, ts.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("OracleUpdatesForMEVScan: %v", err)
	}
	if len(refs) != 1 || refs[0].Asset != "native" {
		t.Fatalf("OracleUpdatesForMEVScan = %+v, want only the native row (raw rows are not cascade evidence)", refs)
	}

	// (2) Bespoke page: totality counted, unmapped surfaced as a KPI.
	blk, err := store.BuildProtocolBespoke(ctx, "reflector-cex", "oracle", 7)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke returned no block for a source with rows in the window")
	}
	kpi := map[string]string{}
	for _, k := range blk.KPIs {
		kpi[k.Label] = k.Value
	}
	if kpi["Updates (7d)"] != "3" || kpi["Distinct feeds"] != "3" {
		t.Errorf("KPIs = %v, want Updates (7d)=3 and Distinct feeds=3 (totality counts raw rows)", kpi)
	}
	if kpi["Unmapped feeds"] != "2" {
		t.Errorf("Unmapped feeds KPI = %q, want 2", kpi["Unmapped feeds"])
	}

	// (3) Unfiltered streams read lists the raw rows.
	streams, err := store.LatestOracleStreams(ctx)
	if err != nil {
		t.Fatalf("LatestOracleStreams: %v", err)
	}
	seen := map[string]bool{}
	for _, u := range streams {
		seen[u.Asset.String()] = true
	}
	for _, want := range []string{"native", "raw:NOTACOIN", "raw:ALSONOTACOIN"} {
		if !seen[want] {
			t.Errorf("LatestOracleStreams missing %s (got %v)", want, seen)
		}
	}
}
