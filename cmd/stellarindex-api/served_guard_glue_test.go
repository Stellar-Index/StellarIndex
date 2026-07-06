package main

import (
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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

func TestSelectGuardedVWAP1m_NormalBucketServedUnchanged(t *testing.T) {
	candidate := mkRow(0, "1.01")
	served, rejected := selectGuardedVWAP1m(candidate, steadyRows(12))
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
	served, rejected := selectGuardedVWAP1m(candidate, rows)
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
	served, rejected := selectGuardedVWAP1m(candidate, rows)
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
	served, rejected := selectGuardedVWAP1m(candidate, steadyRows(3))
	if rejected {
		t.Fatal("thin baseline must fail open (serve candidate)")
	}
	if served.VWAP != "999.0" {
		t.Fatalf("served VWAP = %s, want candidate 999.0 (fail-open)", served.VWAP)
	}
}

func TestSelectGuardedVWAP1m_NoTrailingRowsFailsOpen(t *testing.T) {
	candidate := mkRow(0, "42.0")
	served, rejected := selectGuardedVWAP1m(candidate, nil)
	if rejected || served.VWAP != "42.0" {
		t.Fatalf("no trailing rows must serve candidate unchanged; got served=%s rejected=%v", served.VWAP, rejected)
	}
}

func TestSelectGuardedVWAP1m_DepegServedNotRejected(t *testing.T) {
	// A real stablecoin depeg is news — must be served, not filtered.
	candidate := mkRow(0, "0.965")
	served, rejected := selectGuardedVWAP1m(candidate, steadyRows(12))
	if rejected {
		t.Fatal("a real depeg must be served, never hidden by the guard")
	}
	if served.VWAP != "0.965" {
		t.Fatalf("served VWAP = %s, want the depeg value 0.965", served.VWAP)
	}
}
