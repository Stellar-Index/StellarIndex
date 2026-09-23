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

func TestSelectGuardedVWAP1mAt_EmptyBaselineWithheld(t *testing.T) {
	// No trailing bucket to judge against is the pair's first-ever minute,
	// the one a lone manipulated print most easily owns. /v1/price serves it
	// flagged stale; a point-in-time answer has no stale flag, so it is
	// withheld rather than published as a validated price.
	candidate := mkRow(0, "1.01")
	served, ok := SelectGuardedVWAP1mAt(candidate, nil, baseTS, time.Hour)
	if ok {
		t.Fatalf("empty baseline: served %+v as a validated point-in-time price", served)
	}
	if served.VWAP != "" {
		t.Errorf("withheld answer carried a value (%s) — callers must get an empty row", served.VWAP)
	}
}

// historyReader answers both trailing reads the way the store does over
// one newest-first history: the now-anchored read returns the newest
// `limit` buckets, the anchored read the newest `limit` strictly before
// the anchor.
type historyReader struct{ rows []timescale.Vwap1mRow }

func (h historyReader) RecentClosedVWAP1mCombined(_ context.Context, _ canonical.Pair, limit int) ([]timescale.Vwap1mRow, error) {
	return h.rows[:min(limit, len(h.rows))], nil
}

func (h historyReader) ClosedVWAP1mCombinedBefore(_ context.Context, _ canonical.Pair, before time.Time, limit int) ([]timescale.Vwap1mRow, error) {
	var out []timescale.Vwap1mRow
	for _, r := range h.rows {
		if r.Bucket.Before(before) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestGuardServedVWAP1mAt_HistoricalCandidateJudgedAgainstItsOwnPast(t *testing.T) {
	// Three hours of flat 1.0 with a 100x print two hours back — the
	// reference behind /v1/price/changes' 1h/24h horizons. The newest
	// SampleFetch buckets are all NEWER than that candidate, so a baseline
	// fetched from now holds nothing to judge it by.
	history := make([]timescale.Vwap1mRow, 180)
	for i := range history {
		history[i] = mkRow(i, "1.0")
	}
	const candidateAgo = 120
	history[candidateAgo] = mkRow(candidateAgo, "100.0")
	candidate := history[candidateAgo]
	ts := candidate.Bucket.Add(time.Minute)

	served, ok := GuardServedVWAP1mAt(context.Background(), historyReader{rows: history}, nil,
		mustPair(t), candidate, ts, time.Hour)
	if !ok {
		t.Fatal("a clean bucket one minute before the candidate is in contract — must be served")
	}
	if served.VWAP != "1.0" {
		t.Fatalf("served VWAP = %s at %v, want last-known-good 1.0 — the manipulated 100.0 was judged "+
			"against a baseline that does not precede it", served.VWAP, served.Bucket)
	}
	if !served.Bucket.Equal(history[candidateAgo+1].Bucket) {
		t.Fatalf("served bucket = %v, want the bucket immediately before the candidate %v",
			served.Bucket, history[candidateAgo+1].Bucket)
	}
}

func TestGuardServedVWAP1mSeries_DropsManipulatedAndUnvalidatedBuckets(t *testing.T) {
	// SEP-40 prices(records=5) over a 12-bucket history: a 100x print at
	// index 2 is dropped (not replaced by an older value at its
	// timestamp); every sane bucket keeps its own row.
	history := make([]timescale.Vwap1mRow, 12)
	for i := range history {
		history[i] = mkRow(i, "1.0")
	}
	history[2] = mkRow(2, "100.0")
	got := GuardServedVWAP1mSeries(nil, mustPair(t), history, 5)
	if len(got) != 4 {
		t.Fatalf("got %d records, want 4 (the manipulated bucket dropped): %+v", len(got), got)
	}
	for _, r := range got {
		if r.VWAP != "1.0" {
			t.Errorf("record at %v = %s, want 1.0 — a manipulated bucket was published", r.Bucket, r.VWAP)
		}
		if r.Bucket.Equal(history[2].Bucket) {
			t.Errorf("the rejected bucket's timestamp was filled with another value")
		}
	}
	// A pair whose whole history is one bucket: nothing validates it.
	if got := GuardServedVWAP1mSeries(nil, mustPair(t), history[:1], 5); len(got) != 0 {
		t.Errorf("first-ever bucket published unvalidated: %+v", got)
	}
}

func TestGuardServedVWAP1mAt_EndToEnd(t *testing.T) {
	pair := mustPair(t)
	candidate := mkRow(0, "100.0")

	// A transient fetch failure must not blank a price: fail open.
	served, ok := GuardServedVWAP1mAt(context.Background(), fakeTrailing{err: context.DeadlineExceeded}, nil, pair, candidate, baseTS, time.Hour)
	if !ok || served.VWAP != "100.0" {
		t.Fatalf("fetch error: served %+v ok=%v, want the candidate served unguarded", served, ok)
	}

	// With a baseline, the manipulated bucket is swapped for last-known-good.
	served, ok = GuardServedVWAP1mAt(context.Background(), fakeTrailing{rows: steadyRows(12)}, nil, pair, candidate, baseTS, time.Hour)
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
