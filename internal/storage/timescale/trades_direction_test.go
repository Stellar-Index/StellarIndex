// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"math/big"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A market has no stored direction of its own: the SDEX decoder sets
// base = soldAsset, so one market lands in `trades` as both (A,B) and
// (B,A) rows. /v1/history folds the two in its caller; the two raw-trade
// readers below — [Store.LatestTradePerSource] behind /v1/observations
// and its stream, [Store.LatestTradesForPair] behind /v1/price's
// last-trade arm — bound (base_asset, quote_asset) literally and served
// NOTHING for a market recorded only the other way round. The surfaces
// therefore disagreed about whether such a market existed at all.
//
// These are the behavioural half, in the shape
// pair_direction_readers_test.go established for the CAGG readers: the
// scripted driver replays rows regardless of the SQL, so it can prove
// what happens to a flipped row once fetched but not that the flipped
// row is fetched. TestRawTradeReadsSpanBothStoredDirections pins the
// other half — the query shape.

const (
	dirUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	dirAQUA = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

	// Two hashes whose ORDER is load-bearing: dirTxB > dirTxA, so a
	// tie broken on tx_hash has a knowable winner.
	dirTxA = "a100000000000000000000000000000000000000000000000000000000000000"
	dirTxB = "b200000000000000000000000000000000000000000000000000000000000000"

	// One trade of the market, in smallest units: 100 AQUA for 20 USDC.
	// Stored AQUA/USDC that is price 0.2 USDC per AQUA; stored the other
	// way round it is 5 AQUA per USDC. Far apart, so a dropped or
	// uninverted leg is unmistakable.
	dirAQUAAmount = "1000000000"
	dirUSDCAmount = "200000000"
)

// latestTradeCols is the projection both readers select, in scan order.
var latestTradeCols = []string{
	"source", "ledger", "tx_hash", "op_index", "ts",
	"base_asset", "quote_asset", "base_amount", "quote_amount",
	"maker", "taker", "routed_via",
}

