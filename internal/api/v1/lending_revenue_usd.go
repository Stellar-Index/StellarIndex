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
	PricedHours       int     `json:"priced_hours"`
	UnpricedHours     int     `json:"unpriced_hours"`
	PriceCarriedHours int     `json:"price_carried_hours"`
	LowerBound        bool    `json:"lower_bound"`
	USDReason         *string `json:"usd_reason"`
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

// revenueUSDSum is exact USD per leg; v is nil until an hour is valued.
type revenueUSDSum struct {
	v                         [revenueUSDLegCount]*big.Rat
	priced, unpriced, carried int
}

func (t *revenueUSDSum) add(o *revenueUSDSum) {
	for i, x := range o.v {
		if x == nil {
			continue
		}
		if t.v[i] == nil {
			t.v[i] = new(big.Rat)
		}
		t.v[i].Add(t.v[i], x)
	}
	t.priced, t.unpriced, t.carried = t.priced+o.priced, t.unpriced+o.unpriced, t.carried+o.carried
}

// revenueUSDLegs are the valued amounts in wire order, losses and draws as the magnitudes the base units serve.
func revenueUSDLegs(s clickhouse.LendingRevenueSums) [revenueUSDLegCount]*big.Int {
	return [revenueUSDLegCount]*big.Int{
		s.Gross, s.SupplierIntended, s.SupplierRealised, s.Backstop, s.ProtocolFee,
		new(big.Int).Neg(s.InterestDrawn), new(big.Int).Neg(s.DefaultLoss),
	}
}

// valueHour adds one hour's legs at that hour's price; an hour without a price within the bound is counted unpriced.
func (t *revenueUSDSum) valueHour(u reserveUSDSeries, at time.Time, legs [revenueUSDLegCount]*big.Int) {
	p, ok := u.priceAt(at)
	if u.reason != "" || !ok {
		t.unpriced++
		return
	}
	h := revenueUSDSum{priced: 1}
	for i, a := range legs {
		v, err := usdValueRat(a, p.VWAP, u.dec)
		if err != nil {
			t.unpriced++
			return
		}
		h.v[i] = v
	}
	if at.Sub(p.Bucket) > u.step {
		h.carried = 1
	}
	t.add(&h)
}

// wire rounds once, here; reason (or no hour valued against an unpriced one) nulls every figure.
func (t *revenueUSDSum) wire(reason string, lowerBound bool) LendingRevenueUSD {
	out := LendingRevenueUSD{
		PricedHours: t.priced, UnpricedHours: t.unpriced, PriceCarriedHours: t.carried, LowerBound: lowerBound || t.unpriced > 0,
	}
	if reason == "" && t.priced == 0 && t.unpriced > 0 {
		reason = lendingUSDNoPriceInBound
	}
	if reason != "" {
		out.USDReason = strRef(reason)
		return out
	}
	for i, f := range []**string{
		&out.InterestAccruedUSD, &out.SupplierShareUSD, &out.SupplierCreditedUSD, &out.BackstopTakeUSD,
		&out.ProtocolFeeUSD, &out.BackstopDrawnUSD, &out.DefaultLossUSD,
	} {
		x := t.v[i]
		if x == nil {
			x = new(big.Rat)
		}
		*f = strRef(ratString(x, 2))
	}
	return out
}

func legsEqual(a, b [revenueUSDLegCount]*big.Int) bool {
	for i := range a {
		if a[i].Cmp(b[i]) != 0 {
			return false
		}
	}
	return true
}

// valueRevenueReserve values one reserve's points and totals from its hour rows. Whatever of a bucket its hours do not
// account for counts as one unpriced hour, so a bucket is never valued as if complete.
func valueRevenueReserve(rv *LendingRevenueReserveView, res clickhouse.LendingRevenueReserve, u reserveUSDSeries, width time.Duration) (revenueUSDSum, []revenueUSDSum) {
	reason := u.reason
	if rv.Totals == nil {
		reason = revenueUSDAmountsExcluded
	}
	var total revenueUSDSum
	buckets := make([]revenueUSDSum, len(res.Buckets))
	h := 0
	for j, b := range res.Buckets {
		seen := revenueUSDLegs(clickhouse.NewLendingRevenueSums())
		for ; h < len(res.Hours) && res.Hours[h].Start.Before(b.Start.Add(width)); h++ {
			hr := res.Hours[h]
			if hr.Start.Before(b.Start) {
				continue
			}
			legs := revenueUSDLegs(hr.Sums)
			nonZero := false
			for i, a := range legs {
				seen[i].Add(seen[i], a)
				nonZero = nonZero || a.Sign() != 0
			}
			if nonZero {
				buckets[j].valueHour(u, hr.Start, legs)
			}
		}
		if !legsEqual(seen, revenueUSDLegs(b.Sums)) {
			buckets[j].unpriced++
		}
		if j < len(rv.Points) {
			rv.Points[j].USD = buckets[j].wire(reason, false)
		}
		total.add(&buckets[j])
	}
	rv.USD = total.wire(reason, rv.LowerBound)
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
	v.TotalsUSD.LendingRevenueUSD = pool.wire(reason, v.LowerBound || len(v.TotalsUSD.Excluded) > 0)
	return pool, reserves
}

// valueRevenue reads each summed reserve's hourly USD series over the hours served and values the view with it.
func (s *Server) valueRevenue(ctx context.Context, v *LendingRevenueView, rev clickhouse.LendingRevenue, f Flags) Flags {
	q := clickhouse.LendingHistoryQuery{From: rev.From, To: rev.Before}
	series := make([]reserveUSDSeries, len(v.Reserves))
	read := make([]bool, len(v.Reserves))
	forEachBounded(s.logger, len(v.Reserves), readFanoutConcurrency, func(i int) {
		rv := v.Reserves[i]
		if rv.Totals == nil {
			return
		}
		dec, known := 0, rv.Decimals != nil
		if known {
			dec = int(*rv.Decimals)
		}
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
