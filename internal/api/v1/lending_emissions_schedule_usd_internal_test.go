package v1

import (
	"context"
	"fmt"
	"math/big"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"
)

// oracleScheduleHours is the per-hour schedule input the first USD implementation built, kept verbatim as the oracle.
func oracleScheduleHours(segs []emissionSegment, win emissionsHistoryWindow) (buckets, hours []usdAmountHour) {
	per := int(win.width / time.Hour)
	units := scheduledBucketUnits(segs, win.from, win.end, time.Hour)
	for i, u := range units {
		start := win.from.Add(time.Duration(i) * time.Hour)
		hours = append(hours, oneAmount(start, u))
		if i%per == 0 {
			buckets = append(buckets, oneAmount(start, new(big.Int)))
		}
		b := buckets[len(buckets)-1].amounts[0]
		b.Add(b, u)
	}
	return buckets, hours
}

// oracleScheduleLine values a line hour by hour through the shared valuer, as the first implementation did.
func oracleScheduleLine(segs []emissionSegment, win emissionsHistoryWindow, u reserveUSDSeries) (emisOneSum, []emisOneSum) {
	sched := u
	sched.dec += scheduleScale
	buckets, hours := oracleScheduleHours(segs, win)
	var total emisOneSum
	out := make([]emisOneSum, len(buckets))
	h := 0
	for j, b := range buckets {
		out[j], h = valueUSDBucket[emisUSDOne](b.start, b.amounts, hours, h, sched, win.width)
		total.add(&out[j])
	}
	return total, out
}

// genSchedule is a random timeline: updates at whole seconds, some expiring early, some at eps 0, one before the window.
func genSchedule(r *rand.Rand, from time.Time, span time.Duration) []emissionSegment {
	var segs []emissionSegment
	at := from.Add(-time.Duration(r.IntN(3600)) * time.Second)
	for at.Before(from.Add(span)) {
		eps := big.NewInt(int64(r.IntN(4)) * r.Int64N(1e12))
		exp := at.Add(time.Duration(r.IntN(int(span/time.Second))) * time.Second)
		segs = append(segs, emissionSegment{start: at, eps: eps, exp: exp})
		at = at.Add(time.Duration(1+r.IntN(int(span/time.Second)/3)) * time.Second)
	}
	return segs
}

// genSeries is an hourly price series with stale gaps past the 6 h bound, missing (excluded) hours, prices changing every
// few hours, an unparseable price and stablecoin-quoted buckets.
func genSeries(r *rand.Rand, from time.Time, span time.Duration, basis string) reserveUSDSeries {
	u := reserveUSDSeries{dec: 7, maxAge: lendingUSDHourlyPriceMaxAge, step: time.Hour, basis: basis, stableAt: map[time.Time]bool{}}
	price := "0.0043"
	for t := from.Add(-8 * time.Hour); t.Before(from.Add(span)); t = t.Add(time.Hour) {
		switch k := r.IntN(20); {
		case k < 6: // excluded or never traded
			continue
		case k == 6: // a stale gap past the bound
			t = t.Add(time.Duration(6+r.IntN(4)) * time.Hour)
		case k < 10:
			price = fmt.Sprintf("0.%07d", r.IntN(9_999_999)+1)
		case k == 10:
			u.points = append(u.points, HistoryPoint{Bucket: t, VWAP: "not-a-price"})
			continue
		}
		u.points = append(u.points, HistoryPoint{Bucket: t, VWAP: price})
		if r.IntN(5) == 0 {
			u.stableAt[t] = true
		}
	}
	return u
}

// TestEmisPricesLine_MatchesPerHourValuer: run-summing within a price bucket, the common price denominator and the
// forward cursors serve exactly what the per-hour valuer served, wire for wire, bucket for bucket.
func TestEmisPricesLine_MatchesPerHourValuer(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for c := range 300 {
		width, span := time.Hour, time.Duration(1+r.IntN(60))*time.Hour
		if c%2 == 1 {
			width, span = 24*time.Hour, time.Duration(1+r.IntN(9))*24*time.Hour
		}
		win := emissionsHistoryWindow{from: from, end: from.Add(span - time.Duration(r.IntN(3600))*time.Second), width: width}
		segs := genSchedule(r, from, span)
		u := genSeries(r, from, span, []string{"", "", priceBasisTransitive}[c%3])
		if c%17 == 0 {
			u.reason = exclusionPriceWithheld
		}
		want, wantB := oracleScheduleLine(segs, win, u)
		got, gotB, err := newEmisPrices(u).line(context.Background(), segs, win)
		if err != nil {
			t.Fatal(err)
		}
		if g, w := emisOneUSD(&got, u.reason, false, false), emisOneUSD(&want, u.reason, false, false); !reflect.DeepEqual(g, w) {
			t.Fatalf("case %d total: got %+v %v, want %+v %v", c, g, dz(g.AmountUSD), w, dz(w.AmountUSD))
		}
		if len(gotB) != len(wantB) {
			t.Fatalf("case %d: %d buckets, want %d", c, len(gotB), len(wantB))
		}
		for j := range wantB {
			if (gotB[j].v[0] == nil) != (wantB[j].v[0] == nil) || (gotB[j].v[0] != nil && gotB[j].v[0].Cmp(wantB[j].v[0]) != 0) {
				t.Fatalf("case %d bucket %d: exact %v, want %v", c, j, gotB[j].v[0], wantB[j].v[0])
			}
			if g, w := emisOneUSD(&gotB[j], u.reason, false, false), emisOneUSD(&wantB[j], u.reason, false, false); !reflect.DeepEqual(g, w) {
				t.Fatalf("case %d bucket %d: got %+v, want %+v", c, j, g, w)
			}
		}
	}
}

// TestEmisPricesLine_StopsOnContext: a cancelled request stops the valuation rather than finishing it.
func TestEmisPricesLine_StopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	win := emissionsHistoryWindow{from: from, end: from.Add(48 * time.Hour), width: 24 * time.Hour}
	segs := []emissionSegment{{start: from, eps: big.NewInt(1), exp: from.Add(72 * time.Hour)}}
	if _, _, err := newEmisPrices(reserveUSDSeries{}).line(ctx, segs, win); err == nil {
		t.Fatal("cancelled context: no error")
	}
}

func dz(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
