package aggregate

import (
	"context"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/robuststat"
)

// ExclusionTrimmingReader is a [GlobalPriceReader] that removes operator-excluded aggregator sources and returns them beside the
// survivors, so the tier judges both after its freshness filter (a stale row must not count towards the outlier filter's quorum).
type ExclusionTrimmingReader interface {
	LatestAggregatorPricesTrimmed(ctx context.Context, base, quote canonical.Asset, sources []string) (kept, excluded []canonical.OracleUpdate, err error)
}

// freshAggregatorRows is the aggregator tier's fresh input: the survivors, or a withheld verdict when the fresh survivors are
// empty or disagree with the fresh full set because an exclusion removed some of it.
func freshAggregatorRows(
	ctx context.Context, reader GlobalPriceReader, base, quote canonical.Asset, opts GlobalPriceOptions,
) ([]canonical.OracleUpdate, error) {
	tr, ok := reader.(ExclusionTrimmingReader)
	if !ok {
		rows, err := reader.LatestAggregatorPrices(ctx, base, quote, opts.AggregatorSources)
		return filterFreshAggregatorRows(rows, opts.MaxAggregatorAge), err
	}
	kept, excluded, err := tr.LatestAggregatorPricesTrimmed(ctx, base, quote, opts.AggregatorSources)
	if err != nil {
		return nil, err
	}
	fresh := filterFreshAggregatorRows(kept, opts.MaxAggregatorAge)
	freshExcluded := filterFreshAggregatorRows(excluded, opts.MaxAggregatorAge)
	if len(freshExcluded) == 0 {
		return fresh, nil
	}
	all := append(append(make([]canonical.OracleUpdate, 0, len(fresh)+len(freshExcluded)), fresh...), freshExcluded...)
	if len(fresh) == 0 || !ExcludedSurvivorsAgree(all, fresh) {
		return nil, &TierWithheldError{}
	}
	return fresh, nil
}

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
