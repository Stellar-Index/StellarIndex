// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	dirUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	dirAQUA = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

	// Two hashes whose ORDER is load-bearing: dirTxB > dirTxA, so a
	// tie broken on tx_hash has a knowable winner.
	dirTxA = "a100000000000000000000000000000000000000000000000000000000000000"
	dirTxB = "b200000000000000000000000000000000000000000000000000000000000000"

	// One trade of the market in smallest units: 100 AQUA for 20 USDC, i.e. 0.2 USDC per AQUA stored
	// AQUA/USDC, or 5 AQUA per USDC the other way. Far apart, so a dropped or uninverted leg is unmistakable.
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

// wantAQUAUSDCTrade asserts a returned trade is the fixture expressed as AQUA/USDC: the requested legs,
// amounts on the right legs, and a price that is the exact reciprocal of the stored one, not a rounded
// division.
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
// shape TestSourceShapeRules/cagg_pair_reads_fold_both_directions looks for.
func pairBoundArm(a, b int) *regexp.Regexp {
	return regexp.MustCompile(
		`base_asset\s*=\s*\$` + strconv.Itoa(a) + `\s+AND\s+quote_asset\s*=\s*\$` + strconv.Itoa(b))
}

// TestLatestTradePerSource_ServesAMarketRecordedTheOtherWayRound is the /v1/observations case:
// the market exists only as USDC/AQUA and the caller asks for AQUA/USDC. Binding the requested
// orientation alone would answer `[]` while /v1/history serves the same market from the same rows.
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

// TestLatestTradePerSource_LeavesTheRequestedOrientationUntouched pins that re-expression fires only on a
// flipped row; a row already stored in the requested orientation comes back byte-identical.
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

// TestLatestTradePerSource_OneRowPerSourceAcrossDirections: a source with a trade in each stored direction
// yields one row, the later trade, regardless of which union arm returns first. Each case runs with rows in
// both orders.
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

// TestLatestTradesForPair_ServesAMarketRecordedTheOtherWayRound is the /v1/price last-trade arm: its
// snapshot echoes the row's own legs and derives price from its amounts, so a flipped row served unre-
// expressed would answer a different question.
func TestLatestTradesForPair_ServesAMarketRecordedTheOtherWayRound(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	ts := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	store, conn := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{reverseStoredRow("sdex", 58_000_001, dirTxA, ts)},
	})

	got, err := store.LatestTradesForPair(context.Background(), pair, 1, time.Time{})
	if err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trades, want 1 — a market recorded only as "+
			"USDC/AQUA must be served under base=AQUA&quote=USDC", len(got))
	}
	wantAQUAUSDCTrade(t, got[0], "LatestTradesForPair")

	if got := strings.Count(conn.only(t).sql, "LIMIT 1\n"); got != 1 {
		t.Errorf("outer LIMIT 1 literal appears %d times, want 1:\n%s", got, conn.only(t).sql)
	}
}

// TestLatestTradesForPair_ClampsLimit: the limit is both arms' LIMIT and the outer one, inlined as a literal so
// Postgres caches the generic plan instead of re-planning every trades chunk per call; it alone sizes each walk.
func TestLatestTradesForPair_ClampsLimit(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"default", 0, 100},
		{"in range", 5, 5},
		{"at cap", maxLatestTradesForPair, maxLatestTradesForPair},
		{"above cap", 1 << 30, maxLatestTradesForPair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols}, scriptedResult{cols: latestTradeCols})
			if _, err := store.LatestTradesForPair(context.Background(), pair, tc.limit, time.Time{}); err != nil {
				t.Fatalf("LatestTradesForPair: %v", err)
			}
			if len(conn.stmts) != 2 {
				t.Fatalf("an empty recent window issued %d statements, want the recent try + the fallback", len(conn.stmts))
			}
			for _, stmt := range conn.stmts {
				if got := strings.Count(stmt.sql, fmt.Sprintf("LIMIT %d", tc.want)); got != 3 {
					t.Errorf("limit %d: LIMIT %d literal on %d of 3 clauses:\n%s", tc.limit, tc.want, got, stmt.sql)
				}
				if strings.Contains(stmt.sql, "$3") || len(stmt.args) != 2 {
					t.Errorf("limit %d: want only $1/$2 bound, got %d args", tc.limit, len(stmt.args))
				}
			}
		})
	}
}

