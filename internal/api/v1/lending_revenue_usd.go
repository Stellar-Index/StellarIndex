package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The revenue route's USD reasons, beside its price series' own: unpriced, decimals_unknown, price_scale_mismatch,
// withheld (a scam issuer or a thin market) and price_read_failed.
const (
	revenueUSDAmountsExcluded = "amounts_excluded" // nothing of it was summed in base units; excluded names why
	revenueUSDPriceMissing    = "price_missing"    // hours carrying an amount had no price within price_max_age_hours
)

const revenueUSDLegCount = 7

// LendingRevenueUSD values a bucket's or a reserve's amounts hour by hour, each hour at its own USD price and never a
// later one. The *_usd figures are null together, with usd_reason, when nothing of them could be valued.
type LendingRevenueUSD struct {
	InterestAccruedUSD  *string `json:"interest_accrued_usd"`
	SupplierShareUSD    *string `json:"supplier_share_usd"`
	SupplierCreditedUSD *string `json:"supplier_credited_usd"`
	BackstopTakeUSD     *string `json:"backstop_take_usd"`
	ProtocolFeeUSD      *string `json:"protocol_fee_usd"`
	BackstopDrawnUSD    *string `json:"backstop_drawn_usd"`
	DefaultLossUSD      *string `json:"default_loss_usd"`
	// UnpricedHours carried an amount but no price within the bound: left out, so the figures are a lower bound.
	PricedHours       int `json:"priced_hours"`
	UnpricedHours     int `json:"unpriced_hours"`
	PriceCarriedHours int `json:"price_carried_hours"`
	// PriceBasis is set when every priced hour shares one non-fiat basis (as /history's price_basis); PriceBasisHours
	// counts the priced hours per non-fiat basis when they mix. Neither on totals_usd.
	PriceBasis      string         `json:"price_basis,omitempty"`
	PriceBasisHours map[string]int `json:"price_basis_hours,omitempty"`
	LowerBound      bool           `json:"lower_bound"`
	USDReason       *string        `json:"usd_reason"`
}

// LendingRevenueTotalsUSD sums the reserves' USD across the pool, USD being the one unit in which reserves add up.
type LendingRevenueTotalsUSD struct {
	LendingRevenueUSD
	Excluded []LendingRevenueUSDExclusion `json:"excluded"`
}

