// Package pricingguard is the serving-sanity guard shared by every raw prices_1m closed-bucket
// serving path across the API and aggregator binaries.
//
// Those paths read a bare CAGG bucket that BYPASSES the orchestrator's sigma-outlier filter,
// min-USD-volume gate and freeze protection, so one manipulated print would be served verbatim.
// The consumers: /v1/price (StorePriceReader.LatestPrice), /v1/assets/{slug}
// (GlobalPriceReader.LatestVWAP), the price-alert evaluator (priceAlertVWAPReader.LatestVWAP),
// /v1/price/at + /v1/price/changes (storePriceAtReader.PriceAt, via [GuardServedVWAP1mAt]), the
// SEP-40 prices() series (StorePriceReader.RecentClosedSnapshots, via [GuardServedVWAP1mSeries])
// and change_24h_pct's 24h-ago anchor (storeChange24hReader.USDPrice24hAgo, via
// [GuardServedVWAP1mAt]). TestRawPrices1mReadersPassTheGuard (cmd/stellarindex-api) fails on a raw
// prices_1m read with no guard entry point or missing from this list.
//
// It wraps the pure exact-rational band decision ([robuststat.GuardServedVWAP]) and sits above
// storage because timescale imports aggregate; the reverse would cycle.
package pricingguard

import (
	"context"
	"log/slog"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/robuststat"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SampleFetch is how many recent CLOSED combined-direction 1m buckets the guard pulls for a robust trailing baseline.
const SampleFetch = 40

// BaselineMaxAge is the furthest before the judged bucket a trailing bucket may start and still count toward its baseline.
const BaselineMaxAge = 24 * time.Hour

// TrailingReader is the storage seam the guard needs (*timescale.Store satisfies it): the trailing combined-direction closed-bucket fetch, an interface so wiring tests need no database.
type TrailingReader interface {
	RecentClosedVWAP1mCombined(ctx context.Context, p canonical.Pair, limit int) ([]timescale.Vwap1mRow, error)
}

// TrailingAtReader is the storage seam of the POINT-IN-TIME guard: the trailing baseline anchored at the candidate bucket rather than at now.
type TrailingAtReader interface {
	ClosedVWAP1mCombinedBefore(ctx context.Context, p canonical.Pair, before time.Time, limit int) ([]timescale.Vwap1mRow, error)
}

// GuardServedVWAP1mConfidence is the serving-sanity guard for every raw latest-bucket prices_1m path; it never errors and fails open, serving the candidate on doubt rather than 404 a pair with data.
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

// GuardServedVWAP1mAt is [GuardServedVWAP1mConfidence] for the POINT-IN-TIME path, for prices_1m answers ONLY; a rejected candidate is replaced only by a clean bucket closing within maxStaleness of ts, else ok=false, never a rejected or too-stale value.
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

// SelectGuardedVWAP1mAt is the pure, store-free half of [GuardServedVWAP1mAt].
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
	// The last-known-good bucket is older than the rejected candidate, so it must re-clear the
	// caller's staleness bound before it can stand in.
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

// GuardServedVWAP1mSeries is the guard for a raw prices_1m SERIES (the SEP-40 prices() read).
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

// SelectGuardedVWAP1m is the pure, store-free half of [GuardServedVWAP1mConfidence]: given the candidate and recent closed buckets (newest-first) it returns the row to serve and whether the candidate was rejected.
func SelectGuardedVWAP1m(candidate timescale.Vwap1mRow, rows []timescale.Vwap1mRow) (served timescale.Vwap1mRow, rejected bool) {
	served, rejected, _ = selectGuardedVWAP1m(candidate, rows)
	return served, rejected
}

// selectGuardedVWAP1m is [SelectGuardedVWAP1m] plus the low-confidence signal, true only when the
// candidate is ACCEPTED against an empty baseline ([robuststat.ServedBaselineValidated] false:
// nothing within [BaselineMaxAge] before it). Rejected or unparseable candidates are false.
func selectGuardedVWAP1m(candidate timescale.Vwap1mRow, rows []timescale.Vwap1mRow) (served timescale.Vwap1mRow, rejected, lowConfidence bool) {
	candRat, ok := new(big.Rat).SetString(candidate.VWAP)
	if !ok {
		return candidate, false, false // unparseable candidate → can't judge, serve as-is
	}
	// Baseline = closed buckets STRICTLY older than the candidate and within [BaselineMaxAge],
	// kept index-aligned with their rows so the last-known-good index maps back to a servable row.
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
	accept, lkgIdx := robuststat.GuardServedVWAP(candRat, trailing)
	if accept {
		// Accepting against an empty baseline is an unvalidated fail-open: serve, flag low-confidence.
		return candidate, false, !robuststat.ServedBaselineValidated(trailing)
	}
	return trailingRows[lkgIdx], true, false
}