// TestLatestTradesForPair_SinceIsAPlanTimeLiteral: the /v1/price miss path is cheap only when both arms carry the bound as a literal
// TimescaleDB can exclude chunks on while planning; a zero since keeps the unbounded walk as the fallback.
func TestLatestTradesForPair_SinceIsAPlanTimeLiteral(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	since := time.Date(2025, 9, 12, 8, 30, 0, 0, time.FixedZone("x", 3600))

	store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols}, scriptedResult{cols: latestTradeCols})
	if _, err := store.LatestTradesForPair(context.Background(), pair, 1, since); err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	if got := strings.Count(conn.stmts[1].sql, "AND ts >= TIMESTAMPTZ '2025-09-12 07:30:00+00'"); got != 2 {
		t.Errorf("fallback carries the literal on %d arms, want 2:\n%s", got, conn.stmts[1].sql)
	}

	store, conn = newScriptedStore(t, scriptedResult{cols: latestTradeCols}, scriptedResult{cols: latestTradeCols})
	if _, err := store.LatestTradesForPair(context.Background(), pair, 1, time.Time{}); err != nil {
		t.Fatalf("LatestTradesForPair unbounded: %v", err)
	}
	if strings.Contains(conn.stmts[1].sql, "TIMESTAMPTZ") {
		t.Errorf("zero since must not bound the fallback walk:\n%s", conn.stmts[1].sql)
	}
}

// TestLatestTradesForPair_TriesTheRecentWindowFirst: a wide bound plans every trades chunk it spans (~1s on a fresh connection at 400
// days), so the read first tries a midnight-floored 7-day literal and stops there once it has `limit` rows. A bound already inside
// the window is used as given.
func TestLatestTradesForPair_TriesTheRecentWindowFirst(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	now := time.Now().UTC()
	row := reverseStoredRow("sdex", 58_000_001, dirTxA, now.Add(-time.Hour))

	store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols, rows: [][]driver.Value{row}})
	got, err := store.LatestTradesForPair(context.Background(), pair, 1, now.AddDate(-1, 0, 0))
	if err != nil || len(got) != 1 {
		t.Fatalf("LatestTradesForPair = %d rows, %v; want the recent row", len(got), err)
	}
	sql := conn.only(t).sql
	if strings.Count(sql, "AND ts >= TIMESTAMPTZ '") != 2 {
		t.Fatalf("recent try is not bounded on both arms:\n%s", sql)
	}
	lit := sql[strings.Index(sql, "TIMESTAMPTZ '")+len("TIMESTAMPTZ '"):]
	bound, err := time.Parse("2006-01-02 15:04:05-07", lit[:strings.Index(lit, "'")])
	if err != nil {
		t.Fatalf("recent bound: %v", err)
	}
	if !bound.Equal(bound.Truncate(24*time.Hour)) || bound.After(now.Add(-recentTradesWindow)) || bound.Before(now.Add(-recentTradesWindow-24*time.Hour)) {
		t.Errorf("recent bound %s, want the UTC midnight at or before now-%s", bound, recentTradesWindow)
	}

	inside := now.Add(-time.Hour)
	store, conn = newScriptedStore(t, scriptedResult{cols: latestTradeCols})
	if _, err := store.LatestTradesForPair(context.Background(), pair, 1, inside); err != nil {
		t.Fatalf("LatestTradesForPair inside the window: %v", err)
	}
	if want := "AND ts >= TIMESTAMPTZ '" + inside.Format("2006-01-02 15:04:05-07") + "'"; strings.Count(conn.only(t).sql, want) != 2 {
		t.Errorf("a bound inside the window must be used as given (%s):\n%s", want, conn.only(t).sql)
	}
}

// TestRawTradeReadsSpanBothStoredDirections: these reads must span the same directions as /v1/history's page
// read, so surfaces agree on whether a market exists. The scripted driver replays rows regardless, so this
// requires both arms in each issued statement.
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
				_, err := s.LatestTradesForPair(context.Background(), pair, 1, time.Now().Add(-time.Hour))
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

// TestLatestTradePerSource_SourceFilterAppliesToBothDirections: the filter is applied in SQL on each arm;
// a one-arm filter would serve a single-source request the whole market from the other direction.
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

// latestUnboundedPair is the market under test. Nothing about it is
// load-bearing beyond being a legal non-XLM pair, so no alias family
// widens the read.
func latestUnboundedPair(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.ParseAsset(dirAQUA)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", dirAQUA, err)
	}
	quote, err := canonical.ParseAsset(dirUSDC)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", dirUSDC, err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

// tsLowerBound matches a lower bound on `ts` in any recency-window spelling (`ts >= $4`, `ts > now() - ...`,
// `ts BETWEEN ...`). Upper bounds (`ts <= $3`) are deliberately unmatched: they cannot hide an old row. No
// trailing \b after the operator, as `>=\b` never matches.
var tsLowerBound = regexp.MustCompile(`(?i)\bts\s*(?:>=|>|between\b)`)

