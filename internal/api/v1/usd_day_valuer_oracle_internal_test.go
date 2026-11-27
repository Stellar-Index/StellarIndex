package v1

import (
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The revenue valuer as it was before the leg-generic one, kept verbatim as the differential test's oracle.

type oracleRevenueUSDSum struct {
	v                         [revenueUSDLegCount]*big.Rat
	priced, unpriced, carried int
	bases                     map[string]int
}

func (t *oracleRevenueUSDSum) add(o *oracleRevenueUSDSum) {
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

func oracleLegsNonZero(legs [revenueUSDLegCount]*big.Int) bool {
	for _, a := range legs {
		if a.Sign() != 0 {
			return true
		}
	}
	return false
}

func (t *oracleRevenueUSDSum) valueHour(u reserveUSDSeries, at time.Time, legs [revenueUSDLegCount]*big.Int) {
	p, ok := u.priceAt(at)
	if u.reason != "" || !ok {
		t.unpriced++
		return
	}
	basis := u.basis
	if basis == "" && u.stableAt[p.Bucket.UTC()] {
		basis = priceBasisStablecoinProxy
	}
	h := oracleRevenueUSDSum{priced: 1, bases: map[string]int{basis: 1}}
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

func (t *oracleRevenueUSDSum) wire(reason string, lowerBound bool) LendingRevenueUSD {
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

func oracleLegsEqual(a, b [revenueUSDLegCount]*big.Int) bool {
	for i := range a {
		if a[i].Cmp(b[i]) != 0 {
			return false
		}
	}
	return true
}

func oracleValueRevenueBucket(b clickhouse.LendingRevenueBucket, hours []clickhouse.LendingRevenueBucket, h int, u reserveUSDSeries, width time.Duration) (oracleRevenueUSDSum, int) {
	var sum oracleRevenueUSDSum
	seen := revenueUSDLegs(clickhouse.NewLendingRevenueSums())
	for ; h < len(hours) && hours[h].Start.Before(b.Start.Add(width)); h++ {
		if hours[h].Start.Before(b.Start) {
			continue
		}
		legs := revenueUSDLegs(hours[h].Sums)
		for i, a := range legs {
			seen[i].Add(seen[i], a)
		}
		if oracleLegsNonZero(legs) {
			sum.valueHour(u, hours[h].Start, legs)
		}
	}
	if !oracleLegsEqual(seen, revenueUSDLegs(b.Sums)) {
		sum.unpriced++
	}
	return sum, h
}

func oracleValueRevenueReserve(rv *LendingRevenueReserveView, res clickhouse.LendingRevenueReserve, u reserveUSDSeries, width time.Duration) (oracleRevenueUSDSum, []oracleRevenueUSDSum) {
	var total oracleRevenueUSDSum
	buckets := make([]oracleRevenueUSDSum, len(res.Buckets))
	h := 0
	for j, b := range res.Buckets {
		buckets[j], h = oracleValueRevenueBucket(b, res.Hours, h, u, width)
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

func oracleValueRevenueView(v *LendingRevenueView, rev clickhouse.LendingRevenue, series []reserveUSDSeries) (oracleRevenueUSDSum, []oracleRevenueUSDSum) {
	width := time.Hour
	if v.Interval == "1d" {
		width = 24 * time.Hour
	}
	v.PriceMaxAgeHours = int(lendingUSDHourlyPriceMaxAge / time.Hour)
	v.TotalsUSD.Excluded = []LendingRevenueUSDExclusion{}
	var pool oracleRevenueUSDSum
	reserves := make([]oracleRevenueUSDSum, len(v.Reserves))
	valued, priceGap := 0, false
	for i := range v.Reserves {
		rv := &v.Reserves[i]
		t, _ := oracleValueRevenueReserve(rv, rev.Reserves[i], series[i], width)
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

type oracleRevenueDay struct {
	sum     oracleRevenueUSDSum
	reason  string
	outcome revenueDayOutcome
}

func oracleValueRevenueDay(day time.Time, hours []clickhouse.LendingRevenueBucket, walk func() reserveUSDRead) oracleRevenueDay {
	b := clickhouse.LendingRevenueBucket{Start: day, Sums: clickhouse.NewLendingRevenueSums()}
	amountHours := 0
	for _, h := range hours {
		b.Sums.Add(h.Sums)
		if oracleLegsNonZero(revenueUSDLegs(h.Sums)) {
			amountHours++
		}
	}
	if amountHours == 0 {
		return oracleRevenueDay{}
	}
	r := walk()
	switch usdReadRefusal(r) {
	case exclusionPriceWithheld:
		return oracleRevenueDay{outcome: revenueDayRefused, reason: r.series.reason, sum: oracleRevenueUSDSum{unpriced: amountHours}}
	case exclusionPriceReadFailed:
		return oracleRevenueDay{outcome: revenueDayDegraded}
	}
	sum, _ := oracleValueRevenueBucket(b, hours, 0, r.series, 24*time.Hour)
	return oracleRevenueDay{sum: sum, reason: r.series.reason}
}

func oracleRevenueDayRow(pool, asset string, info clickhouse.LendingInterestDay, sums clickhouse.LendingRevenueSums, d oracleRevenueDay) (timescale.LendingRevenueUSDDay, bool) {
	row := timescale.LendingRevenueUSDDay{
		Pool: pool, Asset: asset, Day: info.Day, InterestDeriveRev: int16(lendinginterest.DeriveRev), ValuationRev: lendingRevenueValuationRev, //nolint:gosec // a small revision number
		InterestDerivedAt: info.DerivedAt, HoursValued: d.sum.priced, HoursUnvalued: d.sum.unpriced, HoursCarried: d.sum.carried,
		BasisHours: map[string]int{}, StaleRows: int64(info.StaleRows), //nolint:gosec // an hour-row count
		AccrualExcluded: sums.UnorderedVersions + sums.AccrualErrors,
	}
	if sums.UnexplainedSupplyLoss.Sign() < 0 {
		row.UnexplainedLoss = sums.UnexplainedSupplyLossVersions
	}
	if d.reason != "" {
		row.WalkReason = strRef(d.reason)
	}
	for b, n := range d.sum.bases {
		if b == "" {
			b = "fiat"
		}
		row.BasisHours[b] = n
	}
	if d.sum.priced == 0 && d.sum.unpriced > 0 {
		return row, true
	}
	for i, x := range d.sum.v {
		if x == nil {
			x = new(big.Rat)
		}
		s, ok := ratExactDecimal(x)
		if !ok {
			return row, false
		}
		row.Legs[i] = &s
	}
	return row, true
}
