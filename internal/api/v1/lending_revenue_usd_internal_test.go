package v1

import (
	"math/big"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestLendingRevenueUSDReasonEnumsMatchHandler(t *testing.T) {
	doc := loadSpecDoc(t)
	series := []string{backstopUSDUnpriced, backstopUSDDecimals, backstopUSDScaleMismatch, exclusionPriceWithheld, exclusionPriceReadFailed}
	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"LendingRevenueUSD", "properties", "usd_reason"}, append(slices.Clone(series), lendingUSDNoPriceInBound, revenueUSDAmountsExcluded, revenueUSDPriceMissing)},
		{[]string{"LendingRevenueUSDExclusion", "properties", "reason"}, append(slices.Clone(series), revenueUSDPriceMissing)},
	} {
		got := specEnumAt(t, doc, append([]string{"components", "schemas"}, tc.path...)...)
		got = slices.DeleteFunc(got, func(s string) bool { return s == "" })
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%v enum = %v, want what the handler emits %v", tc.path, got, tc.want)
		}
	}
}

func usdHour(d, h int) time.Time { return time.Date(2027, 3, d, h, 0, 0, 0, time.UTC) }

// oneToken is 1.0 of a 7-decimal token: gross 1e7, a fifth to the backstop.
const oneToken = 10_000_000

// usdReserve is a reserve whose hours carry the given gross each, folded into daily buckets.
func usdReserve(asset string, gross map[time.Time]int64) clickhouse.LendingRevenueReserve {
	res := clickhouse.LendingRevenueReserve{Asset: asset}
	keys := make([]time.Time, 0, len(gross))
	for at := range gross {
		keys = append(keys, at)
	}
	slices.SortFunc(keys, time.Time.Compare)
	for _, at := range keys {
		h := clickhouse.LendingRevenueBucket{Start: at, Sums: revenueSums(gross[at])}
		res.Hours = append(res.Hours, h)
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		if n := len(res.Buckets); n == 0 || !res.Buckets[n-1].Start.Equal(day) {
			res.Buckets = append(res.Buckets, clickhouse.LendingRevenueBucket{Start: day, Sums: clickhouse.NewLendingRevenueSums()})
		}
		res.Buckets[len(res.Buckets)-1].Sums.Add(h.Sums)
	}
	return res
}

func usdSeries(prices map[time.Time]string) reserveUSDSeries {
	u := reserveUSDSeries{dec: 7, maxAge: lendingUSDHourlyPriceMaxAge, step: time.Hour}
	for at, p := range prices {
		u.points = append(u.points, HistoryPoint{Bucket: at, VWAP: p})
	}
	slices.SortFunc(u.points, func(a, b HistoryPoint) int { return a.Bucket.Compare(b.Bucket) })
	return u
}

func valuedRevenue(t *testing.T, rev clickhouse.LendingRevenue, series ...reserveUSDSeries) LendingRevenueView {
	t.Helper()
	v := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev)
	valueRevenueView(&v, rev, series)
	return v
}