// TestLatestTradeReadsTakeNoRecencyBound: latest-trade reads must carry no window, either as a bound
// time.Time or as a now() in the SQL; both channels are refused.
func TestLatestTradeReadsTakeNoRecencyBound(t *testing.T) {
	t.Parallel()

	pair := latestUnboundedPair(t)

	reads := []struct {
		name    string
		surface string
		loss    string
		run     func(*Store) error
	}{
		{
			name:    "LatestTradePerSource",
			surface: "/v1/observations and its SSE stream",
			loss: "a market whose last trade predates the window reports NO " +
				"observations at all, where it reported one row per source",
			run: func(s *Store) error {
				_, err := s.LatestTradePerSource(context.Background(), pair, "")
				return err
			},
		},
		{
			name:    "LatestTradesForPair",
			surface: "/v1/price's last-trade arm",
			loss: "a market whose last trade predates the window stops having " +
				"a price at all, rather than having a stale one",
			run: func(s *Store) error {
				_, err := s.LatestTradesForPair(context.Background(), pair, 1, time.Time{})
				return err
			},
		},
	}

	// LatestTradesForPair first tries a 7-day window (recentTradesWindow) and, finding nothing, falls back to the unbounded
	// walk: that final probe is the statement checked, and the empty script below forces it.
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()

			store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols}, scriptedResult{cols: latestTradeCols})
			defer func() { _ = store.db.Close() }()
			if err := r.run(store); err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			stmt := conn.stmts[len(conn.stmts)-1]

			const why = `

"Latest" bounded by a window is "latest within the window": a market
whose last trade predates it reports nothing where it reported a real
trade, and the loss lands on the QUIET networks nobody tests against —
futurenet had zero XLM trades in a 14-day window on 2026-09-09 while
testnet had 2,030 in the same one. Unlike Store.HasAsset, which escapes
its window by answering XLM from first principles, there is no
first-principles answer to "what traded last".

The walk is affordable without one: each arm binds base_asset AND
quote_asset, the leading two compress_segmentby columns (migration
0001), so every chunk is an index seek — 47 ms on r1 2026-08-03 to walk
the whole hypertable and prove a novel pair empty. It is not the
Store.HasAsset shape, which bound quote_asset with no leading equality
and had to scan every compressed chunk.

If you are landing something that genuinely keeps the answer — an
expanding window with an unbounded final probe, say — change this test
and say why here. Do not delete it.`

			if loc := tsLowerBound.FindString(stmt.sql); loc != "" {
				t.Errorf("%s now bounds `ts` from below (%q), which silently empties %s.\n\n%s\n\nSQL:\n%s",
					r.name, loc, r.surface, r.loss+why, indent(stmt.sql))
			}
			for i, a := range stmt.args {
				if ts, ok := a.(time.Time); ok {
					t.Errorf("%s binds a timestamp at $%d (%s) — a recency floor by the "+
						"Go-side idiom the twin fix uses.\n\n%s\n\nSQL:\n%s",
						r.name, i+1, ts, r.loss+why, indent(stmt.sql))
				}
			}
			if strings.Contains(strings.ToLower(stmt.sql), "now()") {
				t.Errorf("%s calls now() in SQL — the other spelling of a recency floor.\n\n%s\n\nSQL:\n%s",
					r.name, r.loss+why, indent(stmt.sql))
			}
		})
	}
}

// TestLatestTradePerSource_AnswersAMarketQuietForYears is the behavioural companion to the guard above. The
// scripted driver replays its row whatever the SQL window, so this pins only that a years-old trade is
// returned at its real timestamp.
func TestLatestTradePerSource_AnswersAMarketQuietForYears(t *testing.T) {
	t.Parallel()

	pair := latestUnboundedPair(t)
	// Older than MarketsRecencyWindow by three orders of magnitude, and
	// older than any window a future fixer would plausibly pick.
	last := time.Now().UTC().Add(-5 * 365 * 24 * time.Hour).Truncate(time.Second)

	store, _ := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{{
			"sdex", int64(1), dirTxA, int64(0), last,
			pair.Base.String(), pair.Quote.String(),
			dirAQUAAmount, dirUSDCAmount,
			"", "", "",
		}},
	})
	defer func() { _ = store.db.Close() }()

	got, err := store.LatestTradePerSource(context.Background(), pair, "")
	if err != nil {
		t.Fatalf("LatestTradePerSource: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trade(s), want 1 — a market's last trade is its last "+
			"trade however long ago it was; /v1/observations has no staleness floor", len(got))
	}
	if !got[0].Timestamp.Equal(last) {
		t.Errorf("timestamp = %s, want %s — the row is served as observed, not re-stamped",
			got[0].Timestamp, last)
	}
	if got[0].BaseAmount.String() != dirAQUAAmount || got[0].QuoteAmount.String() != dirUSDCAmount {
		t.Errorf("amounts = (%s, %s), want (%s, %s) — the pair is stored in the requested "+
			"orientation, so no leg swap applies",
			got[0].BaseAmount, got[0].QuoteAmount, dirAQUAAmount, dirUSDCAmount)
	}
}

