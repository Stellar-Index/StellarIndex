//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLatestTradeReadsBreakSameLedgerTiesOnTheFullKey: several of one
// source's trades share (ts, ledger) whenever a ledger fills more than
// one offer, so both latest-trade readers must break that tie on
// (tx_hash, op_index) — the key /v1/history orders on — and agree with
// the newest row TradesInRange serves. The winner is inserted last in
// each stored direction, so a read that stops at (ts, ledger) meets a
// loser first.
func TestLatestTradeReadsBreakSameLedgerTiesOnTheFullKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	// Prepared statements are per-session: the plan check below reads
	// the one the reader prepared.
	db.SetMaxOpenConns(1)

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aqua, err := c.NewClassicAsset("AQUA", issuer)
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := c.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(aqua, usdc)
	if err != nil {
		t.Fatal(err)
	}
	a, b := aqua.String(), usdc.String()
	ts := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	txA := strings.Repeat("a", 64)
	txC := strings.Repeat("c", 64)

	// Older history from another source, so the planner has a market
	// worth skip-scanning.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
		                    base_asset, quote_asset, base_amount, quote_amount)
		SELECT 'bulk', 60000000 + h, lpad(to_hex(h), 64, '0'), 0,
		       $1::timestamptz - make_interval(mins => h), $2, $3, 1, 2
		  FROM generate_series(1, 60000) h`, ts, a, b); err != nil {
		t.Fatalf("seed bulk: %v", err)
	}
	for _, r := range []struct {
		base, quote, tx string
		op              int
	}{
		{a, b, txA, 0},
		{b, a, txA, 1},
		{a, b, txC, 0},
		{b, a, txC, 2}, // the winner: highest tx_hash, then op_index
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
			                    base_asset, quote_asset, base_amount, quote_amount)
			VALUES ('sdex', 61000000, $1, $2, $3, $4, $5, 5, 7)`,
			r.tx, r.op, ts, r.base, r.quote); err != nil {
			t.Fatalf("seed tie: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `ANALYZE trades`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	isWinner := func(tr c.Trade) bool {
		return tr.Source == "sdex" && tr.TxHash == txC && tr.OpIndex == 2
	}

	hist, err := store.TradesInRange(ctx, pair, ts.Add(-time.Hour), ts.Add(time.Hour), 1000)
	if err != nil || len(hist) == 0 {
		t.Fatalf("TradesInRange: %d rows, %v", len(hist), err)
	}
	if newest := hist[len(hist)-1]; !isWinner(newest) {
		t.Fatalf("instrument check: /v1/history's newest row is %s/%d, want %s/2", newest.TxHash, newest.OpIndex, txC)
	}

	last, err := store.LatestTradesForPair(ctx, pair, 1)
	if err != nil || len(last) != 1 {
		t.Fatalf("LatestTradesForPair: %d rows, %v", len(last), err)
	}
	if !isWinner(last[0]) {
		t.Errorf("LatestTradesForPair = %s/%d, want %s/2 — the newest row /v1/history serves",
			last[0].TxHash, last[0].OpIndex, txC)
	}

	perSource, err := store.LatestTradePerSource(ctx, pair, "")
	if err != nil {
		t.Fatalf("LatestTradePerSource: %v", err)
	}
	var sdex *c.Trade
	for i := range perSource {
		if perSource[i].Source == "sdex" {
			sdex = &perSource[i]
		}
	}
	if len(perSource) != 2 || sdex == nil {
		t.Fatalf("LatestTradePerSource returned %d rows (sdex present: %v), want bulk + sdex", len(perSource), sdex != nil)
	}
	if !isWinner(*sdex) {
		t.Errorf("LatestTradePerSource[sdex] = %s/%d, want %s/2 — the newest row /v1/history serves",
			sdex.TxHash, sdex.OpIndex, txC)
	}

	// The tiebreak must not cost the per-source read its skip scan: the
	// index ends at ledger, so ordering the DISTINCT ON by more keys
	// sorts the whole market instead.
	stmt := capturePreparedStatement(t, ctx, db, nil, "DISTINCT ON (source)", "FROM trades")
	mustExecPlan(t, ctx, db, `SET plan_cache_mode = force_custom_plan`)
	mustExecPlan(t, ctx, db, `PREPARE tiebreak_plan_probe AS `+stmt)
	rows, err := db.QueryContext(ctx, `EXPLAIN EXECUTE tiebreak_plan_probe('`+a+`', '`+b+`', '', 'binance,bitstamp,coinbase,kraken')`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(plan.String(), "SkipScan") {
		t.Errorf("LatestTradePerSource lost its skip scan — its cost is now O(rows in market):\n%s", plan.String())
	}
}
