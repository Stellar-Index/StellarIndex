package v1_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func revenueDayHours(at ...time.Time) []clickhouse.LendingRevenueBucket {
	var out []clickhouse.LendingRevenueBucket
	for i, h := range at {
		s := clickhouse.NewLendingRevenueSums()
		s.Gross.SetInt64(12_345_671 * int64(i+3))
		s.Backstop.SetInt64(s.Gross.Int64() / 7)
		s.SupplierIntended.Sub(s.Gross, s.Backstop)
		s.SupplierRealised.Sub(s.SupplierIntended, big.NewInt(1))
		s.InterestDrawn.SetInt64(-int64(i))
		s.DefaultLoss.SetInt64(-3 * int64(i))
		out = append(out, clickhouse.LendingRevenueBucket{Start: h, Sums: s})
	}
	return out
}

// sumDays values each UTC day of hours with its own day walk and adds the days exactly.
func sumDays(t *testing.T, srv *v1.Server, asset string, hours []clickhouse.LendingRevenueBucket) v1.RevenueExactForTest {
	t.Helper()
	byDay := map[time.Time][]clickhouse.LendingRevenueBucket{}
	var days []time.Time
	for _, h := range hours {
		d := h.Start.Truncate(24 * time.Hour)
		if _, ok := byDay[d]; !ok {
			days = append(days, d)
		}
		byDay[d] = append(byDay[d], h)
	}
	var out v1.RevenueExactForTest
	for _, d := range days {
		r := srv.RevenueDayForTest(context.Background(), asset, d, byDay[d])
		if r.Outcome != "valued" {
			t.Fatalf("day %s: outcome %s", d, r.Outcome)
		}
		for i, x := range r.Legs {
			if x == nil {
				continue
			}
			if out.Legs[i] == nil {
				out.Legs[i] = new(big.Rat)
			}
			out.Legs[i].Add(out.Legs[i], x)
		}
		out.Priced, out.Unpriced, out.Carried = out.Priced+r.Priced, out.Unpriced+r.Unpriced, out.Carried+r.Carried
	}
	return out
}

func revenueFixture(hours []clickhouse.LendingRevenueBucket, asset string, from time.Time) clickhouse.LendingRevenue {
	return clickhouse.LendingRevenue{
		Covered: true, FloorLedger: 1, FloorTime: from.Add(-time.Hour), ThroughLedger: 50, ThroughHour: from.Add(95 * time.Hour),
		From: from, Before: from.Add(96 * time.Hour),
		Reserves: []clickhouse.LendingRevenueReserve{{Asset: asset, Hours: hours, Buckets: clickhouse.FoldLendingRevenueDays(hours)}},
	}
}

// Where the window walk and the day walks pick the same price basis, the stored days add up to /revenue's daily totals
// exactly: every leg as a big.Rat, and every hour count, a price carried across midnight included.
func TestLendingRevenueDay_SumOfDaysEqualsRevenueTotals(t *testing.T) {
	xlm := canonical.NativeSACContractID()
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return from.Add(time.Duration(n) * time.Hour) }
	hours := revenueDayHours(h(1), h(2), h(9), h(23), h(25), h(30), h(50), h(70))
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{}, Lending: &stubLendingReader{version: ptrVersion(blend.PoolV2)},
		History:       xlmUSDPrices(map[time.Time]string{h(0): "0.1234567", h(22): "0.31", h(29): "0.2999999", h(48): "0.25", h(69): "0.2"}),
		TokenDecimals: &lockstepDecStub{d: 7, found: true},
	})
	q := clickhouse.LendingHistoryQuery{From: from, To: from.Add(96 * time.Hour), Daily: true, MaxPoints: 10}
	window := srv.RevenueWindowForTest(context.Background(), mkCStrkey(t, 1), q, revenueFixture(hours, xlm, from))[0]
	days := sumDays(t, srv, xlm, hours)
	if window.Priced != days.Priced || window.Unpriced != days.Unpriced || window.Carried != days.Carried {
		t.Fatalf("hours: window %d/%d/%d, days %d/%d/%d", window.Priced, window.Unpriced, window.Carried, days.Priced, days.Unpriced, days.Carried)
	}
	if window.Priced == 0 || window.Unpriced == 0 || window.Carried == 0 {
		t.Fatalf("fixture must price, carry and miss hours: %+v", window)
	}
	for i := range window.Legs {
		if window.Legs[i] == nil || days.Legs[i] == nil || window.Legs[i].Cmp(days.Legs[i]) != 0 {
			t.Errorf("leg %d: window %v, days %v", i, window.Legs[i], days.Legs[i])
		}
	}
}