// dirAQUAUSDCPair is the market as a caller asks for it: AQUA priced in
// USDC.
func dirAQUAUSDCPair(t *testing.T) canonical.Pair {
	t.Helper()
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("NewClassicAsset AQUA: %v", err)
	}
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	pair, err := canonical.NewPair(aqua, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

// storedTradeRow renders one `trades` row exactly as the union selects
// it. `base`/`quote` are the row's OWN stored legs — the whole point is
// that they need not be the requested ones.
func storedTradeRow(
	source string, ledger int64, txHash string, opIndex int64, ts time.Time,
	base, quote, baseAmount, quoteAmount string,
) []driver.Value {
	return []driver.Value{
		source, ledger, txHash, opIndex, ts,
		base, quote, baseAmount, quoteAmount,
		"", "", "",
	}
}

// reverseStoredRow is the market recorded the way the launch plan's row
// 1.16 describes: USDC/AQUA, when the caller asks for AQUA/USDC.
func reverseStoredRow(source string, ledger int64, txHash string, ts time.Time) []driver.Value {
	return storedTradeRow(source, ledger, txHash, 0, ts,
		dirUSDC, dirAQUA, dirUSDCAmount, dirAQUAAmount)
}

// requestedStoredRow is the same market recorded the way round the
// caller asked for it.
func requestedStoredRow(source string, ledger int64, txHash string, ts time.Time) []driver.Value {
	return storedTradeRow(source, ledger, txHash, 0, ts,
		dirAQUA, dirUSDC, dirAQUAAmount, dirUSDCAmount)
}

// wantAQUAUSDCTrade asserts a returned trade is the fixture trade
// expressed as AQUA/USDC — the legs the caller asked for, the amounts
// attached to the right legs, and a price that is the EXACT reciprocal
// of the stored one rather than a rounded division of it.
func wantAQUAUSDCTrade(t *testing.T, got canonical.Trade, who string) {
	t.Helper()
	if got.Pair.Base.String() != dirAQUA || got.Pair.Quote.String() != dirUSDC {
		t.Fatalf("%s: returned %s/%s, want the REQUESTED orientation %s/%s — "+
			"a flipped row must be re-expressed, not relabelled",
			who, got.Pair.Base, got.Pair.Quote, dirAQUA, dirUSDC)
	}
	if got.BaseAmount.String() != dirAQUAAmount {
		t.Errorf("%s: base_amount = %s, want %s (the AQUA leg)",
			who, got.BaseAmount, dirAQUAAmount)
	}
	if got.QuoteAmount.String() != dirUSDCAmount {
		t.Errorf("%s: quote_amount = %s, want %s (the USDC leg)",
			who, got.QuoteAmount, dirUSDCAmount)
	}
	// price = quote/base = 200000000/1000000000 = 1/5 exactly, the
	// reciprocal of the stored 5/1. Compared as a rational so a
	// division that rounded anywhere would fail here.
	price := new(big.Rat).SetFrac(got.QuoteAmount.BigInt(), got.BaseAmount.BigInt())
	want := big.NewRat(1, 5)
	if price.Cmp(want) != 0 {
		t.Errorf("%s: price = %s, want exactly %s (the reciprocal of the stored 5)",
			who, price.RatString(), want.RatString())
	}
}

// pairBoundArm matches `base_asset = $a AND quote_asset = $b`, the same
// shape TestCAGGPairReadsFoldBothDirections looks for.
func pairBoundArm(a, b int) *regexp.Regexp {
	return regexp.MustCompile(
		`base_asset\s*=\s*\$` + strconv.Itoa(a) + `\s+AND\s+quote_asset\s*=\s*\$` + strconv.Itoa(b))
}

// TestLatestTradePerSource_ServesAMarketRecordedTheOtherWayRound is the
// /v1/observations case from row 1.16: the market exists only as
// USDC/AQUA and the caller asks for AQUA/USDC. Before the fold the read
// bound the requested orientation alone and the endpoint answered `[]`
// while /v1/history served the same market from the same rows.
func TestLatestTradePerSource_ServesAMarketRecordedTheOtherWayRound(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	ts := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	store, conn := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{reverseStoredRow("sdex", 58_000_001, dirTxA, ts)},
	})

	got, err := store.LatestTradePerSource(context.Background(), pair, "")
	if err != nil {
		t.Fatalf("LatestTradePerSource: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trades, want 1 — a market recorded only as "+
			"USDC/AQUA must be served under base=AQUA&quote=USDC", len(got))
	}
	wantAQUAUSDCTrade(t, got[0], "LatestTradePerSource")

	stmt := conn.only(t)
	if stmt.arg(t, 1) != dirAQUA || stmt.arg(t, 2) != dirUSDC {
		t.Errorf("bound ($1,$2) = (%v,%v), want (%s,%s)",
			stmt.arg(t, 1), stmt.arg(t, 2), dirAQUA, dirUSDC)
	}
}

// TestLatestTradePerSource_LeavesTheRequestedOrientationUntouched is the
// other half of the destructive-transform pair: the re-expression must
// fire ONLY on a flipped row. A row already stored the way it was asked
// for comes back byte-identical.
func TestLatestTradePerSource_LeavesTheRequestedOrientationUntouched(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	ts := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	store, _ := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{requestedStoredRow("sdex", 58_000_001, dirTxA, ts)},
	})

	got, err := store.LatestTradePerSource(context.Background(), pair, "")
	if err != nil {
		t.Fatalf("LatestTradePerSource: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trades, want 1", len(got))
	}
	wantAQUAUSDCTrade(t, got[0], "LatestTradePerSource")
	if got[0].TxHash != dirTxA || got[0].Ledger != 58_000_001 {
		t.Errorf("identity changed: tx_hash %s ledger %d, want %s / 58000001",
			got[0].TxHash, got[0].Ledger, dirTxA)
	}
}

