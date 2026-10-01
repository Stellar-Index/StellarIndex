//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDivergenceGroupedReads executes the pair-grouped board and the
// per-reference series SQL: the board's limit counts pairs (a pair is never
// cut between its references) and orders pairs by their widest gap; the
// series returns one cell per (bucket, reference).
func TestDivergenceGroupedReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewDivergenceSink(store)

	btc, err := canonical.ParsePair("crypto:BTC/fiat:USD")
	if err != nil {
		t.Fatalf("ParsePair: %v", err)
	}
	eth, err := canonical.ParsePair("crypto:ETH/fiat:USD")
	if err != nil {
		t.Fatalf("ParsePair: %v", err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, rec := range []domain.DivergenceObservationRecord{
		{Pair: btc, Reference: "chainlink", OurPrice: "100", RefPrice: "99.5", DeltaPct: "0.5", ObservedAt: at},
		{Pair: btc, Reference: "coingecko", OurPrice: "100", RefPrice: "99", DeltaPct: "1.0101", ObservedAt: at},
		{Pair: eth, Reference: "band", OurPrice: "10", RefPrice: "9", DeltaPct: "11.11", ObservedAt: at},
		{Pair: eth, Reference: "coingecko", OurPrice: "10", RefPrice: "10", DeltaPct: "0", ObservedAt: at},
	} {
		if err := sink.RecordObservation(ctx, rec); err != nil {
			t.Fatalf("RecordObservation %s/%s: %v", rec.Pair, rec.Reference, err)
		}
	}

	rows, err := store.ListDivergenceLatest(ctx, 7, false, 1)
	if err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	if len(rows) != 2 || rows[0].AssetID != "crypto:ETH" || rows[1].AssetID != "crypto:ETH" {
		t.Fatalf("limit=1 rows = %+v, want both ETH references (widest pair, never split)", rows)
	}
	if rows[0].Reference != "band" {
		t.Errorf("first reference = %s, want band (widest |delta_pct| first within the pair)", rows[0].Reference)
	}

	all, err := store.ListDivergenceLatest(ctx, 7, false, 100)
	if err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	if len(all) != 4 || all[2].AssetID != "crypto:BTC" {
		t.Fatalf("rows = %+v, want ETH's two references then BTC's two", all)
	}

	points, err := store.ListDivergenceSeries(ctx, "crypto:BTC", "fiat:USD", 7)
	if err != nil {
		t.Fatalf("ListDivergenceSeries: %v", err)
	}
	if len(points) != 2 || points[0].Reference != "chainlink" || points[1].Reference != "coingecko" ||
		!points[0].Bucket.Equal(points[1].Bucket) {
		t.Fatalf("series = %+v, want chainlink + coingecko in one bucket", points)
	}
	if points[1].OurPrice != "100" || points[1].RefPrice != "99" || !points[1].LastAt.Equal(at) {
		t.Errorf("coingecko cell = %+v, want our 100 vs ref 99 at %v", points[1], at)
	}
}
