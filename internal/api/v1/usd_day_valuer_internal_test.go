package v1

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// randomRevenueAmount is zero, a small signed amount, or one near the i128 range.
func randomRevenueAmount(rng *rand.Rand) *big.Int {
	switch rng.IntN(4) {
	case 0, 1:
		return new(big.Int)
	case 2:
		return big.NewInt(rng.Int64N(2_000_000_001) - 1_000_000_000)
	}
	x := new(big.Int).Lsh(big.NewInt(rng.Int64N(1<<30)+1), 96)
	if rng.IntN(2) == 0 {
		x.Neg(x)
	}
	return x
}

func randomRevenueHours(rng *rand.Rand, day time.Time) []clickhouse.LendingRevenueBucket {
	n := rng.IntN(12)
	out := make([]clickhouse.LendingRevenueBucket, n)
	for i := range out {
		s := clickhouse.NewLendingRevenueSums()
		for _, p := range []**big.Int{
			&s.Gross, &s.SupplierIntended, &s.SupplierRealised, &s.Backstop, &s.ProtocolFee,
			&s.InterestDrawn, &s.DefaultLoss, &s.UnexplainedSupplyLoss,
		} {
			*p = randomRevenueAmount(rng)
		}
		s.UnorderedVersions, s.AccrualErrors, s.UnexplainedSupplyLossVersions = rng.Int64N(3), rng.Int64N(3), rng.Int64N(3)
		// Mostly inside the day; now and then an hour either side, which the day's sum holds but its hours do not.
		out[i] = clickhouse.LendingRevenueBucket{Start: day.Add(time.Duration(rng.IntN(27)-1) * time.Hour), Sums: s}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// randomVWAP is mostly a decimal price, now and then one that does not parse, is not finite, or is a fraction with no
// exact decimal (whose stored legs are then unrepresentable).
func randomVWAP(rng *rand.Rand) string {
	switch rng.IntN(40) {
	case 0:
		return "not-a-price"
	case 1:
		return []string{"NaN", "+Inf", "-Inf", ""}[rng.IntN(4)]
	case 2:
		return "1/3"
	}
	return new(big.Rat).SetFrac(big.NewInt(rng.Int64N(1_000_000_000_000)), big.NewInt(rng.Int64N(1_000_000)+1)).FloatString(rng.IntN(12))
}

// randomRevenueRead is a walk outcome: a verdict, a failed or cut-short read, the series' own reason, or prices with gaps,
// carried hours, mixed bases and odd price strings. stale (about 30%) is a series whose prices are mostly older than the
// six-hour bound.
func randomRevenueRead(rng *rand.Rand, day time.Time) (read reserveUSDRead, stale bool) {
	stale = rng.IntN(10) < 3
	if !stale {
		switch rng.IntN(8) {
		case 0:
			return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}, false
		case 1:
			return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceReadFailed, degraded: true}}, false
		case 2:
			return reserveUSDRead{series: reserveUSDSeries{reason: backstopUSDUnpriced}}, false
		}
	}
	u := reserveUSDSeries{dec: rng.IntN(19), maxAge: 6 * time.Hour, step: time.Hour, stableAt: map[time.Time]bool{}}
	if rng.IntN(3) == 0 {
		u.basis = "crypto:XLM"
	}
	step, skip := time.Hour, 3
	if stale {
		step, skip = 11*time.Hour, 1_000_000
	}
	for at := day.Add(-8 * time.Hour); at.Before(day.Add(50 * time.Hour)); at = at.Add(step) {
		if rng.IntN(skip) == 0 && !stale {
			continue
		}
		u.points = append(u.points, HistoryPoint{Bucket: at, VWAP: randomVWAP(rng)})
		u.stableAt[at] = rng.IntN(4) == 0
	}
	if rng.IntN(10) == 0 {
		u.degraded = true
	}
	return reserveUSDRead{series: u}, stale
}

