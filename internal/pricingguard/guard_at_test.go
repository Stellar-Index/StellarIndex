package pricingguard

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Finding F031: /v1/price/at and every /v1/price/changes horizon resolve
// an instant through a ladder whose finest rung is the SAME raw
// prices_1m closed bucket /v1/price serves, but they carried only the
// withholding gates — no trailing-baseline guard. One extra path
// segment therefore republished the manipulated minute /v1/price
// refuses. These pin the point-in-time entry point's decisions,
// including the at-or-before staleness contract that makes it different
// from the latest-bucket one.

// baseTS is the candidate bucket's close, i.e. the instant a caller
// asking "price at now" would pass.
var baseTS = time.Date(2026, 7, 6, 12, 1, 0, 0, time.UTC)

func TestSelectGuardedVWAP1mAt_ManipulatedBucketServesLKG(t *testing.T) {
	// A 100x fat-finger in the bucket the ladder picked for `ts`. The
	// caller asked for an hour's tolerance, so the one-minute-older
	// last-known-good is still inside its contract and stands in.
	candidate := mkRow(0, "100.0")
	rows := steadyRows(12)

	served, ok := SelectGuardedVWAP1mAt(candidate, rows, baseTS, time.Hour)
	if !ok {
		t.Fatal("an in-contract last-known-good must be served, not withheld")
	}
	if served.VWAP != "1.0" {
		t.Fatalf("served VWAP = %s, want last-known-good 1.0 (the manipulated 100.0 must not be republished)", served.VWAP)
	}
	if !served.Bucket.Equal(rows[0].Bucket) {
		t.Fatalf("served bucket = %v, want the newest clean trailing bucket %v", served.Bucket, rows[0].Bucket)
	}
}

func TestSelectGuardedVWAP1mAt_SaneBucketServedUnchanged(t *testing.T) {
	// The guard is a pass-through on a healthy bucket: byte-identical
	// candidate, whatever the staleness tolerance.
	candidate := mkRow(0, "1.01")
	for _, tol := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour} {
		served, ok := SelectGuardedVWAP1mAt(candidate, steadyRows(12), baseTS, tol)
		if !ok {
			t.Fatalf("tolerance %v: a sane candidate must be served", tol)
		}
		if served.VWAP != candidate.VWAP || !served.Bucket.Equal(candidate.Bucket) {
			t.Fatalf("tolerance %v: served %+v, want the candidate unchanged", tol, served)
		}
	}
}

func TestSelectGuardedVWAP1mAt_StaleLKGWithheldNotServed(t *testing.T) {
	// The last-known-good is older than the rejected candidate by
	// construction, so it must re-clear the caller's own at-or-before
	// bound. Here the newest clean bucket closes 11 minutes before `ts`
	// and the caller allowed 5 — serving it would silently breach the
	// staleness contract /v1/price/at advertises, so the honest answer
	// is "no price at this instant" (the handler 404s / nulls the
	// horizon) rather than the manipulated value.
	candidate := mkRow(0, "100.0")
	rows := make([]timescale.Vwap1mRow, 12)
	for i := range rows {
		rows[i] = mkRow(i+11, "1.0") // oldest usable clean bucket is 11m back
	}

	served, ok := SelectGuardedVWAP1mAt(candidate, rows, baseTS, 5*time.Minute)
	if ok {
		t.Fatalf("served %+v with a 5m tolerance — a %v-old last-known-good breaches the caller's contract",
			served, baseTS.Sub(rows[0].Bucket.Add(time.Minute)))
	}
	if served.VWAP != "" {
		t.Errorf("withheld answer carried a value (%s) — callers must get an empty row", served.VWAP)
	}
	// The same rejection with a tolerance that covers the gap DOES serve.
	if _, ok := SelectGuardedVWAP1mAt(candidate, rows, baseTS, time.Hour); !ok {
		t.Error("an hour's tolerance covers an 11-minute-old last-known-good — must be served")
	}
}

