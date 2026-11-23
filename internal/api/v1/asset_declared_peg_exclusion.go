package v1

import (
	"context"
	"time"
)

// fiatUSDRead resolves 1 unit of a fiat in USD for the declared-peg fill.
type fiatUSDRead func(ctx context.Context, ticker string) (price string, asOf time.Time, ok bool)

// fillDeclaredPegPricesForRowJudge is [Server.fillDeclaredPegPricesInListing] on the unjudged rate, for the /v1/assets listing
// only: its rows are cached unjudged and judged on the way out by [assetExclusionScope.judgeRows] (writeAssetListing), whose
// [rowJudge.peg] covers the peg's fiat/USD pair over the fill's whole window, so a lifted exclusion serves on the next request.
func (s *Server) fillDeclaredPegPricesForRowJudge(ctx context.Context, rows []AssetDetail) {
	s.fillDeclaredPegRows(ctx, rows, s.fiatUSDPriceUnjudged)
}

// fillDeclaredPegPriceForRowJudge is [Server.fillDeclaredPegPrice] on the unjudged rate, for /v1/assets/{id} only, judged on the
// way out by [assetExclusionScope.judgeDetail] (writeDetail) as fillDeclaredPegPricesForRowJudge is.
func (s *Server) fillDeclaredPegPriceForRowJudge(ctx context.Context, row *AssetDetail) {
	s.fillDeclaredPegPriceWith(ctx, row, nil, s.fiatUSDPriceUnjudged)
}

// priceFill names a producer that may set price_usd after the direct read declined or withheld it.
type priceFill int

const (
	fillCatalogueRow priceFill = iota // the catalogue row's own-market price, re-gated on substance
	fillTransitive                    // a one-hop derived price over the same pooled buckets
	fillThinRelease                   // the include_thin second pass, thin legs released
	fillGlobalMarket                  // a vetted global ticker's whole-token price
	fillDeclaredPeg                   // the operator-declared fiat peg x FX
)

// fillMayReplace is the one rule every price fill consults before writing over reason. Only a fill whose evidence is
// independent of what the refusal judged may replace it; an exclusion, a flagged issuer or a cause-less verdict stands.
func fillMayReplace(reason PriceWithheldReason, f priceFill) bool {
	wholeToken := f == fillGlobalMarket || f == fillDeclaredPeg
	switch reason {
	case "", PriceWithheldSubstance:
		return true
	case PriceWithheldUpstreamLeg:
		// A derived leg beyond the asset's own markets: re-gating those markets cannot clear it, a thin release can.
		return wholeToken || f == fillThinRelease
	case PriceWithheldDecimalsMismatch, PriceWithheldManipulationGuard:
		// A per-unit scale or bucket fault in the Stellar market; a whole-token price never passed through it.
		return wholeToken
	case PriceWithheldFXLeg:
		// The peg converts through the very FX leg that was missing.
		return f == fillGlobalMarket
	case PriceWithheldScamIssuer, PriceWithheldUnattributed, PriceWithheldSourceExcluded:
		return false
	}
	return false
}