// Store.TradesInRange backs the aggregates; a wrongly folded mean yields a plausible number, not empty.
// Each flipped row has its legs swapped, which inverts the price and re-weights by the stored QUOTE amount.
// A fold that skips the re-weighting reports a wrong high, which the fixture exposes.

// Market: AQUA priced in USDC, four prints of 100 AQUA, two stored each way round (0.20, 0.18, 0.25, 0.10).
// Read whole: VWAP 0.1825, high 0.25, low 0.10, n = 4.
const (
	aggAQUA100 = "1000000000" // 100 AQUA, 7dp
	aggUSDC20  = "200000000"  // 20 USDC
	aggUSDC18  = "180000000"  // 18 USDC
	aggUSDC25  = "250000000"  // 25 USDC
	aggUSDC10  = "100000000"  // 10 USDC
)

// aggWindow is the [from, to) the fixture rows fall inside.
var aggWindow = struct{ from, to time.Time }{
	from: time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC),
	to:   time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC),
}

// aggScriptedRows renders the four fixture prints in the order the
// two-armed query returns them: newest first, since the read orders
// DESC and reverses to ascending in Go.
func aggScriptedRows() [][]driver.Value {
	base := aggWindow.from
	return [][]driver.Value{
		// t+30m — stored the other way round, the market's LOW.
		storedTradeRow("sdex", 58_000_004, dirTxB, 0, base.Add(30*time.Minute),
			dirUSDC, dirAQUA, aggUSDC10, aggAQUA100),
		// t+20m — stored the other way round, the market's HIGH.
		storedTradeRow("sdex", 58_000_003, dirTxA, 0, base.Add(20*time.Minute),
			dirUSDC, dirAQUA, aggUSDC25, aggAQUA100),
		// t+10m — stored as asked.
		storedTradeRow("sdex", 58_000_002, dirTxB, 0, base.Add(10*time.Minute),
			dirAQUA, dirUSDC, aggAQUA100, aggUSDC18),
		// t+00m — stored as asked.
		storedTradeRow("sdex", 58_000_001, dirTxA, 0, base,
			dirAQUA, dirUSDC, aggAQUA100, aggUSDC20),
	}
}

// aggSumLegs returns (Σ QuoteAmount, Σ BaseAmount) over the slice —
// aggregate.VWAP's numerator and denominator, and TotalQuoteVolume /
// TotalBaseVolume.
func aggSumLegs(trades []canonical.Trade) (sumQuote, sumBase *big.Int) {
	sumQuote, sumBase = new(big.Int), new(big.Int)
	for i := range trades {
		sumQuote.Add(sumQuote, trades[i].QuoteAmount.BigInt())
		sumBase.Add(sumBase, trades[i].BaseAmount.BigInt())
	}
	return sumQuote, sumBase
}

// aggExtremes returns (high, low) over the slice, each row's price
// taken as QuoteAmount/BaseAmount exactly — aggregate.ComputeOHLC's
// own per-row price and its max/min.
func aggExtremes(t *testing.T, trades []canonical.Trade) (high, low *big.Rat) {
	t.Helper()
	if len(trades) == 0 {
		t.Fatal("no trades to take extremes over")
	}
	for i := range trades {
		p := new(big.Rat).SetFrac(trades[i].QuoteAmount.BigInt(), trades[i].BaseAmount.BigInt())
		if high == nil || p.Cmp(high) > 0 {
			high = p
		}
		if low == nil || p.Cmp(low) < 0 {
			low = p
		}
	}
	return high, low
}

func aggRat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("cannot parse %q as a rational", s)
	}
	return r
}

