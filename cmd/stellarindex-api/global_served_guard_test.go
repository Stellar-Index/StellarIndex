package main

import (
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// The /v1/assets/{slug} GlobalAssetView headline price
// (globalPriceReader.LatestVWAP) reads the same raw prices_1m closed
// bucket as /v1/price and applies the identical guardServedVWAP1m /
// selectGuardedVWAP1m decision. These tests mirror the /v1/price glue
// tests but assert the FULL row that the headline path surfaces —
// VWAP + Bucket (→ asOf) + TradeCount + Sources — so that on a
// manipulation rejection the headline serves the last-known-good row's
// fields, not the fat-finger candidate's. TradeCount in particular is not
// exercised by served_guard_glue_test.go.

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

// asOf mirrors globalPriceReader.LatestVWAP's closed-bucket timestamp
// derivation (bucket start + 1 minute, ADR-0015) so the tests assert the
// consumer-facing observation_at exactly as the handler surfaces it.
func asOf(row timescale.Vwap1mRow) time.Time { return row.Bucket.Add(time.Minute) }

func TestGlobalHeadline_NormalBucketServedUnchanged(t *testing.T) {
	candidate := mkGlobalRow(0, "1.01", 42, "kraken")
	served, rejected := selectGuardedVWAP1m(candidate, steadyGlobalRows(12))
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

func TestGlobalHeadline_FatFingerServesLKGWithLKGFields(t *testing.T) {
	// A 100x fat-finger in the latest bucket. The headline must serve the
	// newest clean trailing bucket's FULL row (VWAP, TradeCount, Sources,
	// asOf) — not the manipulated candidate's.
	candidate := mkGlobalRow(0, "100.0", 999, "kraken")
	rows := steadyGlobalRows(12)
	served, rejected := selectGuardedVWAP1m(candidate, rows)
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

func TestGlobalHeadline_ThinHistoryPassesThrough(t *testing.T) {
	// Too few trailing buckets to judge → serve the candidate even if
	// extreme (fail open — favour serving a real price over over-filtering).
	candidate := mkGlobalRow(0, "999.0", 7, "kraken")
	served, rejected := selectGuardedVWAP1m(candidate, steadyGlobalRows(3))
	if rejected {
		t.Fatal("thin history must fail open (serve candidate)")
	}
	if served.VWAP != candidate.VWAP || served.TradeCount != candidate.TradeCount {
		t.Fatalf("thin-history served %+v, want candidate %+v", served, candidate)
	}
}

func TestGlobalHeadline_DepegServedNotRejected(t *testing.T) {
	// A real stablecoin depeg is news — the headline must show it, never
	// hold it back as last-known-good.
	candidate := mkGlobalRow(0, "0.965", 55, "kraken")
	served, rejected := selectGuardedVWAP1m(candidate, steadyGlobalRows(12))
	if rejected {
		t.Fatal("a real depeg must be served on the headline, never hidden")
	}
	if served.VWAP != "0.965" {
		t.Fatalf("served VWAP = %s, want the depeg value 0.965", served.VWAP)
	}
}