// The leg-generic valuer values every revenue day exactly as the pre-generic one did: legs, hour counts, bases, reasons,
// outcomes, the stored row and the served figures, over random days of gaps, carries, verdicts, zeros and i128 amounts.
func TestUSDDayValuer_MatchesPreGenericRevenueValuer(t *testing.T) {
	rng := rand.New(rand.NewPCG(20271101, 7)) //nolint:gosec // a reproducible test input, not a secret
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	info := clickhouse.LendingInterestDay{Day: day, DerivedAt: day.Add(30 * time.Hour), StaleRows: 2}
	const cases = 2000
	valued, staleCases, unrepresentable := 0, 0, 0
	for c := range cases {
		hours := randomRevenueHours(rng, day)
		read, stale := randomRevenueRead(rng, day)
		if stale {
			staleCases++
		}
		walks, oracleWalks := 0, 0
		got := valueRevenueDay(day, hours, func() reserveUSDRead { walks++; return read })
		want := oracleValueRevenueDay(day, hours, func() reserveUSDRead { oracleWalks++; return read })
		if walks != oracleWalks || got.outcome != want.outcome || got.reason != want.reason || got.sum.priced != want.sum.priced ||
			got.sum.unpriced != want.sum.unpriced || got.sum.carried != want.sum.carried || !reflect.DeepEqual(got.sum.bases, want.sum.bases) {
			t.Fatalf("case %d: got %+v (walks %d), want %+v (walks %d)", c, got, walks, want, oracleWalks)
		}
		for i := range want.sum.v {
			g, w := got.sum.v[i], want.sum.v[i]
			if (g == nil) != (w == nil) || (g != nil && g.Cmp(w) != 0) {
				t.Fatalf("case %d leg %d: got %v, want %v", c, i, g, w)
			}
		}
		sums := clickhouse.NewLendingRevenueSums()
		for _, h := range hours {
			sums.Add(h.Sums)
		}
		gotRow, gotOK := revenueDayRow("P", "A", info, sums, got)
		wantRow, wantOK := oracleRevenueDayRow("P", "A", info, sums, want)
		if gotOK != wantOK || !reflect.DeepEqual(gotRow, wantRow) {
			t.Fatalf("case %d: row %+v (%v), want %+v (%v)", c, gotRow, gotOK, wantRow, wantOK)
		}
		if !gotOK {
			unrepresentable++
		}
		for _, reason := range []string{"", read.series.reason, revenueUSDAmountsExcluded} {
			lb := rng.IntN(2) == 0
			if g, w := revenueUSDWire(&got.sum, reason, lb), want.sum.wire(reason, lb); !reflect.DeepEqual(g, w) {
				t.Fatalf("case %d reason %q: wire %+v, want %+v", c, reason, g, w)
			}
		}
		// The /revenue route's hourly buckets: each hour row is its own bucket.
		h, oh, rh := 0, 0, revenueHours(hours)
		for _, b := range hours {
			var g revenueUSDSum
			var w oracleRevenueUSDSum
			g, h = valueUSDBucket[revenueUSDLegRats](b.Start, revenueUSDAmounts(b.Sums), rh, h, read.series, time.Hour)
			w, oh = oracleValueRevenueBucket(b, hours, oh, read.series, time.Hour)
			if h != oh || !reflect.DeepEqual(revenueUSDWire(&g, "", false), w.wire("", false)) {
				t.Fatalf("case %d bucket %v: %+v, want %+v", c, b.Start, g, w)
			}
		}
		if got.sum.priced > 0 {
			valued++
		}
	}
	if valued < cases/10 || staleCases < cases/5 || unrepresentable == 0 {
		t.Fatalf("of %d cases %d valued an hour, %d had stale prices, %d had an unrepresentable leg: inputs too narrow",
			cases, valued, staleCases, unrepresentable)
	}
}

