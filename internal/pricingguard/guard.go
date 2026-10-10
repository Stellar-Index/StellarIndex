// Package pricingguard is the serving-sanity guard shared by every raw
// prices_1m closed-bucket serving path across the API and aggregator binaries.
//
// Those paths read a CLOSED prices_1m bucket directly, a bare
// Σ(quote)/Σ(base) CAGG bucket that BYPASSES the orchestrator's σ-outlier
// filter, min-USD-volume gate and freeze protection (they guard only the path
// that writes the filtered VWAP to Redis). A single manipulated print would
// otherwise be served verbatim with stale=false. The consumers, each a WIRED call
// site: /v1/price (StorePriceReader.LatestPrice), /v1/assets/{slug}
// (GlobalPriceReader.LatestVWAP), the price-alert evaluator
// (priceAlertVWAPReader.LatestVWAP), /v1/price/at + /v1/price/changes
// (storePriceAtReader.PriceAt, via [GuardServedVWAP1mAt]), the SEP-40 prices()
// series (StorePriceReader.RecentClosedSnapshots, via [GuardServedVWAP1mSeries]) and /v1/assets change_24h_pct's 24h-ago
// anchor (storeChange24hReader.USDPrice24hAgo, via [GuardServedVWAP1mAt]).
// TestRawPrices1mReadersPassTheGuard (cmd/stellarindex-api) fails when a
// function under cmd/ calls a raw prices_1m store read without a guard entry
// point, or is missing from this list.
//
// This package is the WIRING around the pure robust-band decision
// ([aggregate.GuardServedVWAP], ADR-0003 exact-rational): it fetches the
// trailing baseline and, on gross deviation, swaps in the newest clean
// last-known-good bucket. It sits above storage and above the pure decision
// (timescale already imports aggregate, so the reverse edge would cycle), so both
// binaries can import it without duplicating the glue.
package pricingguard

