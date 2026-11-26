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
		res.Hours = append(res.Hours, clickhouse.LendingRevenueBucket{Start: at, Sums: revenueSums(gross[at])})
	}
	res.Buckets = clickhouse.FoldLendingRevenueDays(res.Hours)
	return res
}

// hoursReserve folds the given hour rows into days with the reader's own fold.
func hoursReserve(asset string, hours ...clickhouse.LendingRevenueBucket) clickhouse.LendingRevenueReserve {
	return clickhouse.LendingRevenueReserve{Asset: asset, Hours: hours, Buckets: clickhouse.FoldLendingRevenueDays(hours)}
}

// Draws and losses are stored ≤ 0 and valued as the magnitudes the base units serve.
func TestRevenueUSD_DrawsAndLossesAreMagnitudes(t *testing.T) {
	s := clickhouse.NewLendingRevenueSums()
	s.InterestDrawn.SetInt64(-oneToken)
	s.DefaultLoss.SetInt64(-oneToken)
	rev := completeRevenue()
	rev.Reserves = []clickhouse.LendingRevenueReserve{hoursReserve("CA", clickhouse.LendingRevenueBucket{Start: usdHour(1, 2), Sums: s})}
	u := valuedRevenue(t, rev, usdSeries(map[time.Time]string{usdHour(1, 2): "1"})).Reserves[0].USD
	if usdOf(u.BackstopDrawnUSD) != "1.00" || usdOf(u.DefaultLossUSD) != "1.00" || usdOf(u.InterestAccruedUSD) != "0.00" {
		t.Fatalf("drawn %s loss %s accrued %s, want 1.00 1.00 0.00", usdOf(u.BackstopDrawnUSD), usdOf(u.DefaultLossUSD), usdOf(u.InterestAccruedUSD))
	}
}

// A price exactly one hour old is not carried; two hours old is.
func TestRevenueUSD_CarriedBoundary(t *testing.T) {
	rev := completeRevenue()
	rev.Reserves = []clickhouse.LendingRevenueReserve{usdReserve("CA", map[time.Time]int64{usdHour(1, 4): oneToken, usdHour(1, 5): oneToken})}
	u := valuedRevenue(t, rev, usdSeries(map[time.Time]string{usdHour(1, 3): "1"})).Reserves[0].USD
	if u.PricedHours != 2 || u.PriceCarriedHours != 1 {
		t.Fatalf("priced %d carried %d, want 2 priced, only the 2-hour-old one carried", u.PricedHours, u.PriceCarriedHours)
	}
}

// Nothing to value is not a gap: an unpriced hour whose valued amounts are all zero, or a withheld reserve with no
// amounts, leaves the figures complete and names nothing.
func TestRevenueUSD_NothingToValueIsNoLowerBound(t *testing.T) {
	gulpOnly := clickhouse.NewLendingRevenueSums()
	gulpOnly.Gulped.SetInt64(oneToken)
	rev := completeRevenue()
	priced := usdReserve("CA", map[time.Time]int64{usdHour(1, 2): oneToken})
	priced.Hours = append(priced.Hours, clickhouse.LendingRevenueBucket{Start: usdHour(1, 20), Sums: gulpOnly})
	priced.Buckets = clickhouse.FoldLendingRevenueDays(priced.Hours)
	rev.Reserves = []clickhouse.LendingRevenueReserve{priced, hoursReserve("CB", clickhouse.LendingRevenueBucket{Start: usdHour(1, 3), Sums: gulpOnly})}
	v := valuedRevenue(t, rev, usdSeries(map[time.Time]string{usdHour(1, 2): "1"}), reserveUSDSeries{reason: exclusionPriceWithheld})
	a, b := v.Reserves[0].USD, v.Reserves[1].USD
	if a.LowerBound || a.UnpricedHours != 0 || usdOf(a.InterestAccruedUSD) != "1.00" {
		t.Errorf("reserve A = %+v, want complete 1.00", a)
	}
	if b.LowerBound || b.USDReason != nil || usdOf(b.InterestAccruedUSD) != "0.00" {
		t.Errorf("withheld reserve with no amounts = %+v, want 0.00, complete", b)
	}
	if v.TotalsUSD.LowerBound || len(v.TotalsUSD.Excluded) != 0 || usdOf(v.TotalsUSD.InterestAccruedUSD) != "1.00" {
		t.Errorf("totals_usd = %+v, want 1.00, complete, nothing named", v.TotalsUSD)
	}
}

