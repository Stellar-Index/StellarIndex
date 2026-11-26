package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingRevenueValuationRev versions how a stored day is valued; bump it when the valuer changes, and readers leave out
// (and count) rows of an older one until the rollup re-values them.
const lendingRevenueValuationRev int16 = 1

// lendingRevenueDayWalkBudget replaces the request walk budget for a day walk: a background walk is not cut short at
// 2s, and one cut short anyway is degraded and never stored.
const lendingRevenueDayWalkBudget = 30 * time.Second

type revenueDayOutcome int

const (
	revenueDayValued   revenueDayOutcome = iota
	revenueDayDegraded                   // a read failed or was cut short: nothing stored, retried
	revenueDayRefused                    // a read-time verdict refused the reserve: nothing stored
)

// revenueDay is one reserve's USD for one UTC day: each hour at the price a walk of that UTC day gives. reason is the
// price data's own reason no hour could be valued (unpriced, decimals_unknown, price_scale_mismatch).
type revenueDay struct {
	sum     revenueUSDSum
	reason  string
	outcome revenueDayOutcome
}

// revenueDayRead walks assetC's USD series over the UTC day starting at day (the walk adds the price-age look-back).
func (s *Server) revenueDayRead(ctx context.Context, assetC string, day time.Time, budget time.Duration) reserveUSDRead {
	dec, known := s.reserveDecimals(ctx, clickhouse.BlendReserveState{Asset: assetC})
	return s.reserveUSDWalk(ctx, assetC, dec, known, clickhouse.LendingHistoryQuery{From: day, To: day.Add(23 * time.Hour)}, budget)
}

// valueRevenueDay values one day's hour rows with read, through the same bucket valuer as /revenue. A day with nothing
// to value is zero without a read; walk is called only when one is needed.
func valueRevenueDay(day time.Time, hours []clickhouse.LendingRevenueBucket, walk func() reserveUSDRead) revenueDay {
	b := clickhouse.LendingRevenueBucket{Start: day, Sums: clickhouse.NewLendingRevenueSums()}
	valued := false
	for _, h := range hours {
		b.Sums.Add(h.Sums)
		valued = valued || legsNonZero(revenueUSDLegs(h.Sums))
	}
	if !valued {
		return revenueDay{}
	}
	r := walk()
	switch {
	case r.verdict:
		return revenueDay{outcome: revenueDayRefused}
	case r.series.degraded || r.series.reason == exclusionPriceReadFailed:
		return revenueDay{outcome: revenueDayDegraded}
	}
	sum, _ := valueRevenueBucket(b, hours, 0, r.series, 24*time.Hour)
	return revenueDay{sum: sum, reason: r.series.reason}
}

// revenueDayRow is the stored form of a valued day; ok is false when a leg is not a finite decimal (never, for decimal
// prices over a power of ten), and the day is then left unstored.
func revenueDayRow(pool, asset string, info clickhouse.LendingInterestDay, sums clickhouse.LendingRevenueSums, d revenueDay) (timescale.LendingRevenueUSDDay, bool) {
	row := timescale.LendingRevenueUSDDay{
		Pool: pool, Asset: asset, Day: info.Day, InterestDeriveRev: int16(lendinginterest.DeriveRev), ValuationRev: lendingRevenueValuationRev, //nolint:gosec // a small revision number
		InterestDerivedAt: info.DerivedAt, PricedHours: d.sum.priced, UnpricedHours: d.sum.unpriced, PriceCarriedHours: d.sum.carried,
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

// ratExactDecimal is r as an exact decimal string, false when its denominator has a prime factor other than 2 and 5.
func ratExactDecimal(r *big.Rat) (string, bool) {
	d := new(big.Int).Set(r.Denom())
	count := func(p int64) int {
		n, q, m, bp := 0, new(big.Int), new(big.Int), big.NewInt(p)
		for {
			q.QuoRem(d, bp, m)
			if m.Sign() != 0 {
				return n
			}
			d.Set(q)
			n++
		}
	}
	twos, fives := count(2), count(5)
	if d.Cmp(big.NewInt(1)) != 0 {
		return "", false
	}
	return r.FloatString(max(twos, fives)), true
}

// revenueDayRowsEqual compares every stored fact of two rows but valued_at, legs as exact numbers.
func revenueDayRowsEqual(a, b timescale.LendingRevenueUSDDay) bool {
	if a.InterestDeriveRev != b.InterestDeriveRev || a.ValuationRev != b.ValuationRev || !a.InterestDerivedAt.Equal(b.InterestDerivedAt) ||
		a.StaleRows != b.StaleRows || a.AccrualExcluded != b.AccrualExcluded || a.UnexplainedLoss != b.UnexplainedLoss {
		return false
	}
	return revenueDayValuesEqual(a, b)
}

// revenueDayValuesEqual compares the USD a row states: the legs exactly, the hour counts, the bases and the reason.
func revenueDayValuesEqual(a, b timescale.LendingRevenueUSDDay) bool {
	if a.PricedHours != b.PricedHours || a.UnpricedHours != b.UnpricedHours || a.PriceCarriedHours != b.PriceCarriedHours ||
		(a.WalkReason == nil) != (b.WalkReason == nil) || (a.WalkReason != nil && *a.WalkReason != *b.WalkReason) ||
		len(a.BasisHours) != len(b.BasisHours) {
		return false
	}
	for k, n := range a.BasisHours {
		if b.BasisHours[k] != n {
			return false
		}
	}
	for i := range a.Legs {
		x, y := a.Legs[i], b.Legs[i]
		if (x == nil) != (y == nil) {
			return false
		}
		if x == nil {
			continue
		}
		rx, okx := new(big.Rat).SetString(*x)
		ry, oky := new(big.Rat).SetString(*y)
		if !okx || !oky || rx.Cmp(ry) != 0 {
			return false
		}
	}
	return true
}
