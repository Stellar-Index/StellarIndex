// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// PriceChangeHorizon is one trailing-window delta on
// GET /v1/price/changes. Every field except Available is nil when the
// pair has no closed bucket that far back (a young pair, or a horizon
// predating recorded history) — the miss is per-horizon, never an
// error for the whole call. ReferenceAt + Resolution disclose exactly
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
	// Available is the per-horizon flag: false means no closed bucket
	// exists that far back (all the sibling fields are null).
	Available bool `json:"available"`
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
// the four horizons. Missing horizons (no closed bucket that far back)
// are null with available=false — never an error. A 404 only when the
// pair has no CURRENT price to anchor against; a 503 when no
// point-in-time reader is wired.
func (s *Server) handlePriceChanges(w http.ResponseWriter, r *http.Request) {
	if s.priceAt == nil {
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

	now := time.Now().UTC()
	pair, current, triangulated, found := s.resolvePriceChangePair(r.Context(), asset, quote, now)
	if !found {
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
	horizons := []*PriceChangeHorizon{&resp.H1, &resp.H24, &resp.D7, &resp.D30}
	for i, h := range priceChangeHorizons {
		*horizons[i] = s.priceChangeHorizon(r.Context(), pair, current.value, now.Add(-h.dur), h.dur)
	}

	writeJSON(w, resp, Flags{Triangulated: triangulated})
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
// dual-form aliases (F-1340) and, when the quote is fiat:USD and no
// direct/aliased bucket exists, the operator's USD-pegged classics
// (the same stablecoin-proxy chain /v1/price and /v1/price/at use) —
// flagging triangulated=true on that path. found=false when no
// orientation has a fresh-enough current bucket.
func (s *Server) resolvePriceChangePair(
	ctx context.Context, asset, quote canonical.Asset, now time.Time,
) (canonical.Pair, priceAtResult, bool, bool) {
	if pair, res, ok := s.currentPriceForAliases(ctx, asset, quote, now); ok {
		return pair, res, false, true
	}
	// Stablecoin fiat-proxy fallback: retry each USD peg. First hit
	// wins; the response still echoes the requested quote (fiat:USD)
	// and flags triangulated.
	if quote.Type == canonical.AssetFiat && quote.Code == "USD" {
		for _, peg := range s.usdPeggedClassics {
			if peg.Equal(asset) {
				continue
			}
			if pair, res, ok := s.currentPriceForAliases(ctx, asset, peg, now); ok {
				return pair, res, true, true
			}
		}
	}
	return canonical.Pair{}, priceAtResult{}, false, false
}

// currentPriceForAliases returns the first (assetAlias, quoteAlias)
// orientation with a current closed bucket within
// priceChangesCurrentStaleness of now.
func (s *Server) currentPriceForAliases(
	ctx context.Context, asset, quote canonical.Asset, now time.Time,
) (canonical.Pair, priceAtResult, bool) {
	for _, a := range assetAliases(asset) {
		for _, q := range assetAliases(quote) {
			if a.Equal(q) {
				continue
			}
			pair, pairErr := canonical.NewPair(a, q)
			if pairErr != nil {
				continue
			}
			value, observedAt, resSec, err := s.priceAt.PriceAt(ctx, pair, now, priceChangesCurrentStaleness)
			if err != nil {
				continue
			}
			// dex-nonstandard-decimals forward normalization (M2) on the
			// absolute current price. The pct deltas are scale-invariant (a
			// constant K cancels in (ref-cur)/cur), so they are UNCHANGED; only
			// the served current_price / reference_price absolute values are
			// corrected. Resolve against the actual traded legs. No-op at 7dp.
			value = s.normalizeRawRatioString(value, pair.Base, pair.Quote)
			return pair, priceAtResult{value: value, observedAt: observedAt, resSec: resSec}, true
		}
	}
	return canonical.Pair{}, priceAtResult{}, false
}

// priceChangeHorizon computes one horizon's delta for an already-
// resolved pair. Returns an unavailable (all-null) horizon on any miss
// — reader error, no bucket that far back, or an unparseable ratio —
// so a single bad horizon never fails the whole response.
func (s *Server) priceChangeHorizon(
	ctx context.Context, pair canonical.Pair, currentPrice string, target time.Time, tolerance time.Duration,
) PriceChangeHorizon {
	value, observedAt, resSec, err := s.priceAt.PriceAt(ctx, pair, target, tolerance)
	if err != nil {
		return PriceChangeHorizon{Available: false}
	}
	// dex-nonstandard-decimals forward normalization (M2) on the absolute
	// reference price. `currentPrice` was already normalized against this SAME
	// pair (resolvePriceChangePair), so pctChange sees both legs scaled by the
	// identical K and the returned percentage is byte-identical to pre-fix —
	// only the emitted reference_price absolute value changes. No-op at 7dp.
	value = s.normalizeRawRatioString(value, pair.Base, pair.Quote)
	pct, err := pctChange(currentPrice, value)
	if err != nil {
		return PriceChangeHorizon{Available: false}
	}
	at := observedAt.UTC().Format(time.RFC3339)
	res := resolutionLabel(resSec)
	return PriceChangeHorizon{
		ChangePct:      &pct,
		ReferencePrice: &value,
		ReferenceAt:    &at,
		Resolution:     &res,
		Available:      true,
	}
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
