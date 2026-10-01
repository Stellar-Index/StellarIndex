// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

const subBatchUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// subBatchTrades returns n storable native/USDC trades on source, one per
// ledger starting at 1, so conflict-key order is ledger order and rows
// [0, tradeInsertMaxRows) form the first sub-batch.
func subBatchTrades(t *testing.T, source string, n int) []canonical.Trade {
	t.Helper()
	pair := osztPair(t)
	ts := time.Date(2026, 2, 15, 22, 0, 0, 0, time.UTC)
	out := make([]canonical.Trade, n)
	for i := range out {
		out[i] = canonical.Trade{
			Source: source, Ledger: uint32(i + 1), TxHash: fmt.Sprintf("%064x", i+1), //nolint:gosec // G115: small test index
			Timestamp: ts, Pair: pair, BaseAmount: osztAmt(100), QuoteAmount: osztAmt(25),
		}
	}
	return out
}

// landedRows is a sub-batch's RETURNING set: one landed native/USDC row per
// ledger on source.
func landedRows(source string, ledgers ...int64) scriptedResult {
	ts := time.Date(2026, 2, 15, 22, 0, 0, 0, time.UTC)
	res := scriptedResult{cols: []string{"source", "ledger", "ts", "base_asset", "quote_asset", "unit_ratio"}}
	for _, l := range ledgers {
		res.rows = append(res.rows, []driver.Value{source, l, ts, "native", subBatchUSDC, false})
	}
	return res
}

// registeredUSDCLedger returns the ledger the classic_assets upsert bound
// for USDC, failing when the registry hook never wrote it.
func registeredUSDCLedger(t *testing.T, conn *scriptedConn) int64 {
	t.Helper()
	for _, st := range conn.stmts {
		if strings.Contains(st.sql, "INSERT INTO classic_assets") {
			l, ok := st.arg(t, 5).(int)
			if !ok {
				t.Fatalf("classic_assets $5 is %T, want int", st.arg(t, 5))
			}
			return int64(l)
		}
	}
	t.Fatalf("no classic_assets upsert issued; statements:\n%s", strings.Join(conn.statements(), "\n---\n"))
	return 0
}

// A failed later sub-batch must not erase the outcome of the sub-batches
// that already committed: a replay sees those rows as updates (xmax <> 0)
// and can never count or register them, so this call is the only chance.
func TestBatchInsertTrades_FailedSubBatchKeepsCommittedOutcome(t *testing.T) {
	t.Parallel()
	const source = "subbatch_partial"
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	store, conn := newScriptedStore(t,
		landedRows(source, 7, 4999),        // sub-batch 1: 2 landed rows
		scriptedResult{err: deadlock},      // sub-batch 2: the fault
		scriptedResult{}, scriptedResult{}, // registry: issuer, classic_assets
	)
	newBefore := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "new"))
	dupBefore := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "duplicate"))

	err := store.BatchInsertTrades(context.Background(), subBatchTrades(t, source, tradeInsertMaxRows+1000))

	var sbErr *TradeSubBatchError
	switch {
	case !errors.As(err, &sbErr):
		t.Errorf("err = %v, want *TradeSubBatchError", err)
	case sbErr.Start != tradeInsertMaxRows || sbErr.End != tradeInsertMaxRows+1000 || sbErr.Total != tradeInsertMaxRows+1000:
		t.Errorf("failed range = [%d,%d) of %d, want [%d,%d) of %d",
			sbErr.Start, sbErr.End, sbErr.Total, tradeInsertMaxRows, tradeInsertMaxRows+1000, tradeInsertMaxRows+1000)
	case sbErr.FirstLedger != tradeInsertMaxRows+1 || sbErr.LastLedger != tradeInsertMaxRows+1000:
		t.Errorf("failed ledgers = %d-%d, want %d-%d", sbErr.FirstLedger, sbErr.LastLedger, tradeInsertMaxRows+1, tradeInsertMaxRows+1000)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
		t.Errorf("cause not reachable via errors.As: %v", err)
	}
	if got := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "new")) - newBefore; got != 2 {
		t.Errorf("outcome=new delta = %v, want 2 (the committed sub-batch's landed rows)", got)
	}
	if got := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(source, "duplicate")) - dupBefore; got != tradeInsertMaxRows-2 {
		t.Errorf("outcome=duplicate delta = %v, want %d (committed sub-batch sent minus landed)", got, tradeInsertMaxRows-2)
	}
	if got := registeredUSDCLedger(t, conn); got != 4999 {
		t.Errorf("registered USDC at ledger %d, want 4999 from the committed sub-batch", got)
	}
	if conn.commits != 1 {
		t.Errorf("commits = %d, want 1 (only the first sub-batch)", conn.commits)
	}
}

// An asset landing in several sub-batches is registered at its HIGHEST
// ledger, the same rule the per-row fold applies within one sub-batch.
func TestBatchInsertTrades_RegistryKeepsHighestLedgerAcrossSubBatches(t *testing.T) {
	t.Parallel()
	const source = "subbatch_merge"
	store, conn := newScriptedStore(t,
		landedRows(source, 10),
		landedRows(source, 5500),
		scriptedResult{}, scriptedResult{},
	)
	if err := store.BatchInsertTrades(context.Background(), subBatchTrades(t, source, tradeInsertMaxRows+1000)); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}
	if got := registeredUSDCLedger(t, conn); got != 5500 {
		t.Errorf("registered USDC at ledger %d, want 5500 (highest across sub-batches)", got)
	}
}

// Callers replay a failed batch in their own order (the external retry ring
// trims its oldest rows by position), so the insert must not reorder it.
func TestBatchInsertTrades_LeavesCallerOrderOnFailure(t *testing.T) {
	t.Parallel()
	const source = "subbatch_caller_order"
	store, _ := newScriptedStore(t,
		scriptedResult{err: &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}},
	)
	trades := subBatchTrades(t, source, 5)
	slices.Reverse(trades)
	want := make([]uint32, len(trades))
	for i := range trades {
		want[i] = trades[i].Ledger
	}

	if err := store.BatchInsertTrades(context.Background(), trades); err == nil {
		t.Fatal("BatchInsertTrades: want the scripted deadlock, got nil")
	}
	for i := range trades {
		if trades[i].Ledger != want[i] {
			t.Fatalf("caller slice reordered: ledger[%d] = %d, want %d", i, trades[i].Ledger, want[i])
		}
	}
}
