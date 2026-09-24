// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// PriceAtReader returns the closed VWAP bucket at-or-before a
// historical instant for a pair. Production wiring is a thin adapter
// around timescale.Store.ClosedVWAPAtOrBefore, which picks the finest
// CAGG resolution (prices_1m for recent instants, coarser bars for
// older ones — prices_1d spans to 2015) whose nearest at-or-before
// bucket is within the honesty cap, and reports which it used.
//
// The same reader backs GET /v1/price/changes: each horizon's
// reference price is a PriceAt call at ts=now-horizon, which is why
// the staleness tolerance is a parameter rather than a fixed cap
// (/v1/price/at passes [priceAtMaxLookback]; the changes endpoint
// passes a per-horizon tolerance).
type PriceAtReader interface {
	// PriceAt returns (vwap decimal string, the bucket's CLOSE time,
	// the resolution of the CAGG that served it in seconds, error).
	// resolutionSeconds tells the caller the true window the answer
	// spans (60 for a 1-minute bar, 86400 for a daily bar) so it
	// labels window_seconds honestly. maxStaleness caps how far before
	// ts the nearest bucket may close before the answer is refused —
	// past it the reader returns ErrPriceAtUnavailable rather than
	// fabricating continuity across a dead-market gap. A bucket that
	// exists but is refused returns ErrPriceWithheld or ErrPriceAtGuarded.
	PriceAt(ctx context.Context, pair canonical.Pair, ts time.Time, maxStaleness time.Duration) (value string, observedAt time.Time, resolutionSeconds int, err error)
}

// ErrPriceAtUnavailable is the sentinel a PriceAtReader returns when no
// closed bucket exists within maxStaleness at-or-before the requested
// instant (pair younger than ts, ts predates recorded history, or a
// dead-market gap). It never means a bucket exists and was refused.
var ErrPriceAtUnavailable = errors.New("api: no closed bucket at or before requested timestamp")

// ErrPriceAtGuarded is the sentinel a PriceAtReader returns when a
// closed bucket exists but the serving-sanity guard refused it (a gross
// outlier with no clean bucket inside maxStaleness, or no prior bucket to
// validate it against). It matches errors.Is(err, ErrPriceWithheld), so
// every withheld branch treats it as withheld, and carries
// [PriceWithheldManipulationGuard] for the problem wording.
var ErrPriceAtGuarded = newPriceWithheld(PriceWithheldManipulationGuard)

// priceAtMaxLookback caps the gap the endpoint tolerates between the
// requested instant and the bucket actually found. Without a cap, a
// ts inside a pair's multi-week quiet gap would silently serve a
// weeks-old price as if it were "the price at ts". One day covers
// every real market-data gap (CEX outages, sparse historical candles
// synthesised at 1h) while refusing to fabricate continuity across
// dead markets — the RFP's transparency intent applied backwards in
// time.
const priceAtMaxLookback = 24 * time.Hour

// handlePriceAt serves GET /v1/price/at?asset=&quote=&ts=RFC3339 —
// the point-in-time price for portfolio cost-basis / PnL / tax
// tooling (wallet-builder accommodation, board #46). The answer is
// the closed VWAP bucket at-or-before ts from the finest CAGG
// resolution that covers it (prices_1m for recent instants, coarser
// bars back to prices_1d for older ones); observed_at is the BUCKET's
// close time, never ts, and window_seconds reports the resolution
// used — so callers see exactly how far the nearest observation was
// and at what granularity.
func (s *Server) handlePriceAt(w http.ResponseWriter, r *http.Request) {
	if s.priceAt == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Point-in-time price serving not configured", http.StatusServiceUnavailable,
			"this deployment has no PriceAtReader wired")
		return
	}
	rawAsset, ok := parsePriceAssetParam(w, r)
	if !ok {
		return
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest, err.Error())
		return
	}
	quote, ok := parsePriceQuoteParam(w, r)
	if !ok {
		return
	}
	if asset.Equal(quote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-price",
			"Asset and quote are the same", http.StatusBadRequest,
			"price of an asset in itself is always 1; parameters must differ")
		return
	}
	ts, ok := parsePriceAtTS(w, r)
	if !ok {
		return
	}

	// Per-request DB ceiling (RLT-455): the alias walk tries up to 4
	// combinations directly, then every configured USD peg on the
	// stablecoin-fallback path, each a ClosedVWAPAtOrBefore CAGG
	// lookup. Without a bounded context a slow run holds its pool
	// connection until the client gives up. 8s matches the sibling
	// single-shot read endpoints (oracle.go, vwap.go, ohlc.go) and
	// fires before the blanket request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	snap, flags, found, withheld, err := s.resolvePriceAt(ctx, asset, quote, ts)
	if err != nil {
		s.writePriceAtReadFailure(ctx, w, r, "/v1/price/at", err)
		return
	}
	if found {
		writeJSON(w, snap, flags)
		return
	}
	if withheld != nil {
		// At least one orientation HAS a closed bucket at-or-before ts
		// and a serving gate refused to publish it — the distinct 404
		// type so integrators can branch, same contract as /v1/price and
		// /v1/price/changes (see ErrPriceWithheld), worded for the gate
		// that fired.
		writePriceWithheldProblem(w, r, asset, quote, priceWithheldReason(withheld))
		return
	}
	// The 404 below carries the same ambiguity the series surfaces have
	// in their empty arrays: "no bucket within the lookback" is what a
	// dead market and an instant predating the held history both look
	// like. `ts` is the exclusive end of a point request — the daily
	// bucket that STARTS at the floor has not closed at that instant, so
	// an instant exactly at the floor is still outside coverage. The
	// floor is measured over the pair AND the pegs the fallback above
	// retried ([Server.priceAtCoverageSet]), since those are the buckets
	// this lookup can answer from.
	var (
		coverageFrom *time.Time
		outside      bool
	)
	if pair, pairErr := canonical.NewPair(asset, quote); pairErr == nil {
		coverageFrom, outside = s.coverageAnnotation(ctx, s.priceAtCoverageSet(pair), ts)
	}
	writeProblemCoverage(w, r,
		"https://api.stellarindex.io/errors/price-not-found",
		"No price at requested time", http.StatusNotFound,
		"no closed bucket within "+priceAtMaxLookback.String()+" before "+ts.Format(time.RFC3339)+" for "+asset.String()+" / "+quote.String(),
		coverageFrom, outside)
}

