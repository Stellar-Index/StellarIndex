package pricingguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// mkRow builds a combined-direction closed-bucket row at a given
// minutes-ago offset with the supplied VWAP text.
func mkRow(minutesAgo int, vwap string) timescale.Vwap1mRow {
	return timescale.Vwap1mRow{
		Bucket:  time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute),
		VWAP:    vwap,
		Sources: []string{"soroswap"},
	}
}

// steadyRows returns n newest-first flat-1.0 trailing buckets starting one
// minute older than the candidate (which is at minutesAgo=0).
func steadyRows(n int) []timescale.Vwap1mRow {
	rows := make([]timescale.Vwap1mRow, n)
	for i := 0; i < n; i++ {
		rows[i] = mkRow(i+1, "1.0")
	}
	return rows
}

// ─── SelectGuardedVWAP1m: pure decision ──────────────────────────

func TestSelectGuardedVWAP1m_NormalBucketServedUnchanged(t *testing.T) {
	candidate := mkRow(0, "1.01")
	served, rejected := SelectGuardedVWAP1m(candidate, steadyRows(12))
	if rejected {
		t.Fatal("a sane candidate must not be rejected")
	}
	if served.VWAP != candidate.VWAP || !served.Bucket.Equal(candidate.Bucket) {
		t.Fatalf("served %+v, want the candidate unchanged", served)
	}
}

func TestSelectGuardedVWAP1m_ManipulatedBucketServesLKG(t *testing.T) {
	// A 100x fat-finger in the latest bucket. Serve the newest clean
	// trailing bucket instead (last-known-good), not the manipulated value.
	candidate := mkRow(0, "100.0")
	rows := steadyRows(12)
	served, rejected := SelectGuardedVWAP1m(candidate, rows)
	if !rejected {
		t.Fatal("100x manipulated candidate must be rejected")
	}
	if served.VWAP != "1.0" {
		t.Fatalf("served VWAP = %s, want last-known-good 1.0", served.VWAP)
	}
	// LKG must be the NEWEST clean trailing bucket (1 minute older).
	if !served.Bucket.Equal(rows[0].Bucket) {
		t.Fatalf("served bucket = %v, want newest trailing bucket %v", served.Bucket, rows[0].Bucket)
	}
	// Sources must come from the served (LKG) row, not be dropped.
	if len(served.Sources) == 0 {
		t.Fatal("served last-known-good row lost its sources")
	}
}

func TestSelectGuardedVWAP1m_CandidateBucketExcludedFromOwnBaseline(t *testing.T) {
	// RecentClosedVWAP1mCombined returns the candidate bucket too (rows[0]
	// at the same bucket). It must be excluded from the trailing baseline —
	// otherwise a manipulated candidate would pollute the very baseline
	// meant to catch it. Here rows[0] duplicates the manipulated candidate
	// bucket; the guard must still reject using the older clean buckets.
	candidate := mkRow(0, "100.0")
	rows := []timescale.Vwap1mRow{mkRow(0, "100.0")} // same bucket as candidate
	rows = append(rows, steadyRows(12)...)           // older clean buckets
	served, rejected := SelectGuardedVWAP1m(candidate, rows)
	if !rejected {
		t.Fatal("candidate must be rejected despite its bucket duplicate in rows")
	}
	if served.VWAP != "1.0" {
		t.Fatalf("served VWAP = %s, want clean 1.0", served.VWAP)
	}
}

func TestSelectGuardedVWAP1m_ThinBaselineFailsOpen(t *testing.T) {
	// Too few trailing buckets to judge → serve the candidate even if
	// extreme (favour serving a real price over over-filtering).
	candidate := mkRow(0, "999.0")
	served, rejected := SelectGuardedVWAP1m(candidate, steadyRows(3))
	if rejected {
		t.Fatal("thin baseline must fail open (serve candidate)")
	}
	if served.VWAP != "999.0" {
		t.Fatalf("served VWAP = %s, want candidate 999.0 (fail-open)", served.VWAP)
	}
}

