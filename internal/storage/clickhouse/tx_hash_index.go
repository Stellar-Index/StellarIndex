package clickhouse

import (
	"context"
	"fmt"
	"time"
)

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

	const q = `INSERT INTO stellar.tx_hash_index (tx_hash, ledger_seq, tx_index)
		SELECT tx_hash, ledger_seq, tx_index FROM stellar.transactions
		WHERE ledger_seq >= ? AND ledger_seq <= ?`

	start := time.Now()
	for lo := from; ; {
		hi := to
		if rem := to - lo; rem >= window { // window fits without overflow
			hi = lo + window - 1
		}
		wStart := time.Now()
		if err := conn.Exec(ctx, q, lo, hi); err != nil {
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