// resolvePriceAt is the direct alias walk, then the stablecoin
// fallback (flagged triangulated). withheld is the first withheld-class
// refusal either stage saw; err is a reader failure that ended the walk.
func (s *Server) resolvePriceAt(
	ctx context.Context, asset, quote canonical.Asset, ts time.Time,
) (PriceSnapshot, Flags, bool, error, error) {
	snap, found, withheld, err := s.lookupPriceAt(ctx, asset, quote, ts)
	if err != nil || found {
		return snap, Flags{}, found, nil, err
	}
	fbSnap, fbFound, fbWithheld, err := s.lookupPriceAtStablecoinFallback(ctx, asset, quote, ts)
	if err != nil || fbFound {
		return fbSnap, Flags{Triangulated: true}, fbFound, nil, err
	}
	if withheld == nil {
		withheld = fbWithheld
	}
	return PriceSnapshot{}, Flags{}, false, withheld, nil
}

// lookupPriceAt walks the alias combinations (F-1340, same as every
// other price surface) and returns the first in-lookback bucket.
// withheld is the first ErrPriceWithheld-class error any orientation
// returned (a substance/scam gate or the serving-sanity guard refused an
// existing bucket), nil when none did — the caller needs it to choose the
// correct 404 type and wording once every orientation (and, via
// lookupPriceAtStablecoinFallback, every peg) is exhausted. err is a
// reader failure ([isPriceAtMiss] false): it ends the walk, because a
// later orientation's hit would stand in for an unknown answer.
func (s *Server) lookupPriceAt(ctx context.Context, asset, quote canonical.Asset, ts time.Time) (PriceSnapshot, bool, error, error) {
	var withheld error
	for _, a := range assetAliases(asset) {
		for _, q := range assetAliases(quote) {
			if a.Equal(q) {
				continue
			}
			pair, pairErr := canonical.NewPair(a, q)
			if pairErr != nil {
				continue
			}
			value, bucketAt, resSec, lookErr := s.priceAt.PriceAt(ctx, pair, ts, priceAtMaxLookback)
			if lookErr != nil {
				if failErr := notePriceAtMiss(&withheld, lookErr); failErr != nil {
					return PriceSnapshot{}, false, nil, failErr
				}
				continue
			}
			if ts.Sub(bucketAt) > priceAtMaxLookback {
				// The nearest observation is older than the honesty
				// cap — refusing beats fabricating continuity.
				continue
			}
			// dex-nonstandard-decimals forward normalization (M2): PriceAt
			// returns the RAW prices_<n> ratio for the ACTUAL traded pair
			// (a/q — which, after the lookupPriceAtStablecoinFallback retry,
			// can be asset/<peg>). Resolve decimals against those legs, not the
			// requested asset/quote. This is the sub-chokepoint for /v1/price/at
			// (the fallback also routes through here). Byte-identical no-op for
			// a pair with no confirmed non-7-decimals leg.
			value = s.normalizeRawRatioString(value, pair.Base, pair.Quote)
			return PriceSnapshot{
				AssetID:       asset.String(),
				Quote:         quote.String(),
				Price:         value,
				PriceType:     "vwap",
				ObservedAt:    WireTime(bucketAt),
				WindowSeconds: resSec,
			}, true, nil, nil
		}
	}
	return PriceSnapshot{}, false, withheld, nil
}

