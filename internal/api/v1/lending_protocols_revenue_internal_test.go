package v1

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var revT = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func revDay(back int) time.Time { return revT.Add(-time.Duration(back) * day24) }

func revCov(pool string, floorBack, throughBack int) timescale.LendingRevenueUSDCoverage {
	return timescale.LendingRevenueUSDCoverage{
		Pool: pool, FloorDay: revDay(floorBack), ThroughDay: revDay(throughBack),
		ValuationRev: lendingRevenueValuationRev, InterestDeriveRev: int16(lendinginterest.DeriveRev), //nolint:gosec // a small revision number
	}
}

// revGroup is one current, fully priced group whose seven legs are all leg.
func revGroup(pool, asset string, bucket int, leg string) timescale.LendingRevenueUSDBucketSum {
	g := timescale.LendingRevenueUSDBucketSum{Pool: pool, Asset: asset, Bucket: bucket, Current: true, Days: 1}
	for i := range g.Legs {
		g.Legs[i] = strRef(leg)
	}
	return g
}

func revIn(pools []revenuePool, covs []timescale.LendingRevenueUSDCoverage, sums ...timescale.LendingRevenueUSDBucketSum) protocolRevenueInput {
	m := map[string]timescale.LendingRevenueUSDCoverage{}
	for _, c := range covs {
		m[c.Pool] = c
	}
	return protocolRevenueInput{versioned: true, pools: pools, covs: m, sums: sums, verdicts: map[string]string{}}
}

func revExcl(t *testing.T, w LendingProtocolRevenueWindow, pool, asset, reason string, count int64) {
	t.Helper()
	for _, e := range w.Excluded {
		if deref(e.Pool) == pool && e.Reason == reason && (asset == "" && e.Asset == nil || e.Asset != nil && *e.Asset == asset) {
			if count >= 0 && (e.Count == nil || *e.Count != count) {
				t.Fatalf("%s %s %s count = %v; want %d", pool, asset, reason, e.Count, count)
			}
			if !w.LowerBound {
				t.Fatalf("%s named but lower_bound false", reason)
			}
			return
		}
	}
	t.Fatalf("no %s exclusion for %s %s in %+v", reason, pool, asset, w.Excluded)
}

var v2Pool = []revenuePool{{pool: "CP2", version: "v2"}}

func TestProtocolRevenue_EmptyTableIsNullNotZero(t *testing.T) {
	out := foldProtocolRevenue(revIn([]revenuePool{{pool: "CP1", version: "v1"}, {pool: "CP2", version: "v2"}}, nil))
	for _, w := range []LendingProtocolRevenueWindow{out.Day, out.Week, out.Month, out.AllTime} {
		if w.InterestAccruedUSD != nil || w.DefaultLossUSD != nil || w.ThroughDay != nil || deref(w.USDReason) != protocolRevenueNotValued {
			t.Fatalf("empty table served %+v; want null amounts, not_valued", w.LendingProtocolRevenueAmounts)
		}
		revExcl(t, w, "CP1", "", protocolRevenueNotValued, -1)
		revExcl(t, w, "CP2", "", protocolRevenueNotValued, -1)
		if w.Versions == nil || w.Versions.V1.InterestAccruedUSD != nil || deref(w.Versions.V2.USDReason) != protocolRevenueNotValued {
			t.Fatalf("versions = %+v; want null with not_valued", w.Versions)
		}
	}
}

// Inside coverage a day with no row is a day with nothing to value, so a covered window holding no row is an exact zero,
// not not_valued: the rollup is what keeps that true (TestLendingRevenueRollup_UnvaluedHoursHoldCoverage).
func TestProtocolRevenue_CoveredWindowWithoutRowsIsZero(t *testing.T) {
	out := foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{revCov("CP2", 40, 0)}))
	for _, w := range []LendingProtocolRevenueWindow{out.Day, out.Week, out.Month, out.AllTime} {
		if deref(w.InterestAccruedUSD) != "0.00" || w.USDReason != nil || w.LowerBound || len(w.Excluded) != 0 {
			t.Fatalf("covered window with no rows served %+v, excluded %+v; want an exact 0.00", w.LendingProtocolRevenueAmounts, w.Excluded)
		}
	}
}