// TestTradesInRange_AggregateOverBothStoredDirections checks the VWAP, OHLC and TWAP shapes together. An
// unfolded read drops half the prints, and a merely relabelled fold reads the stored price in the wrong
// orientation; both fail silently.
func TestTradesInRange_AggregateOverBothStoredDirections(t *testing.T) {
	pair := dirAQUAUSDCPair(t)

	store, _ := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: aggScriptedRows(),
	})

	got, err := store.TradesInRange(context.Background(), pair, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("returned %d trades, want 4 — the window holds two prints in "+
			"each stored direction and an aggregate is computed over all of them", len(got))
	}

	// (1) Every row arrives in the requested orientation. A relabelled
	// row would fail here before any aggregate is taken, which is the
	// point: the aggregates below are only meaningful once this holds.
	for i := range got {
		if got[i].Pair.Base.String() != dirAQUA || got[i].Pair.Quote.String() != dirUSDC {
			t.Errorf("row %d returned %s/%s, want the REQUESTED orientation %s/%s",
				i, got[i].Pair.Base, got[i].Pair.Quote, dirAQUA, dirUSDC)
		}
		if got[i].BaseAmount.String() != aggAQUA100 {
			t.Errorf("row %d base_amount = %s, want %s — every fixture print is "+
				"100 AQUA in the requested orientation, so a flipped row whose "+
				"legs were not swapped shows up here",
				i, got[i].BaseAmount, aggAQUA100)
		}
	}

	// (2) The volume-weighted mean, Σquote/Σbase = 73/400 = 0.1825 exactly. A flipped row's
	// weight in the requested base is its STORED QUOTE leg, so base volume is 400 AQUA, not
	// the 235 that summing stored base legs gives.
	sumQuote, sumBase := aggSumLegs(got)
	if want := aggRat(t, "4000000000"); new(big.Rat).SetInt(sumBase).Cmp(want) != 0 {
		t.Errorf("Σbase = %s, want %s (4 x 100 AQUA).\n"+
			"The weight in a volume-weighted mean is the row's base leg IN THE "+
			"REQUESTED ORIENTATION. A flipped row contributes its stored QUOTE "+
			"amount here; summing stored base legs instead gives 2350000000 and "+
			"weights the two flipped prints by their USDC size.",
			sumBase, want.RatString())
	}
	vwap := new(big.Rat).SetFrac(sumQuote, sumBase)
	if want := aggRat(t, "0.1825"); vwap.Cmp(want) != 0 {
		t.Errorf("VWAP = %s, want exactly %s.\n"+
			"0.19 is the answer over the two same-direction prints alone; "+
			"anything near 1 is the two flipped prints summed with their legs "+
			"where the database left them.",
			vwap.FloatString(10), want.FloatString(10))
	}

	// (3) The extremes. Both high and low of this window are set by a print stored the other way round, the
	// shape that makes a naive fold worse than none: max()/min() over un-re-expressed rows return a wrong number
	// rather than nothing.
	high, low := aggExtremes(t, got)
	if want := aggRat(t, "0.25"); high.Cmp(want) != 0 {
		t.Errorf("high = %s, want exactly %s.\n"+
			"0.2 is the high of the same-direction prints alone; 10 is the "+
			"flipped print's STORED price (AQUA per USDC) compared as though it "+
			"were USDC per AQUA. An inverted price turns a maximum into a "+
			"minimum, so the re-expression has to happen BEFORE the comparison, "+
			"never after.",
			high.FloatString(10), want.FloatString(10))
	}
	if want := aggRat(t, "0.10"); low.Cmp(want) != 0 {
		t.Errorf("low = %s, want exactly %s (0.18 is the same-direction low)",
			low.FloatString(10), want.FloatString(10))
	}

	// (4) The extremes bracket the mean, and the mean is strictly
	// inside them. An orientation-mixed population routinely violates
	// this, which is the cheapest possible smoke test for the class.
	if !(low.Cmp(vwap) < 0 && vwap.Cmp(high) < 0) {
		t.Errorf("low %s / VWAP %s / high %s are not ordered — a population "+
			"holding two orientations at once cannot satisfy this",
			low.FloatString(10), vwap.FloatString(10), high.FloatString(10))
	}

	// (5) Chronological order survives the union: aggregate.TWAP and aggregate.ComputeOHLC take time weights and
	// open/close from slice position and do not sort, so an unordered merge silently changes both.
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp.Before(got[i-1].Timestamp) {
			t.Fatalf("row %d (%s) precedes row %d (%s) — TradesInRange promises "+
				"ascending close time, and TWAP's Δt weights come straight from it",
				i, got[i].Timestamp, i-1, got[i-1].Timestamp)
		}
	}
}