// The documented divergence: a market priced on day one only. The window walk finds its own prices and leaves day two's
// hour unpriced; the walk of day two alone finds none and prices it through XLM.
func TestLendingRevenueDay_QuietDayDivergesThroughXLM(t *testing.T) {
	tok := mkCStrkey(t, 9)
	tokAsset, err := canonical.ParseAsset(tok)
	if err != nil {
		t.Fatal(err)
	}
	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatal(err)
	}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return from.Add(time.Duration(n) * time.Hour) }
	hours := revenueDayHours(h(1), h(30))
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{}, Lending: &stubLendingReader{version: ptrVersion(blend.PoolV2)},
		History: rangedHistory{pairPrices(map[string]map[time.Time]string{
			canonical.Pair{Base: tokAsset, Quote: usd}.String():    {h(1): "2"},
			canonical.Pair{Base: tokAsset, Quote: native}.String(): {h(1): "20", h(30): "21"},
			"native/fiat:USD": {h(1): "0.1", h(30): "0.1"},
		})},
		TokenDecimals: &lockstepDecStub{d: 7, found: true},
	})
	q := clickhouse.LendingHistoryQuery{From: from, To: from.Add(48 * time.Hour), Daily: true, MaxPoints: 10}
	window := srv.RevenueWindowForTest(context.Background(), mkCStrkey(t, 1), q, revenueFixture(hours, tok, from))[0]
	if window.Priced != 1 || window.Unpriced != 1 {
		t.Fatalf("window walk: %d priced, %d unpriced; want day two's hour unpriced", window.Priced, window.Unpriced)
	}
	dayTwo := srv.RevenueDayForTest(context.Background(), tok, h(24), hours[1:])
	if dayTwo.Outcome != "valued" || dayTwo.Priced != 1 || dayTwo.Unpriced != 0 {
		t.Fatalf("day two walk: %+v; want its hour priced through XLM", dayTwo)
	}
}

// A thin latest market is a read-time verdict: the day is refused and nothing of it is valued, never stored as zero.
func TestLendingRevenueDay_ThinMarketIsRefusedNotZero(t *testing.T) {
	thin := mkCStrkey(t, 5)
	thinAsset, err := canonical.ParseAsset(thin)
	if err != nil {
		t.Fatal(err)
	}
	usd := canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{}, Lending: &stubLendingReader{version: ptrVersion(blend.PoolV2)},
		History:       pairPrices(map[string]map[time.Time]string{canonical.Pair{Base: thinAsset, Quote: usd}.String(): {day.Add(time.Hour): "2"}}),
		TokenDecimals: &lockstepDecStub{d: 7, found: true},
		Substance:     pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{thinAsset.String(): true}}, pricingguard.SubstanceGateOptions{}),
	})
	r := srv.RevenueDayForTest(context.Background(), thin, day, revenueDayHours(day.Add(2*time.Hour)))
	if r.Outcome != "refused" || r.Priced != 0 || r.Legs[0] != nil {
		t.Fatalf("thin day: %+v; want refused with nothing valued", r)
	}
}

func ptrVersion(v blend.PoolVersion) *blend.PoolVersion { return &v }

// rangedHistory answers only the buckets inside each read's [from, to), as the price store does.
type rangedHistory struct{ *lockedHistoryReader }

func (r rangedHistory) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	pts, err := r.lockedHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, limit)
	var out []v1.HistoryPoint
	for _, pt := range pts {
		if !pt.Bucket.Before(from) && (to.IsZero() || pt.Bucket.Before(to)) {
			out = append(out, pt)
		}
	}
	return out, err
}
