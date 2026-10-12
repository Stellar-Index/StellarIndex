package chops

import (
	"context"
	"errors"
	"io"
	"math/big"
	"sort"
	"strings"
	"testing"

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

// The documented invocation must clear every refusal that runs before the config load (flag conflicts, BackfillSafe leg 1).
func TestCHRebuild_TradesByAccountInvocationPassesEarlyRefusals(t *testing.T) {
	err := chRebuild(chRebuildArgs(t, "-write", "-sdex", "-trades-by-account"))
	if err == nil || strings.Contains(err.Error(), "refusing") || strings.Contains(err.Error(), "-trades-by-account") {
		t.Fatalf("err = %v, want only the absent-config failure", err)
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
