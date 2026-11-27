package v1

import (
	"context"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// RevenueExactForTest is one reserve's exact USD: the seven legs (nil when unvalued) and its hour counts.
type RevenueExactForTest struct {
	Legs                      [revenueUSDLegCount]*big.Rat
	Priced, Unpriced, Carried int
	Outcome                   string // valued | degraded | refused (day valuations only)
	Reason                    string
}

func exactOf(t revenueUSDSum) RevenueExactForTest {
	return RevenueExactForTest{Legs: t.v, Priced: t.priced, Unpriced: t.unpriced, Carried: t.carried, Outcome: "valued"}
}

// RevenueDayForTest values one reserve-day exactly as the rollup does.
func (s *Server) RevenueDayForTest(ctx context.Context, asset string, day time.Time, hours []clickhouse.LendingRevenueBucket) RevenueExactForTest {
	d := valueRevenueDay(day, hours, func() reserveUSDRead { return s.revenueDayRead(ctx, asset, day, lendingRevenueDayWalkBudget) })
	out := exactOf(d.sum)
	out.Reason = d.reason
	switch d.outcome {
	case revenueDayValued:
	case revenueDayDegraded:
		out.Outcome = "degraded"
	case revenueDayRefused:
		out.Outcome = "refused"
	}
	return out
}

// RevenueWindowForTest values rev as GET /revenue does (interval 1d) and returns each reserve's exact totals.
func (s *Server) RevenueWindowForTest(ctx context.Context, pool string, q clickhouse.LendingHistoryQuery, rev clickhouse.LendingRevenue) []RevenueExactForTest {
	view := revenueView(pool, "", "1d", q, rev)
	series := make([]reserveUSDSeries, len(view.Reserves))
	for i, rv := range view.Reserves {
		sc := s.reserveDecimals(ctx, clickhouse.BlendReserveState{Asset: rv.Asset})
		view.Reserves[i].Decimals = sc.wire()
		first, last, has := revenueValuedSpan(rev.Reserves[i])
		if rv.Totals == nil || !has {
			continue
		}
		series[i] = s.reserveUSDSeriesFor(ctx, rv.Asset, sc, clickhouse.LendingHistoryQuery{From: first, To: last})
	}
	_, reserves := valueRevenueView(&view, rev, series)
	out := make([]RevenueExactForTest, len(reserves))
	for i, r := range reserves {
		out[i] = exactOf(r)
	}
	return out
}