// randomRevenueView is a pool's revenue over three days, hourly or daily, of one to three reserves: some excluded in base
// units, some with a bucket its hours do not account for, some with fewer points than buckets.
func randomRevenueView(rng *rand.Rand, day time.Time) (func() *LendingRevenueView, clickhouse.LendingRevenue, []reserveUSDSeries) {
	interval, lb := []string{"1h", "1d"}[rng.IntN(2)], rng.IntN(4) == 0
	n := rng.IntN(3) + 1
	rev := clickhouse.LendingRevenue{Reserves: make([]clickhouse.LendingRevenueReserve, n)}
	series := make([]reserveUSDSeries, n)
	excluded, resLB, points := make([]bool, n), make([]bool, n), make([]int, n)
	for i := range n {
		var hours []clickhouse.LendingRevenueBucket
		for d := range 3 {
			hours = append(hours, randomRevenueHours(rng, day.Add(time.Duration(d)*day24))...)
		}
		sort.SliceStable(hours, func(a, b int) bool { return hours[a].Start.Before(hours[b].Start) })
		res := clickhouse.LendingRevenueReserve{Asset: fmt.Sprintf("R%d", i), Hours: hours, Buckets: hours}
		if interval == "1d" {
			res.Buckets = nil
			for d := range 3 {
				b := clickhouse.LendingRevenueBucket{Start: day.Add(time.Duration(d) * day24), Sums: clickhouse.NewLendingRevenueSums()}
				for _, h := range hours {
					if h.Start.UTC().Truncate(day24).Equal(b.Start) {
						b.Sums.Add(h.Sums)
					}
				}
				if rng.IntN(8) == 0 {
					b.Sums.Gross.Add(b.Sums.Gross, big.NewInt(1))
				}
				res.Buckets = append(res.Buckets, b)
			}
		}
		rev.Reserves[i] = res
		read, _ := randomRevenueRead(rng, day)
		series[i] = read.series
		excluded[i], resLB[i], points[i] = rng.IntN(6) == 0, rng.IntN(4) == 0, len(res.Buckets)-rng.IntN(2)
	}
	mk := func() *LendingRevenueView {
		v := &LendingRevenueView{Interval: interval, LowerBound: lb, Reserves: make([]LendingRevenueReserveView, n)}
		for i := range n {
			v.Reserves[i] = LendingRevenueReserveView{Asset: rev.Reserves[i].Asset, LowerBound: resLB[i], Points: make([]LendingRevenuePoint, max(points[i], 0))}
			if !excluded[i] {
				v.Reserves[i].Totals = &LendingRevenueAmounts{}
			}
		}
		return v
	}
	return mk, rev, series
}

// The /revenue view, hourly and daily, per reserve and across reserves into totals_usd, is valued exactly as before.
func TestUSDDayValuer_RevenueViewMatchesPreGeneric(t *testing.T) {
	rng := rand.New(rand.NewPCG(20271102, 9)) //nolint:gosec // a reproducible test input, not a secret
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	for c := range 500 {
		mk, rev, series := randomRevenueView(rng, day)
		got, want := mk(), mk()
		gotPool, gotRes := valueRevenueView(got, rev, series)
		wantPool, wantRes := oracleValueRevenueView(want, rev, series)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: view\n%+v\nwant\n%+v", c, got, want)
		}
		if !reflect.DeepEqual(revenueUSDWire(&gotPool, "", false), wantPool.wire("", false)) || len(gotRes) != len(wantRes) {
			t.Fatalf("case %d: pool sums differ", c)
		}
		for i := range gotRes {
			if !reflect.DeepEqual(revenueUSDWire(&gotRes[i], "", false), wantRes[i].wire("", false)) {
				t.Fatalf("case %d reserve %d: sums differ", c, i)
			}
		}
	}
}