// Coverage written at an older valuation or derive revision vouches for nothing: the pool reads not_valued, never 0.00.
func TestProtocolRevenue_OldRevisionCoverageIsNotCovered(t *testing.T) {
	for name, set := range map[string]func(c *timescale.LendingRevenueUSDCoverage){
		"valuation": func(c *timescale.LendingRevenueUSDCoverage) { c.ValuationRev = lendingRevenueValuationRev - 1 },
		"derive":    func(c *timescale.LendingRevenueUSDCoverage) { c.InterestDeriveRev-- },
	} {
		cov := revCov("CP2", 40, 0)
		set(&cov)
		out := foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{cov}))
		if w := out.Day; w.InterestAccruedUSD != nil || deref(w.USDReason) != protocolRevenueNotValued {
			t.Fatalf("%s: old-revision coverage served %+v; want null, not_valued", name, w.LendingProtocolRevenueAmounts)
		}
		revExcl(t, out.Day, "CP2", "", protocolRevenueNotValued, -1)
	}
}

func TestProtocolRevenue_CompleteWindowsAndBuckets(t *testing.T) {
	out := foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{revCov("CP2", 40, 0)},
		revGroup("CP2", "CA", 0, "1.005"), revGroup("CP2", "CA", 1, "2"), revGroup("CP2", "CA", 2, "3"), revGroup("CP2", "CA", 3, "4")))
	for _, c := range []struct {
		w          LendingProtocolRevenueWindow
		want, from string
	}{
		{out.Day, "1.01", "2026-11-20"}, {out.Week, "3.01", "2026-11-14"}, {out.Month, "6.01", "2026-10-22"}, {out.AllTime, "10.01", "2026-10-11"},
	} {
		if c.w.LowerBound || len(c.w.Excluded) != 0 || deref(c.w.InterestAccruedUSD) != c.want || deref(c.w.ThroughDay) != "2026-11-20" || deref(c.w.FromDay) != c.from {
			t.Fatalf("window %s..%s = %s lower=%v %+v; want %s..2026-11-20 = %s complete",
				deref(c.w.FromDay), deref(c.w.ThroughDay), deref(c.w.InterestAccruedUSD), c.w.LowerBound, c.w.Excluded, c.from, c.want)
		}
		if !c.w.BackstopDrawnLowerBound {
			t.Fatal("backstop_drawn_lower_bound must always be true")
		}
	}
}

// The window ends at the latest valued day of any pool; a pool valued through an earlier one is named for each day it lacks.
func TestProtocolRevenue_MixedThroughDays(t *testing.T) {
	pools := []revenuePool{{pool: "CP1", version: "v1"}, {pool: "CP2", version: "v2"}}
	out := foldProtocolRevenue(revIn(pools, []timescale.LendingRevenueUSDCoverage{revCov("CP1", 40, 2), revCov("CP2", 40, 0)},
		revGroup("CP1", "CA", 1, "5"), revGroup("CP2", "CA", 0, "1")))
	if deref(out.Day.ThroughDay) != "2026-11-20" || deref(out.Day.InterestAccruedUSD) != "1.00" {
		t.Fatalf("1d = %s through %s; want 1.00 through 2026-11-20", deref(out.Day.InterestAccruedUSD), deref(out.Day.ThroughDay))
	}
	revExcl(t, out.Day, "CP1", "", protocolRevenueDaysNotValued, 1)
	revExcl(t, out.Week, "CP1", "", protocolRevenueDaysNotValued, 2)
	if deref(out.Week.InterestAccruedUSD) != "6.00" || !out.Week.Versions.V1.LowerBound || out.Week.Versions.V2.LowerBound {
		t.Fatalf("7d = %s, v1 lower %v, v2 lower %v; want 6.00, only v1 a lower bound",
			deref(out.Week.InterestAccruedUSD), out.Week.Versions.V1.LowerBound, out.Week.Versions.V2.LowerBound)
	}
}

