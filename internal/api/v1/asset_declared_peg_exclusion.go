package v1

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
