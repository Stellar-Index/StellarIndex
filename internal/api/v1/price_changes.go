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

// PriceChangeHorizon is one trailing-window delta on /v1/price/changes; nil pointer fields mean that horizon is unavailable, never a whole-call error.
type PriceChangeHorizon struct {
	// ChangePct is the signed percentage move of the current price vs
	// the reference price, two fractional digits with an explicit
	// leading "+" on gains (e.g. "+3.62", "-1.04", "0.00"). Same
	// format as /v1/assets/{id}.change_24h_pct. Null when unavailable.
	ChangePct *string `json:"change_pct"`
	// ReferencePrice is the closed VWAP at-or-before now-horizon, a
	// decimal string (ADR-0003). Null when unavailable.
	ReferencePrice *string `json:"reference_price"`
	// ReferenceAt is the CLOSE time of the reference bucket (RFC 3339),
	// never the exact horizon instant — so callers see how far the
	// nearest observation was. Null when unavailable.
	ReferenceAt *string `json:"reference_at"`
	// Resolution is the CAGG that served the reference bucket
	// ("1m" | "15m" | "1h" | "4h" | "1d"). Null when unavailable.
	Resolution *string `json:"resolution"`
	// Available is the per-horizon flag: false means the horizon has no
	// reference price (all the pointer fields are null).
	Available bool `json:"available"`
	// Withheld is true when the reference bucket exists but a serving gate refused it. It
	// separates "withheld" from "no data that far back"; always false when Available is true.
	Withheld bool `json:"withheld"`
	// ThinMarket: the reference bucket was served under `include_thin=true`
	// from a market below the substance floor.
	ThinMarket bool `json:"thin_market,omitempty"`
}

// PriceChanges is the GET /v1/price/changes payload: the current closed price plus signed change over 1h/24h/7d/30d via the same reader as /v1/price/at.
type PriceChanges struct {
	AssetID          string `json:"asset_id"`
	Quote            string `json:"quote"`
	CurrentPrice     string `json:"current_price"`
	CurrentPriceType string `json:"current_price_type"`
	// ObservedAt is the CLOSE time of the current-price bucket, and
	// Resolution the CAGG that served it — the same honesty labels the
	// horizons carry, applied to the anchor.
	ObservedAt string `json:"observed_at"`
	Resolution string `json:"resolution"`

	H1  PriceChangeHorizon `json:"1h"`
	H24 PriceChangeHorizon `json:"24h"`
	D7  PriceChangeHorizon `json:"7d"`
	D30 PriceChangeHorizon `json:"30d"`

	// Substance is the measurement behind a CURRENT bucket served under
	// `include_thin=true`; a horizon carries only its thin_market flag.
	Substance *SubstanceEvidence `json:"substance,omitempty"`
}

// priceChangesCurrentStaleness bounds how old the "current" bucket may be before the pair has no current price (404); it matches
// [priceAtMaxLookback].
const priceChangesCurrentStaleness = priceAtMaxLookback

// priceChangeHorizons are the trailing windows /v1/price/changes reports, ascending. Staleness tolerance is the horizon itself: a
// daily bar can answer a multi-week horizon, but a bucket more than one horizon-width stale means "no data that far back".
var priceChangeHorizons = []struct {
	label string
	dur   time.Duration
}{
	{"1h", time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
}

// handlePriceChanges serves GET /v1/price/changes?asset=&quote=: missing horizons are null with available=false, withheld=true when a
// serving gate refused the reference bucket.
// 404 only when the pair has no current price; 503/500 on a failed read, which is not evidence a bucket is missing.
func (s *Server) handlePriceChanges(w http.ResponseWriter, r *http.Request) {
	if s.PriceAt == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-unavailable",
			"Price-change serving not configured", http.StatusServiceUnavailable,
			"this deployment has no point-in-time price reader wired")
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
			"change of an asset against itself is always 0; parameters must differ")
		return
	}

	// Per-request DB ceiling: up to five sequential PriceAt reads, each walking aliases (and USD
	// pegs for a fiat:USD quote); unbounded, a slow run holds its pool connection. 8s matches the
	// sibling read endpoints and fires before the blanket request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	now := time.Now().UTC()
	cur, err := s.priceChangeCurrentThin(ctx, r, asset, quote, now)
	if err != nil {
		s.writePriceAtReadFailure(ctx, w, r, "/v1/price/changes", err)
		return
	}
	pair, current, withheld := cur.pair, cur.res, cur.withheld
	if !cur.found {
		if withheld != nil {
			// At least one orientation HAS a closed bucket and a serving
			// gate refused to publish it — the distinct 404 type so
			// integrators can branch, same contract as /v1/price and
			// /v1/price/tip (see ErrPriceWithheld).
			reason := priceWithheldReason(withheld)
			writePriceWithheldProblemEvidence(w, r, asset, quote, reason, thinEvidenceFor(cur.adm, reason))
			return
		}
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/price-not-found",
			"No current price for pair", http.StatusNotFound,
			"no closed bucket within "+priceChangesCurrentStaleness.String()+" for "+asset.String()+" / "+quote.String()+"; cannot anchor a change")
		return
	}

	resp := PriceChanges{
		AssetID:          asset.String(),
		Quote:            quote.String(),
		CurrentPrice:     current.value,
		CurrentPriceType: "vwap",
		ObservedAt:       current.observedAt.UTC().Format(time.RFC3339),
		Resolution:       resolutionLabel(current.resSec),
	}
	flags := Flags{Triangulated: cur.triangulated}
	flags.ProxyDeviation = cur.triangulated && s.proxyDeviation(ctx, now)
	if cur.adm.Admitted() {
		flags.ThinMarket = true
		resp.Substance = substanceEvidenceWire(cur.adm.Evidence())
	}
	horizons := []*PriceChangeHorizon{&resp.H1, &resp.H24, &resp.D7, &resp.D30}
	for i, h := range priceChangeHorizons {
		horizon, err := s.priceChangeHorizonThin(ctx, r, asset, quote, pair, current.value, now.Add(-h.dur), h.dur)
		if err != nil {
			s.writePriceAtReadFailure(ctx, w, r, "/v1/price/changes", err)
			return
		}
		flags.ThinMarket = flags.ThinMarket || horizon.ThinMarket
		*horizons[i] = horizon
	}

	writeJSON(w, resp, flags)
}

