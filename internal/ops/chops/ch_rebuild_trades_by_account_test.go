package chops

import (
	"context"
	"errors"
	"io"
	"math"
	"math/big"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestCheckCHRebuildTradesByAccountFlags(t *testing.T) {
	ok := chRebuildTradesByAccountConflicts{write: true, sdex: true}
	if err := checkCHRebuildTradesByAccountFlags(true, ok); err != nil {
		t.Fatalf("full set refused: %v", err)
	}
	if err := checkCHRebuildTradesByAccountFlags(false, chRebuildTradesByAccountConflicts{}); err != nil {
		t.Fatalf("flag off must not check: %v", err)
	}
	for name, c := range map[string]chRebuildTradesByAccountConflicts{
		"-write":               {sdex: true},
		"-sdex":                {write: true},
		"-sources":             {write: true, sdex: true, sources: "aquarius"},
		"-contracts":           {write: true, sdex: true, contracts: "CA"},
		"-sdex-gaps":           {write: true, sdex: true, sdexGaps: true},
		"-bulk-trades":         {write: true, sdex: true, bulkTrades: true},
		"-record-dirty-window": {write: true, sdex: true, recordDirty: true},
	} {
		err := checkCHRebuildTradesByAccountFlags(true, c)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := checkCHRebuildTradesByAccountFlags(false, chRebuildTradesByAccountConflicts{replace: true}); err == nil {
		t.Error("-replace without -trades-by-account accepted")
	}
	if err := checkCHRebuildTradesByAccountFlags(true, chRebuildTradesByAccountConflicts{write: true, sdex: true, replace: true}); err != nil {
		t.Errorf("-replace with -trades-by-account refused: %v", err)
	}
}

// Leg 2 of the BackfillSafe gate, the per-WASM lake gate and the preseed all read the catalogue; a whole-catalogue scope hits
// unaudited sources (upshift) and refuses every -trades-by-account run.
func TestScopeCatalogueToTradeSources_PassesBackfillSafeGate(t *testing.T) {
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	cat = dropReconSources(cat, sep41transfers.SourceName, sep41supply.SourceName)
	scoped, err := scopeCatalogueToTradeSources(cat)
	if err != nil {
		t.Fatal(err)
	}
	inRun := reDerivedSourcesInRun(scoped, nil, chRebuildPasses{sdex: true}, func(string) bool { return true })
	sort.Strings(inRun)
	want := []string{aquarius.SourceName, comet.SourceName, phoenix.SourceName, sdex.SourceName, soroswap.SourceName, sushiswap_v3.SourceName}
	sort.Strings(want)
	if strings.Join(inRun, ",") != strings.Join(want, ",") {
		t.Fatalf("sources reaching the gate = %v, want %v", inRun, want)
	}
	if gerr := checkCHRebuildBackfillSafe(inRun); gerr != nil {
		t.Fatalf("scoped run refused: %v", gerr)
	}
	if got := tradeSourcesKey(scoped); got != clickhouse.TradesSourcesKey(clickhouse.TradesByAccountSources) {
		t.Errorf("recorded key %q differs from the key the API counts", got)
	}
	if _, err := scopeCatalogueToTradeSources(dropReconSources(cat, comet.SourceName)); err == nil || !strings.Contains(err.Error(), comet.SourceName) {
		t.Errorf("catalogue missing comet: err = %v", err)
	}
	// Each scoped source's trades must be read by tradeOf, or the window would claim trades it dropped.
	for _, ev := range []consumer.Event{aquarius.TradeEvent{}, comet.TradeEvent{}, phoenix.TradeEvent{}, sdex.TradeEvent{}, soroswap.TradeEvent{}, sushiswap_v3.TradeEvent{}} {
		if _, ok := tradeOf(ev); !ok {
			t.Errorf("tradeOf ignores %T", ev)
		}
	}
}

func TestWriteCHRebuildTradesByAccount_RefusesAmountOutsideInt128(t *testing.T) {
	over := new(big.Int).Lsh(big.NewInt(1), 127)
	prepare := func(context.Context, []canonical.Trade) ([]canonical.Trade, []string, error) {
		return []canonical.Trade{{Taker: "GT", BaseAmount: canonical.NewAmount(over), QuoteAmount: canonical.NewAmount(big.NewInt(1))}}, []string{""}, nil
	}
	var inserted, recorded bool
	insert := func(context.Context, []clickhouse.TradeByAccountRow) (int64, error) { inserted = true; return 0, nil }
	record := func(context.Context, uint32, uint32, int64) error { recorded = true; return nil }
	if err := writeCHRebuildTradesByAccount(context.Background(), io.Discard, prepare, insert, record, nil, 1, 2); err == nil || inserted || recorded {
		t.Fatalf("err=%v inserted=%v recorded=%v", err, inserted, recorded)
	}
}

// A window is the API's proof of completeness, so it must never be recorded after a failed or partial insert.
func TestWriteCHRebuildTradesByAccount_RecordsOnlyAfterFullInsert(t *testing.T) {
	prepare := func(context.Context, []canonical.Trade) ([]canonical.Trade, []string, error) {
		return []canonical.Trade{{Taker: "GT", Maker: "GM"}}, []string{""}, nil
	}
	for _, tc := range []struct {
		name      string
		insertErr error
		wantRec   bool
	}{{"success", nil, true}, {"insert fails", errors.New("boom"), false}} {
		t.Run(tc.name, func(t *testing.T) {
			var recorded bool
			var recRows int64
			insert := func(_ context.Context, rows []clickhouse.TradeByAccountRow) (int64, error) {
				if len(rows) != 2 {
					t.Errorf("fan-out gave %d rows, want 2", len(rows))
				}
				if tc.insertErr != nil {
					return 1, tc.insertErr
				}
				return int64(len(rows)), nil
			}
			record := func(_ context.Context, lo, hi uint32, n int64) error {
				recorded, recRows = true, n
				return nil
			}
			err := writeCHRebuildTradesByAccount(context.Background(), io.Discard, prepare, insert, record, nil, 10, 20)
			if (err != nil) == tc.wantRec || recorded != tc.wantRec {
				t.Fatalf("err=%v recorded=%v", err, recorded)
			}
			if tc.wantRec && recRows != 2 {
				t.Errorf("recorded %d rows, want 2", recRows)
			}
		})
	}
}

func TestEachTradesByAccountWindow(t *testing.T) {
	type win struct{ lo, hi uint32 }
	collect := func(lo, hi, step uint32, failAt uint32) ([]win, error) {
		var got []win
		err := eachTradesByAccountWindow(lo, hi, step, func(wlo, whi uint32) error {
			got = append(got, win{wlo, whi})
			if wlo == failAt {
				return errors.New("boom")
			}
			return nil
		})
		return got, err
	}
	for _, tc := range []struct {
		name         string
		lo, hi, step uint32
		want         []win
	}{
		{"partial tail", 100, 349, 100, []win{{100, 199}, {200, 299}, {300, 349}}},
		{"exact fit", 1, 20, 10, []win{{1, 10}, {11, 20}}},
		{"single ledger", 7, 7, 10, []win{{7, 7}}},
		{"top of uint32", math.MaxUint32 - 4, math.MaxUint32, 3, []win{{math.MaxUint32 - 4, math.MaxUint32 - 2}, {math.MaxUint32 - 1, math.MaxUint32}}},
	} {
		got, err := collect(tc.lo, tc.hi, tc.step, 0)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	// A failed window stops the run: no later window is decoded or recorded.
	got, err := collect(1, 50, 10, 21)
	if err == nil || !slices.Equal(got, []win{{1, 10}, {11, 20}, {21, 30}}) {
		t.Errorf("stop on error: got %v, %v", got, err)
	}
}

func TestCheckCHRebuildRangeFlags(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		from, to                     uint
		toHorizon, tba, replace, bad bool
	}{
		{name: "range", from: 2, to: 10},
		{name: "no range", bad: true},
		{name: "inverted range", from: 10, to: 2, bad: true},
		{name: "to-horizon", toHorizon: true, tba: true},
		{name: "to-horizon without trades-by-account", toHorizon: true, bad: true},
		{name: "to-horizon with a range", from: 2, to: 10, toHorizon: true, tba: true, bad: true},
		{name: "to-horizon with replace", toHorizon: true, tba: true, replace: true, bad: true},
	} {
		err := checkCHRebuildRangeFlags("c.toml", tc.from, tc.to, tc.toHorizon, tc.tba, tc.replace)
		if (err != nil) != tc.bad {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.bad)
		}
	}
}

func TestTradesByAccountCatchUp(t *testing.T) {
	horizon := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	run := func(tip uint32, recorded bool, stop uint32, inLake bool) (uint32, uint32, bool, error) {
		return tradesByAccountCatchUp(context.Background(),
			func(context.Context) (uint32, bool, error) { return tip, recorded, nil },
			func(context.Context) (time.Time, error) { return horizon, nil },
			func(_ context.Context, at time.Time) (uint32, bool, error) {
				if !at.Equal(horizon) {
					t.Fatalf("ledgerAt(%s), want the horizon", at)
				}
				return stop, inLake, nil
			}, 1000)
	}
	if lo, hi, done, err := run(500, true, 800, true); err != nil || done || lo != 501 || hi != 800 {
		t.Errorf("behind: [%d,%d] done=%v err=%v, want [501,800]", lo, hi, done, err)
	}
	if lo, hi, _, _ := run(500, true, 5000, true); lo != 501 || hi != 1500 {
		t.Errorf("far behind: [%d,%d], want capped at [501,1500]", lo, hi)
	}
	if _, _, done, err := run(800, true, 800, true); err != nil || !done {
		t.Errorf("at the horizon: done=%v err=%v, want caught up", done, err)
	}
	if _, _, _, err := run(0, false, 800, true); err == nil {
		t.Error("no recorded window: want an error, not a run from ledger 1")
	}
	if _, _, _, err := run(500, true, 0, false); err == nil {
		t.Error("lake short of the horizon: want an error")
	}
}