func TestProtocolRevenue_WindowBeforeFloor(t *testing.T) {
	out := foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{revCov("CP2", 9, 0)}, revGroup("CP2", "CA", 1, "1")))
	if out.Week.LowerBound {
		t.Fatalf("7d inside coverage is a lower bound: %+v", out.Week.Excluded)
	}
	revExcl(t, out.Month, "CP2", "", protocolRevenueDaysNotValued, 20)
	if out.AllTime.LowerBound || deref(out.AllTime.FromDay) != "2026-11-11" {
		t.Fatalf("all_time from %s lower %v; want from the floor, complete", deref(out.AllTime.FromDay), out.AllTime.LowerBound)
	}
}

func TestProtocolRevenue_StaleRevisionExcludedNotSummed(t *testing.T) {
	stale := revGroup("CP2", "CA", 0, "100")
	stale.Current, stale.Days = false, 3
	out := foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{revCov("CP2", 40, 0)}, stale, revGroup("CP2", "CB", 0, "1")))
	revExcl(t, out.Day, "CP2", "CA", protocolRevenueStale, 3)
	if deref(out.Day.InterestAccruedUSD) != "1.00" {
		t.Fatalf("1d = %s; a stale row was summed", deref(out.Day.InterestAccruedUSD))
	}
	only := foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{revCov("CP2", 40, 0)}, stale))
	if only.Day.InterestAccruedUSD != nil || deref(only.Day.USDReason) != protocolRevenueAllExcluded {
		t.Fatalf("only stale rows served %s (%s); want null, all_excluded", deref(only.Day.InterestAccruedUSD), deref(only.Day.USDReason))
	}
}

func TestProtocolRevenue_RowLevelTriggers(t *testing.T) {
	cov := []timescale.LendingRevenueUSDCoverage{revCov("CP2", 40, 0)}
	missing := revGroup("CP2", "CA", 0, "1")
	missing.HoursUnvalued = 4
	withheld := timescale.LendingRevenueUSDBucketSum{Pool: "CP2", Asset: "CB", Bucket: 0, Current: true, Days: 2, WithheldDays: 2}
	partial := revGroup("CP2", "CC", 0, "1")
	partial.ExcludedCount = 5
	for name, c := range map[string]struct {
		g             timescale.LendingRevenueUSDBucketSum
		asset, reason string
		count         int64
		want          string
	}{
		"price_missing":    {missing, "CA", revenueUSDPriceMissing, 4, "1.00"},
		"withheld":         {withheld, "CB", exclusionPriceWithheld, 2, ""},
		"amounts_excluded": {partial, "CC", revenueUSDAmountsExcluded, 5, "1.00"},
	} {
		t.Run(name, func(t *testing.T) {
			out := foldProtocolRevenue(revIn(v2Pool, cov, c.g))
			revExcl(t, out.Day, "CP2", c.asset, c.reason, c.count)
			if deref(out.Day.InterestAccruedUSD) != c.want {
				t.Fatalf("1d = %s; want %s", deref(out.Day.InterestAccruedUSD), c.want)
			}
		})
	}
}

func TestProtocolRevenue_RedoPending(t *testing.T) {
	c := revCov("CP2", 40, 0)
	c.RedoFrom = revDay(-1)
	out := foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{c}, revGroup("CP2", "CA", 0, "1")))
	if out.Day.LowerBound {
		t.Fatal("a redo after through_day made the window a lower bound")
	}
	c.RedoFrom = revDay(0)
	out = foldProtocolRevenue(revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{c}, revGroup("CP2", "CA", 0, "1")))
	revExcl(t, out.Day, "CP2", "", revenueReasonRedoPending, -1)
}