// TestLatestTradePerSource_OneRowPerSourceAcrossDirections is the case
// the fold has to get right rather than merely survive: ONE source with
// a trade in each stored direction.
//
// "Latest per source" means one row, so the two are folded to whichever
// trade came later — and the answer must not depend on which arm of the
// union the database happened to return first, because that order flips
// with the requested orientation. Each case is therefore driven twice,
// with the two rows arriving in both possible orders.
func TestLatestTradePerSource_OneRowPerSourceAcrossDirections(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	ts := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		// rows in the requested and the reverse orientation
		requested, reverse []driver.Value
		wantTx             string
		wantFlipped        bool
		why                string
	}{
		{
			name:      "later trade is the reverse-stored one",
			requested: requestedStoredRow("sdex", 58_000_001, dirTxA, ts),
			reverse:   reverseStoredRow("sdex", 58_000_002, dirTxB, ts.Add(time.Second)),
			wantTx:    dirTxB,
			why:       "a later ts wins whichever direction holds it",
		},
		{
			name:      "later trade is the requested-stored one",
			requested: requestedStoredRow("sdex", 58_000_002, dirTxB, ts.Add(time.Second)),
			reverse:   reverseStoredRow("sdex", 58_000_001, dirTxA, ts),
			wantTx:    dirTxB,
			why:       "the reverse direction must not displace a newer row",
		},
		{
			name:      "same ts, higher ledger wins",
			requested: requestedStoredRow("sdex", 58_000_001, dirTxA, ts),
			reverse:   reverseStoredRow("sdex", 58_000_002, dirTxB, ts),
			wantTx:    dirTxB,
			why:       "ledger breaks a ts tie, as it does on every latest-trade read",
		},
		{
			name:      "same ts and ledger, tx_hash decides",
			requested: requestedStoredRow("sdex", 58_000_001, dirTxA, ts),
			reverse:   reverseStoredRow("sdex", 58_000_001, dirTxB, ts),
			wantTx:    dirTxB,
			why:       "tx_hash is the third component /v1/history orders on",
		},
	}

	for _, tc := range cases {
		for _, order := range []struct {
			name string
			rows [][]driver.Value
		}{
			{"requested arm first", [][]driver.Value{tc.requested, tc.reverse}},
			{"reverse arm first", [][]driver.Value{tc.reverse, tc.requested}},
		} {
			t.Run(tc.name+"/"+order.name, func(t *testing.T) {
				store, _ := newScriptedStore(t, scriptedResult{
					cols: latestTradeCols,
					rows: order.rows,
				})
				got, err := store.LatestTradePerSource(context.Background(), pair, "")
				if err != nil {
					t.Fatalf("LatestTradePerSource: %v", err)
				}
				if len(got) != 1 {
					t.Fatalf("returned %d rows for ONE source, want 1 — folding the "+
						"two stored directions must not double-count a source", len(got))
				}
				if got[0].TxHash != tc.wantTx {
					t.Fatalf("kept tx_hash %s, want %s (%s)", got[0].TxHash, tc.wantTx, tc.why)
				}
				// Whichever row won, it is served in the requested
				// orientation with the amounts on the right legs.
				wantAQUAUSDCTrade(t, got[0], "LatestTradePerSource")
			})
		}
	}
}

// TestLatestTradesForPair_ServesAMarketRecordedTheOtherWayRound is the
// /v1/price last-trade arm. Its snapshot echoes the row's OWN legs and
// derives the price from the row's own amounts, so a flipped row served
// unre-expressed would report `asset_id: USDC, quote: AQUA` at 5 for a
// request that asked what one AQUA is worth in USDC.
func TestLatestTradesForPair_ServesAMarketRecordedTheOtherWayRound(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	ts := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	store, conn := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{reverseStoredRow("sdex", 58_000_001, dirTxA, ts)},
	})

	got, err := store.LatestTradesForPair(context.Background(), pair, 1)
	if err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trades, want 1 — a market recorded only as "+
			"USDC/AQUA must be served under base=AQUA&quote=USDC", len(got))
	}
	wantAQUAUSDCTrade(t, got[0], "LatestTradesForPair")

	stmt := conn.only(t)
	if stmt.arg(t, 3) != 1 {
		t.Errorf("bound $3 = %v, want the caller's limit 1", stmt.arg(t, 3))
	}
}

