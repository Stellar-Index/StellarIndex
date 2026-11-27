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

type revenueDayOutcome = usdDayOutcome

const (
	revenueDayValued   = usdDayValued
	revenueDayDegraded = usdDayDegraded
	revenueDayRefused  = usdDayRefused
)

// revenueDayRefusedRow reports whether row records a verdict refusal; only a verdict gives the withheld reason.
func revenueDayRefusedRow(row timescale.LendingRevenueUSDDay) bool {
	return row.WalkReason != nil && *row.WalkReason == exclusionPriceWithheld
}

// revenueDay is one reserve's USD for one UTC day: each hour at the price a walk of that UTC day gives. reason is the
// price data's own reason no hour could be valued (unpriced, decimals_unknown, price_scale_mismatch).
type revenueDay = usdDay[revenueUSDLegRats, *revenueUSDLegRats]

// revenueDayRead walks assetC's USD series over the UTC day starting at day (the walk adds the price-age look-back).
func (s *Server) revenueDayRead(ctx context.Context, assetC string, day time.Time, budget time.Duration) reserveUSDRead {
	dec, known := s.reserveDecimals(ctx, clickhouse.BlendReserveState{Asset: assetC})
	return s.reserveUSDWalk(ctx, assetC, dec, known, clickhouse.LendingHistoryQuery{From: day, To: day.Add(23 * time.Hour)}, budget)
}

// valueRevenueDay values one day's hour rows with read, through the same bucket valuer as /revenue. A day with nothing
// to value is zero without a read; walk is called only when one is needed.
func valueRevenueDay(day time.Time, hours []clickhouse.LendingRevenueBucket, walk func() reserveUSDRead) revenueDay {
	d, err := valueUSDDay[revenueUSDLegRats](day, revenueHours(hours), walk)
	if err != nil { // unreachable: revenueHours gives every hour all seven legs; never stored, retried
		return revenueDay{outcome: usdDayDegraded}
	}
	return d
}

// revenueDayRow is the stored form of a valued day; ok is false when a leg is not a finite decimal (never, for decimal
// prices over a power of ten), and the day is then left unstored.
func revenueDayRow(pool, asset string, info clickhouse.LendingInterestDay, sums clickhouse.LendingRevenueSums, d revenueDay) (timescale.LendingRevenueUSDDay, bool) {
	row := timescale.LendingRevenueUSDDay{
		Pool: pool, Asset: asset, Day: info.Day, InterestDeriveRev: int16(lendinginterest.DeriveRev), ValuationRev: lendingRevenueValuationRev, //nolint:gosec // a small revision number
		InterestDerivedAt: info.DerivedAt, HoursValued: d.sum.priced, HoursUnvalued: d.sum.unpriced, HoursCarried: d.sum.carried,
		BasisHours: storedBasisHours(d.sum.bases), StaleRows: int64(info.StaleRows), //nolint:gosec // an hour-row count
		AccrualExcluded: sums.UnorderedVersions + sums.AccrualErrors,
	}
	if sums.UnexplainedSupplyLoss.Sign() < 0 {
		row.UnexplainedLoss = sums.UnexplainedSupplyLossVersions
	}
	if d.reason != "" {
		row.WalkReason = strRef(d.reason)
	}
	legs, ok := d.storedLegs()
	copy(row.Legs[:], legs)
	return row, ok
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
	if a.HoursValued != b.HoursValued || a.HoursUnvalued != b.HoursUnvalued || a.HoursCarried != b.HoursCarried ||
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
