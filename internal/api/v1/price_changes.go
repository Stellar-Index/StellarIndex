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

// PriceChangeHorizon is one trailing-window delta on
// GET /v1/price/changes. Every pointer field is nil when the horizon is
// unavailable — no closed bucket that far back (a young pair, or a
// horizon predating recorded history), a read failure, or a withheld
// reference (Withheld) — and the miss is per-horizon, never an error
// for the whole call. ReferenceAt + Resolution disclose exactly
// which closed bucket the comparison used, so a consumer can see the
// delta was measured against, say, a daily bar and not a 1-minute one.
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
	// Withheld is true when the reference bucket EXISTS but a serving
	// gate refused to publish it (the thin-market or scam-issuer gate, or
	// the serving-sanity guard). It is what separates "withheld" from
	// "no data that far back" on an unavailable horizon; always false
	// when Available is true.
	Withheld bool `json:"withheld"`
	// ThinMarket: the reference bucket was served under `include_thin=true`
	// from a market below the substance floor.
	ThinMarket bool `json:"thin_market,omitempty"`
}

// PriceChanges is the GET /v1/price/changes payload: the current
// closed price plus signed change over 1h / 24h / 7d / 30d in one
// call — the multi-horizon accommodation for wallet/portfolio UIs
// (RFP §6). Each horizon is computed as the current closed VWAP vs the
// closed VWAP at-or-before now-horizon, both from the same
// point-in-time reader /v1/price/at uses (finest CAGG that covers the
// instant; prices_1d spans to 2015).
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

// priceChangesCurrentStaleness bounds how old the "current" bucket may
// be before the pair is treated as having no current price (a 404).
// One day matches /v1/price/at's [priceAtMaxLookback]: a pair with no
// trade in the last day has no honest "current" price to anchor a
// change on.
const priceChangesCurrentStaleness = priceAtMaxLookback

// priceChangeHorizons are the trailing windows /v1/price/changes
// reports, in ascending order. Each horizon's reference is the closed
// bucket at-or-before now-dur; the staleness tolerance passed to the
// reader is the horizon itself — generous enough to let a coarse
// (daily) bar answer a multi-week horizon, while still refusing a
// bucket more than one horizon-width stale (which is the "no data that
// far back" signal). reference_at + resolution disclose the bucket
// actually used.
var priceChangeHorizons = []struct {
	label string
	dur   time.Duration
}{
	{"1h", time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
}

// handlePriceChanges serves GET /v1/price/changes?asset=&quote=.
//
// Returns the current closed price plus the signed change over each of
// the four horizons. Missing horizons are null with available=false —
// never an error — and withheld=true when the reference bucket exists but
// a serving gate refused it. A 404 only when the
// pair has no CURRENT price to anchor against; a 503 when no
// point-in-time reader is wired; a 503/500 when any read fails, since a
// failed read is not evidence that a bucket is missing.
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

	// Per-request DB ceiling: the endpoint issues up to five
	// sequential PriceAt reads (the current anchor plus one per
	// horizon), each walking alias combinations and, for a fiat:USD
	// quote, every configured USD peg. Without a bounded context a slow
	// run holds its pool connection until the client gives up. 8s
	// matches the sibling single-shot read endpoints (oracle.go,
	// vwap.go, ohlc.go) and fires before the blanket request-timeout
	// middleware.
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

// priceChangeCurrentThin resolves the anchor with the default read, and
// only when that ended withheld on a measured thin leg and the request
// opted in, again with that leg admitted. A failed second pass serves the
// first pass's verdict: an optional retry never turns a 404 into a 5xx.
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

// priceChangeHorizonThin is [Server.priceChangeHorizon] with the same
// two-pass rule, on a record of the horizon's own: each horizon is a
// point-in-time read with its own verdict.
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

// resolvePriceChangePair finds the (base, quote) orientation that
// yields a current price for the request and returns that pair so
// every horizon is measured against the SAME market. It walks the XLM
// dual-form aliases and, when the quote is fiat:USD and no
// direct/aliased bucket exists, the operator's USD-pegged classics
// (the same stablecoin-proxy chain /v1/price and /v1/price/at use) —
// flagging triangulated=true on that path. found=false when no
// orientation has a fresh-enough current bucket; a non-nil withheld
// (only meaningful when found=false) is the first ErrPriceWithheld-class
// error an orientation returned — a bucket existed but a serving gate
// refused it — and the caller reports the distinct price-withheld 404,
// worded for that gate, rather than the generic not-found. The last
// return is a reader failure that ended the walk.
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

// currentPriceForAliases returns the first (assetAlias, quoteAlias)
// orientation with a current closed bucket within
// priceChangesCurrentStaleness of now. withheld is the first
// ErrPriceWithheld-class error any orientation returned (nil when none
// did) — the caller needs it to choose the correct 404 once every
// orientation is exhausted. err is a reader failure that ended the walk.
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
			// dex-nonstandard-decimals forward normalization (M2) on the
			// absolute current price. The pct deltas are scale-invariant (a
			// constant K cancels in (ref-cur)/cur), so they are UNCHANGED; only
			// the served current_price / reference_price absolute values are
			// corrected. Resolve against the actual traded legs. No-op at 7dp.
			value = s.normalizeRawRatioString(value, pair.Base, pair.Quote)
			return pair, priceAtResult{value: value, observedAt: observedAt, resSec: resSec}, true, nil, nil
		}
	}
	return canonical.Pair{}, priceAtResult{}, false, withheld, nil
}

// priceChangeHorizon computes one horizon's delta for an already-
// resolved pair. Returns an unavailable (all-null) horizon on a miss —
// no bucket that far back, a withheld reference, or an unparseable ratio.
// A reader failure is returned as err instead: rendering it as
// available=false would claim the pair has no history that far back. A
// withheld reference (ErrPriceWithheld, which includes
// ErrPriceAtGuarded) additionally sets Withheld: the gates are asked
// about `target`, not `now`, so one horizon can be withheld while
// its siblings are not, and a consumer must not read that null as "no
// history that far back". The reason is the withheld reference's.
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
	// dex-nonstandard-decimals forward normalization (M2) on the absolute
	// reference price. `currentPrice` was already normalized against this SAME
	// pair (resolvePriceChangePair), so pctChange sees both legs scaled by the
	// identical K and the returned percentage is unaffected; only the
	// emitted reference_price absolute value is rescaled. No-op at 7dp.
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

// resolutionLabel maps a bucket width in seconds back to the CAGG
// granularity label the reader used, for the wire `resolution` field.
// Falls back to a "<n>s" form for any width outside the known ladder
// (never expected — the reader only serves the fixed rungs).
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
