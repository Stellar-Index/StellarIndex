package v1

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// switchableDecimals answers every contract with 7, none found, or a failed read, as set between cycles.
type switchableDecimals struct {
	mu    sync.Mutex
	found bool
	err   error
}

func (d *switchableDecimals) set(found bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.found, d.err = found, err
}

func (d *switchableDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return 7, d.found, d.err
}

// hourlyUSDPrice prices one pair at 2 USD in every hour bucket; any other History method is unexpected.
type hourlyUSDPrice struct {
	HistoryReader
	pair string
}

func (h hourlyUSDPrice) HistoryPointsInRange(_ context.Context, p canonical.Pair, _ string, from, to time.Time, _ int) ([]HistoryPoint, error) {
	if p.String() != h.pair {
		return nil, nil
	}
	var out []HistoryPoint
	for b := from.Truncate(time.Hour); b.Before(to); b = b.Add(time.Hour) {
		if !b.Before(from) {
			out = append(out, HistoryPoint{Bucket: b, VWAP: "2"})
		}
	}
	return out, nil
}

// decimalsRollupHarness is the revenue rollup over a non-SAC token whose decimals are read from the lake, walked by the
// real day read.
func decimalsRollupHarness(t *testing.T) (*rollupHarness, *switchableDecimals) {
	t.Helper()
	tok, err := strkey.Encode(strkey.VersionByteContract, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	tokAsset, err := canonical.ParseAsset(tok)
	if err != nil {
		t.Fatal(err)
	}
	dec := &switchableDecimals{}
	srv := New(Options{
		History:       hourlyUSDPrice{pair: canonical.Pair{Base: tokAsset, Quote: defaultPriceQuote}.String()},
		TokenDecimals: dec,
	})
	h := newRollupHarness(t, map[string]int{"P": 3})
	h.r.walk = func(ctx context.Context, _ string, day time.Time) reserveUSDRead {
		return srv.revenueDayRead(ctx, tok, day, lendingRevenueDayWalkBudget)
	}
	return h, dec
}

// A failed decimals read is a failed read: the day is degraded, never stored as decimals_unknown, coverage does not pass
// it, and the next cycle values and stores it once the read succeeds.
func TestLendingRevenueRollup_DecimalsReadErrorIsRetriedNotFinal(t *testing.T) {
	h, dec := decimalsRollupHarness(t)
	dec.set(false, errors.New("lake read timed out"))
	if st := h.mustCycle(); st.Written != 0 || st.Degraded != 3 {
		t.Fatalf("failing cycle %+v; want all 3 days degraded and none written", st)
	}
	for i := range 3 {
		if row, ok := h.row("P", i); ok {
			t.Fatalf("day %d stored after a failed decimals read: %+v", i, row)
		}
	}
	if got := h.through("P"); !got.Equal(rollupDay(-1)) {
		t.Fatalf("coverage through %s; want the day before the floor", got.Format(time.DateOnly))
	}
	dec.set(true, nil)
	if st := h.mustCycle(); st.Written != 3 {
		t.Fatalf("recovered cycle %+v; want the 3 days written", st)
	}
	want := big.NewRat(2*1000, 10_000_000)
	for i := range 3 {
		row, ok := h.row("P", i)
		if !ok || row.WalkReason != nil || row.HoursValued != 1 || row.Legs[0] == nil {
			t.Fatalf("day %d after recovery: %+v (stored=%v); want its hour valued", i, row, ok)
		}
		if got, _ := new(big.Rat).SetString(*row.Legs[0]); got == nil || got.Cmp(want) != 0 {
			t.Fatalf("day %d interest USD %s; want %s (1000 base units at 7 decimals, 2 USD)", i, *row.Legs[0], want.FloatString(4))
		}
	}
	if got := h.through("P"); !got.Equal(rollupDay(2)) {
		t.Fatalf("coverage through %s; want day 2", got.Format(time.DateOnly))
	}
}

// A read that finds no declaration is a settled answer: stored final as decimals_unknown, coverage moving past it.
func TestLendingRevenueRollup_UndeclaredDecimalsIsStoredFinal(t *testing.T) {
	h, dec := decimalsRollupHarness(t)
	dec.set(false, nil)
	if st := h.mustCycle(); st.Written != 3 || st.Degraded != 0 {
		t.Fatalf("cycle %+v; want all 3 days written", st)
	}
	row, ok := h.row("P", 1)
	if !ok || row.WalkReason == nil || *row.WalkReason != backstopUSDDecimals || row.Legs[0] != nil {
		t.Fatalf("undeclared row %+v (stored=%v); want unvalued with reason %q", row, ok, backstopUSDDecimals)
	}
	if got := h.through("P"); !got.Equal(rollupDay(2)) {
		t.Fatalf("coverage through %s; want day 2", got.Format(time.DateOnly))
	}
}