// An hour whose amounts do not match the valuer's legs is an error before any read, never a silent partial value.
func TestUSDDayValuer_LegCountMismatch(t *testing.T) {
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 3} {
		hours := []usdAmountHour{{start: day, amounts: zeroAmounts(n)}}
		hours[0].amounts[0].SetInt64(5)
		d, err := valueUSDDay[fakeEmissionLegs](day, hours, func() reserveUSDRead {
			t.Fatal("walked despite a leg-count mismatch")
			return reserveUSDRead{}
		})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("has %d amounts for 2 legs", n)) || d.sum.priced+d.sum.unpriced != 0 {
			t.Fatalf("%d amounts: err %v day %+v", n, err, d)
		}
	}
}

// fakeEmissionLegs is a second instantiation: one token's scheduled and claimed amounts, valued as revenue's legs are.
type fakeEmissionLegs [2]*big.Rat

func (a *fakeEmissionLegs) slice() []*big.Rat { return a[:] }

type fakeEmissionDay = usdDay[fakeEmissionLegs, *fakeEmissionLegs]

// A two-leg emissions-shaped day goes through the same walk, per-hour pricing, carry and basis accounting, refusal and
// stored-leg rules as revenue: no revenue type is involved.
func TestUSDDayValuer_SecondInstantiation(t *testing.T) {
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	hour := func(h int, scheduled, claimed int64) usdAmountHour {
		return usdAmountHour{start: day.Add(time.Duration(h) * time.Hour), amounts: []*big.Int{big.NewInt(scheduled), big.NewInt(claimed)}}
	}
	hours := []usdAmountHour{hour(1, 10_000_000, 0), hour(2, 0, 0), hour(5, 20_000_000, 5_000_000), hour(20, 1, 1)}
	priced := reserveUSDSeries{
		points: []HistoryPoint{{Bucket: day.Add(time.Hour), VWAP: "0.5"}, {Bucket: day.Add(3 * time.Hour), VWAP: "2"}},
		maxAge: 6 * time.Hour, step: time.Hour, dec: 7, stableAt: map[time.Time]bool{day.Add(3 * time.Hour): true},
	}
	d, err := valueUSDDay[fakeEmissionLegs](day, hours, func() reserveUSDRead { return reserveUSDRead{series: priced} })
	// 1 token at 0.5, then 2 + 0.5 tokens at 2 carried two hours; the hour-20 amount is past the price bound.
	if err != nil || d.outcome != usdDayValued || d.sum.priced != 2 || d.sum.unpriced != 1 || d.sum.carried != 1 ||
		d.sum.v[0].Cmp(big.NewRat(9, 2)) != 0 || d.sum.v[1].Cmp(big.NewRat(1, 1)) != 0 ||
		!reflect.DeepEqual(d.sum.bases, map[string]int{"": 1, priceBasisStablecoinProxy: 1}) {
		t.Fatalf("day %+v legs %v %v", d, d.sum.v[0], d.sum.v[1])
	}
	legs, ok := d.storedLegs()
	if !ok || len(legs) != 2 || *legs[0] != "4.5" || *legs[1] != "1" {
		t.Fatalf("stored legs %v ok %v", legs, ok)
	}
	w := d.sum.wire("", false)
	if len(w.legs) != 2 || *w.legs[0] != "4.50" || !w.lowerBound || w.basisHours[priceBasisStablecoinProxy] != 1 {
		t.Fatalf("wire %+v", w)
	}
	refused, err := valueUSDDay[fakeEmissionLegs](day, hours, func() reserveUSDRead {
		return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}
	})
	if legs, _ := refused.storedLegs(); err != nil || refused.outcome != usdDayRefused || refused.sum.unpriced != 3 || legs != nil {
		t.Fatalf("refused %+v legs %v err %v", refused, legs, err)
	}
	var idle fakeEmissionDay
	if got, err := valueUSDDay[fakeEmissionLegs](day, []usdAmountHour{hour(1, 0, 0)}, func() reserveUSDRead {
		t.Fatal("a day with nothing to value walked")
		return reserveUSDRead{}
	}); err != nil || !reflect.DeepEqual(got, idle) {
		t.Fatalf("idle %+v err %v", got, err)
	}
}