// TestTradesInRange_FoldedMeanIsReciprocalConsistent: after the leg swap, the two orientations' means are
// exact reciprocals. A fold that inverts prices but keeps the old weights computes a weighted mean of
// reciprocals and fails on any window with more than one price.
func TestTradesInRange_FoldedMeanIsReciprocalConsistent(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	flipped := pair.Flip()

	forward, _ := newScriptedStore(t, scriptedResult{cols: latestTradeCols, rows: aggScriptedRows()})
	reverse, _ := newScriptedStore(t, scriptedResult{cols: latestTradeCols, rows: aggScriptedRows()})

	fwd, err := forward.TradesInRange(context.Background(), pair, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange(AQUA/USDC): %v", err)
	}
	rev, err := reverse.TradesInRange(context.Background(), flipped, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange(USDC/AQUA): %v", err)
	}
	if len(fwd) != len(rev) {
		t.Fatalf("the same market returned %d rows one way round and %d the other",
			len(fwd), len(rev))
	}

	fq, fb := aggSumLegs(fwd)
	rq, rb := aggSumLegs(rev)
	fwdVWAP := new(big.Rat).SetFrac(fq, fb)
	revVWAP := new(big.Rat).SetFrac(rq, rb)

	product := new(big.Rat).Mul(fwdVWAP, revVWAP)
	if product.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("VWAP(AQUA/USDC) x VWAP(USDC/AQUA) = %s, want exactly 1.\n"+
			"%s and %s are the two answers. They are reciprocals only if the "+
			"fold swapped each flipped row's WEIGHT along with its price; "+
			"inverting the price alone gives a weighted mean of reciprocals.",
			product.RatString(), fwdVWAP.FloatString(10), revVWAP.FloatString(10))
	}

	// The extremes swap roles under inversion: the high one way round is the reciprocal of the
	// low the other. Store.OHLCSeries' `norm` CTE writes this out (hi = 1/low_price) on the
	// bucket side; a per-ROW fold gets it free because a row carries one price.
	fwdHigh, fwdLow := aggExtremes(t, fwd)
	revHigh, revLow := aggExtremes(t, rev)
	if got := new(big.Rat).Mul(fwdHigh, revLow); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("high(AQUA/USDC) x low(USDC/AQUA) = %s, want exactly 1 — "+
			"an inverted price turns a maximum into a minimum", got.RatString())
	}
	if got := new(big.Rat).Mul(fwdLow, revHigh); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Errorf("low(AQUA/USDC) x high(USDC/AQUA) = %s, want exactly 1", got.RatString())
	}
}

// TestTradesInRange_LeavesTheRequestedOrientationUntouched is the other half of a destructive transform: a
// window with nothing flipped must come back byte-for-byte unchanged, or the fold regresses every one-way
// market.
func TestTradesInRange_LeavesTheRequestedOrientationUntouched(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	ts := aggWindow.from

	store, _ := newScriptedStore(t, scriptedResult{
		cols: latestTradeCols,
		rows: [][]driver.Value{requestedStoredRow("sdex", 58_000_001, dirTxA, ts)},
	})

	got, err := store.TradesInRange(context.Background(), pair, aggWindow.from, aggWindow.to, 1000)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d trades, want 1", len(got))
	}
	wantAQUAUSDCTrade(t, got[0], "TradesInRange")
	if got[0].TxHash != dirTxA || got[0].Ledger != 58_000_001 || got[0].Source != "sdex" {
		t.Errorf("identity changed: source %s ledger %d tx_hash %s",
			got[0].Source, got[0].Ledger, got[0].TxHash)
	}
}

// TestTradesInRange_BindsTheWindowAndLimitOnEveryArm: both arms must carry the whole
// predicate. A missing window bound would aggregate flipped rows from outside [from, to); a
// missing limit would uncap half the scan the ceiling bounds.
func TestTradesInRange_BindsTheWindowAndLimitOnEveryArm(t *testing.T) {
	pair := dirAQUAUSDCPair(t)

	store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
	if _, err := store.TradesInRange(context.Background(), pair,
		aggWindow.from, aggWindow.to, 250); err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	stmt := conn.only(t)

	if got := stmt.arg(t, 1); got != dirAQUA {
		t.Errorf("bound $1 = %v, want %s", got, dirAQUA)
	}
	if got := stmt.arg(t, 2); got != dirUSDC {
		t.Errorf("bound $2 = %v, want %s", got, dirUSDC)
	}
	if got := stmt.arg(t, 5); got != 250 {
		t.Errorf("bound $5 = %v, want the caller's limit 250", got)
	}

	for _, want := range []struct {
		what string
		pat  string
		n    int
	}{
		{"the window lower bound `ts >= $3`", `ts\s*>=\s*\$3`, 2},
		{"the window upper bound `ts < $4`", `ts\s*<\s*\$4`, 2},
		// One cut per arm plus the outer cut: without the outer one a
		// populated pair returns up to 2x the limit the ceiling promises.
		{"`LIMIT $5`", `LIMIT\s+\$5`, 3},
	} {
		if n := countSQLMatches(stmt.sql, want.pat); n != want.n {
			t.Errorf("%s appears %d time(s), want %d — every arm carries the "+
				"whole predicate, or the two are not the read they replace:\n%s",
				want.what, n, want.n, stmt.sql)
		}
	}
}

