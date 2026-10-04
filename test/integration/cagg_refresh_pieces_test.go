//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCAGGBucketsMatchCatalog holds every CAGGSpec.Bucket to the view's
// real time_bucket: RefreshPieces cuts a refresh on that grid, and a cut
// off the grid leaves the bucket it splits unrefreshed.
func TestCAGGBucketsMatchCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, spec := range slices.Concat(timescale.TradesCAGGs, timescale.OracleCAGGs) {
		want := fmt.Sprintf("%d seconds", int64(spec.Bucket/time.Second))
		if spec.Bucket == timescale.MonthBucket {
			want = "1 month"
		}
		var width, origin, offset, tz string
		var same bool
		err := store.DB().QueryRowContext(ctx, `
			SELECT bf.bucket_width, bf.bucket_width::interval = $2::interval,
			       coalesce(bf.bucket_origin, ''), coalesce(bf.bucket_offset, ''), coalesce(bf.bucket_timezone, '')
			  FROM _timescaledb_catalog.continuous_agg ca
			  JOIN _timescaledb_catalog.continuous_aggs_bucket_function bf ON bf.mat_hypertable_id = ca.mat_hypertable_id
			 WHERE ca.user_view_name = $1`, spec.Name, want).Scan(&width, &same, &origin, &offset, &tz)
		if err != nil {
			t.Fatalf("%s: bucket function: %v", spec.Name, err)
		}
		if !same || origin != "" || offset != "" || (tz != "" && tz != "UTC") {
			t.Errorf("%s: catalog bucket width=%q origin=%q offset=%q tz=%q, CAGGSpec.Bucket %s (%s) with the default origin",
				spec.Name, width, origin, offset, tz, spec.Bucket, want)
		}
	}
}

// TestRunCAGGRefreshStepPiecesMatchOneCall runs the cut refresh of every
// trades aggregate against TimescaleDB, then one forced CALL over the same
// window, and requires the same materialised rows: no bucket is lost at a
// cut. Trades sit one second either side of every cut, so a cut off the
// view's real grid would split a populated bucket.
func TestRunCAGGRefreshStepPiecesMatchOneCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	from := time.Date(2024, 1, 10, 13, 17, 23, 0, time.UTC)
	plan := timescale.PlanCAGGRefresh(timescale.TradesCAGGs, func(s timescale.CAGGSpec) (time.Time, time.Time) {
		span := max(timescale.CAGGRefreshPieceSpan, s.MinWindow)
		return from, from.Add(span * 5 / 2)
	})

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	var stamps []time.Time
	for _, st := range plan {
		pieces := timescale.RefreshPieces(st)
		if len(pieces) < 2 {
			t.Fatalf("%s over [%s, %s) is one CALL; the test needs a cut", st.View, st.From, st.To)
		}
		for _, p := range pieces[1:] {
			stamps = append(stamps, p.From.Add(-time.Second), p.From.Add(time.Second))
		}
		for ts := st.From; ts.Before(st.To); ts = ts.Add(5 * time.Hour) {
			stamps = append(stamps, ts)
		}
	}
	slices.SortFunc(stamps, func(a, b time.Time) int { return a.Compare(b) })
	stamps = slices.CompactFunc(stamps, func(a, b time.Time) bool { return a.Equal(b) })
	for i, ts := range stamps {
		tr := c.Trade{
			Source: "integ-cagg-pieces", Ledger: uint32(60_000_000 + i), TxHash: fmt.Sprintf("%064x", i),
			Timestamp: ts, Pair: pair,
			BaseAmount: c.NewAmount(big.NewInt(1_000_000_000)), QuoteAmount: c.NewAmount(big.NewInt(int64(12_000_000 + i))),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	matRows := func(view string) int {
		t.Helper()
		var ht string
		if err := store.DB().QueryRowContext(ctx, `
			SELECT format('%I.%I', materialization_hypertable_schema, materialization_hypertable_name)
			  FROM timescaledb_information.continuous_aggregates WHERE view_name = $1`, view).Scan(&ht); err != nil {
			t.Fatalf("%s: materialisation hypertable: %v", view, err)
		}
		var n int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM `+ht).Scan(&n); err != nil {
			t.Fatalf("%s: count: %v", view, err)
		}
		return n
	}

	cut := map[string]int{}
	for _, st := range plan {
		if err := timescale.RunCAGGRefreshStep(ctx, store, st, false); err != nil {
			t.Fatalf("RunCAGGRefreshStep(%s): %v", st.View, err)
		}
		cut[st.View] = matRows(st.View)
	}
	for _, st := range plan {
		if err := store.RefreshContinuousAggregateForced(ctx, st.View, st.From, st.To); err != nil {
			t.Fatalf("one forced CALL over %s: %v", st.View, err)
		}
		if whole := matRows(st.View); cut[st.View] == 0 || cut[st.View] != whole {
			t.Errorf("%s: %d rows from %d cut CALLs, %d from one CALL over the same window",
				st.View, cut[st.View], len(timescale.RefreshPieces(st)), whole)
		}
	}
}