// A reserve a read-time verdict refuses now contributes nothing to any window, however it was stored, and is named.
func TestProtocolRevenue_RefusedReserveNamed(t *testing.T) {
	in := revIn(v2Pool, []timescale.LendingRevenueUSDCoverage{revCov("CP2", 40, 0)}, revGroup("CP2", "CSCAM", 0, "50"), revGroup("CP2", "CA", 0, "1"))
	in.verdicts["CSCAM"] = exclusionPriceWithheld
	out := foldProtocolRevenue(in)
	revExcl(t, out.Day, "CP2", "CSCAM", exclusionPriceWithheld, 1)
	if deref(out.AllTime.InterestAccruedUSD) != "1.00" {
		t.Fatalf("all_time = %s; the refused reserve was summed", deref(out.AllTime.InterestAccruedUSD))
	}
}

func TestProtocolRevenue_SkippedPoolsAndBLNT(t *testing.T) {
	in := revIn([]revenuePool{{pool: "CP1", skip: exclusionPoolReadFailed}, {pool: "CP3", skip: protocolRevenueVersionUnknown}},
		[]timescale.LendingRevenueUSDCoverage{revCov("CP1", 40, 0), revCov("CP3", 40, 0)}, revGroup("CP1", "CA", 0, "9"))
	out := foldProtocolRevenue(in)
	revExcl(t, out.Day, "CP1", "", exclusionPoolReadFailed, -1)
	revExcl(t, out.Day, "CP3", "", protocolRevenueVersionUnknown, -1)
	if out.Day.InterestAccruedUSD != nil {
		t.Fatalf("a skipped pool was summed: %s", deref(out.Day.InterestAccruedUSD))
	}
	b := revIn([]revenuePool{{pool: "CB1"}}, []timescale.LendingRevenueUSDCoverage{revCov("CB1", 40, 0)}, revGroup("CB1", "CA", 0, "2"))
	b.fee, b.versioned = true, false
	bo := foldProtocolRevenue(b)
	if !bo.FeeRateInferred || bo.Day.Versions != nil || deref(bo.Day.ProtocolFeeUSD) != "2.00" {
		t.Fatalf("BLNT: fee_rate_inferred %v, versions %+v, fee %s", bo.FeeRateInferred, bo.Day.Versions, deref(bo.Day.ProtocolFeeUSD))
	}
}

// Property: V1 + V2 is the window exactly, and all_time is the exact sum of every stored leg rounded once.
func TestProtocolRevenue_VersionsSumAndAllTimeExact(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for iter := range 200 {
		pools := []revenuePool{{pool: "CP1", version: "v1"}, {pool: "CP2", version: "v2"}, {pool: "CP3", version: "v2"}}
		covs := []timescale.LendingRevenueUSDCoverage{revCov("CP1", 60, 0), revCov("CP2", 60, 0), revCov("CP3", 60, 0)}
		var sums []timescale.LendingRevenueUSDBucketSum
		exact, cents := new(big.Rat), [2]*big.Rat{new(big.Rat), new(big.Rat)}
		for _, p := range pools {
			for _, a := range []string{"CA", "CB"} {
				for b := range 4 {
					fine := fmt.Sprintf("%d.%07d", r.IntN(1e6), r.IntN(1e7))
					sums = append(sums, revGroup(p.pool, a, b, fine))
					x, _ := new(big.Rat).SetString(fine)
					exact.Add(exact, x)
				}
			}
		}
		out := foldProtocolRevenue(revIn(pools, covs, sums...))
		if got, want := deref(out.AllTime.InterestAccruedUSD), ratString(exact, 2); got != want {
			t.Fatalf("iter %d: all_time %s; want exact sum %s", iter, got, want)
		}
		// With cent-exact legs rounding is the identity, so the versions must add up to the window to the cent.
		for i := range sums {
			c := fmt.Sprintf("%d.%02d", r.IntN(1e6), r.IntN(100))
			for j := range sums[i].Legs {
				sums[i].Legs[j] = strRef(c)
			}
			x, _ := new(big.Rat).SetString(c)
			v := 1
			if sums[i].Pool == "CP1" {
				v = 0
			}
			cents[v].Add(cents[v], x)
		}
		out = foldProtocolRevenue(revIn(pools, covs, sums...))
		w := out.AllTime
		v1, _ := new(big.Rat).SetString(deref(w.Versions.V1.InterestAccruedUSD))
		v2, _ := new(big.Rat).SetString(deref(w.Versions.V2.InterestAccruedUSD))
		tot, _ := new(big.Rat).SetString(deref(w.InterestAccruedUSD))
		if new(big.Rat).Add(v1, v2).Cmp(tot) != 0 || v1.Cmp(cents[0]) != 0 || v2.Cmp(cents[1]) != 0 {
			t.Fatalf("iter %d: v1 %s + v2 %s != total %s", iter, v1.FloatString(2), v2.FloatString(2), tot.FloatString(2))
		}
	}
}