// countSQLMatches counts non-overlapping matches of `pat` in a
// statement. Query-shape assertions in this package are about how MANY
// arms carry a predicate, not merely whether one does.
func countSQLMatches(sql, pat string) int {
	return len(regexp.MustCompile(pat).FindAllString(sql, -1))
}

// TestFXQuoteAtOrBefore_ServesAQuoteRecordedTheOtherWayRound: the legacy `trades` arm of the
// FX snap serves a price on the triangulation money path, so the class guard applies. A
// EUR/USD rate recorded USD/EUR is the exact reciprocal once the legs are swapped.
func TestFXQuoteAtOrBefore_ServesAQuoteRecordedTheOtherWayRound(t *testing.T) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatalf("NewFiatAsset EUR: %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset USD: %v", err)
	}
	pair, err := canonical.NewPair(eur, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	ts := aggWindow.from

	// Stored USD/EUR: 5 USD bought 4 EUR, i.e. 0.8 EUR per USD. The
	// EUR/USD rate the caller asked for is 5/4 = 1.25 exactly.
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"source", "ts", "base_asset", "base_amount", "quote_amount"},
		rows: [][]driver.Value{{"exchangeratesapi", ts, "fiat:USD", "5000000", "4000000"}},
	})

	price, observedAt, source, err := store.FXQuoteAtOrBefore(
		context.Background(), pair, ts.Add(time.Hour), []string{"exchangeratesapi"})
	if err != nil {
		t.Fatalf("FXQuoteAtOrBefore: %v", err)
	}
	if want := big.NewRat(5, 4); price.Cmp(want) != 0 {
		t.Errorf("rate = %s, want exactly %s — a quote recorded USD/EUR answers "+
			"EUR/USD by swapping its legs, which is the reciprocal at full "+
			"precision. %s is the stored rate served without re-expression.",
			price.RatString(), want.RatString(), big.NewRat(4, 5).RatString())
	}
	if !observedAt.Equal(ts) || source != "exchangeratesapi" {
		t.Errorf("observed_at %s / source %q, want %s / exchangeratesapi",
			observedAt, source, ts)
	}
	if n := countSQLMatches(conn.only(t).sql, `source\s*=\s*ANY\(\$4\)`); n != 2 {
		t.Errorf("the FX source filter appears %d time(s), want 2 — once per "+
			"stored direction, or one arm serves any source at all:\n%s",
			n, conn.only(t).sql)
	}
}

// TestReDeriveNullVolumeGuard_ACRIT1 locks in the fail-closed guard: a re-derive with a positive generation
// but no InstallUSDVolumeResolution must refuse, or it overwrites stored usd_volume with NULL unrecoverably.
// The guard is precise: it must not fire when a resolver is wired or Tier-1 CEX-USD trades need none.
func TestReDeriveNullVolumeGuard_ACRIT1(t *testing.T) {
	priced := "12.34000000"
	tr := c.Trade{Source: "binance", Ledger: 7}

	cases := []struct {
		name      string
		gen       int64
		installed bool
		computed  *string
		wantErr   bool
	}{
		// Live ingest is never guarded, however the volume resolved.
		{"live ingest, null volume", 0, false, nil, false},
		{"live ingest, priced", 0, false, &priced, false},

		// THE destructive combination — re-derive, no resolution wired, and this
		// write would land a NULL over a possibly-correct stored value.
		{"re-derive, not installed, null volume", 1, false, nil, true},

		// Precision cases: neither of these can corrupt, so neither may be refused.
		// A priced trade (tier 1 CEX-USD needs no resolver) writes a real value.
		{"re-derive, not installed, but priced", 1, false, &priced, false},
		// Properly-wired re-derive: a genuinely unpriceable trade writes an
		// honest NULL (e.g. a no-pegs deployment, where Install is a no-op).
		{"re-derive, installed, null volume", 1, true, nil, false},
		{"re-derive, installed, priced", 1, true, &priced, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Store{}
			s.SetDeriveGeneration(tc.gen)
			s.usdVolumeResolutionInstalled = tc.installed
			err := s.reDeriveNullVolumeGuard(tr, tc.computed)
			if tc.wantErr && err == nil {
				t.Fatal("want the write refused (A-CRIT-1 destructive combination), got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want the write allowed, got refusal: %v", err)
			}
		})
	}
}