// LendingRevenueUSDExclusion names a reserve the pool's USD totals leave out: Count of its hours carrying an amount.
type LendingRevenueUSDExclusion struct {
	Asset  string `json:"asset"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// revenueUSDLegRats are the revenue legs' exact USD in wire order.
type revenueUSDLegRats [revenueUSDLegCount]*big.Rat

func (a *revenueUSDLegRats) slice() []*big.Rat { return a[:] }

// revenueUSDSum is the revenue legs' exact USD, through the valuer every per-day USD rollup shares.
type revenueUSDSum = usdSum[revenueUSDLegRats, *revenueUSDLegRats]

// revenueUSDLegs are the valued amounts in wire order, losses and draws as the magnitudes the base units serve.
func revenueUSDLegs(s clickhouse.LendingRevenueSums) [revenueUSDLegCount]*big.Int {
	return [revenueUSDLegCount]*big.Int{
		s.Gross, s.SupplierIntended, s.SupplierRealised, s.Backstop, s.ProtocolFee,
		new(big.Int).Neg(s.InterestDrawn), new(big.Int).Neg(s.DefaultLoss),
	}
}

func revenueUSDAmounts(s clickhouse.LendingRevenueSums) []*big.Int {
	legs := revenueUSDLegs(s)
	return legs[:]
}

// revenueHours are hour rows as the valuer's hours.
func revenueHours(rows []clickhouse.LendingRevenueBucket) []usdAmountHour {
	out := make([]usdAmountHour, len(rows))
	for i, h := range rows {
		out[i] = usdAmountHour{start: h.Start, amounts: revenueUSDAmounts(h.Sums)}
	}
	return out
}

// revenueUSDWire is t's served figures, null together with usd_reason when nothing of them could be valued.
func revenueUSDWire(t *revenueUSDSum, reason string, lowerBound bool) LendingRevenueUSD {
	w := t.wire(reason, lowerBound)
	out := LendingRevenueUSD{
		PricedHours: w.priced, UnpricedHours: w.unpriced, PriceCarriedHours: w.carried, PriceBasis: w.basis,
		PriceBasisHours: w.basisHours, LowerBound: w.lowerBound, USDReason: w.reason,
	}
	if w.legs != nil {
		out.InterestAccruedUSD, out.SupplierShareUSD, out.SupplierCreditedUSD, out.BackstopTakeUSD = w.legs[0], w.legs[1], w.legs[2], w.legs[3]
		out.ProtocolFeeUSD, out.BackstopDrawnUSD, out.DefaultLossUSD = w.legs[4], w.legs[5], w.legs[6]
	}
	return out
}

// valueRevenueReserve values one reserve's points and totals from its hour rows. Whatever of a bucket its hours do not
// account for counts as one unpriced hour, so a bucket is never valued as if complete. A reserve with nothing to value
// is "0.00" whatever its price: zero tokens are worth zero.
func valueRevenueReserve(rv *LendingRevenueReserveView, res clickhouse.LendingRevenueReserve, u reserveUSDSeries, width time.Duration) (revenueUSDSum, []revenueUSDSum) {
	var total revenueUSDSum
	buckets := make([]revenueUSDSum, len(res.Buckets))
	hours, h := revenueHours(res.Hours), 0
	for j, b := range res.Buckets {
		buckets[j], h = valueUSDBucket[revenueUSDLegRats](b.Start, revenueUSDAmounts(b.Sums), hours, h, u, width)
		total.add(&buckets[j])
	}
	reason := u.reason
	switch {
	case rv.Totals == nil:
		reason = revenueUSDAmountsExcluded
	case total.priced+total.unpriced == 0:
		reason = ""
	}
	for j := range buckets {
		if j < len(rv.Points) {
			pr := reason
			if pr != revenueUSDAmountsExcluded && buckets[j].priced+buckets[j].unpriced == 0 {
				pr = ""
			}
			rv.Points[j].USD = revenueUSDWire(&buckets[j], pr, false)
		}
	}
	rv.USD = revenueUSDWire(&total, reason, rv.LowerBound)
	return total, buckets
}

// valueRevenueView values every reserve and sums the valued ones into totals_usd. A reserve with no USD value is
// named and left out, never added as zero; any base-unit exclusion carries into the USD totals as a lower bound.
func valueRevenueView(v *LendingRevenueView, rev clickhouse.LendingRevenue, series []reserveUSDSeries) (revenueUSDSum, []revenueUSDSum) {
	width := time.Hour
	if v.Interval == "1d" {
		width = 24 * time.Hour
	}
	v.PriceMaxAgeHours = int(lendingUSDHourlyPriceMaxAge / time.Hour)
	v.TotalsUSD.Excluded = []LendingRevenueUSDExclusion{}
	var pool revenueUSDSum
	reserves := make([]revenueUSDSum, len(v.Reserves))
	valued, priceGap := 0, false
	for i := range v.Reserves {
		rv := &v.Reserves[i]
		t, _ := valueRevenueReserve(rv, rev.Reserves[i], series[i], width)
		reserves[i] = t
		switch {
		case rv.Totals == nil: // named in excluded already
		case rv.USD.USDReason != nil:
			reason := *rv.USD.USDReason
			if reason == lendingUSDNoPriceInBound {
				reason = revenueUSDPriceMissing
			}
			v.TotalsUSD.Excluded = append(v.TotalsUSD.Excluded, LendingRevenueUSDExclusion{Asset: rv.Asset, Reason: reason, Count: t.unpriced})
			pool.unpriced += t.unpriced
			priceGap = true
		default:
			if t.unpriced > 0 {
				v.TotalsUSD.Excluded = append(v.TotalsUSD.Excluded, LendingRevenueUSDExclusion{Asset: rv.Asset, Reason: revenueUSDPriceMissing, Count: t.unpriced})
			}
			pool.add(&t)
			valued++
		}
	}
	reason := ""
	switch {
	case valued > 0:
	case priceGap:
		reason = revenueUSDPriceMissing
	case len(v.Reserves) > 0 || v.LowerBound:
		reason = revenueUSDAmountsExcluded
	}
	pool.bases = nil // a basis describes one token's series, not a sum across reserves
	v.TotalsUSD.LendingRevenueUSD = revenueUSDWire(&pool, reason, v.LowerBound || len(v.TotalsUSD.Excluded) > 0)
	return pool, reserves
}

// revenueValuedSpan is the first and last hour of res carrying a valued amount; ok false when none does.
func revenueValuedSpan(res clickhouse.LendingRevenueReserve) (first, last time.Time, ok bool) {
	for _, h := range res.Hours {
		if !amountsNonZero(revenueUSDAmounts(h.Sums)) {
			continue
		}
		if !ok {
			first, ok = h.Start, true
		}
		last = h.Start
	}
	return first, last, ok
}

// valueRevenue reads each reserve's hourly USD series over only the hours it has amounts in (the walk adds the price-age
// look-back before the first) and values the view with it; a reserve with nothing to value reads no prices.
func (s *Server) valueRevenue(ctx context.Context, v *LendingRevenueView, rev clickhouse.LendingRevenue, f Flags) Flags {
	series := make([]reserveUSDSeries, len(v.Reserves))
	read := make([]bool, len(v.Reserves))
	forEachBounded(s.logger, len(v.Reserves), readFanoutConcurrency, func(i int) {
		rv := v.Reserves[i]
		first, last, has := revenueValuedSpan(rev.Reserves[i])
		if rv.Totals == nil || !has {
			return
		}
		dec, known := 0, rv.Decimals != nil
		if known {
			dec = int(*rv.Decimals)
		}
		// To is the last hour itself: the walk's ceiling is one step past it.
		q := clickhouse.LendingHistoryQuery{From: first, To: last}
		series[i], read[i] = s.reserveUSDSeriesFor(ctx, rv.Asset, dec, known, q), true // distinct indices
	})
	for i := range series {
		if read[i] {
			f = series[i].usdFlags(f)
		}
	}
	valueRevenueView(v, rev, series)
	return f
}
