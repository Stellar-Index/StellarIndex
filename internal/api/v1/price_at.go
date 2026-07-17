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
	// fabricating continuity across a dead-market gap.
	PriceAt(ctx context.Context, pair canonical.Pair, ts time.Time, maxStaleness time.Duration) (value string, observedAt time.Time, resolutionSeconds int, err error)
}

// ErrPriceAtUnavailable is the sentinel a PriceAtReader returns when
// no closed bucket exists at-or-before the requested instant (pair
// younger than ts, or ts predates recorded history).
var ErrPriceAtUnavailable = errors.New("api: no closed bucket at or before requested timestamp")

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

	if snap, found := s.lookupPriceAt(r.Context(), asset, quote, ts); found {
		writeJSON(w, snap, Flags{})
		return
	}
	if snap, found := s.lookupPriceAtStablecoinFallback(r.Context(), asset, quote, ts); found {
		writeJSON(w, snap, Flags{Triangulated: true})
		return
	}
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/price-not-found",
		"No price at requested time", http.StatusNotFound,
		"no closed bucket within "+priceAtMaxLookback.String()+" before "+ts.Format(time.RFC3339)+" for "+asset.String()+" / "+quote.String())
}

// lookupPriceAt walks the alias combinations (F-1340, same as every
// other price surface) and returns the first in-lookback bucket.
func (s *Server) lookupPriceAt(ctx context.Context, asset, quote canonical.Asset, ts time.Time) (PriceSnapshot, bool) {
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
				ObservedAt:    bucketAt,
				WindowSeconds: resSec,
			}, true
		}
	}
	return PriceSnapshot{}, false
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
// contract as tryStablecoinFiatProxy on /v1/price.
func (s *Server) lookupPriceAtStablecoinFallback(
	ctx context.Context, asset, quote canonical.Asset, ts time.Time,
) (PriceSnapshot, bool) {
	if quote.Type != canonical.AssetFiat || quote.Code != "USD" {
		return PriceSnapshot{}, false
	}
	for _, peg := range s.usdPeggedClassics {
		if peg.Equal(asset) {
			continue
		}
		snap, found := s.lookupPriceAt(ctx, asset, peg, ts)
		if !found {
			continue
		}
		snap.AssetID = asset.String()
		snap.Quote = quote.String()
		return snap, true
	}
	return PriceSnapshot{}, false
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
