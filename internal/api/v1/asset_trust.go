package v1

import "github.com/Stellar-Index/StellarIndex/internal/pricingguard"

// Asset trust summary for /v1/assets/{id}: a banded score plus the
// factor breakdown behind it.
//
// Invariants:
//   - READ-ONLY OUTPUT. buildAssetTrust runs after every price producer and
//     suppressor and only reads their results; nothing in pricing, ranking
//     or the substance gates reads AssetDetail.Trust.
//   - UNKNOWN != ZERO. A factor with no evidence is banded "unknown", carries
//     null points and is left out of both the numerator and the denominator;
//     only an observed bad signal scores low.
//   - Every factor names the data source it was read from.
//   - The formula is versioned; any change to a weight, threshold or factor
//     bumps trustFormulaVersion.
const trustFormulaVersion = 1

// Factor ids (stable wire values).
const (
	trustFactorIssuerReputation = "issuer_reputation"
	trustFactorTickerCollision  = "ticker_collision"
	trustFactorSep1             = "sep1_attestation"
	trustFactorMarketSubstance  = "market_substance"
	trustFactorSupplyData       = "supply_data"
)

// Factor / overall bands.
const (
	trustBandHigh    = "high"
	trustBandMedium  = "medium"
	trustBandLow     = "low"
	trustBandUnknown = "unknown"
)

const (
	trustHighMin   = 75
	trustMediumMin = 40
)

// AssetTrust is the trust summary payload.
type AssetTrust struct {
	// FormulaVersion identifies the weights and thresholds that produced Score.
	FormulaVersion int `json:"formula_version"`
	// Score is 0-100, the weight-averaged points of the KNOWN factors; null
	// when no factor has evidence.
	Score *int `json:"score"`
	// Band is high / medium / low / unknown, derived from Score.
	Band string `json:"band"`
	// CoveragePct is the share (0-100) of total factor weight that had
	// evidence; a score over low coverage is a weak claim.
	CoveragePct int           `json:"coverage_pct"`
	Factors     []TrustFactor `json:"factors"`
}

// TrustFactor is one scored input.
type TrustFactor struct {
	ID string `json:"id"`
	// Band is high / medium / low, or unknown when there is no evidence.
	Band string `json:"band"`
	// Points is 0-100; null when Band is unknown.
	Points *int `json:"points"`
	Weight int  `json:"weight"`
	// Source names where the evidence comes from.
	Source string `json:"source"`
	// Observed is a short machine-readable description of what was read.
	Observed string `json:"observed"`
}

func trustBand(points int) string {
	switch {
	case points >= trustHighMin:
		return trustBandHigh
	case points >= trustMediumMin:
		return trustBandMedium
	}
	return trustBandLow
}

func trustKnown(id string, weight, points int, source, observed string) TrustFactor {
	return TrustFactor{ID: id, Band: trustBand(points), Points: &points, Weight: weight, Source: source, Observed: observed}
}

func trustUnknown(id string, weight int, source, observed string) TrustFactor {
	return TrustFactor{ID: id, Band: trustBandUnknown, Weight: weight, Source: source, Observed: observed}
}

func trustIssuerReputation(d *AssetDetail) (f TrustFactor, scam bool) {
	const src = "account_directory"
	switch {
	case d.IssuerScamReason != "" || pricingguard.IsDirectoryScamFlagged(d.IssuerDirectoryTags):
		return trustKnown(trustFactorIssuerReputation, 35, 0, src, "scam_flagged"), true
	case d.issuerDirectoryUnchecked:
		return trustUnknown(trustFactorIssuerReputation, 35, src, "lookup_failed"), false
	case d.IssuerDirectoryName != "" || d.IssuerDirectoryDomain != "" || len(d.IssuerDirectoryTags) > 0:
		return trustKnown(trustFactorIssuerReputation, 35, 90, src, "listed_unflagged"), false
	}
	return trustUnknown(trustFactorIssuerReputation, 35, src, "not_listed"), false
}

func trustTickerCollision(d *AssetDetail, flagged bool) TrustFactor {
	const src = "verified_currency_catalogue"
	if flagged || d.UnverifiedTickerCollision {
		return trustKnown(trustFactorTickerCollision, 10, 0, src, "collides_with_verified_ticker")
	}
	return trustUnknown(trustFactorTickerCollision, 10, src, "no_collision_observed")
}

func trustSep1(d *AssetDetail) TrustFactor {
	const src = "issuer_stellar_toml"
	switch d.Sep1Status {
	case "verified":
		return trustKnown(trustFactorSep1, 25, 100, src, d.Sep1Status)
	case "no_match":
		return trustKnown(trustFactorSep1, 25, 40, src, d.Sep1Status)
	case "unreachable":
		return trustKnown(trustFactorSep1, 25, 20, src, d.Sep1Status)
	}
	// not_fetched is our backlog and not_applicable has no document: neither
	// is evidence about the issuer.
	return trustUnknown(trustFactorSep1, 25, src, d.Sep1Status)
}

func trustMarketSubstance(d *AssetDetail) TrustFactor {
	const src = "served_price_gates"
	switch {
	case d.PriceWithheldReason == PriceWithheldSubstance || d.MarketCapLowLiquidity:
		return trustKnown(trustFactorMarketSubstance, 15, 20, src, "thin_market")
	case d.PriceUSD != nil && d.PriceBasis == "":
		return trustKnown(trustFactorMarketSubstance, 15, 100, src, "market_priced")
	}
	return trustUnknown(trustFactorMarketSubstance, 15, src, "no_market_price")
}

func trustSupplyData(d *AssetDetail) TrustFactor {
	const src = "asset_supply_history"
	switch {
	case d.SupplyBasis == nil:
		return trustUnknown(trustFactorSupplyData, 15, src, "no_supply_reading")
	case d.SupplyStale:
		return trustKnown(trustFactorSupplyData, 15, 50, src, "stale")
	}
	return trustKnown(trustFactorSupplyData, 15, 100, src, "current")
}

// buildAssetTrust derives the summary from the finished detail. tickerCollision
// is the response's unverified_ticker_collision flag.
func buildAssetTrust(d *AssetDetail, tickerCollision bool) *AssetTrust {
	rep, scam := trustIssuerReputation(d)
	factors := []TrustFactor{
		rep,
		trustTickerCollision(d, tickerCollision),
		trustSep1(d),
		trustMarketSubstance(d),
		trustSupplyData(d),
	}
	var sum, knownW, totalW int
	for _, f := range factors {
		totalW += f.Weight
		if f.Points != nil {
			sum += f.Weight * *f.Points
			knownW += f.Weight
		}
	}
	t := &AssetTrust{FormulaVersion: trustFormulaVersion, Band: trustBandUnknown, Factors: factors}
	t.CoveragePct = knownW * 100 / totalW
	if knownW == 0 {
		return t
	}
	score := (sum + knownW/2) / knownW
	if scam {
		// A scam-class flag is decisive; other factors must not average it up.
		score = 0
	}
	if factors[1].Points != nil && score >= trustMediumMin {
		// A ticker collision with a verified currency is an impersonation
		// signal; price and supply evidence must not lift it out of "low".
		score = trustMediumMin - 1
	}
	t.Score = &score
	t.Band = trustBand(score)
	return t
}
