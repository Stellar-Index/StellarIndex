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
			revenueReasonRedoPending, revenueReasonAccrual, revenueReasonSupplyLoss, revenueReasonDrawnPartial,
		}},
		{[]string{"LendingRevenueExclusion", "properties", "kind"}, []string{revenueKindPool, revenueKindWindow, revenueKindReserve, revenueKindField}},
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

// completeRevenue is a covered pool whose read names nothing but the always-present drawn lower bound.
func completeRevenue() clickhouse.LendingRevenue {
	return clickhouse.LendingRevenue{
		Covered: true, FloorLedger: 10, FloorTime: time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC),
		ThroughLedger: 900, ThroughHour: time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC),
		Reserves: []clickhouse.LendingRevenueReserve{{Asset: "CASSET", Buckets: []clickhouse.LendingRevenueBucket{
			{Start: revenueTestQuery.From, Sums: revenueSums(100)},
		}}},
	}
}

// Each lower-bound trigger fires alone: from a read naming only interest_drawn_partial, one change adds exactly one reason.
func TestRevenueView_EachLowerBoundTriggerFiresAlone(t *testing.T) {
	base := revenueView("CPOOL", "blend", "1d", revenueTestQuery, completeRevenue())
	if len(base.Excluded) != 1 || base.Excluded[0].Reason != revenueReasonDrawnPartial || !base.LowerBound {
		t.Fatalf("complete read: lower_bound %v, excluded %+v", base.LowerBound, base.Excluded)
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
			s.UnexplainedSupplyVersions = 2
		}, revenueReasonSupplyLoss, revenueKindReserve},
	}
	for _, tc := range cases {
		rev := completeRevenue()
		tc.mut(&rev)
		v := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev)
		var got []LendingRevenueExclusion
		for _, e := range v.Excluded {
			if e.Reason != revenueReasonDrawnPartial {
				got = append(got, e)
			}
		}
		if !v.LowerBound || len(got) != 1 || got[0].Reason != tc.reason || got[0].Kind != tc.kind {
			t.Errorf("%s: lower_bound %v, excluded %+v", tc.name, v.LowerBound, v.Excluded)
		}
	}
	// The supply loss names its amount and versions; a stale count names its rows.
	rev := completeRevenue()
	rev.Reserves[0].StaleRows = 3
	rev.Reserves[0].Buckets[0].Sums.UnexplainedSupplyLoss.SetInt64(-7)
	rev.Reserves[0].Buckets[0].Sums.UnexplainedSupplyVersions = 2
	for _, e := range revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev).Excluded {
		switch e.Reason {
		case revenueReasonSupplyLoss:
			if *e.Amount != "-7" || *e.Count != 2 || *e.Asset != "CASSET" {
				t.Errorf("supply loss = %+v", e)
			}
		case revenueReasonDeriveStale:
			if *e.Count != 3 || e.Amount != nil {
				t.Errorf("stale = %+v", e)
			}
		}
	}
}

func TestRevenueView_FeeAndRealised(t *testing.T) {
	rev := completeRevenue()
	s := &rev.Reserves[0].Buckets[0].Sums
	s.InterestDrawn.SetInt64(-40)
	s.ProtocolFee.SetInt64(2)
	for _, tc := range []struct{ protocol, reason string }{{"blend", revenueFeeNotApplicable}, {blnt.SourceName, revenueFeeNotDerivable}} {
		r := revenueView("CPOOL", tc.protocol, "1d", revenueTestQuery, rev).Reserves[0]
		if r.Totals.Realised.FeeWithdrawn != nil || r.Totals.Realised.FeeWithdrawnReason != tc.reason || r.Totals.Realised.BackstopDrawn != "40" ||
			r.FeeRateInferred != (tc.protocol == blnt.SourceName) || r.Totals.ProtocolRevenue != "22" {
			t.Errorf("%s: %+v", tc.protocol, r)
		}
	}
}

// Property: every reserve's totals are the exact sum of its served points, field by field, at Int256 width; and
// interest_accrued = supplier_share + backstop_take + protocol_fee holds in every served bucket and total.
func TestRevenueView_TotalsAreTheSumOfPoints(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	big2 := func() *big.Int {
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
	for range 200 {
		rev := completeRevenue()
		rev.Reserves = nil
		for a := range 1 + rng.IntN(3) {
			res := clickhouse.LendingRevenueReserve{Asset: string(rune('A' + a))}
			for b := range rng.IntN(30) {
				s := clickhouse.NewLendingRevenueSums()
				s.Gross, s.ProtocolFee, s.Backstop = big2(), big2(), big2()
				s.SupplierIntended = new(big.Int).Sub(new(big.Int).Sub(s.Gross, s.ProtocolFee), s.Backstop)
				s.SupplierRealised, s.InterestDrawn, s.DefaultLoss, s.UnexplainedSupplyLoss = big2(), big2(), big2(), big2()
				s.UnorderedVersions, s.DefaultVersions = rng.Int64N(5), rng.Int64N(5)
				res.Buckets = append(res.Buckets, clickhouse.LendingRevenueBucket{Start: revenueTestQuery.From.Add(time.Duration(b) * 24 * time.Hour), Sums: s})
			}
			rev.Reserves = append(rev.Reserves, res)
		}
		for _, r := range revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev).Reserves {
			fields := func(a LendingRevenueAmounts) []string {
				return []string{
					a.InterestAccrued, a.SupplierShare, a.SupplierCredited, a.BackstopTake, a.ProtocolFee, a.ProtocolRevenue,
					a.Realised.BackstopDrawn, a.DefaultLoss, a.UnexplainedSupplyLoss,
				}
			}
			sum := make([]*big.Int, len(fields(r.Totals)))
			for i := range sum {
				sum[i] = new(big.Int)
			}
			var unordered, defaults int64
			for _, p := range append(slices.Clone(r.Points), LendingRevenuePoint{LendingRevenueAmounts: r.Totals}) {
				split := new(big.Int).Add(new(big.Int).Add(parse(p.SupplierShare), parse(p.BackstopTake)), parse(p.ProtocolFee))
				if split.Cmp(parse(p.InterestAccrued)) != 0 {
					t.Fatalf("%s at %v: interest_accrued %s != split %s", r.Asset, p.T, p.InterestAccrued, split)
				}
			}
			for _, p := range r.Points {
				for i, f := range fields(p.LendingRevenueAmounts) {
					sum[i].Add(sum[i], parse(f))
				}
				unordered += p.UnorderedVersions
				defaults += p.DefaultVersions
			}
			for i, f := range fields(r.Totals) {
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
