package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func dayHour(at time.Time, gross int64) clickhouse.LendingRevenueBucket {
	s := clickhouse.NewLendingRevenueSums()
	s.Gross.SetInt64(gross)
	s.SupplierIntended.SetInt64(gross)
	s.SupplierRealised.SetInt64(gross)
	return clickhouse.LendingRevenueBucket{Start: at, Sums: s}
}

// Each kind of refusal lands where the rollup's rule puts it: a read-time verdict (scam issuer, operator withholding,
// thin latest market, all reported as verdict) is refused; a failed, cut-short or exclusion-unread walk is degraded; the
// price data's own answer (unpriced) is stored with its reason; a day with nothing to value reads no price at all.
func TestValueRevenueDay_RefusalKinds(t *testing.T) {
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	hours := []clickhouse.LendingRevenueBucket{dayHour(day.Add(time.Hour), 10_000_000)}
	priced := reserveUSDSeries{points: []HistoryPoint{{Bucket: day.Add(time.Hour), VWAP: "0.5"}}, maxAge: 6 * time.Hour, step: time.Hour, dec: 7}
	for _, c := range []struct {
		name    string
		read    reserveUSDRead
		outcome revenueDayOutcome
		reason  string
	}{
		{"scam issuer or operator withholding", reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}, revenueDayRefused, exclusionPriceWithheld},
		{"thin latest market", reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceWithheld}, verdict: true}, revenueDayRefused, exclusionPriceWithheld},
		{"exclusion list unread", reserveUSDRead{series: reserveUSDSeries{reason: exclusionPriceReadFailed, degraded: true}}, revenueDayDegraded, ""},
		{"walk cut short at its budget", reserveUSDRead{series: func() reserveUSDSeries { s := priced; s.degraded = true; return s }()}, revenueDayDegraded, ""},
		{"no market", reserveUSDRead{series: reserveUSDSeries{reason: backstopUSDUnpriced}}, revenueDayValued, backstopUSDUnpriced},
	} {
		d := valueRevenueDay(day, hours, func() reserveUSDRead { return c.read })
		if d.outcome != c.outcome || d.reason != c.reason {
			t.Errorf("%s: outcome %d reason %q, want %d %q", c.name, d.outcome, d.reason, c.outcome, c.reason)
		}
		if c.outcome != revenueDayValued && (d.sum.priced != 0 || d.sum.v[0] != nil) {
			t.Errorf("%s: valued %+v; a refused or degraded day values nothing", c.name, d.sum)
		}
	}
	d := valueRevenueDay(day, hours, func() reserveUSDRead { return reserveUSDRead{series: priced} })
	if d.outcome != revenueDayValued || d.sum.priced != 1 || d.sum.v[0].Cmp(big.NewRat(1, 2)) != 0 {
		t.Fatalf("priced day: %+v", d)
	}
	gulp := clickhouse.NewLendingRevenueSums()
	gulp.Gulped.SetInt64(5)
	idle := valueRevenueDay(day, []clickhouse.LendingRevenueBucket{{Start: day, Sums: gulp}}, func() reserveUSDRead {
		t.Fatal("a day with nothing to value read a price")
		return reserveUSDRead{}
	})
	if idle.outcome != revenueDayValued || idle.sum.priced+idle.sum.unpriced != 0 {
		t.Fatalf("idle day: %+v", idle)
	}
}

// A stored leg is the exact decimal of the valuer's big.Rat, unrounded; a day with nothing to value stores zeros, and a
// day whose amounts went unpriced stores no legs.
func TestRevenueDayRow_ExactLegs(t *testing.T) {
	day := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	info := clickhouse.LendingInterestDay{Day: day, DerivedAt: day.Add(time.Hour)}
	var sum revenueUSDSum
	sum.priced, sum.bases = 1, map[string]int{"": 1}
	for i := range sum.v {
		sum.v[i] = big.NewRat(int64(i)+1, 3*0+8) // eighths: exact in decimal
	}
	sum.v[0] = new(big.Rat).SetFrac(big.NewInt(123456789), new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil))
	row, ok := revenueDayRow("P", "A", info, clickhouse.NewLendingRevenueSums(), revenueDay{sum: sum})
	if !ok || *row.Legs[0] != "0.00000000123456789" || *row.Legs[1] != "0.25" || row.BasisHours["fiat"] != 1 {
		t.Fatalf("row %+v legs0 %v", row, row.Legs[0])
	}
	zero, _ := revenueDayRow("P", "A", info, clickhouse.NewLendingRevenueSums(), revenueDay{})
	if zero.Legs[0] == nil || *zero.Legs[0] != "0" {
		t.Errorf("nothing to value: legs %v, want 0", zero.Legs[0])
	}
	unpriced, _ := revenueDayRow("P", "A", info, clickhouse.NewLendingRevenueSums(), revenueDay{sum: revenueUSDSum{unpriced: 2}})
	if unpriced.Legs[0] != nil {
		t.Errorf("unpriced day stores legs %v", *unpriced.Legs[0])
	}
	if _, ok := ratExactDecimal(big.NewRat(1, 3)); ok {
		t.Error("1/3 has no exact decimal")
	}
}