import (
	"context"
	"log/slog"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SampleFetch is how many recent CLOSED combined-direction 1m buckets the
// serving-sanity guard pulls to build a robust trailing baseline for the
// latest bucket ([aggregate.GuardServedVWAP]). 40 is a few tens of minutes
// for an active pair — enough to clear the guard's minimum-sample floor
// while staying a cheap, index-driven LIMIT-N read (only ever run for a
// pair already confirmed populated). A thin pair's 40 buckets can span
// months; [BaselineMaxAge] is what keeps the baseline trailing.
const SampleFetch = 40

// BaselineMaxAge is the furthest before the judged bucket a trailing
// bucket may start and still count toward its baseline. A price from
// before a dormancy says nothing about a relisting one — judging against
// it rejects a real move and serves the dormant row as a confident price.
// One day keeps a pair that trades at least daily fully validated; a
// candidate with nothing in the horizon is the empty-baseline case.
const BaselineMaxAge = 24 * time.Hour

// TrailingReader is the storage seam the guard needs: the trailing
// combined-direction closed-bucket fetch. *timescale.Store satisfies it;
// keeping it an interface makes the wiring unit-testable without a
// database.
type TrailingReader interface {
	RecentClosedVWAP1mCombined(ctx context.Context, p canonical.Pair, limit int) ([]timescale.Vwap1mRow, error)
}

// TrailingAtReader is the storage seam of the POINT-IN-TIME guard: the
// trailing baseline anchored at the candidate bucket rather than at now.
// *timescale.Store satisfies it.
type TrailingAtReader interface {
	ClosedVWAP1mCombinedBefore(ctx context.Context, p canonical.Pair, before time.Time, limit int) ([]timescale.Vwap1mRow, error)
}

// GuardServedVWAP1mConfidence is the serving-sanity guard shared by every raw
// latest-bucket prices_1m serving path (see the package doc). Given the latest
// CLOSED bucket (`candidate`) it returns the row to serve: the candidate when it
// is robust-sane or there is no baseline / the trailing fetch failed (fail-open,
// favour serving a real price), else the newest trailing bucket within the robust
// band (last-known-good). The math is exact-rational (ADR-0003). It never
// errors: on doubt it serves the candidate rather than 404 a pair with data. A
// nil logger disables warn logging.
//
// lowConfidence is true when a SUCCESSFUL trailing fetch returned no usable
// baseline (a pair's first-ever served minute, or first after more than
// [BaselineMaxAge] dormant). [aggregate.GuardServedVWAP] fails open there, so the
// value is still served (no blackout of a new pair) but the caller must surface
// it as stale / low-confidence. A transient fetch error fails open with
// lowConfidence=false, so a DB blip does not flag every price stale.
//
// substituted is true when the candidate was rejected and `served` is the older
// last-known-good bucket. A caller that staples enrichment from a separate cache
// keyed by (pair, window) (confidence score, composite-router flags) must treat
// it as "that enrichment answers for the CURRENT tick, not the bucket served"
// and withhold it.
//
// There is deliberately no row-only form: every caller must decide what an
// unvalidated or substituted bucket means on its surface, and a caller with no
// flag to carry it withholds (as [GuardServedVWAP1mAt] does).
func GuardServedVWAP1mConfidence(
	ctx context.Context,
	store TrailingReader,
	logger *slog.Logger,
	pair canonical.Pair,
	candidate timescale.Vwap1mRow,
) (served timescale.Vwap1mRow, lowConfidence, substituted bool) {
	rows, err := store.RecentClosedVWAP1mCombined(ctx, pair, SampleFetch)
	if err != nil {
		obs.PricingGuardTrailingFetchFailedTotal.WithLabelValues("latest").Inc()
		if logger != nil {
			logger.Warn("served-vwap guard: trailing fetch failed — serving candidate unguarded",
				"pair", pair.String(), "err", err)
		}
		return candidate, false, false // fail-open (transient) — not low-confidence
	}
	served, rejected, lowConfidence := selectGuardedVWAP1m(candidate, rows)
	countDegraded("latest", rejected, lowConfidence)
	if rejected && logger != nil {
		logger.Warn("served-vwap guard: candidate bucket rejected as outlier — serving last-known-good",
			"pair", pair.String(),
			"candidate_bucket", candidate.Bucket,
			"candidate_vwap", candidate.VWAP,
			"served_bucket", served.Bucket,
			"served_vwap", served.VWAP)
	}
	if lowConfidence && logger != nil {
		logger.Warn("served-vwap guard: empty baseline — serving pair's first bucket as low-confidence/stale",
			"pair", pair.String(),
			"candidate_bucket", candidate.Bucket,
			"candidate_vwap", candidate.VWAP)
	}
	return served, lowConfidence, rejected
}

// GuardServedVWAP1mAt is [GuardServedVWAP1mConfidence] for the POINT-IN-TIME
// path (/v1/price/at and every /v1/price/changes horizon), whose FIRST ladder rung
// is the same raw prices_1m bucket. Callers apply it ONLY to a prices_1m answer;
// coarser rungs are hour/day bars, a diluted exposure the 1-minute baseline
// cannot judge.
//
// `ts` and `maxStaleness` are the caller's at-or-before contract: a rejected
// candidate is replaced by the newest clean trailing bucket only while it still
// CLOSES within maxStaleness of ts (the test [timescale.Store.ClosedVWAPAtOrBefore]
// applied). Otherwise ok=false and the caller reports "no price at this instant",
// never a rejected value nor one breaching the requested staleness.
//
// The baseline is the [SampleFetch] closed buckets immediately BEFORE the
// candidate ([TrailingAtReader]), not the newest: a now-anchored baseline holds
// nothing older than a historical candidate, so it would pass unjudged.
//
// An empty baseline (nothing within [BaselineMaxAge] before the candidate) is
// ok=false here, where [GuardServedVWAP1mConfidence] serves it flagged
// low-confidence: a point-in-time answer has no stale flag to carry that doubt.
// A trailing-fetch error still fails open.
func GuardServedVWAP1mAt(
	ctx context.Context,
	store TrailingAtReader,
	logger *slog.Logger,
	pair canonical.Pair,
	candidate timescale.Vwap1mRow,
	ts time.Time,
	maxStaleness time.Duration,
) (served timescale.Vwap1mRow, ok bool) {
	rows, err := store.ClosedVWAP1mCombinedBefore(ctx, pair, candidate.Bucket, SampleFetch)
	if err != nil {
		obs.PricingGuardTrailingFetchFailedTotal.WithLabelValues("at").Inc()
		if logger != nil {
			logger.Warn("served-vwap guard (point-in-time): trailing fetch failed — serving candidate unguarded",
				"pair", pair.String(), "err", err)
		}
		return candidate, true // fail-open (transient), as on the /v1/price path
	}
	served, ok, rejected, lowConfidence := selectGuardedVWAP1mAt(candidate, rows, ts, maxStaleness)
	countDegraded("at", rejected, lowConfidence)
	if logger != nil && (!ok || !served.Bucket.Equal(candidate.Bucket)) {
		logger.Warn("served-vwap guard (point-in-time): candidate bucket rejected or unvalidated",
			"pair", pair.String(),
			"requested_at", ts,
			"trailing_rows", len(rows),
			"candidate_bucket", candidate.Bucket,
			"candidate_vwap", candidate.VWAP,
			"served_bucket", served.Bucket,
			"served_vwap", served.VWAP,
			"served", ok)
	}
	return served, ok
}

// SelectGuardedVWAP1mAt is the pure decision half of
// [GuardServedVWAP1mAt]. ok=false means the caller has no servable
// answer for that instant: the candidate had no trailing baseline to be
// validated against, or it was rejected and no last-known-good bucket
// closes within `maxStaleness` of `ts`. Store-free so the staleness
// contract is unit-testable without a database.
func SelectGuardedVWAP1mAt(
	candidate timescale.Vwap1mRow,
	rows []timescale.Vwap1mRow,
	ts time.Time,
	maxStaleness time.Duration,
) (served timescale.Vwap1mRow, ok bool) {
	served, ok, _, _ = selectGuardedVWAP1mAt(candidate, rows, ts, maxStaleness)
	return served, ok
}

// selectGuardedVWAP1mAt is [SelectGuardedVWAP1mAt] plus the band's
// rejected / lowConfidence verdict on the candidate.
func selectGuardedVWAP1mAt(
	candidate timescale.Vwap1mRow,
	rows []timescale.Vwap1mRow,
	ts time.Time,
	maxStaleness time.Duration,
) (served timescale.Vwap1mRow, ok, rejected, lowConfidence bool) {
	served, rejected, lowConfidence = selectGuardedVWAP1m(candidate, rows)
	if lowConfidence {
		return timescale.Vwap1mRow{}, false, false, true
	}
	if !rejected {
		return served, true, false, false
	}
	// The last-known-good bucket is by construction OLDER than the
	// rejected candidate, so it has to re-clear the caller's own
	// at-or-before staleness bound (bucket close within maxStaleness of
	// ts) before it can stand in for it.
	if ts.Sub(served.Bucket.Add(time.Minute)) > maxStaleness {
		return timescale.Vwap1mRow{}, false, true, false
	}
	return served, true, true, false
}

// countDegraded records a guard decision that did not serve the current
// bucket as a validated price on [obs.PricingGuardDegradedTotal].
func countDegraded(path string, rejected, lowConfidence bool) {
	switch {
	case rejected:
		obs.PricingGuardDegradedTotal.WithLabelValues(path, "outlier").Inc()
	case lowConfidence:
		obs.PricingGuardDegradedTotal.WithLabelValues(path, "unvalidated").Inc()
	}
}

// GuardServedVWAP1mSeries is the guard for a raw prices_1m SERIES (the
// SEP-40 prices(asset, records) read). `rows` is one newest-first
// history holding at least `n` + [SampleFetch] buckets when the pair has
// that many, so each of the first `n` buckets is judged against the
// [SampleFetch] buckets immediately before it. A bucket the band rejects,
// or one with no prior bucket to validate it, is DROPPED rather than
// replaced: a series has one entry per bucket, and standing an older
// value in at a newer timestamp would fabricate a record. Returns at
// most `n` rows, newest-first. Exact-rational (ADR-0003).
func GuardServedVWAP1mSeries(logger *slog.Logger, pair canonical.Pair, rows []timescale.Vwap1mRow, n int) []timescale.Vwap1mRow {
	if n > len(rows) {
		n = len(rows)
	}
	out := make([]timescale.Vwap1mRow, 0, n)
	for i := 0; i < n; i++ {
		trailing := rows[i+1 : min(i+1+SampleFetch, len(rows))]
		_, rejected, lowConfidence := selectGuardedVWAP1m(rows[i], trailing)
		countDegraded("series", rejected, lowConfidence)
		if rejected || lowConfidence {
			if logger != nil {
				logger.Warn("served-vwap guard (series): bucket dropped as outlier or unvalidated",
					"pair", pair.String(),
					"bucket", rows[i].Bucket,
					"vwap", rows[i].VWAP,
					"trailing_rows", len(trailing))
			}
			continue
		}
		out = append(out, rows[i])
	}
	return out
}

// SelectGuardedVWAP1m is the pure decision half of [GuardServedVWAP1mConfidence]:
// given the candidate bucket and the recent combined-direction closed
// buckets (`rows`, newest-first, as returned by
// [timescale.Store.RecentClosedVWAP1mCombined]), it returns the row to
// serve and whether the candidate was rejected. Kept store-free so the
// selection + index-alignment logic is unit-testable without a database.
// Exact-rational throughout (ADR-0003).
func SelectGuardedVWAP1m(candidate timescale.Vwap1mRow, rows []timescale.Vwap1mRow) (served timescale.Vwap1mRow, rejected bool) {
	served, rejected, _ = selectGuardedVWAP1m(candidate, rows)
	return served, rejected
}

// selectGuardedVWAP1m is [SelectGuardedVWAP1m] plus the low-confidence
// (empty-baseline) signal. lowConfidence is true only when the candidate
// is ACCEPTED against an empty/unvalidated baseline
// ([aggregate.ServedBaselineValidated] is false — nothing within
// [BaselineMaxAge] before it, GuardServedVWAP's fail-open). A rejected candidate, an
// unparseable candidate, and any accept validated against a populated or
// thin baseline are all lowConfidence=false. Kept store-free so the
// selection is unit-testable without a database. Exact-rational (ADR-0003).
func selectGuardedVWAP1m(candidate timescale.Vwap1mRow, rows []timescale.Vwap1mRow) (served timescale.Vwap1mRow, rejected, lowConfidence bool) {
	candRat, ok := new(big.Rat).SetString(candidate.VWAP)
	if !ok {
		return candidate, false, false // unparseable candidate → can't judge, serve as-is
	}
	// Trailing baseline = combined-direction closed buckets STRICTLY older
	// than the candidate bucket and within [BaselineMaxAge] of it, kept
	// index-aligned with their rows so the guard's last-known-good index
	// maps straight back to a servable row.
	trailingRows := make([]timescale.Vwap1mRow, 0, len(rows))
	trailing := make([]*big.Rat, 0, len(rows))
	for i := range rows {
		if !rows[i].Bucket.Before(candidate.Bucket) || candidate.Bucket.Sub(rows[i].Bucket) > BaselineMaxAge {
			continue
		}
		trailingRows = append(trailingRows, rows[i])
		if v, ok := new(big.Rat).SetString(rows[i].VWAP); ok {
			trailing = append(trailing, v)
		} else {
			trailing = append(trailing, nil)
		}
	}
	accept, lkgIdx := aggregate.GuardServedVWAP(candRat, trailing)
	if accept {
		// An accept against an empty baseline is an unvalidated fail-open —
		// serve the value, but flag it low-confidence.
		return candidate, false, !aggregate.ServedBaselineValidated(trailing)
	}
	return trailingRows[lkgIdx], true, false
}