func usdOf(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

// Each hour is valued at its own hour's price: 1 token at 0.10 plus 1 token at 0.20 is 0.30, which no single price
// for the day gives, and an hour after its price is valued at the price before it, never a later one.
func TestRevenueUSD_EachHourAtItsOwnPrice(t *testing.T) {
	rev := completeRevenue()
	rev.Reserves = []clickhouse.LendingRevenueReserve{usdReserve("CA", map[time.Time]int64{
		usdHour(1, 3): oneToken, usdHour(1, 5): oneToken, usdHour(1, 9): oneToken,
	})}
	// 09:00 carries the 05:00 price (0.20, four hours old); the 10:00 price is never used for it.
	v := valuedRevenue(t, rev, usdSeries(map[time.Time]string{usdHour(1, 3): "0.1", usdHour(1, 5): "0.2", usdHour(1, 10): "9"}))
	u := v.Reserves[0].USD
	if usdOf(u.InterestAccruedUSD) != "0.50" || usdOf(u.BackstopTakeUSD) != "0.10" || usdOf(u.SupplierShareUSD) != "0.40" ||
		u.PricedHours != 3 || u.PriceCarriedHours != 1 || u.LowerBound || u.USDReason != nil {
		t.Fatalf("reserve usd = %+v (accrued %s)", u, usdOf(u.InterestAccruedUSD))
	}
	if p := v.Reserves[0].Points[0].USD; usdOf(p.InterestAccruedUSD) != "0.50" {
		t.Fatalf("point usd = %s", usdOf(p.InterestAccruedUSD))
	}
	if tot := v.TotalsUSD; usdOf(tot.InterestAccruedUSD) != "0.50" || tot.LowerBound || len(tot.Excluded) != 0 {
		t.Fatalf("totals_usd = %+v", tot)
	}
}

// Rounding happens once, at serialisation: three hours of 0.004 USD each are 0.012, served 0.01, where rounding each
// hour would serve 0.00.
func TestRevenueUSD_RoundsOnceAtServe(t *testing.T) {
	rev := completeRevenue()
	rev.Reserves = []clickhouse.LendingRevenueReserve{usdReserve("CA", map[time.Time]int64{
		usdHour(1, 1): 40_000, usdHour(1, 2): 40_000, usdHour(1, 3): 40_000,
	})}
	v := valuedRevenue(t, rev, usdSeries(map[time.Time]string{usdHour(1, 0): "1"}))
	if got := usdOf(v.TotalsUSD.InterestAccruedUSD); got != "0.01" {
		t.Fatalf("interest_accrued_usd = %s, want 0.01", got)
	}
}

// Each USD lower-bound or null trigger, alone, from a fully priced two-reserve pool.
func TestRevenueUSD_MissingPricesAreNamedNeverZero(t *testing.T) {
	hours := map[time.Time]int64{usdHour(1, 2): oneToken, usdHour(1, 20): oneToken}
	priced := map[time.Time]string{usdHour(1, 2): "1", usdHour(1, 20): "1"}
	build := func() clickhouse.LendingRevenue {
		r := completeRevenue()
		r.Reserves = []clickhouse.LendingRevenueReserve{usdReserve("CA", hours), usdReserve("CB", hours)}
		return r
	}
	t.Run("both priced", func(t *testing.T) {
		v := valuedRevenue(t, build(), usdSeries(priced), usdSeries(priced))
		if usdOf(v.TotalsUSD.InterestAccruedUSD) != "4.00" || v.TotalsUSD.LowerBound {
			t.Fatalf("totals_usd = %+v", v.TotalsUSD)
		}
	})
	t.Run("one bucket unpriced", func(t *testing.T) {
		// 20:00 is 18 hours past the only price: outside the six-hour bound.
		v := valuedRevenue(t, build(), usdSeries(map[time.Time]string{usdHour(1, 2): "1"}), usdSeries(priced))
		a := v.Reserves[0].USD
		if usdOf(a.InterestAccruedUSD) != "1.00" || !a.LowerBound || a.UnpricedHours != 1 || usdOf(v.TotalsUSD.InterestAccruedUSD) != "3.00" ||
			!v.TotalsUSD.LowerBound || !slices.Equal(v.TotalsUSD.Excluded, []LendingRevenueUSDExclusion{{Asset: "CA", Reason: revenueUSDPriceMissing, Count: 1}}) {
			t.Fatalf("reserve %+v totals %+v", a, v.TotalsUSD)
		}
	})
	t.Run("one reserve with no price at all", func(t *testing.T) {
		v := valuedRevenue(t, build(), reserveUSDSeries{reason: backstopUSDUnpriced}, usdSeries(priced))
		a := v.Reserves[0].USD
		if a.InterestAccruedUSD != nil || usdOf(a.USDReason) != backstopUSDUnpriced || usdOf(v.Reserves[0].Points[0].USD.InterestAccruedUSD) != "null" ||
			usdOf(v.TotalsUSD.InterestAccruedUSD) != "2.00" || !v.TotalsUSD.LowerBound ||
			!slices.Equal(v.TotalsUSD.Excluded, []LendingRevenueUSDExclusion{{Asset: "CA", Reason: backstopUSDUnpriced, Count: 2}}) {
			t.Fatalf("reserve %+v totals %+v", a, v.TotalsUSD)
		}
	})
	t.Run("thin market withheld", func(t *testing.T) {
		v := valuedRevenue(t, build(), reserveUSDSeries{reason: exclusionPriceWithheld}, usdSeries(priced))
		if usdOf(v.Reserves[0].USD.USDReason) != exclusionPriceWithheld || usdOf(v.TotalsUSD.InterestAccruedUSD) != "2.00" || !v.TotalsUSD.LowerBound {
			t.Fatalf("reserve %+v totals %+v", v.Reserves[0].USD, v.TotalsUSD)
		}
	})
	t.Run("no reserve priced", func(t *testing.T) {
		v := valuedRevenue(t, build(), usdSeries(nil), reserveUSDSeries{reason: backstopUSDDecimals})
		if v.TotalsUSD.InterestAccruedUSD != nil || usdOf(v.TotalsUSD.USDReason) != revenueUSDPriceMissing || !v.TotalsUSD.LowerBound ||
			usdOf(v.Reserves[0].USD.USDReason) != lendingUSDNoPriceInBound || len(v.TotalsUSD.Excluded) != 2 {
			t.Fatalf("totals %+v reserve %+v", v.TotalsUSD, v.Reserves[0].USD)
		}
	})
	t.Run("a base-unit exclusion carries", func(t *testing.T) {
		r := build()
		r.Redo, r.RedoHour = &clickhouse.LendingInterestRedo{}, usdHour(2, 0)
		v := valuedRevenue(t, r, usdSeries(priced), usdSeries(priced))
		if usdOf(v.TotalsUSD.InterestAccruedUSD) != "4.00" || !v.TotalsUSD.LowerBound || !v.Reserves[0].USD.LowerBound || len(v.TotalsUSD.Excluded) != 0 {
			t.Fatalf("totals %+v", v.TotalsUSD)
		}
	})
}

// A reserve nothing of which was summed has null USD, never "0.00", even with a price.
func TestRevenueUSD_ExcludedReserveIsNullNotZero(t *testing.T) {
	rev := completeRevenue()
	rev.Reserves = []clickhouse.LendingRevenueReserve{{Asset: "CA", StaleRows: 4}}
	v := valuedRevenue(t, rev, usdSeries(map[time.Time]string{usdHour(1, 0): "1"}))
	if v.Reserves[0].Totals != nil || v.Reserves[0].USD.InterestAccruedUSD != nil || usdOf(v.Reserves[0].USD.USDReason) != revenueUSDAmountsExcluded ||
		v.TotalsUSD.InterestAccruedUSD != nil || usdOf(v.TotalsUSD.USDReason) != revenueUSDAmountsExcluded || !v.TotalsUSD.LowerBound {
		t.Fatalf("reserve %+v totals %+v", v.Reserves[0].USD, v.TotalsUSD)
	}
	none := valuedRevenue(t, clickhouse.LendingRevenue{})
	if none.TotalsUSD.InterestAccruedUSD != nil || usdOf(none.TotalsUSD.USDReason) != revenueUSDAmountsExcluded {
		t.Fatalf("uncovered pool totals_usd = %+v", none.TotalsUSD)
	}
}

// A bucket its hour rows do not account for is not valued as complete.
func TestRevenueUSD_BucketBeyondItsHoursIsNotValuedComplete(t *testing.T) {
	rev := completeRevenue()
	res := usdReserve("CA", map[time.Time]int64{usdHour(1, 2): oneToken})
	res.Hours = nil
	rev.Reserves = []clickhouse.LendingRevenueReserve{res}
	v := valuedRevenue(t, rev, usdSeries(map[time.Time]string{usdHour(1, 2): "1"}))
	if p := v.Reserves[0].Points[0].USD; p.InterestAccruedUSD != nil || !p.LowerBound || usdOf(v.Reserves[0].USD.InterestAccruedUSD) != "null" {
		t.Fatalf("point %+v reserve %+v", p, v.Reserves[0].USD)
	}
}

// Under exact rational arithmetic, pool totals = Σ reserve totals = Σ bucket values = Σ each hour's amount at its own price.
func TestRevenueUSD_TotalsAreExactSumsOfHourValues(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for iter := 0; iter < 50; iter++ {
		rev := completeRevenue()
		rev.Reserves = nil
		var series []reserveUSDSeries
		want := make([]*big.Rat, 0)
		for r := 0; r < 1+rng.IntN(4); r++ {
			gross, prices := map[time.Time]int64{}, map[time.Time]string{}
			exact := new(big.Rat)
			for h := 0; h < 48; h++ {
				at := usdHour(1, 0).Add(time.Duration(h) * time.Hour)
				if rng.IntN(3) == 0 {
					continue
				}
				g := rng.Int64N(1 << 50)
				p := strconv.Itoa(rng.IntN(100_000)) + "." + strconv.Itoa(rng.IntN(1_000_000))
				gross[at], prices[at] = g, p
				pr, _ := new(big.Rat).SetString(p)
				exact.Add(exact, new(big.Rat).Quo(new(big.Rat).Mul(new(big.Rat).SetInt64(g), pr), big.NewRat(oneToken, 1)))
			}
			rev.Reserves = append(rev.Reserves, usdReserve("C"+strconv.Itoa(r), gross))
			series = append(series, usdSeries(prices))
			want = append(want, exact)
		}
		v := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev)
		pool, reserves := valueRevenueView(&v, rev, series)
		sumReserves := new(big.Rat)
		for i, t0 := range reserves {
			_, buckets := valueRevenueReserve(&v.Reserves[i], rev.Reserves[i], series[i], 24*time.Hour)
			sumBuckets := new(big.Rat)
			for _, b := range buckets {
				if b.v[0] != nil {
					sumBuckets.Add(sumBuckets, b.v[0])
				}
			}
			got := t0.v[0]
			if got == nil {
				got = new(big.Rat)
			}
			if got.Cmp(sumBuckets) != 0 || got.Cmp(want[i]) != 0 {
				t.Fatalf("iter %d reserve %d: total %s, buckets %s, hours at own price %s", iter, i, got.RatString(), sumBuckets.RatString(), want[i].RatString())
			}
			sumReserves.Add(sumReserves, got)
		}
		poolGot := pool.v[0]
		if poolGot == nil {
			poolGot = new(big.Rat)
		}
		if poolGot.Cmp(sumReserves) != 0 {
			t.Fatalf("iter %d: pool %s != Σ reserves %s", iter, poolGot.RatString(), sumReserves.RatString())
		}
	}
}