func TestSelectGuardedVWAP1mAt_EmptyBaselineFailsOpen(t *testing.T) {
	// Posture unchanged from the latest-bucket guard: no baseline means
	// nothing to judge against, so the real price is still served.
	candidate := mkRow(0, "1.01")
	served, ok := SelectGuardedVWAP1mAt(candidate, nil, baseTS, time.Minute)
	if !ok || served.VWAP != candidate.VWAP {
		t.Fatalf("empty baseline: served %+v ok=%v, want the candidate served unchanged", served, ok)
	}
}

// fetchErrReader is a TrailingReader whose fetch always fails.
type fetchErrReader struct{}

func (fetchErrReader) RecentClosedVWAP1mCombined(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return nil, context.DeadlineExceeded
}

// rowsReader serves a fixed trailing slice.
type rowsReader struct{ rows []timescale.Vwap1mRow }

func (r rowsReader) RecentClosedVWAP1mCombined(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return r.rows, nil
}

func TestGuardServedVWAP1mAt_EndToEnd(t *testing.T) {
	pair := mustPair(t)
	candidate := mkRow(0, "100.0")

	// A transient fetch failure must not blank a price: fail open.
	served, ok := GuardServedVWAP1mAt(context.Background(), fetchErrReader{}, nil, pair, candidate, baseTS, time.Hour)
	if !ok || served.VWAP != "100.0" {
		t.Fatalf("fetch error: served %+v ok=%v, want the candidate served unguarded", served, ok)
	}

	// With a baseline, the manipulated bucket is swapped for last-known-good.
	served, ok = GuardServedVWAP1mAt(context.Background(), rowsReader{rows: steadyRows(12)}, nil, pair, candidate, baseTS, time.Hour)
	if !ok {
		t.Fatal("an in-contract last-known-good must be served")
	}
	if served.VWAP != "1.0" {
		t.Fatalf("served VWAP = %s, want last-known-good 1.0", served.VWAP)
	}
}

func mustPair(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("base asset: %v", err)
	}
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("quote asset: %v", err)
	}
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return p
}

// TestSelectGuardedVWAP1mAt_StalenessBoundaryIsTheBucketClose pins both
// edges of the at-or-before contract: the substitute's age is measured
// from its bucket CLOSE (start + 1m), and a gap exactly equal to
// maxStaleness is still inside it.
func TestSelectGuardedVWAP1mAt_StalenessBoundaryIsTheBucketClose(t *testing.T) {
	candidate := mkRow(0, "100.0")
	rows := make([]timescale.Vwap1mRow, 12)
	for i := range rows {
		rows[i] = mkRow(i+11, "1.0") // newest clean bucket closes exactly 11m before baseTS
	}
	served, ok := SelectGuardedVWAP1mAt(candidate, rows, baseTS, 11*time.Minute)
	if !ok || !served.Bucket.Equal(rows[0].Bucket) {
		t.Fatalf("gap == maxStaleness (11m from the bucket close) must serve the last-known-good; ok=%v served=%+v", ok, served)
	}
	if _, ok := SelectGuardedVWAP1mAt(candidate, rows, baseTS, 11*time.Minute-time.Nanosecond); ok {
		t.Fatal("a gap 1ns past maxStaleness must withhold")
	}
}

// TestSelectGuardedVWAP1m_UnparseableCandidateIsNotRejected: a candidate
// the guard cannot parse is served as-is — never reported as a rejection
// (which would make GuardServedVWAP1mConfidence flag it substituted).
func TestSelectGuardedVWAP1m_UnparseableCandidateIsNotRejected(t *testing.T) {
	candidate := mkRow(0, "not-a-number")
	served, rejected := SelectGuardedVWAP1m(candidate, steadyRows(12))
	if rejected || served.VWAP != candidate.VWAP || !served.Bucket.Equal(candidate.Bucket) {
		t.Fatalf("unparseable candidate: served %+v rejected=%v, want the candidate unchanged, not rejected", served, rejected)
	}
}
