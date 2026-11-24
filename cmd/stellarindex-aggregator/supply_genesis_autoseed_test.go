package main

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Every kind is compared: equal nets with different mints and burns are still a hole in the served events.
func TestSEP41TotalsMatch(t *testing.T) {
	lake := clickhouse.TokenSupply{Mint: big.NewInt(900), Burn: big.NewInt(100), Clawback: big.NewInt(0)}
	for _, tc := range []struct {
		name   string
		served timescale.SEP41KindTotals
		ok     bool
	}{
		{"equal", timescale.SEP41KindTotals{Mint: big.NewInt(900), Burn: big.NewInt(100), Clawback: big.NewInt(0)}, true},
		{"nothing replayed", timescale.SEP41KindTotals{Mint: big.NewInt(0), Burn: big.NewInt(0), Clawback: big.NewInt(0)}, false},
		{"same net, missing pair", timescale.SEP41KindTotals{Mint: big.NewInt(850), Burn: big.NewInt(50), Clawback: big.NewInt(0)}, false},
		{"clawback differs", timescale.SEP41KindTotals{Mint: big.NewInt(900), Burn: big.NewInt(100), Clawback: big.NewInt(1)}, false},
	} {
		if err := sep41TotalsMatch(lake, tc.served, 50457424, 60000000); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestSEP41GapWindow_NamesAdmissionRange(t *testing.T) {
	from, to, ok := sep41GapWindow(65134033, 65588751)
	if !ok || from != 65134033 || to != 65588750 {
		t.Fatalf("got [%d, %d] ok=%v, want [65134033, 65588750] ok=true", from, to, ok)
	}
	for _, c := range []struct{ lake, served uint32 }{{65134033, 65134033}, {65134033, 65000000}, {0, 65588751}} {
		if _, _, ok := sep41GapWindow(c.lake, c.served); ok {
			t.Errorf("lake %d served %d: reported a gap", c.lake, c.served)
		}
	}
}

type fakeCoverageLake struct {
	tip      uint32
	sum      clickhouse.TokenSupply
	first    uint32
	firstAt  time.Time
	firstErr error
}

func (f *fakeCoverageLake) LakeTip(context.Context) (uint32, error) { return f.tip, nil }
func (f *fakeCoverageLake) TokenSupplyInLedgerRange(context.Context, string, uint32, uint32) (clickhouse.TokenSupply, error) {
	return f.sum, nil
}

func (f *fakeCoverageLake) FirstContractEvent(context.Context, string) (uint32, time.Time, error) {
	return f.first, f.firstAt, f.firstErr
}

type fakeCoverageStore struct {
	projected   uint32
	totals      timescale.SEP41KindTotals
	servedFirst uint32
	notBefore   time.Time
}

func (f *fakeCoverageStore) SEP41SupplyDurableLedger(context.Context) (uint32, error) {
	return f.projected, nil
}

func (f *fakeCoverageStore) SEP41EventTotalsInRange(context.Context, string, uint32, uint32) (timescale.SEP41KindTotals, error) {
	return f.totals, nil
}

func (f *fakeCoverageStore) SEP41FirstServedLedger(_ context.Context, _ string, notBefore time.Time) (uint32, error) {
	f.notBefore = notBefore
	return f.servedFirst, nil
}

// A contract watched after its first event names the missing window and both rebuild commands, sets the gauge to the window's
// start, and clears it once the served tier matches; a mismatch with no leading gap keeps the plain error and a zero gauge.
func TestSEP41CoverageCheck_GapGauge(t *testing.T) {
	const contract = "CGAPTESTCONTRACT"
	gauge := obs.AggregatorSEP41HistoryGapFromLedger.WithLabelValues(contract)
	firstAt := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	lake := &fakeCoverageLake{
		tip:   65700000,
		sum:   clickhouse.TokenSupply{Mint: big.NewInt(900), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		first: 65134033, firstAt: firstAt,
	}
	store := &fakeCoverageStore{
		projected:   65700000,
		totals:      timescale.SEP41KindTotals{Mint: big.NewInt(400), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		servedFirst: 65588751,
	}
	ctx := context.Background()

	err := checkSEP41Coverage(ctx, lake, store, 50457424, contract, 65700000)
	if !errors.Is(err, supply.ErrSEP41HistoryNotCovered) {
		t.Fatalf("gap: err=%v, want ErrSEP41HistoryNotCovered", err)
	}
	for _, want := range []string{
		contract, "[65134033, 65588750]", "projected-rebuild", "-source sep41_transfers -from 65134033 -to 65588750",
		"-source sep41_supply -from 65134033 -to 65588750", "#stellarindex_aggregator_sep41_history_gap",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("gap error lacks %q: %v", want, err)
		}
	}
	if got := testutil.ToFloat64(gauge); got != 65134033 {
		t.Errorf("gap gauge = %v, want 65134033", got)
	}
	if !store.notBefore.Equal(firstAt) {
		t.Errorf("served lookup floor = %v, want the lake's first close time %v", store.notBefore, firstAt)
	}

	store.servedFirst = 65134033
	err = checkSEP41Coverage(ctx, lake, store, 50457424, contract, 65700000)
	if !errors.Is(err, supply.ErrSEP41HistoryNotCovered) || strings.Contains(err.Error(), "projected-rebuild -config") {
		t.Errorf("no leading gap: err=%v, want the plain mismatch", err)
	}
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Errorf("no leading gap gauge = %v, want 0", got)
	}

	store.servedFirst = 65588751
	if err := checkSEP41Coverage(ctx, lake, store, 50457424, contract, 65700000); err == nil {
		t.Fatal("gap re-check passed")
	}
	store.totals = timescale.SEP41KindTotals{Mint: big.NewInt(900), Burn: big.NewInt(0), Clawback: big.NewInt(0)}
	if err := checkSEP41Coverage(ctx, lake, store, 50457424, contract, 65700000); err != nil {
		t.Fatalf("covered: %v", err)
	}
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Errorf("covered gauge = %v, want 0", got)
	}
}

func TestSEP41CoverageUpTo(t *testing.T) {
	for _, c := range []struct {
		name                            string
		boundary, tip, snap, proj, want uint32
		ok                              bool
	}{
		{"projector behind bounds the range", 100, 10_000, 10_000, 5_000, 5_000, true},
		{"settle margin below the slower tip", 100, 9_000, 10_000, 20_000, 8_880, true},
		{"no projector cursor", 100, 10_000, 10_000, 0, 0, false},
		{"tip inside the settle margin", 100, 150, 10_000, 10_000, 0, false},
	} {
		got, ok := sep41CoverageUpTo(c.boundary, c.tip, c.snap, c.proj)
		if got != c.want && ok || ok != c.ok {
			t.Errorf("%s: got %d/%v, want %d/%v", c.name, got, ok, c.want, c.ok)
		}
	}
}
