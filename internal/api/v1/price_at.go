// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// PriceAtReader returns the closed VWAP bucket at-or-before a
// historical instant for a pair. Production wiring is a thin adapter
// around timescale.Store.ClosedVWAP1mAtOrBefore (the same anchor the
// change_24h_pct path uses, generalized to any timestamp).
type PriceAtReader interface {
	// PriceAt returns (vwap decimal string, the bucket's close time,
	// error). ErrPriceAtUnavailable when the pair has no closed
	// bucket at or before ts.
	PriceAt(ctx context.Context, pair canonical.Pair, ts time.Time) (string, time.Time, error)
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
// the closed 1-minute VWAP bucket at-or-before ts; observed_at is
// the BUCKET's time, never ts, so callers see exactly how far the
// nearest observation was.
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
			value, bucketAt, lookErr := s.priceAt.PriceAt(ctx, pair, ts)
			if lookErr != nil {
				continue
			}
			if ts.Sub(bucketAt) > priceAtMaxLookback {
				// The nearest observation is older than the honesty
				// cap — refusing beats fabricating continuity.
				continue
			}
			return PriceSnapshot{
				AssetID:       asset.String(),
				Quote:         quote.String(),
				Price:         value,
				PriceType:     "vwap",
				ObservedAt:    bucketAt,
				WindowSeconds: 60,
			}, true
		}
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
