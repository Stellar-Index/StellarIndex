package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// txHashIndexBackfillQuery backs BackfillTxHashIndex's per-window
// INSERT…SELECT. FINAL on the source read (audit DAT-10): stellar.transactions
// is ReplacingMergeTree(ingested_at), so a window that has already seen a
// re-ingest (retry, or a decode-fix re-derive) can hold un-merged duplicate
// PARTS for the same (ledger_seq, tx_index) key; without FINAL, this INSERT…
// SELECT would enqueue BOTH — including, on a genuine correction, the STALE
// pre-fix tx_hash alongside the corrected one — into stellar.tx_hash_index,
// which is itself keyed on tx_hash and just as exposed to the same
// ingested_at-tie ambiguity documented on txByLedgerAndHash. Cheap here: this
// is an operator-run backfill (not a per-request path) and FINAL is bounded
// by the SAME `ledger_seq >= ? AND ledger_seq <= ?` window predicate that
// already caps this function's per-iteration work — no new full-scan.
const txHashIndexBackfillQuery = `INSERT INTO stellar.tx_hash_index (tx_hash, ledger_seq, tx_index)
	SELECT tx_hash, ledger_seq, tx_index FROM stellar.transactions FINAL
	WHERE ledger_seq >= ? AND ledger_seq <= ?`

// BackfillTxHashIndex fills stellar.tx_hash_index (the hash-ordered
// GET /v1/tx/{hash} lookup table, perf-todo §4) from stellar.transactions in
// inclusive [from, to] ledger windows of `window` ledgers each — one
// server-side INSERT…SELECT per window. Windowing is what makes the 10.2B-row
// full-history fill operable on r1: each window is bounded work, progress is
// reported after every window with the exact resume point, and an interrupt /
// failure loses at most one window (re-running a window is idempotent — the
// table is ReplacingMergeTree keyed on tx_hash).
//
// The materialized view (tx_hash_index_mv) already covers everything ingested
// AFTER the schema deploy; this fills the history behind it.
//
// logf receives one line per completed window (progress + resume point).
func BackfillTxHashIndex(ctx context.Context, addr string, from, to, window uint32, logf func(format string, args ...any)) error {
	if from == 0 || to < from || window == 0 {
		return fmt.Errorf("clickhouse: tx-hash-index backfill: need 0 < from <= to and window > 0 (got from=%d to=%d window=%d)", from, to, window)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	start := time.Now()
	for lo := from; ; {
		hi := ledgerWindowHi(lo, to, window)
		wStart := time.Now()
		if err := conn.Exec(ctx, txHashIndexBackfillQuery, lo, hi); err != nil {
			return fmt.Errorf("clickhouse: tx-hash-index window [%d,%d]: %w — resume with -from %d", lo, hi, err, lo)
		}
		logf("window [%d,%d] done in %s (total %s; resume point -from %d)",
			lo, hi, time.Since(wStart).Round(time.Second), time.Since(start).Round(time.Second), hi+1)
		if hi >= to {
			return nil
		}
		lo = hi + 1
	}
}
