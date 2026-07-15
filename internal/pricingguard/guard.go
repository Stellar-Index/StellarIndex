// Package pricingguard is the serving-sanity guard shared by every raw
// prices_1m closed-bucket serving path across the API and aggregator
// binaries (adversarial-review HIGH).
//
// Several serving paths read the most-recent CLOSED prices_1m bucket
// directly via [timescale.Store.LatestClosedVWAP1mForPair] — a bare
// Σ(quote)/Σ(base) continuous-aggregate bucket that BYPASSES the
// orchestrator's σ-outlier filter, its min-USD-volume gate, and freeze
// value-protection (those guard the ORCHESTRATOR path that writes the
// filtered VWAP to Redis, which the CAGG does not touch). So each such
// path carries the identical unfiltered fat-finger / manipulation vector:
// a single manipulated print in the served minute would otherwise be
// served verbatim, with stale=false and no volume floor. The three known
// raw-bucket consumers are:
//
//   - /v1/price               (cmd/stellarindex-api storePriceReader.LatestPrice)
//   - /v1/assets/{slug}        (cmd/stellarindex-api globalPriceReader.LatestVWAP, GlobalAssetView headline)
//   - the price-alert evaluator (cmd/stellarindex-aggregator priceAlertVWAPReader.LatestVWAP)
//
// This package hosts the WIRING that turns the pure robust-band decision
// ([aggregate.GuardServedVWAP], ADR-0003 exact-rational) into a servable
// row: it fetches the trailing baseline and, on a gross deviation, swaps
// the candidate for the newest clean last-known-good bucket. It lives
// ABOVE the storage tier (it depends on the store) and above the pure
// aggregate decision (which cannot depend on storage — timescale already
// imports aggregate, so the reverse edge would cycle). A dedicated package
// lets BOTH binaries import it without duplicating the glue.
package pricingguard

import (
	"context"
	"log/slog"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SampleFetch is how many recent CLOSED combined-direction 1m buckets the
// serving-sanity guard pulls to build a robust trailing baseline for the
// latest bucket ([aggregate.GuardServedVWAP]). 40 is a few tens of minutes
// for an active pair — enough to clear the guard's minimum-sample floor
// while staying a cheap, index-driven LIMIT-N read (only ever run for a
// pair already confirmed populated).
const SampleFetch = 40

// TrailingReader is the storage seam the guard needs: the trailing
// combined-direction closed-bucket fetch. *timescale.Store satisfies it;
// keeping it an interface makes the wiring unit-testable without a
// database.
type TrailingReader interface {
	RecentClosedVWAP1mCombined(ctx context.Context, p canonical.Pair, limit int) ([]timescale.Vwap1mRow, error)
}

// GuardServedVWAP1m is the serving-sanity guard shared by every raw
// prices_1m serving path (see the package doc). Given the latest CLOSED
// bucket (`candidate`) it returns the row to actually serve:
//   - the candidate unchanged, when it is robust-sane against the pair's
//     recent trailing closed buckets, or when there is no baseline / the
//     trailing fetch failed (fail-open — favour serving a real price);
//   - the newest trailing closed bucket that IS within the robust band
//     (last-known-good), when the candidate is grossly off — a fat-finger
//     / manipulation print the raw CAGG would otherwise serve unfiltered.
//
// The decision math is exact-rational ([aggregate.GuardServedVWAP],
// ADR-0003 — no float64 in the value path). This never errors: on any
// doubt it serves the candidate rather than 404 a pair that has data. A
// nil logger disables the guard's warn logging (the decision is
// unaffected).
func GuardServedVWAP1m(
	ctx context.Context,
	store TrailingReader,
	logger *slog.Logger,
	pair canonical.Pair,
	candidate timescale.Vwap1mRow,
) timescale.Vwap1mRow {
	rows, err := store.RecentClosedVWAP1mCombined(ctx, pair, SampleFetch)
	if err != nil {
		if logger != nil {
			logger.Warn("served-vwap guard: trailing fetch failed — serving candidate unguarded",
				"pair", pair.String(), "err", err)
		}
		return candidate // fail-open
	}
	served, rejected := SelectGuardedVWAP1m(candidate, rows)
	if rejected && logger != nil {
		logger.Warn("served-vwap guard: candidate bucket rejected as outlier — serving last-known-good",
			"pair", pair.String(),
			"candidate_bucket", candidate.Bucket,
			"candidate_vwap", candidate.VWAP,
			"served_bucket", served.Bucket,
			"served_vwap", served.VWAP)
	}
	return served
}

// SelectGuardedVWAP1m is the pure decision half of [GuardServedVWAP1m]:
// given the candidate bucket and the recent combined-direction closed
// buckets (`rows`, newest-first, as returned by
// [timescale.Store.RecentClosedVWAP1mCombined]), it returns the row to
// serve and whether the candidate was rejected. Kept store-free so the
// selection + index-alignment logic is unit-testable without a database.
// Exact-rational throughout (ADR-0003).
func SelectGuardedVWAP1m(candidate timescale.Vwap1mRow, rows []timescale.Vwap1mRow) (served timescale.Vwap1mRow, rejected bool) {
	candRat, ok := new(big.Rat).SetString(candidate.VWAP)
	if !ok {
		return candidate, false // unparseable candidate → can't judge, serve as-is
	}
	// Trailing baseline = combined-direction closed buckets STRICTLY older
	// than the candidate bucket, kept index-aligned with their rows so the
	// guard's last-known-good index maps straight back to a servable row.
	trailingRows := make([]timescale.Vwap1mRow, 0, len(rows))
	trailing := make([]*big.Rat, 0, len(rows))
	for i := range rows {
		if !rows[i].Bucket.Before(candidate.Bucket) {
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
		return candidate, false // byte-identical to the pre-guard served value
	}
	return trailingRows[lkgIdx], true
}
