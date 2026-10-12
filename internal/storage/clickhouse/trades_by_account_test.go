package clickhouse

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestFanOutTradeByAccount(t *testing.T) {
	usdc, err := canonical.ParseAsset("USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	huge, _ := new(big.Int).SetString("170141183460469231731687303715884105727", 10) // i128 max: must survive untruncated
	tr := canonical.Trade{Pair: pair, BaseAmount: canonical.NewAmount(huge), QuoteAmount: canonical.NewAmount(big.NewInt(7)), Ledger: 9, TxHash: "ab", OpIndex: 1, Source: "sdex"}

	for _, tc := range []struct {
		name, taker, maker string
		roles              []string
	}{
		{"taker and maker", "GT", "GM", []string{TradeRoleTaker, TradeRoleMaker}},
		{"self-trade once", "GT", "GT", []string{TradeRoleTaker}},
		{"soroban, no maker", "GT", "", []string{TradeRoleTaker}},
		{"no parties", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr.Taker, tr.Maker = tc.taker, tc.maker
			rows := FanOutTradeByAccount(tr, "12.5")
			if len(rows) != len(tc.roles) {
				t.Fatalf("got %d rows, want %d", len(rows), len(tc.roles))
			}
			for i, r := range rows {
				if r.Role != tc.roles[i] {
					t.Errorf("row %d role %q, want %q", i, r.Role, tc.roles[i])
				}
				if r.Account == r.Counterparty && tc.taker != tc.maker {
					t.Errorf("row %d counterparty is the account itself", i)
				}
				if r.BaseAmount.Cmp(huge) != 0 || r.USDVolume != "12.5" {
					t.Errorf("row %d amount %s usd %q", i, r.BaseAmount, r.USDVolume)
				}
			}
		})
	}
}

func TestMergeAndCoverTradesByAccountWindows(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	runs := MergeTradesByAccountWindows([]TradesByAccountRun{
		{FromLedger: 201, ToLedger: 300, FromTs: day(3), ToTs: day(4)},
		{FromLedger: 2, ToLedger: 100, FromTs: day(1), ToTs: day(2)},
		{FromLedger: 101, ToLedger: 200, FromTs: day(2), ToTs: day(3)},
		{FromLedger: 500, ToLedger: 600, FromTs: day(8), ToTs: day(9)}, // gap 301..499
	})
	if len(runs) != 2 || runs[0].FromLedger != 2 || runs[0].ToLedger != 300 || !runs[0].ToTs.Equal(day(4)) {
		t.Fatalf("runs = %+v", runs)
	}
	if r, ok := CoveringTradesByAccountRun(runs, day(8).Add(time.Hour)); !ok || r.FromLedger != 500 {
		t.Errorf("horizon inside the later run: got %+v %v", r, ok)
	}
	if _, ok := CoveringTradesByAccountRun(runs, day(6)); ok {
		t.Error("horizon in the gap must not be covered: serving the earlier run would skip trades")
	}
}

func TestArchivedAccountTradesQuery_DedupsAndBounds(t *testing.T) {
	for _, cursor := range []bool{false, true} {
		q := archivedAccountTradesQuery(cursor)
		for _, want := range []string{
			"LIMIT 1 BY ts, ledger, tx_hash, op_index, source",
			"ingested_at DESC",
			"ts >= fromUnixTimestamp64Milli(?, 'UTC')",
			"ts < fromUnixTimestamp64Milli(?, 'UTC')",
		} {
			if !strings.Contains(q, want) {
				t.Errorf("cursor=%v: query lacks %q", cursor, want)
			}
		}
		if got := strings.Contains(q, "(ts, ledger, tx_hash, op_index) <"); got != cursor {
			t.Errorf("cursor=%v: keyset predicate present=%v", cursor, got)
		}
		if n := strings.Count(q, "?"); n != map[bool]int{false: 4, true: 8}[cursor] {
			t.Errorf("cursor=%v: %d placeholders", cursor, n)
		}
	}
}