// The raw trade window behind /v1/vwap, /v1/twap and the single-bar /v1/ohlc binds the non-VWAP set as $6 in both
// direction arms; otherwise a vendor-only month answers a window VWAP with carrier amounts as volume.
func TestTradesInRange_BindsNonVWAPSources(t *testing.T) {
	pair := testXLMUSDCPair(t)
	to := time.Now().UTC().Truncate(time.Second)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"source", "ledger", "tx_hash", "op_index", "ts", "base_asset", "quote_asset", "base_amount", "quote_amount", "maker", "taker", "routed_via"},
	})
	defer func() { _ = store.db.Close() }()

	if _, err := store.TradesInRange(context.Background(), pair, to.Add(-time.Hour), to, 100); err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(conn.stmts) != 1 {
		t.Fatalf("issued %d statements, want 1", len(conn.stmts))
	}
	stmt := conn.stmts[0]
	const pred = "NOT (source = ANY(string_to_array($6, ',')))"
	if n := strings.Count(stmt.sql, pred); n != 2 {
		t.Errorf("predicate appears %d times, want 2 (both direction arms)\nSQL: %s", n, stmt.sql)
	}
	if got := stmt.arg(t, 6); got != nonVWAPSourcesArg() {
		t.Errorf("$6 = %v, want the non-VWAP source set", got)
	}
}

// TestLatestTradesForPair_ArmsOrderByTheOuterKey: a FETCH FIRST ... WITH TIES arm under
// ChunkAppend trips the planner's bounded sort on a busy pair (served as a /v1/price 500), so
// each arm orders by the outer key and takes a plain LIMIT instead.
func TestLatestTradesForPair_ArmsOrderByTheOuterKey(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
	if _, err := store.LatestTradesForPair(context.Background(), dirAQUAUSDCPair(t), 1, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	sql := conn.only(t).sql
	if strings.Contains(sql, "WITH TIES") {
		t.Fatal("an arm still uses FETCH FIRST ... WITH TIES")
	}
	const key = "ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, source DESC"
	if n := strings.Count(sql, key); n != 3 {
		t.Fatalf("full-key ORDER BY appears %d times, want 3 (both arms and the outer sort)", n)
	}
}

// TestLatestTradesForPair_RemembersAnEmptyOlderRange: a /v1/price/batch burst repeats the same never-traded pairs, and each repeat
// of the wide pass plans every trades chunk in range; the recent pass still runs, so a live trade is never hidden.
func TestLatestTradesForPair_RemembersAnEmptyOlderRange(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	since := time.Now().Add(-400 * 24 * time.Hour)
	empty := scriptedResult{cols: latestTradeCols}
	store, conn := newScriptedStore(t, empty, empty, empty)
	for range 2 {
		if got, err := store.LatestTradesForPair(context.Background(), pair, 1, since); err != nil || len(got) != 0 {
			t.Fatalf("LatestTradesForPair = %d rows, %v; want 0, nil", len(got), err)
		}
	}
	if len(conn.stmts) != 3 {
		t.Fatalf("issued %d statements, want recent + wide, then recent alone", len(conn.stmts))
	}
}

// TestLatestTradesForPair_RemembersANonEmptyOlderRange: a pair whose newest trade is older than the recent window paid the wide pass
// on every call too. The recent pass still runs, so a trade landing after the first read ranks ahead of the remembered rows.
func TestLatestTradesForPair_RemembersANonEmptyOlderRange(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	since := time.Now().Add(-400 * 24 * time.Hour)
	oldTS := time.Now().Add(-30 * 24 * time.Hour)
	freshTS := time.Now().Add(-time.Hour)
	empty := scriptedResult{cols: latestTradeCols}
	older := scriptedResult{cols: latestTradeCols, rows: [][]driver.Value{reverseStoredRow("sdex", 58_000_001, dirTxA, oldTS)}}
	fresh := scriptedResult{cols: latestTradeCols, rows: [][]driver.Value{reverseStoredRow("sdex", 58_900_001, dirTxB, freshTS)}}
	store, conn := newScriptedStore(t, empty, older, fresh, fresh)

	if got, err := store.LatestTradesForPair(context.Background(), pair, 2, since); err != nil || len(got) != 1 || !got[0].Timestamp.Equal(oldTS) {
		t.Fatalf("first call = %v, %v; want the 30-day-old trade", got, err)
	}
	got, err := store.LatestTradesForPair(context.Background(), pair, 2, since)
	if err != nil || len(got) != 2 || !got[0].Timestamp.Equal(freshTS) || !got[1].Timestamp.Equal(oldTS) {
		t.Fatalf("second call = %v, %v; want the fresh trade then the remembered one", got, err)
	}
	got, err = store.LatestTradesForPair(context.Background(), pair, 1, since)
	if err != nil || len(got) != 1 || !got[0].Timestamp.Equal(freshTS) {
		t.Fatalf("limit-1 call = %v, %v; want the fresh trade alone", got, err)
	}
	if len(conn.stmts) != 4 {
		t.Fatalf("issued %d statements, want recent + wide, then recent alone twice", len(conn.stmts))
	}
}
