package aggregate

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/robuststat"
)

// ExcludedSurvivorsAgree reports whether the aggregator rows that survived an operator exclusion may be averaged. When the
// exclusion left fewer than [aggregatorMinForOutlierReject] of a set that had enough, [rejectAggregatorOutliers] no longer
// judges them, so every usable survivor must sit inside the band the full set defines; otherwise the tier has no data.
func ExcludedSurvivorsAgree(all, survivors []canonical.OracleUpdate) bool {
	if len(survivors) >= aggregatorMinForOutlierReject {
		return true // the tier's own filter still judges them
	}
	vals := make([]*big.Rat, 0, len(all))
	for i := range all {
		if v, ok := scaledAggregatorRat(&all[i]); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) < aggregatorMinForOutlierReject {
		return true // the full set had no consensus to lose
	}
	centre, scale := robuststat.CentreScale(vals)
	half := new(big.Rat).Mul(aggregatorMADFactor, scale)
	for i := range survivors {
		v, ok := scaledAggregatorRat(&survivors[i])
		if !ok {
			continue // averageAggregatorPrices skips it too
		}
		if dev := robuststat.SymmetricDev(v, centre); dev == nil || dev.Cmp(half) > 0 {
			return false
		}
	}
	return true
}