func TestSelectGuardedVWAP1m_NoTrailingRowsFailsOpen(t *testing.T) {
	candidate := mkRow(0, "42.0")
	served, rejected := SelectGuardedVWAP1m(candidate, nil)
	if rejected || served.VWAP != "42.0" {
		t.Fatalf("no trailing rows must serve candidate unchanged; got served=%s rejected=%v", served.VWAP, rejected)
	}
}

func TestSelectGuardedVWAP1m_DepegServedNotRejected(t *testing.T) {
	// A real stablecoin depeg is news — must be served, not filtered.
	candidate := mkRow(0, "0.965")
	served, rejected := SelectGuardedVWAP1m(candidate, steadyRows(12))
	if rejected {
		t.Fatal("a real depeg must be served, never hidden by the guard")
	}
	if served.VWAP != "0.965" {
		t.Fatalf("served VWAP = %s, want the depeg value 0.965", served.VWAP)
	}
}

// ─── SelectGuardedVWAP1m: full-row surfacing (headline path) ──────
//
// The /v1/assets/{slug} GlobalAssetView headline path surfaces the FULL
// row (VWAP + Bucket→asOf + TradeCount + Sources). These cases assert that
// on a manipulation rejection the served row's fields all come from the
// last-known-good bucket, not the fat-finger candidate. TradeCount in
// particular is not exercised by the flat-source cases above.

// mkGlobalRow builds a combined-direction closed-bucket row at a given
// minutes-ago offset with the supplied VWAP, trade count, and a single
// source, so a served-row's headline fields can be asserted end to end.
func mkGlobalRow(minutesAgo int, vwap string, tradeCount int64, source string) timescale.Vwap1mRow {
	return timescale.Vwap1mRow{
		Bucket:     time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute),
		VWAP:       vwap,
		TradeCount: tradeCount,
		Sources:    []string{source},
	}
}

// steadyGlobalRows returns n newest-first flat-1.0 trailing buckets, each
// with a distinct trade count / source, starting one minute older than the
// candidate (which is at minutesAgo=0).
func steadyGlobalRows(n int) []timescale.Vwap1mRow {
	rows := make([]timescale.Vwap1mRow, n)
	for i := 0; i < n; i++ {
		rows[i] = mkGlobalRow(i+1, "1.0", int64(100+i), "soroswap")
	}
	return rows
}

// asOf mirrors the closed-bucket timestamp derivation every raw-bucket
// serving path uses (bucket start + 1 minute, ADR-0015).
func asOf(row timescale.Vwap1mRow) time.Time { return row.Bucket.Add(time.Minute) }

func TestSelectGuardedVWAP1m_HeadlineNormalServedUnchanged(t *testing.T) {
	candidate := mkGlobalRow(0, "1.01", 42, "kraken")
	served, rejected := SelectGuardedVWAP1m(candidate, steadyGlobalRows(12))
	if rejected {
		t.Fatal("a sane headline candidate must not be rejected")
	}
	// Byte-identical pass-through: every field the headline surfaces is the
	// candidate's own.
	if served.VWAP != candidate.VWAP {
		t.Fatalf("served VWAP = %s, want candidate %s", served.VWAP, candidate.VWAP)
	}
	if served.TradeCount != candidate.TradeCount {
		t.Fatalf("served TradeCount = %d, want candidate %d", served.TradeCount, candidate.TradeCount)
	}
	if !asOf(served).Equal(asOf(candidate)) {
		t.Fatalf("served asOf = %v, want candidate %v", asOf(served), asOf(candidate))
	}
}

func TestSelectGuardedVWAP1m_HeadlineFatFingerServesLKGFields(t *testing.T) {
	// A 100x fat-finger in the latest bucket. The headline must serve the
	// newest clean trailing bucket's FULL row (VWAP, TradeCount, Sources,
	// asOf) — not the manipulated candidate's.
	candidate := mkGlobalRow(0, "100.0", 999, "kraken")
	rows := steadyGlobalRows(12)
	served, rejected := SelectGuardedVWAP1m(candidate, rows)
	if !rejected {
		t.Fatal("100x manipulated headline candidate must be rejected")
	}
	lkg := rows[0] // newest clean trailing bucket
	if served.VWAP != lkg.VWAP {
		t.Fatalf("served VWAP = %s, want last-known-good %s", served.VWAP, lkg.VWAP)
	}
	if served.TradeCount != lkg.TradeCount {
		t.Fatalf("served TradeCount = %d, want last-known-good %d (must not be the candidate's %d)",
			served.TradeCount, lkg.TradeCount, candidate.TradeCount)
	}
	if len(served.Sources) == 0 || served.Sources[0] != lkg.Sources[0] {
		t.Fatalf("served Sources = %v, want last-known-good %v", served.Sources, lkg.Sources)
	}
	// asOf must reflect the older (naturally staler) LKG bucket, never the
	// manipulated candidate minute.
	if !asOf(served).Equal(asOf(lkg)) {
		t.Fatalf("served asOf = %v, want last-known-good %v", asOf(served), asOf(lkg))
	}
	if asOf(served).Equal(asOf(candidate)) {
		t.Fatal("served asOf must not be the rejected candidate's bucket")
	}
}

