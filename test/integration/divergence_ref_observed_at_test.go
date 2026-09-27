//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMigration0186_DivergenceRefObservedAt runs 0186 against a compressed
// divergence_observations chunk holding a row the previous binary wrote, then
// round-trips the reference time through the sink and the /v1/divergence
// reader (GH-823): the new row returns exactly what was recorded, the old row
// and a record with no reference time return nil, and down drops the column.
func TestMigration0186_DivergenceRefObservedAt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 185)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	comparedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	seedCompressedPre0186Row(t, ctx, db, comparedAt)

	applyMigrationsUpTo(t, dsn, 186)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewDivergenceSink(store)

	btcUSD, err := canonical.ParsePair("crypto:BTC/fiat:USD")
	if err != nil {
		t.Fatalf("ParsePair: %v", err)
	}
	refAt := comparedAt.Add(-47*time.Minute - 123456*time.Microsecond)
	for _, rec := range []domain.DivergenceObservationRecord{
		{
			Pair: btcUSD, Reference: "redstone", OurPrice: "100", RefPrice: "99", DeltaPct: "1.0101",
			ObservedAt: comparedAt, RefObservedAt: refAt.In(time.FixedZone("UTC-5", -5*3600)),
		},
		{
			Pair: btcUSD, Reference: "coingecko", OurPrice: "100", RefPrice: "100", DeltaPct: "0",
			ObservedAt: comparedAt,
		},
	} {
		if err := sink.RecordObservation(ctx, rec); err != nil {
			t.Fatalf("RecordObservation %s: %v", rec.Reference, err)
		}
	}

	rows, err := store.ListDivergenceLatest(ctx, 7, false, 100)
	if err != nil {
		t.Fatalf("ListDivergenceLatest: %v", err)
	}
	got := map[string]*time.Time{}
	for _, r := range rows {
		got[r.Reference] = r.RefObservedAt
	}
	if len(got) != 3 {
		t.Fatalf("rows = %+v, want redstone, coingecko and the pre-0186 chainlink row", rows)
	}
	if at := got["redstone"]; at == nil || !at.Equal(refAt) {
		t.Errorf("redstone ref_observed_at = %v, want %v", at, refAt)
	}
	if at := got["coingecko"]; at != nil {
		t.Errorf("coingecko recorded no reference time; ref_observed_at = %v, want nil", at)
	}
	if at := got["chainlink"]; at != nil {
		t.Errorf("pre-0186 row ref_observed_at = %v, want nil", at)
	}

	applyMigrationsUpTo(t, dsn, 185)
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'divergence_observations' AND column_name = 'ref_observed_at'`).Scan(&n); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if n != 0 {
		t.Errorf("0186 down left ref_observed_at in place")
	}
}

// seedCompressedPre0186Row writes a row in the previous binary's INSERT shape
// and compresses its chunk, so 0186's ADD COLUMN has to survive compressed
// data rather than an empty table.
func seedCompressedPre0186Row(t *testing.T, ctx context.Context, db *sql.DB, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO divergence_observations (
		    asset_id, quote_id, reference, observed_at, observed_at_ledger,
		    our_price, ref_price, delta_pct, status)
		VALUES ('crypto:BTC', 'fiat:USD', 'chainlink', $1, 0, 100, 100, 0, 'clear')`, at); err != nil {
		t.Fatalf("seed pre-0186 row: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c) FROM show_chunks('divergence_observations') c
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no divergence_observations chunk was compressed — the ADD COLUMN-on-compressed claim would be vacuous")
	}
}