// TestRawTradeReadsSpanBothStoredDirections is the query-shape half, and
// the cross-surface statement: these two reads now span exactly what
// /v1/history's page read spans, so the three surfaces agree about
// whether a market exists.
//
// The scripted driver replays its rows whatever the SQL says, so no
// behavioural test above can prove the flipped rows were FETCHED. This
// one reads the statement each method actually issued and requires both
// arms, in the same shape TestCAGGPairReadsFoldBothDirections requires
// of the CAGG readers.
func TestRawTradeReadsSpanBothStoredDirections(t *testing.T) {
	pair := dirAQUAUSDCPair(t)

	reads := []struct {
		name string
		run  func(*Store) error
	}{
		{
			name: "LatestTradePerSource",
			run: func(s *Store) error {
				_, err := s.LatestTradePerSource(context.Background(), pair, "")
				return err
			},
		},
		{
			name: "LatestTradesForPair",
			run: func(s *Store) error {
				_, err := s.LatestTradesForPair(context.Background(), pair, 1)
				return err
			},
		},
		{
			// The aggregate read, folded with row 1.16's second half.
			// Its arms are what /v1/vwap, /v1/twap, single-bar /v1/ohlc,
			// /v1/price/tip and the orchestrator all compute over.
			name: "TradesInRange",
			run: func(s *Store) error {
				_, err := s.TradesInRange(context.Background(), pair,
					time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC),
					time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC), 100)
				return err
			},
		},
	}

	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
			if err := r.run(store); err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			stmtSQL := conn.only(t).sql
			if !pairBoundArm(1, 2).MatchString(stmtSQL) {
				t.Errorf("%s does not bind the requested orientation ($1,$2):\n%s", r.name, stmtSQL)
			}
			if !pairBoundArm(2, 1).MatchString(stmtSQL) {
				t.Errorf(`%s reads ONE stored market direction.

`+"`trades`"+` holds this market as both (base, quote) and (quote, base)
rows, so binding `+"`base_asset = $1 AND quote_asset = $2`"+` alone drops
every trade of a market recorded the other way round — while /v1/history
serves it, because its caller folds the two. Read both directions and
re-express the flipped rows with orientTradeTo (swap the legs and the two
integer amounts; never divide).

Query:
%s`, r.name, stmtSQL)
			}
		})
	}
}

// TestLatestTradePerSource_SourceFilterAppliesToBothDirections: the
// filter is applied in SQL, so it has to be applied on each arm. A
// filter on one arm only would serve a single-source request the whole
// market from the other direction.
func TestLatestTradePerSource_SourceFilterAppliesToBothDirections(t *testing.T) {
	pair := dirAQUAUSDCPair(t)

	store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
	if _, err := store.LatestTradePerSource(context.Background(), pair, "sdex"); err != nil {
		t.Fatalf("LatestTradePerSource: %v", err)
	}
	stmt := conn.only(t)
	if stmt.arg(t, 3) != "sdex" {
		t.Errorf("bound $3 = %v, want the source filter %q", stmt.arg(t, 3), "sdex")
	}
	filter := regexp.MustCompile(`\(\$3\s*=\s*''\s+OR\s+source\s*=\s*\$3\)`)
	if n := len(filter.FindAllString(stmt.sql, -1)); n != 2 {
		t.Errorf("source filter appears %d time(s), want 2 — once per stored direction:\n%s",
			n, stmt.sql)
	}
}
