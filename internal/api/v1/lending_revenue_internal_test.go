package v1

import (
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestLendingRevenueExclusionReasonEnumMatchesHandler(t *testing.T) {
	doc := loadSpecDoc(t)
	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"LendingRevenueExclusion", "properties", "reason"}, []string{
			revenueReasonNotDerived, revenueReasonDeriveStale,
			revenueReasonRedoPending, revenueReasonAccrual, revenueReasonSupplyLoss,
		}},
		{[]string{"LendingRevenueExclusion", "properties", "kind"}, []string{revenueKindPool, revenueKindWindow, revenueKindReserve}},
		{
			[]string{"LendingRevenueAmounts", "properties", "realised", "properties", "fee_withdrawn_reason"},
			[]string{revenueFeeNotDerivable, revenueFeeNotApplicable},
		},
	} {
		got := specEnumAt(t, doc, append([]string{"components", "schemas"}, tc.path...)...)
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%v enum = %v, want what the handler emits %v", tc.path, got, tc.want)
		}
	}
}

var revenueTestQuery = clickhouse.LendingHistoryQuery{
	From: time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC), Daily: true, MaxPoints: 2000,
}

func revenueSums(gross int64) clickhouse.LendingRevenueSums {
	s := clickhouse.NewLendingRevenueSums()
	s.Gross.SetInt64(gross)
	s.Backstop.SetInt64(gross / 5)
	s.SupplierIntended.SetInt64(gross - gross/5)
	s.SupplierRealised.Set(s.SupplierIntended)
	return s
}

// completeRevenue is a covered pool whose read names nothing.
func completeRevenue() clickhouse.LendingRevenue {
	return clickhouse.LendingRevenue{
		Covered: true, FloorLedger: 10, FloorTime: time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC),
		ThroughLedger: 900, ThroughHour: time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC), Before: revenueTestQuery.To,
		Reserves: []clickhouse.LendingRevenueReserve{{Asset: "CASSET", Buckets: []clickhouse.LendingRevenueBucket{
			{Start: revenueTestQuery.From, Sums: revenueSums(100)},
		}}},
	}
}

// Each lower-bound trigger fires alone: from a clean read (lower_bound false), one change adds exactly one reason.
func TestRevenueView_EachLowerBoundTriggerFiresAlone(t *testing.T) {
	base := revenueView("CPOOL", "blend", "1d", revenueTestQuery, completeRevenue())
	if len(base.Excluded) != 0 || base.LowerBound || base.Reserves[0].LowerBound || base.Reserves[0].Totals == nil ||
		!base.Reserves[0].Totals.Realised.BackstopDrawnLowerBound {
		t.Fatalf("complete read: %+v", base)
	}
	cases := []struct {
		name   string
		mut    func(*clickhouse.LendingRevenue)
		reason string
		kind   string
	}{
		{"pool not covered", func(r *clickhouse.LendingRevenue) { *r = clickhouse.LendingRevenue{} }, revenueReasonNotDerived, revenueKindPool},
		{"window before the floor", func(r *clickhouse.LendingRevenue) { r.FloorTime = revenueTestQuery.From.Add(time.Second) }, revenueReasonNotDerived, revenueKindWindow},
		{"window past coverage", func(r *clickhouse.LendingRevenue) { r.ThroughHour = revenueTestQuery.From.Add(5 * time.Hour) }, revenueReasonNotDerived, revenueKindWindow},
		{"stale derive", func(r *clickhouse.LendingRevenue) { r.Reserves[0].StaleRows = 3 }, revenueReasonDeriveStale, revenueKindReserve},
		{"redo pending", func(r *clickhouse.LendingRevenue) {
			r.Redo, r.RedoHour = &clickhouse.LendingInterestRedo{Gen: 1, From: 500}, revenueTestQuery.From.Add(time.Hour)
		}, revenueReasonRedoPending, revenueKindWindow},
		{"unordered versions", func(r *clickhouse.LendingRevenue) { r.Reserves[0].Buckets[0].Sums.UnorderedVersions = 1 }, revenueReasonAccrual, revenueKindReserve},
		{"accrual errors", func(r *clickhouse.LendingRevenue) { r.Reserves[0].Buckets[0].Sums.AccrualErrors = 2 }, revenueReasonAccrual, revenueKindReserve},
		{"unexplained supply loss", func(r *clickhouse.LendingRevenue) {
			s := &r.Reserves[0].Buckets[0].Sums
			s.UnexplainedSupplyLoss.SetInt64(-7)
			s.UnexplainedSupplyLossVersions = 2
		}, revenueReasonSupplyLoss, revenueKindReserve},
	}
	for _, tc := range cases {
		rev := completeRevenue()
		tc.mut(&rev)
		v := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev)
		if !v.LowerBound || len(v.Excluded) != 1 || v.Excluded[0].Reason != tc.reason || v.Excluded[0].Kind != tc.kind {
			t.Errorf("%s: lower_bound %v, excluded %+v", tc.name, v.LowerBound, v.Excluded)
		}
		for _, r := range v.Reserves {
			if !r.LowerBound || r.Totals == nil || r.Totals.InterestAccrued != "100" {
				t.Errorf("%s: reserve %+v: a partial reserve is a lower bound with its summed totals", tc.name, r)
			}
		}
	}
	rev := completeRevenue()
	rev.Reserves[0].StaleRows = 3
	rev.Reserves[0].Buckets[0].Sums.UnexplainedSupplyLoss.SetInt64(-7)
	rev.Reserves[0].Buckets[0].Sums.UnexplainedSupplyLossVersions = 2
	for _, e := range revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev).Excluded {
		switch e.Reason {
		case revenueReasonSupplyLoss:
			if *e.Amount != "7" || *e.Count != 2 || *e.Asset != "CASSET" {
				t.Errorf("supply loss = %+v", e)
			}
		case revenueReasonDeriveStale:
			if *e.Count != 3 || e.Amount != nil {
				t.Errorf("stale = %+v", e)
			}
		}
	}
}

