package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// txHashIndexBackfillQuery backs BackfillTxHashIndex's per-window
// INSERT…SELECT. FINAL on the source read: stellar.transactions is
// ReplacingMergeTree(ingested_at), so a re-ingested window can hold un-merged
// duplicate PARTS, and without FINAL the INSERT would enqueue the STALE
// tx_hash beside the corrected one into stellar.tx_hash_index (same
// ingested_at-tie ambiguity as txByLedgerAndHash). FINAL is bounded by the
// `ledger_seq` window predicate. The ARRAY JOIN indexes a fee bump's inner
// hash beside its outer one, the backfill twin of tx_hash_index_inner_mv.
const txHashIndexBackfillQuery = `INSERT INTO stellar.tx_hash_index (tx_hash, ledger_seq, tx_index)
	SELECT h, ledger_seq, tx_index FROM stellar.transactions FINAL
	ARRAY JOIN arrayFilter(x -> x != '', [tx_hash, inner_tx_hash]) AS h
	WHERE ledger_seq >= ? AND ledger_seq <= ?`

// BackfillTxHashIndex fills stellar.tx_hash_index (the hash-ordered
// GET /v1/tx/{hash} lookup table) from stellar.transactions in inclusive
// [from, to] ledger windows of `window` ledgers each — one server-side
// INSERT…SELECT per window, so an interrupt loses at most one window and
// re-running one is idempotent (ReplacingMergeTree keyed on tx_hash). The
// materialized view (tx_hash_index_mv) covers everything ingested after the
// schema deploy; this fills the history behind it.
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

	return runWindowedBackfill(ctx, conn, from, to, window, "tx-hash-index",
		func(ctx context.Context, conn driver.Conn, lo, hi uint32) error {
			return conn.Exec(ctx, txHashIndexBackfillQuery, lo, hi)
		}, logf)
}

// MarkTxHashIndexCovered records that stellar.tx_hash_index holds every
// transaction in [from, to] so the reader may treat an index miss as
// authoritative. Call it only after a genesis→tip BackfillTxHashIndex run
// succeeded.
func MarkTxHashIndexCovered(ctx context.Context, addr string, from, to uint32) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec(ctx,
		`INSERT INTO stellar.tx_hash_index_coverage (covered_from, covered_to) VALUES (?, ?)`, from, to); err != nil {
		return fmt.Errorf("clickhouse: tx-hash-index coverage marker: %w", err)
	}
	return nil
}

// runWindowedBackfill walks [from, to] in windows of `window` ledgers,
// running exec once per window and reporting progress (+ the exact resume
// point) after each. Shared loop shell behind BackfillTxHashIndex and
// BackfillContractInstanceChanges — only the query (baked into exec) and the
// error-message label differ between them; keep the resume-point wording
// identical, operators paste `-from %d` back in on restart.
func runWindowedBackfill(ctx context.Context, conn driver.Conn, from, to, window uint32, label string, exec func(ctx context.Context, conn driver.Conn, lo, hi uint32) error, logf func(format string, args ...any)) error {
	start := time.Now()
	for lo := from; ; {
		hi := ledgerWindowHi(lo, to, window)
		wStart := time.Now()
		if err := exec(ctx, conn, lo, hi); err != nil {
			return fmt.Errorf("clickhouse: %s window [%d,%d]: %w — resume with -from %d", label, lo, hi, err, lo)
		}
		logf("window [%d,%d] done in %s (total %s; resume point -from %d)",
			lo, hi, time.Since(wStart).Round(time.Second), time.Since(start).Round(time.Second), hi+1)
		if hi >= to {
			return nil
		}
		lo = hi + 1
	}
}
