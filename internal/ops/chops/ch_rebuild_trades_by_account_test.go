package chops

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