// priceChangeCurrent is the resolved anchor of /v1/price/changes and the
// [ThinAdmission] record of the pass that resolved it.
type priceChangeCurrent struct {
	pair         canonical.Pair
	res          priceAtResult
	triangulated bool
	found        bool
	withheld     error
	adm          *ThinAdmission
}

// priceChangeCurrentThin resolves the anchor with the default read and, only when that ended withheld on a measured thin leg and the
// request opted in, again with that leg admitted.
// A failed second pass serves the first pass's verdict: an optional retry never turns a 404 into a 5xx.
func (s *Server) priceChangeCurrentThin(ctx context.Context, r *http.Request, asset, quote canonical.Asset, now time.Time) (priceChangeCurrent, error) {
	offCtx, off := WithThinAdmission(ctx, asset, quote, false)
	cur, err := s.priceChangeCurrentPass(offCtx, asset, quote, now)
	cur.adm = off
	if err != nil || cur.found || cur.withheld == nil || !includeThinRequested(r) || !off.ThinWithheld(priceWithheldReason(cur.withheld)) {
		return cur, err
	}
	onCtx, on := WithThinAdmission(ctx, asset, quote, true)
	thin, err := s.priceChangeCurrentPass(onCtx, asset, quote, now)
	if err != nil {
		s.logger.Warn("include_thin second pass failed; serving the withheld verdict",
			"err", err, "asset", asset.String(), "quote", quote.String())
		return cur, nil
	}
	if !thin.found {
		return cur, nil
	}
	thin.adm = on
	return thin, nil
}

func (s *Server) priceChangeCurrentPass(ctx context.Context, asset, quote canonical.Asset, now time.Time) (priceChangeCurrent, error) {
	pair, res, triangulated, found, withheld, err := s.resolvePriceChangePair(ctx, asset, quote, now)
	return priceChangeCurrent{pair: pair, res: res, triangulated: triangulated, found: found, withheld: withheld}, err
}

// priceChangeHorizonThin is [Server.priceChangeHorizon] with the same two-pass rule, on the horizon's own record (each horizon is a
// point-in-time read with its own verdict).
func (s *Server) priceChangeHorizonThin(
	ctx context.Context, r *http.Request, asset, quote canonical.Asset, pair canonical.Pair,
	currentPrice string, target time.Time, tolerance time.Duration,
) (PriceChangeHorizon, error) {
	offCtx, off := WithThinAdmission(ctx, asset, quote, false)
	h, reason, err := s.priceChangeHorizon(offCtx, pair, currentPrice, target, tolerance)
	if err != nil || !includeThinRequested(r) || !off.ThinWithheld(reason) {
		return h, err
	}
	onCtx, on := WithThinAdmission(ctx, asset, quote, true)
	thin, _, err := s.priceChangeHorizon(onCtx, pair, currentPrice, target, tolerance)
	if err != nil {
		s.logger.Warn("include_thin second pass failed; serving the withheld verdict",
			"err", err, "asset", asset.String(), "quote", quote.String())
		return h, nil
	}
	if !thin.Available {
		return h, nil
	}
	thin.ThinMarket = on.Admitted()
	return thin, nil
}

// priceAtResult carries a single point-in-time reader hit.
type priceAtResult struct {
	value      string
	observedAt time.Time
	resSec     int
}