func TestSelectGuardedVWAP1m_HeadlineThinHistoryPassesThrough(t *testing.T) {
	candidate := mkGlobalRow(0, "999.0", 7, "kraken")
	served, rejected := SelectGuardedVWAP1m(candidate, steadyGlobalRows(3))
	if rejected {
		t.Fatal("thin history must fail open (serve candidate)")
	}
	if served.VWAP != candidate.VWAP || served.TradeCount != candidate.TradeCount {
		t.Fatalf("thin-history served %+v, want candidate %+v", served, candidate)
	}
}

// ─── GuardServedVWAP1m: the store-backed wiring ──────────────────

// fakeTrailing is a TrailingReader that returns a canned trailing set (or
// error), so the wiring wrapper is testable without a database.
type fakeTrailing struct {
	rows []timescale.Vwap1mRow
	err  error
}

func (f fakeTrailing) RecentClosedVWAP1mCombined(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return f.rows, f.err
}

// testPair is a valid canonical pair for the wrapper cases (native/fiat:USD
// — the aggregator's headline pair). Its only use in the guard is
// pair.String() inside the warn-log branches.
func testPair(t *testing.T) canonical.Pair {
	t.Helper()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

func TestGuardServedVWAP1m_NormalBucketUnchanged(t *testing.T) {
	candidate := mkRow(0, "1.01")
	store := fakeTrailing{rows: steadyRows(12)}
	served := GuardServedVWAP1m(context.Background(), store, nil, testPair(t), candidate)
	if served.VWAP != candidate.VWAP || !served.Bucket.Equal(candidate.Bucket) {
		t.Fatalf("healthy bucket must pass through byte-identical; got %+v", served)
	}
}

func TestGuardServedVWAP1m_FatFingerServesLKG(t *testing.T) {
	candidate := mkRow(0, "100.0")
	rows := steadyRows(12)
	store := fakeTrailing{rows: rows}
	served := GuardServedVWAP1m(context.Background(), store, nil, testPair(t), candidate)
	if served.VWAP != "1.0" {
		t.Fatalf("fat-finger must serve last-known-good 1.0; got %s", served.VWAP)
	}
	if !served.Bucket.Equal(rows[0].Bucket) {
		t.Fatalf("served bucket = %v, want newest trailing %v", served.Bucket, rows[0].Bucket)
	}
}

func TestGuardServedVWAP1m_ThinHistoryPassesThrough(t *testing.T) {
	candidate := mkRow(0, "999.0")
	store := fakeTrailing{rows: steadyRows(3)}
	served := GuardServedVWAP1m(context.Background(), store, nil, testPair(t), candidate)
	if served.VWAP != "999.0" {
		t.Fatalf("thin history must pass the candidate through; got %s", served.VWAP)
	}
}

func TestGuardServedVWAP1m_TrailingFetchErrorFailsOpen(t *testing.T) {
	// The baseline fetch failing must never drop a real price: serve the
	// candidate unguarded even if it would otherwise look extreme.
	candidate := mkRow(0, "100.0")
	store := fakeTrailing{err: errors.New("boom")}
	served := GuardServedVWAP1m(context.Background(), store, nil, testPair(t), candidate)
	if served.VWAP != candidate.VWAP || !served.Bucket.Equal(candidate.Bucket) {
		t.Fatalf("fetch error must fail open (serve candidate); got %+v", served)
	}
}
