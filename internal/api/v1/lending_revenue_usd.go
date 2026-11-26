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

// revenueUSDSum is exact USD per leg; v is nil until an hour is valued. bases counts priced hours per basis, "" fiat.
type revenueUSDSum struct {
	v                         [revenueUSDLegCount]*big.Rat
	priced, unpriced, carried int
	bases                     map[string]int
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
	for b, n := range o.bases {
		if t.bases == nil {
			t.bases = map[string]int{}
		}
		t.bases[b] += n
	}
}

// revenueUSDLegs are the valued amounts in wire order, losses and draws as the magnitudes the base units serve.
func revenueUSDLegs(s clickhouse.LendingRevenueSums) [revenueUSDLegCount]*big.Int {
	return [revenueUSDLegCount]*big.Int{
		s.Gross, s.SupplierIntended, s.SupplierRealised, s.Backstop, s.ProtocolFee,
		new(big.Int).Neg(s.InterestDrawn), new(big.Int).Neg(s.DefaultLoss),
	}
}

func legsNonZero(legs [revenueUSDLegCount]*big.Int) bool {
	for _, a := range legs {
		if a.Sign() != 0 {
			return true
		}
	}
	return false
}

// valueHour adds one hour's legs at that hour's price; an hour without a price within the bound is counted unpriced.
func (t *revenueUSDSum) valueHour(u reserveUSDSeries, at time.Time, legs [revenueUSDLegCount]*big.Int) {
	p, ok := u.priceAt(at)
	if u.reason != "" || !ok {
		t.unpriced++
		return
	}
	basis := u.basis
	if basis == "" && u.stableAt[p.Bucket.UTC()] {
		basis = priceBasisStablecoinProxy
	}
	h := revenueUSDSum{priced: 1, bases: map[string]int{basis: 1}}
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
	for b, n := range t.bases {
		switch {
		case b == "":
		case n == t.priced:
			out.PriceBasis = b
		default:
			if out.PriceBasisHours == nil {
				out.PriceBasisHours = map[string]int{}
			}
			out.PriceBasisHours[b] = n
		}
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

// valueRevenueBucket values bucket b from hours[h:] that fall inside it and returns the index past them; whatever of b
// those hours do not account for counts as one unpriced hour.
func valueRevenueBucket(b clickhouse.LendingRevenueBucket, hours []clickhouse.LendingRevenueBucket, h int, u reserveUSDSeries, width time.Duration) (revenueUSDSum, int) {
	var sum revenueUSDSum
	seen := revenueUSDLegs(clickhouse.NewLendingRevenueSums())
	for ; h < len(hours) && hours[h].Start.Before(b.Start.Add(width)); h++ {
		if hours[h].Start.Before(b.Start) {
			continue
		}
		legs := revenueUSDLegs(hours[h].Sums)
		for i, a := range legs {
			seen[i].Add(seen[i], a)
		}
		if legsNonZero(legs) {
			sum.valueHour(u, hours[h].Start, legs)
		}
	}
	if !legsEqual(seen, revenueUSDLegs(b.Sums)) {
		sum.unpriced++
	}
	return sum, h
}

// valueRevenueReserve values one reserve's points and totals from its hour rows. Whatever of a bucket its hours do not
// account for counts as one unpriced hour, so a bucket is never valued as if complete. A reserve with nothing to value
// is "0.00" whatever its price: zero tokens are worth zero.
func valueRevenueReserve(rv *LendingRevenueReserveView, res clickhouse.LendingRevenueReserve, u reserveUSDSeries, width time.Duration) (revenueUSDSum, []revenueUSDSum) {
	var total revenueUSDSum
	buckets := make([]revenueUSDSum, len(res.Buckets))
	h := 0
	for j, b := range res.Buckets {
		buckets[j], h = valueRevenueBucket(b, res.Hours, h, u, width)
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
			rv.Points[j].USD = buckets[j].wire(pr, false)
		}
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
	pool.bases = nil // a basis describes one token's series, not a sum across reserves
	v.TotalsUSD.LendingRevenueUSD = pool.wire(reason, v.LowerBound || len(v.TotalsUSD.Excluded) > 0)
	return pool, reserves
}

// revenueValuedSpan is the first and last hour of res carrying a valued amount; ok false when none does.
func revenueValuedSpan(res clickhouse.LendingRevenueReserve) (first, last time.Time, ok bool) {
	for _, h := range res.Hours {
		if !legsNonZero(revenueUSDLegs(h.Sums)) {
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