// A number is never "0" because rows were left out: a reserve whose summed rows are nothing (all stale, or only
// accrual-refused) has null totals; a clean reserve with no interest keeps its true "0".
func TestRevenueView_ExcludedReserveIsNeverZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		res   clickhouse.LendingRevenueReserve
		null  bool
		bound bool
	}{
		{"every row stale", clickhouse.LendingRevenueReserve{Asset: "CA", StaleRows: 4}, true, true},
		{"only accrual-refused", clickhouse.LendingRevenueReserve{Asset: "CA", Buckets: []clickhouse.LendingRevenueBucket{
			{Start: revenueTestQuery.From, Sums: func() clickhouse.LendingRevenueSums {
				s := clickhouse.NewLendingRevenueSums()
				s.AccrualErrors = 1
				return s
			}()},
		}}, true, true},
		{"clean, no interest", clickhouse.LendingRevenueReserve{Asset: "CA", Buckets: []clickhouse.LendingRevenueBucket{
			{Start: revenueTestQuery.From, Sums: clickhouse.NewLendingRevenueSums()},
		}}, false, false},
	} {
		rev := completeRevenue()
		rev.Reserves = []clickhouse.LendingRevenueReserve{tc.res}
		r := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev).Reserves[0]
		if (r.Totals == nil) != tc.null || r.LowerBound != tc.bound {
			t.Errorf("%s: totals %+v, lower_bound %v", tc.name, r.Totals, r.LowerBound)
		}
	}
	// Under a window exclusion (here a pending redo) a zero-interest reserve is null too: its zero may be the cut hours.
	rev := completeRevenue()
	rev.Redo, rev.RedoHour, rev.Before = &clickhouse.LendingInterestRedo{Gen: 1}, revenueTestQuery.From.Add(time.Hour), revenueTestQuery.From.Add(time.Hour)
	rev.Reserves[0].Buckets[0].Sums = clickhouse.NewLendingRevenueSums()
	if r := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev).Reserves[0]; r.Totals != nil || !r.LowerBound || !r.Points[0].Partial {
		t.Errorf("redo-clipped zero reserve: %+v", r)
	}
}

// Losses are served as magnitudes, so a partial sum of them is a lower bound in the same direction as every amount.
func TestRevenueView_LossesAreMagnitudes(t *testing.T) {
	rev := completeRevenue()
	s := &rev.Reserves[0].Buckets[0].Sums
	s.DefaultLoss.SetInt64(-5)
	s.ReconcileBackstopLoss.SetInt64(-3)
	s.UnexplainedSupplyLoss.SetInt64(-7)
	s.UnexplainedSupplyGain.SetInt64(4)
	s.InterestDrawn.SetInt64(-40)
	s.Gulped.SetInt64(6)
	tot := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev).Reserves[0].Totals
	if tot.DefaultLoss != "5" || tot.ReconcileBackstopLoss != "3" || tot.UnexplainedSupplyLoss != "7" || tot.UnexplainedSupplyGain != "4" ||
		tot.Realised.BackstopDrawn != "40" || tot.BackstopGulped != "6" {
		t.Fatalf("signs = %+v", tot)
	}
}