func TestProtocolRevenueEnumsMatchHandler(t *testing.T) {
	doc := loadSpecDoc(t)
	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"LendingProtocolRevenueAmounts", "properties", "usd_reason"}, []string{protocolRevenueNotValued, protocolRevenueAllExcluded}},
		{[]string{"LendingProtocolRevenueExclusion", "properties", "reason"}, []string{
			protocolRevenueNotValued, protocolRevenueDaysNotValued, revenueReasonRedoPending, protocolRevenueStale, revenueUSDPriceMissing,
			exclusionPriceWithheld, revenueUSDAmountsExcluded, exclusionPriceReadFailed, exclusionPoolReadFailed,
			protocolRevenueVersionUnknown, protocolRevenueReadFailed, protocolRevenueTimedOut, exclusionPoolListFailed,
		}},
	} {
		got := slices.DeleteFunc(specEnumAt(t, doc, append([]string{"components", "schemas"}, tc.path...)...), func(s string) bool { return s == "" })
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%v enum = %v, want what the handler emits %v", tc.path, got, tc.want)
		}
	}
}

// A Blend pool read with an unknown version is named and summed into neither version nor the total.
func TestProtocolRevenue_UnknownVersionPoolRead(t *testing.T) {
	reads := []protocolPoolRead{
		{ratePoolRead: ratePoolRead{ratePool: ratePool{protocol: "blend", pool: "CPU"}, res: poolReserves{version: blend.PoolVersionUnknown}}},
		{ratePoolRead: ratePoolRead{ratePool: ratePool{protocol: "blend", pool: "CP2"}, res: poolReserves{version: blend.PoolV2}}},
	}
	in := revIn(protocolRevenuePools("blend", reads), []timescale.LendingRevenueUSDCoverage{revCov("CPU", 40, 0), revCov("CP2", 40, 0)},
		revGroup("CPU", "CA", 0, "7"), revGroup("CP2", "CA", 0, "1"))
	out := foldProtocolRevenue(in)
	revExcl(t, out.Day, "CPU", "", protocolRevenueVersionUnknown, -1)
	if deref(out.Day.InterestAccruedUSD) != "1.00" || deref(out.Day.Versions.V2.InterestAccruedUSD) != "1.00" ||
		deref(out.Day.Versions.V1.USDReason) != protocolRevenueNotValued {
		t.Fatalf("unknown-version pool summed: total %s v1 %s/%s v2 %s", deref(out.Day.InterestAccruedUSD),
			deref(out.Day.Versions.V1.InterestAccruedUSD), deref(out.Day.Versions.V1.USDReason), deref(out.Day.Versions.V2.InterestAccruedUSD))
	}
}

// A failed BLNT pool list is named with a null pool, never an empty not_valued block, and degrades the block.
func TestProtocolRevenue_PoolListFailureNamed(t *testing.T) {
	in := revIn(nil, nil)
	in.listFailed, in.versioned = true, false
	out := foldProtocolRevenue(in)
	if len(out.AllTime.Excluded) != 1 || out.AllTime.Excluded[0].Pool != nil || out.AllTime.Excluded[0].Reason != exclusionPoolListFailed ||
		!out.AllTime.LowerBound || !out.Degraded {
		t.Fatalf("list failure: %+v degraded %v", out.AllTime.Excluded, out.Degraded)
	}
}
