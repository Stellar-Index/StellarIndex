package v1

import (
	"math/big"
	"math/rand/v2"
	"reflect"
	"sort"
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

// randomRevenueRead is a walk outcome: a verdict, a failed or cut-short read, the series' own reason, or prices with gaps,
// carried hours, mixed bases and the odd unparsable price.
func randomRevenueRead(rng *rand.Rand, day time.Time) reserveUSDRead {
	switch rng.IntN(8) {
	case 0:
		return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}
	case 1:
		return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceReadFailed, degraded: true}}
	case 2:
		return reserveUSDRead{series: reserveUSDSeries{reason: backstopUSDUnpriced}}
	}
	u := reserveUSDSeries{dec: rng.IntN(19), maxAge: 6 * time.Hour, step: time.Hour, stableAt: map[time.Time]bool{}}
	if rng.IntN(3) == 0 {
		u.basis = "crypto:XLM"
	}
	for at := day.Add(-8 * time.Hour); at.Before(day.Add(26 * time.Hour)); at = at.Add(time.Hour) {
		if rng.IntN(3) == 0 {
			continue
		}
		vwap := new(big.Rat).SetFrac(big.NewInt(rng.Int64N(1_000_000_000_000)), big.NewInt(rng.Int64N(1_000_000)+1)).FloatString(rng.IntN(12))
		if rng.IntN(60) == 0 {
			vwap = "not-a-price"
		}
		u.points = append(u.points, HistoryPoint{Bucket: at, VWAP: vwap})
		u.stableAt[at] = rng.IntN(4) == 0
	}
	if rng.IntN(10) == 0 {
		u.degraded = true
	}
	return reserveUSDRead{series: u}
}

// The leg-generic valuer values every revenue day exactly as the pre-generic one did: legs, hour counts, bases, reasons,
// outcomes, the stored row and the served figures, over random days of gaps, carries, verdicts, zeros and i128 amounts.
func TestUSDDayValuer_MatchesPreGenericRevenueValuer(t *testing.T) {
	rng := rand.New(rand.NewPCG(20271101, 7)) //nolint:gosec // a reproducible test input, not a secret
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	info := clickhouse.LendingInterestDay{Day: day, DerivedAt: day.Add(30 * time.Hour), StaleRows: 2}
	const cases = 2000
	valued := 0
	for c := range cases {
		hours, read := randomRevenueHours(rng, day), randomRevenueRead(rng, day)
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
	if valued < cases/10 {
		t.Fatalf("only %d of %d cases valued an hour: the inputs do not exercise the price path", valued, cases)
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
	d := valueUSDDay[fakeEmissionLegs](day, hours, func() reserveUSDRead { return reserveUSDRead{series: priced} })
	// 1 token at 0.5, then 2 + 0.5 tokens at 2 carried two hours; the hour-20 amount is past the price bound.
	if d.outcome != revenueDayValued || d.sum.priced != 2 || d.sum.unpriced != 1 || d.sum.carried != 1 ||
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
	refused := valueUSDDay[fakeEmissionLegs](day, hours, func() reserveUSDRead {
		return reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}
	})
	if legs, _ := refused.storedLegs(); refused.outcome != revenueDayRefused || refused.sum.unpriced != 3 || legs != nil {
		t.Fatalf("refused %+v legs %v", refused, legs)
	}
	var idle fakeEmissionDay
	if got := valueUSDDay[fakeEmissionLegs](day, []usdAmountHour{hour(1, 0, 0)}, func() reserveUSDRead {
		t.Fatal("a day with nothing to value walked")
		return reserveUSDRead{}
	}); !reflect.DeepEqual(got, idle) {
		t.Fatalf("idle %+v", got)
	}
}
