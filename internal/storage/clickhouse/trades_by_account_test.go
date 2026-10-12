package clickhouse

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

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
			rows, err := FanOutTradeByAccount(tr, "12.5")
			if err != nil {
				t.Fatal(err)
			}
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

// The column would wrap an out-of-range amount into a wrong served number, so the fan-out must fail the window instead.
func TestFanOutTradeByAccount_RefusesAmountOutsideInt128(t *testing.T) {
	over := new(big.Int).Add(int128Max, big.NewInt(1))
	under := new(big.Int).Sub(int128Min, big.NewInt(1))
	for name, tr := range map[string]canonical.Trade{
		"base above max":  {BaseAmount: canonical.NewAmount(over), QuoteAmount: canonical.NewAmount(big.NewInt(1)), Taker: "GT"},
		"quote below min": {BaseAmount: canonical.NewAmount(big.NewInt(1)), QuoteAmount: canonical.NewAmount(under), Taker: "GT"},
	} {
		if rows, err := FanOutTradeByAccount(tr, ""); err == nil || rows != nil {
			t.Errorf("%s: rows=%v err=%v, want refusal", name, rows, err)
		}
	}
	edge := canonical.Trade{BaseAmount: canonical.NewAmount(int128Min), QuoteAmount: canonical.NewAmount(int128Max), Taker: "GT"}
	if _, err := FanOutTradeByAccount(edge, ""); err != nil {
		t.Errorf("Int128 bounds themselves refused: %v", err)
	}
}

func TestMergeAndCoverTradesByAccountWindows(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	key := TradesSourcesKey(TradesByAccountSources)
	runs := MergeTradesByAccountWindows([]TradesByAccountRun{
		{FromLedger: 201, ToLedger: 300, FromTs: day(3), ToTs: day(4), Sources: key},
		{FromLedger: 2, ToLedger: 100, FromTs: day(1), ToTs: day(2), Sources: key},
		{FromLedger: 101, ToLedger: 200, FromTs: day(2), ToTs: day(3), Sources: key},
		{FromLedger: 500, ToLedger: 600, FromTs: day(8), ToTs: day(9), Sources: key},    // gap 301..499
		{FromLedger: 301, ToLedger: 499, FromTs: day(4), ToTs: day(8), Sources: "sdex"}, // other source set: not coverage
		{FromLedger: 601, ToLedger: 700, FromTs: day(9), ToTs: day(10)},                 // pre-key row: not coverage
	}, key)
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

func TestTradesSourcesKey_StableAcrossOrder(t *testing.T) {
	if a, b := TradesSourcesKey([]string{"sdex", "aquarius", "sdex"}), TradesSourcesKey([]string{"aquarius", "sdex"}); a != b || a != "aquarius,sdex" {
		t.Errorf("keys %q %q", a, b)
	}
}

// recordWindowFakeConn answers the close-time read and captures the window insert.
type recordWindowFakeConn struct {
	driver.Conn
	inserted []any
}

type closeTimeRow struct{}

func (closeTimeRow) Err() error { return nil }
func (closeTimeRow) Scan(dest ...any) error {
	for _, d := range dest {
		*d.(*time.Time) = time.Unix(0, 0).UTC()
	}
	return nil
}
func (closeTimeRow) ScanStruct(any) error { return nil }

func (c *recordWindowFakeConn) QueryRow(context.Context, string, ...any) driver.Row {
	return closeTimeRow{}
}

func (c *recordWindowFakeConn) Exec(_ context.Context, _ string, args ...any) error {
	c.inserted = args
	return nil
}

// A decode over a lake hole finds no trades there; recording that window would claim them absent.
func TestRecordTradesByAccountWindow_RefusesUncertifiedLake(t *testing.T) {
	key := TradesSourcesKey(TradesByAccountSources)
	for _, tc := range []struct {
		name    string
		check   tradesWindowSubstrateCheck
		key     string
		wantRec bool
	}{
		{"complete", func(context.Context, uint32, uint32) (uint32, bool, string, error) { return 0, false, "", nil }, key, true},
		{"hole", func(context.Context, uint32, uint32) (uint32, bool, string, error) {
			return 150, true, "substrate: missing ledger at 150", nil
		}, key, false},
		{"check fails", func(context.Context, uint32, uint32) (uint32, bool, string, error) {
			return 0, false, "", errors.New("down")
		}, key, false},
		{"no source set", func(context.Context, uint32, uint32) (uint32, bool, string, error) { return 0, false, "", nil }, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &recordWindowFakeConn{}
			err := recordTradesByAccountWindowOn(context.Background(), conn, tc.check, 100, 200, 5, tc.key)
			if (err == nil) != tc.wantRec || (conn.inserted != nil) != tc.wantRec {
				t.Fatalf("err=%v inserted=%v", err, conn.inserted)
			}
			if tc.wantRec && conn.inserted[5] != key {
				t.Errorf("window sources = %v, want %q", conn.inserted[5], key)
			}
		})
	}
}

// A re-run over a recorded window would leave its stale rows beside the new ones, so it needs -replace, and -replace must not cut a
// window it cannot fully clear.
func TestCheckTradesByAccountOverlap(t *testing.T) {
	recorded := []TradesByAccountRun{{FromLedger: 100, ToLedger: 199}, {FromLedger: 200, ToLedger: 299}}
	for _, tc := range []struct {
		name    string
		lo, hi  uint32
		replace bool
		ok      bool
	}{
		{"disjoint", 300, 399, false, true},
		{"overlap refused", 150, 160, false, false},
		{"touching end refused", 299, 400, false, false},
		{"replace covering", 100, 299, true, true},
		{"replace cutting a window", 150, 299, true, false},
	} {
		if err := CheckTradesByAccountOverlap(recorded, tc.lo, tc.hi, tc.replace); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}
