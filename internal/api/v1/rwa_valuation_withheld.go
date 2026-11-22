package v1

// RWAValuationCapWithheld: a price and a circulating supply are both served and no market cap is; the row's
// market_cap_withheld_reason, verbatim from /v1/assets, says why (absent: a compute gap, not a guard verdict).
const RWAValuationCapWithheld = "market_cap_withheld"

// rwaCapWithheldValuation names a priced row with no market cap. supply_unavailable is reserved for a row that has no
// supply: a cap refused on its price basis or a ticker collision must not send a reader chasing a supply read.
func rwaCapWithheldValuation(d AssetDetail) RWAValuation {
	v := RWAValuation{PriceUSD: d.PriceUSD, PriceBasis: d.PriceBasis, MarketCapWithheldReason: d.MarketCapWithheldReason}
	switch {
	case d.MarketCapLowLiquidity:
		v.Status = RWAValuationLowLiquidity
	case d.scaleUnknown() || d.MarketCapDecimalsMismatch:
		// Reported before the supply reason: the row has a supply but lacks the exponent.
		v.Status = RWAValuationDecimalsUnknown
	case d.CirculatingSupply == nil:
		v.Status = RWAValuationNoSupply
	default:
		v.Status = RWAValuationCapWithheld
	}
	return v
}
