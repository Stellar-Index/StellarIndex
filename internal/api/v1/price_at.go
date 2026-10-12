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

// PriceAtReader returns the closed VWAP bucket at-or-before an instant, picking the finest CAGG within the honesty cap; staleness tolerance is a parameter because /v1/price/changes shares it.
type PriceAtReader interface {
	// PriceAt returns (vwap decimal string, the bucket's CLOSE time, the serving CAGG resolution in
	// seconds, error); the resolution lets the caller label window_seconds honestly. Past
	// maxStaleness the reader returns ErrPriceAtUnavailable rather than fabricating continuity
	// across a dead-market gap; an existing but refused bucket returns ErrPriceWithheld or
	// ErrPriceAtGuarded.
	PriceAt(ctx context.Context, pair canonical.Pair, ts time.Time, maxStaleness time.Duration) (value string, observedAt time.Time, resolutionSeconds int, err error)
}

// ErrPriceAtUnavailable means no closed bucket exists within maxStaleness at-or-before the instant; it never means a bucket exists and was refused.
var ErrPriceAtUnavailable = errors.New("api: no closed bucket at or before requested timestamp")

// ErrPriceAtGuarded means a closed bucket exists but the serving-sanity guard refused it; it matches errors.Is(err, ErrPriceWithheld) with [PriceWithheldManipulationGuard].
var ErrPriceAtGuarded = newPriceWithheld(PriceWithheldManipulationGuard)

// priceAtMaxLookback caps the gap tolerated between the requested instant and the bucket found.
// Without it a ts inside a multi-week quiet gap would serve a weeks-old price as "the price at
// ts". One day covers real data gaps while refusing to fabricate continuity across dead markets.
const priceAtMaxLookback = 24 * time.Hour

// handlePriceAt serves GET /v1/price/at?asset=&quote=&ts=RFC3339, the point-in-time price for
// cost-basis / PnL tooling. The answer is the closed VWAP bucket at-or-before ts from the finest
// CAGG that covers it; observed_at is the BUCKET's close time, never ts, and window_seconds is the
// resolution used, so callers see how far and how coarse the nearest observation was.
func (s *Server) handlePriceAt(w http.ResponseWriter, r *http.Request) {
	if s.PriceAt == nil {
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

	// Per-request DB ceiling: up to 4 alias combinations plus every USD peg on the fallback path,
	// each a CAGG lookup; unbounded, a slow run holds its pool connection. 8s matches the sibling
	// read endpoints and fires before the blanket request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	offCtx, off := WithThinAdmission(ctx, asset, quote, false)
	snap, flags, found, withheld, err := s.resolvePriceAt(offCtx, asset, quote, ts)
	if err == nil && !found && withheld != nil && includeThinRequested(r) && off.ThinWithheld(priceWithheldReason(withheld)) {
		onCtx, on := WithThinAdmission(ctx, asset, quote, true)
		thinSnap, thinFlags, thinFound, _, thinErr := s.resolvePriceAt(onCtx, asset, quote, ts)
		switch {
		case thinErr != nil:
			s.logger.Warn("include_thin second pass failed; serving the withheld verdict",
				"err", thinErr, "asset", asset.String(), "quote", quote.String())
		case thinFound:
			if on.Admitted() && thinSnap.Price != "" {
				thinFlags.ThinMarket = true
				thinSnap.Substance = substanceEvidenceWire(on.Evidence())
			}
			writeJSON(w, thinSnap, thinFlags)
			return
		}
	}
	if err != nil {
		s.writePriceAtReadFailure(ctx, w, r, "/v1/price/at", err)
		return
	}
	if found {
		writeJSON(w, snap, flags)
		return
	}
	if withheld != nil {
		// An orientation has a closed bucket at-or-before ts but a serving gate refused it: a
		// distinct 404 type integrators can branch on (see ErrPriceWithheld), worded for the gate.
		reason := priceWithheldReason(withheld)
		writePriceWithheldProblemEvidence(w, r, asset, quote, reason, thinEvidenceFor(off, reason))
		return
	}
	// "No bucket within the lookback" looks the same for a dead market and an instant predating
	// held history. `ts` is the exclusive end of a point request: the daily bucket that STARTS at
	// the floor has not closed yet, so an instant exactly at the floor is outside coverage. The
	// floor spans the pair AND the pegs the fallback retried ([Server.priceAtCoverageSet]).
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
		coverageFrom, outside, nil, "")
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
		return fbSnap, Flags{Triangulated: true, ProxyDeviation: fbFound && s.proxyDeviation(ctx, ts)}, fbFound, nil, err
	}
	if withheld == nil {
		withheld = fbWithheld
	}
	return PriceSnapshot{}, Flags{}, false, withheld, nil
}

// lookupPriceAt walks the alias combinations and returns the first in-lookback bucket. withheld is
// the first ErrPriceWithheld-class error seen, so the caller can choose the right 404 once every
// orientation and peg is exhausted. err is a reader failure ([isPriceAtMiss] false); it ends the
// walk because a later orientation's hit would stand in for an unknown answer.
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
			value, bucketAt, resSec, lookErr := s.PriceAt.PriceAt(ctx, pair, ts, priceAtMaxLookback)
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
			// PriceAt returns the RAW ratio for the ACTUAL traded pair (a/q, which after the
			// stablecoin fallback can be asset/<peg>), so resolve decimals against those legs,
			// not the requested asset/quote. The fallback also routes through here.
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

// lookupPriceAtStablecoinFallback is the CAGG sibling of the raw-trades stablecoin fallback.
// CAGG buckets key on the REAL stored quote asset, so a historical X/fiat:USD lookup misses unless
// something traded in fiat:USD then. When the alias walk found nothing for a fiat:USD quote, it
// retries each USD-pegged classic in priority order; the first in-lookback bucket wins. The
// snapshot echoes the REQUESTED quote and flags.triangulated (set by the caller) marks the proxy.
// withheld carries the withheld signal across every peg; a reader failure on any peg ends the walk.
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

// priceAtUSDPegPairs is the ordered proxy list [Server.lookupPriceAtStablecoinFallback] walks: for
// a `fiat:USD` quote, the asset against each USD-pegged classic (skipping the asset itself); empty
// for any other quote. Kept separate so the coverage floor enumerates the SAME pairs the lookup
// retries.
func (s *Server) priceAtUSDPegPairs(asset, quote canonical.Asset) []canonical.Pair {
	if quote.Type != canonical.AssetFiat || quote.Code != "USD" {
		return nil
	}
	out := make([]canonical.Pair, 0, len(s.USDPeggedClassics))
	for _, peg := range s.USDPeggedClassics {
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