// lookupPriceAtStablecoinFallback is the CAGG sibling of the
// raw-trades stablecoin fallback (vwap.go's
// tradesInRangeWithStablecoinFallback / chart.go's
// chartStablecoinFallback) — the deferred half of the #1217 family.
// The 1m VWAP CAGG keys buckets by the REAL stored quote asset, so a
// historical X/fiat:USD lookup misses unless something traded
// directly in fiat:USD at that instant. When the literal + alias
// walk found nothing and the quote is fiat:USD, retry each
// operator-declared USD-pegged classic in priority order; the first
// in-lookback bucket wins. The snapshot echoes the REQUESTED quote —
// flags.triangulated (stamped by the caller) marks the proxy, same
// contract as tryStablecoinFiatProxy on /v1/price. withheld carries
// [Server.lookupPriceAt]'s withheld signal up across every peg tried;
// a reader failure on any peg ends the walk as err.
func (s *Server) lookupPriceAtStablecoinFallback(
	ctx context.Context, asset, quote canonical.Asset, ts time.Time,
) (PriceSnapshot, bool, error, error) {
	var withheld error
	for _, proxied := range s.priceAtUSDPegPairs(asset, quote) {
		snap, found, w, err := s.lookupPriceAt(ctx, proxied.Base, proxied.Quote, ts)
		if err != nil {
			return PriceSnapshot{}, false, nil, err
		}
		if withheld == nil {
			withheld = w
		}
		if !found {
			continue
		}
		snap.AssetID = asset.String()
		snap.Quote = quote.String()
		return snap, true, nil, nil
	}
	return PriceSnapshot{}, false, withheld, nil
}

// isPriceAtMiss reports whether a [PriceAtReader] error is an answer —
// no bucket in range, or a bucket a serving gate refused — rather than a
// failed read. Only a miss lets a walk move on to the next orientation.
func isPriceAtMiss(err error) bool {
	return errors.Is(err, ErrPriceAtUnavailable) || errors.Is(err, ErrPriceWithheld)
}

// notePriceAtMiss folds a [PriceAtReader] miss into a walk's sticky
// withheld verdict (first withheld-class error wins) and returns nil, or
// returns err unchanged when it is a read failure the walk must stop on.
func notePriceAtMiss(withheld *error, err error) error {
	if !isPriceAtMiss(err) {
		return err
	}
	if *withheld == nil && errors.Is(err, ErrPriceWithheld) {
		*withheld = err
	}
	return nil
}

// writePriceAtReadFailure answers a [PriceAtReader] read failure on the
// point-in-time routes. Never the 404 / available:false a miss gets: a
// timed-out or failed read says nothing about whether a bucket exists.
func (s *Server) writePriceAtReadFailure(callCtx context.Context, w http.ResponseWriter, r *http.Request, route string, err error) {
	if clientAborted(r, err) {
		return
	}
	if handlerTimedOut(callCtx, err) || transientStorageErr(err) {
		s.logger.Warn("point-in-time price read unavailable", "route", route, "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Point-in-time price read unavailable", http.StatusServiceUnavailable,
			"the price store did not answer in time or hit a transient error; retry shortly. This is not a finding that no price exists.")
		return
	}
	s.logger.Error("point-in-time price read failed", "route", route, "err", err)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/internal",
		"Internal error", http.StatusInternalServerError, "")
}

// priceAtUSDPegPairs is the ordered proxy list
// [Server.lookupPriceAtStablecoinFallback] walks: for a `fiat:USD`
// quote, the asset against each operator-declared USD-pegged classic
// in priority order, skipping a peg that is the asset itself; empty for
// any other quote. It is a separate function so the coverage floor
// ([Server.priceAtCoverageSet]) enumerates the SAME pairs the lookup
// retries, from one definition.
func (s *Server) priceAtUSDPegPairs(asset, quote canonical.Asset) []canonical.Pair {
	if quote.Type != canonical.AssetFiat || quote.Code != "USD" {
		return nil
	}
	out := make([]canonical.Pair, 0, len(s.usdPeggedClassics))
	for _, peg := range s.usdPeggedClassics {
		// A peg asked for under any of its spellings — the classic id or
		// its SAC wrapper — is not a market against itself.
		if sameAsset(peg, asset) {
			continue
		}
		proxied, err := canonical.NewPair(asset, peg)
		if err != nil {
			continue
		}
		out = append(out, proxied)
	}
	return out
}

// parsePriceAtTS validates the required historical `ts` param.
// ok=false means a 400 problem+json was written.
func parsePriceAtTS(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	rawTS := r.URL.Query().Get("ts")
	if rawTS == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-ts",
			"Missing ts parameter", http.StatusBadRequest,
			"ts is required, RFC 3339 (e.g. 2024-06-01T12:00:00Z); for the current price use /v1/price or /v1/price/tip")
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, rawTS)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-ts",
			"Invalid ts parameter", http.StatusBadRequest, err.Error())
		return time.Time{}, false
	}
	if ts.After(time.Now().UTC()) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-ts",
			"ts is in the future", http.StatusBadRequest,
			"point-in-time lookups are historical; for the current price use /v1/price or /v1/price/tip")
		return time.Time{}, false
	}
	return ts, true
}