// The basis is disclosed as /history's: one basis when every priced hour shares it, hour counts when they mix.
func TestRevenueUSD_PriceBasis(t *testing.T) {
	hours := map[time.Time]int64{usdHour(1, 2): oneToken, usdHour(1, 3): oneToken}
	prices := map[time.Time]string{usdHour(1, 2): "1", usdHour(1, 3): "1"}
	rev := completeRevenue()
	rev.Reserves = []clickhouse.LendingRevenueReserve{usdReserve("CA", hours), usdReserve("CB", hours), usdReserve("CC", hours)}
	transitive, mixed := usdSeries(prices), usdSeries(prices)
	transitive.basis = priceBasisTransitive
	mixed.stableAt = map[time.Time]bool{usdHour(1, 3): true}
	v := valuedRevenue(t, rev, usdSeries(prices), transitive, mixed)
	if u := v.Reserves[0].USD; u.PriceBasis != "" || u.PriceBasisHours != nil {
		t.Errorf("fiat reserve basis = %q %v, want none", u.PriceBasis, u.PriceBasisHours)
	}
	if u := v.Reserves[1].USD; u.PriceBasis != priceBasisTransitive || u.PriceBasisHours != nil {
		t.Errorf("transitive reserve basis = %q %v", u.PriceBasis, u.PriceBasisHours)
	}
	if u := v.Reserves[2].USD; u.PriceBasis != "" || u.PriceBasisHours[priceBasisStablecoinProxy] != 1 || len(u.PriceBasisHours) != 1 {
		t.Errorf("mixed reserve basis = %q %v, want {stablecoin_proxy: 1}", u.PriceBasis, u.PriceBasisHours)
	}
	if v.TotalsUSD.PriceBasis != "" || v.TotalsUSD.PriceBasisHours != nil {
		t.Errorf("totals_usd carries a basis: %+v", v.TotalsUSD)
	}
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
		var wants []propertyWant
		for r := 0; r < 1+rng.IntN(4); r++ {
			res, u, w := randomValuedReserve(rng, "C"+strconv.Itoa(r))
			rev.Reserves, series, wants = append(rev.Reserves, res), append(series, u), append(wants, w)
		}
		v := revenueView("CPOOL", "blend", "1d", revenueTestQuery, rev)
		pool, reserves := valueRevenueView(&v, rev, series)
		sumReserves := zeroRats()
		for i, got := range reserves {
			w := wants[i]
			if got.priced != w.priced || got.unpriced != w.unpriced || got.carried != w.carried {
				t.Fatalf("iter %d reserve %d: priced/unpriced/carried %d/%d/%d, want %d/%d/%d", iter, i, got.priced, got.unpriced, got.carried, w.priced, w.unpriced, w.carried)
			}
			_, buckets := valueRevenueReserve(&v.Reserves[i], rev.Reserves[i], series[i], 24*time.Hour)
			sumBuckets := zeroRats()
			for _, b := range buckets {
				addRats(&sumBuckets, b.v)
			}
			total := zeroRats()
			addRats(&total, got.v)
			for leg := range total {
				if total[leg].Cmp(sumBuckets[leg]) != 0 || total[leg].Cmp(w.value[leg]) != 0 {
					t.Fatalf("iter %d reserve %d leg %d: total %s, Σ buckets %s, Σ hours at own price %s",
						iter, i, leg, total[leg].RatString(), sumBuckets[leg].RatString(), w.value[leg].RatString())
				}
			}
			if got.priced > 0 {
				addRats(&sumReserves, got.v)
			}
		}
		poolTotal := zeroRats()
		addRats(&poolTotal, pool.v)
		for leg := range poolTotal {
			if poolTotal[leg].Cmp(sumReserves[leg]) != 0 {
				t.Fatalf("iter %d leg %d: pool %s != Σ valued reserves %s", iter, leg, poolTotal[leg].RatString(), sumReserves[leg].RatString())
			}
		}
	}
}

type propertyWant struct {
	value                     [revenueUSDLegCount]*big.Rat
	priced, unpriced, carried int
}

func zeroRats() [revenueUSDLegCount]*big.Rat {
	var out [revenueUSDLegCount]*big.Rat
	for i := range out {
		out[i] = new(big.Rat)
	}
	return out
}

func addRats(dst *[revenueUSDLegCount]*big.Rat, src [revenueUSDLegCount]*big.Rat) {
	for i, x := range src {
		if x != nil {
			dst[i].Add(dst[i], x)
		}
	}
}

// randomValuedReserve is two days of random hours, several signed legs, prices on a random subset of hours (so others
// carry an older price or go unpriced), folded by the reader's own fold; want is computed independently of priceAt.
func randomValuedReserve(rng *rand.Rand, asset string) (clickhouse.LendingRevenueReserve, reserveUSDSeries, propertyWant) {
	prices := map[time.Time]string{}
	var hours []clickhouse.LendingRevenueBucket
	for h := 0; h < 48; h++ {
		at := usdHour(1, 0).Add(time.Duration(h) * time.Hour)
		if rng.IntN(4) == 0 {
			prices[at] = strconv.Itoa(rng.IntN(100_000)) + "." + strconv.Itoa(rng.IntN(1_000_000))
		}
		if rng.IntN(3) == 0 {
			continue
		}
		s := clickhouse.NewLendingRevenueSums()
		s.Gross.SetInt64(rng.Int64N(1 << 50))
		s.Backstop.SetInt64(rng.Int64N(1 << 40))
		s.SupplierRealised.SetInt64(rng.Int64N(1<<40) - 1<<39)
		s.InterestDrawn.SetInt64(-rng.Int64N(1 << 30))
		hours = append(hours, clickhouse.LendingRevenueBucket{Start: at, Sums: s})
	}
	w := propertyWant{value: zeroRats()}
	for _, h := range hours {
		var priceAt time.Time
		found := false
		for back := 0; back <= 6; back++ {
			if _, ok := prices[h.Start.Add(-time.Duration(back)*time.Hour)]; ok {
				priceAt, found = h.Start.Add(-time.Duration(back)*time.Hour), true
				break
			}
		}
		if !found {
			w.unpriced++
			continue
		}
		w.priced++
		if h.Start.Sub(priceAt) > time.Hour {
			w.carried++
		}
		p, _ := new(big.Rat).SetString(prices[priceAt])
		for leg, a := range revenueUSDLegs(h.Sums) {
			w.value[leg].Add(w.value[leg], new(big.Rat).Quo(new(big.Rat).Mul(new(big.Rat).SetInt(a), p), big.NewRat(oneToken, 1)))
		}
	}
	return hoursReserve(asset, hours...), usdSeries(prices), w
}