func TestRevenueView_FeeAndPartialBucket(t *testing.T) {
	rev := completeRevenue()
	rev.Reserves[0].Buckets[0].Sums.ProtocolFee.SetInt64(2)
	for _, tc := range []struct{ protocol, reason string }{{"blend", revenueFeeNotApplicable}, {blnt.SourceName, revenueFeeNotDerivable}} {
		r := revenueView("CPOOL", tc.protocol, "1d", revenueTestQuery, rev).Reserves[0]
		if r.Totals.Realised.FeeWithdrawn != nil || r.Totals.Realised.FeeWithdrawnReason != tc.reason || r.FeeRateInferred != (tc.protocol == blnt.SourceName) {
			t.Errorf("%s: %+v", tc.protocol, r)
		}
	}
	// A daily bucket cut by a mid-day `to` is partial; one wholly inside is not.
	q := revenueTestQuery
	q.To = q.From.Add(36 * time.Hour)
	rev.Before = q.To
	rev.Reserves[0].Buckets = append(rev.Reserves[0].Buckets, clickhouse.LendingRevenueBucket{Start: q.From.Add(24 * time.Hour), Sums: revenueSums(1)})
	if p := revenueView("CPOOL", "blend", "1d", q, rev).Reserves[0].Points; p[0].Partial || !p[1].Partial {
		t.Errorf("partial = %v, %v", p[0].Partial, p[1].Partial)
	}
}

// The interest watermark moves only at hour ends: stale only past the hour after the coverage, one derive run and the grace.
func TestRevenueStale_HourlyWatermarkEdge(t *testing.T) {
	through := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	edge := through.Add(time.Hour + lendingInterestDeriveCadence + lendingRevenueStaleGrace)
	if revenueStale(through, edge) || revenueStale(through, through.Add(time.Hour+41*time.Minute)) || !revenueStale(through, edge.Add(time.Second)) {
		t.Fatal("stale edge misplaced")
	}
}

// Property: every reserve's totals are the exact sum of its served points, field by field, at Int256 width.
func TestRevenueView_TotalsAreTheSumOfPoints(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	wide := func() *big.Int {
		v := new(big.Int).Lsh(big.NewInt(rng.Int64N(1<<40)), uint(rng.IntN(150)))
		if rng.IntN(4) == 0 {
			v.Neg(v)
		}
		return v
	}
	parse := func(s string) *big.Int {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			t.Fatalf("not a decimal: %q", s)
		}
		return v
	}
	fields := func(a LendingRevenueAmounts) []string {
		return []string{
			a.InterestAccrued, a.SupplierShare, a.SupplierCredited, a.BackstopTake, a.ProtocolFee,
			a.Realised.BackstopDrawn, a.DefaultLoss, a.ReconcileBackstopLoss, a.UnexplainedSupplyLoss, a.UnexplainedSupplyGain,
			a.UnexplainedBackstopResidual, a.BackstopGulped,
		}
	}
	for range 200 {
		rev := completeRevenue()
		rev.Reserves = nil
		for a := range 1 + rng.IntN(3) {
			res := clickhouse.LendingRevenueReserve{Asset: string(rune('A' + a))}
			for b := range 1 + rng.IntN(30) {
				s := clickhouse.NewLendingRevenueSums()
				s.Gross, s.SupplierIntended, s.SupplierRealised, s.Backstop, s.ProtocolFee = wide(), wide(), wide(), wide(), wide()
				s.InterestDrawn, s.DefaultLoss, s.ReconcileBackstopLoss, s.UnexplainedSupplyLoss = wide(), wide(), wide(), wide()
				s.UnexplainedSupplyGain, s.UnexplainedBackstopResidual, s.Gulped = wide(), wide(), wide()
				s.UnorderedVersions, s.DefaultVersions = rng.Int64N(5), rng.Int64N(5)
				res.Buckets = append(res.Buckets, clickhouse.LendingRevenueBucket{Start: revenueTestQuery.From.Add(time.Duration(b) * 24 * time.Hour), Sums: s})
			}
			rev.Reserves = append(rev.Reserves, res)
		}
		for _, r := range revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev).Reserves {
			if r.Totals == nil {
				continue // a null total has no sum to check; TestRevenueView_ExcludedReserveIsNeverZero pins when
			}
			sum := make([]*big.Int, len(fields(*r.Totals)))
			for i := range sum {
				sum[i] = new(big.Int)
			}
			var unordered, defaults int64
			for _, p := range r.Points {
				for i, f := range fields(p.LendingRevenueAmounts) {
					sum[i].Add(sum[i], parse(f))
				}
				unordered += p.UnorderedVersions
				defaults += p.DefaultVersions
			}
			for i, f := range fields(*r.Totals) {
				if sum[i].Cmp(parse(f)) != 0 {
					t.Fatalf("%s field %d: total %s, points sum %s", r.Asset, i, f, sum[i])
				}
			}
			if unordered != r.Totals.UnorderedVersions || defaults != r.Totals.DefaultVersions {
				t.Fatalf("%s: counts total %+v, points %d/%d", r.Asset, r.Totals, unordered, defaults)
			}
		}
	}
}