// resolvePriceChangePair finds the (base, quote) orientation with a current price so every horizon is measured against the SAME
// market, walking XLM aliases and, for fiat:USD, the USD-pegged classics (triangulated=true). found=false means no fresh current
// bucket; withheld (meaningful then) is the first ErrPriceWithheld-class error for the price-withheld 404. The last return is a
// reader failure.
func (s *Server) resolvePriceChangePair(
	ctx context.Context, asset, quote canonical.Asset, now time.Time,
) (canonical.Pair, priceAtResult, bool, bool, error, error) {
	pair, res, ok, withheld, err := s.currentPriceForAliases(ctx, asset, quote, now)
	if err != nil {
		return canonical.Pair{}, priceAtResult{}, false, false, nil, err
	}
	if ok {
		return pair, res, false, true, nil, nil
	}
	// Stablecoin fiat-proxy fallback: retry each USD peg. First hit
	// wins; the response still echoes the requested quote (fiat:USD)
	// and flags triangulated.
	if quote.Type == canonical.AssetFiat && quote.Code == "USD" {
		for _, peg := range s.USDPeggedClassics {
			// A peg asked for under any of its spellings — the classic
			// id or its SAC wrapper — is not a market against itself.
			if sameAsset(peg, asset) {
				continue
			}
			pair, res, ok, w, err := s.currentPriceForAliases(ctx, asset, peg, now)
			if err != nil {
				return canonical.Pair{}, priceAtResult{}, false, false, nil, err
			}
			if ok {
				return pair, res, true, true, nil, nil
			}
			if withheld == nil {
				withheld = w
			}
		}
	}
	return canonical.Pair{}, priceAtResult{}, false, false, withheld, nil
}

// currentPriceForAliases returns the first (assetAlias, quoteAlias) orientation with a current closed bucket within
// priceChangesCurrentStaleness;
// withheld is the first ErrPriceWithheld-class error, so the caller picks the right 404.
func (s *Server) currentPriceForAliases(
	ctx context.Context, asset, quote canonical.Asset, now time.Time,
) (canonical.Pair, priceAtResult, bool, error, error) {
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
			value, observedAt, resSec, err := s.PriceAt.PriceAt(ctx, pair, now, priceChangesCurrentStaleness)
			if err != nil {
				if failErr := notePriceAtMiss(&withheld, err); failErr != nil {
					return canonical.Pair{}, priceAtResult{}, false, nil, failErr
				}
				continue
			}
			// Decimals normalization applies to the absolute price only: pct deltas are
			// scale-invariant (a constant K cancels in (ref-cur)/cur). Resolve against the
			// actual traded legs.
			value = s.normalizeRawRatioString(value, pair.Base, pair.Quote)
			return pair, priceAtResult{value: value, observedAt: observedAt, resSec: resSec}, true, nil, nil
		}
	}
	return canonical.Pair{}, priceAtResult{}, false, withheld, nil
}

// priceChangeHorizon computes one horizon's delta for a resolved pair. A miss (no bucket, withheld reference, unparseable ratio)
// yields an all-null horizon; a reader failure is returned as err, since available=false would falsely claim no history. Gates
// are asked about `target`, not `now`, so one horizon can be withheld while siblings are not.
func (s *Server) priceChangeHorizon(
	ctx context.Context, pair canonical.Pair, currentPrice string, target time.Time, tolerance time.Duration,
) (PriceChangeHorizon, PriceWithheldReason, error) {
	value, observedAt, resSec, err := s.PriceAt.PriceAt(ctx, pair, target, tolerance)
	if err != nil {
		if !isPriceAtMiss(err) {
			return PriceChangeHorizon{}, "", err
		}
		if errors.Is(err, ErrPriceWithheld) {
			return PriceChangeHorizon{Available: false, Withheld: true}, priceWithheldReason(err), nil
		}
		return PriceChangeHorizon{Available: false}, "", nil
	}
	// currentPrice was already normalized against this SAME pair, so both legs carry the identical
	// K and pctChange is unaffected; only the emitted reference_price is rescaled.
	value = s.normalizeRawRatioString(value, pair.Base, pair.Quote)
	pct, err := pctChange(currentPrice, value)
	if err != nil {
		return PriceChangeHorizon{Available: false}, "", nil //nolint:nilerr // documented above: an unparseable ratio degrades to unavailable, like populateChange24h/batchChange24h treat the same pctChange failure modes
	}
	at := observedAt.UTC().Format(time.RFC3339)
	res := resolutionLabel(resSec)
	return PriceChangeHorizon{
		ChangePct:      &pct,
		ReferencePrice: &value,
		ReferenceAt:    &at,
		Resolution:     &res,
		Available:      true,
	}, "", nil
}

// resolutionLabel maps a bucket width in seconds back to the CAGG granularity label for the wire `resolution` field; unknown widths
// (never expected) fall back to "<n>s".
func resolutionLabel(sec int) string {
	switch sec {
	case 60:
		return "1m"
	case 900:
		return "15m"
	case 3600:
		return "1h"
	case 14400:
		return "4h"
	case 86400:
		return "1d"
	case 604800:
		return "1w"
	case 2592000:
		return "1mo"
	default:
		return (time.Duration(sec) * time.Second).String()
	}
}
