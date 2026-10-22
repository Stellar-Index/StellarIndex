package clickhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// OpsBySourceProofName is ops_by_source's row in stellar.entry_history_watermark: the ledger through which
// ch-index-proof showed the projection holds every sourced tx and op key of its two source tables.
const OpsBySourceProofName = "ops_by_source"

// op_index 4294967295 marks the tx-source stream (deploy/clickhouse/ops_by_source.sql). Per-ledger roaring bitmaps count distinct keys exactly and ignore ReplacingMergeTree duplicates, at a fraction of
// uniqExact's memory over a million-ledger window (uniqExact of the tx pairs overran 7.45 GiB on r1).
const (
	opsProofSettings = ` SETTINGS max_threads = 4, max_memory_usage = 8000000000, max_bytes_before_external_group_by = 4000000000`
	opsProofTxQuery  = `SELECT sum(c) FROM (SELECT groupBitmap(tx_index) AS c FROM stellar.transactions
		WHERE ledger_seq >= ? AND ledger_seq <= ? AND source_account != '' GROUP BY ledger_seq)` + opsProofSettings
	opsProofOpQuery = `SELECT sum(c) FROM (SELECT groupBitmap(toUInt64(tx_index) * 65536 + op_index) AS c FROM stellar.operations
		WHERE ledger_seq >= ? AND ledger_seq <= ? AND source_account != '' GROUP BY ledger_seq)` + opsProofSettings
	opsProofProjQuery = `SELECT sum(t), sum(o) FROM (SELECT
		groupBitmapIf(tx_index, op_index = 4294967295) AS t,
		groupBitmapIf(toUInt64(tx_index) * 65536 + op_index, op_index != 4294967295) AS o
		FROM stellar.ops_by_source WHERE ledger_seq >= ? AND ledger_seq <= ? GROUP BY ledger_seq)` + opsProofSettings
)

// OpsBySourceWindow is one window's key counts: source tables against the projection.
type OpsBySourceWindow struct {
	From, To     uint32
	Txs, ProjTxs uint64
	Ops, ProjOps uint64
}

// Complete reports whether the projection holds exactly the source tables' key counts.
func (w OpsBySourceWindow) Complete() bool { return w.Txs == w.ProjTxs && w.Ops == w.ProjOps }

// ProveOpsBySourceWindow counts [from, to]'s distinct sourced tx and op keys on both sides. The projection is fed only
// from those tables, so equal counts mean it holds every key.
func ProveOpsBySourceWindow(ctx context.Context, addr string, from, to uint32) (OpsBySourceWindow, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return OpsBySourceWindow{}, err
	}
	defer func() { _ = conn.Close() }()
	return proveOpsBySourceWindow(ctx, conn, from, to)
}

func proveOpsBySourceWindow(ctx context.Context, conn driver.Conn, from, to uint32) (OpsBySourceWindow, error) {
	w := OpsBySourceWindow{From: from, To: to}
	if err := conn.QueryRow(ctx, opsProofTxQuery, from, to).Scan(&w.Txs); err != nil {
		return w, fmt.Errorf("clickhouse: ops_by_source proof tx keys [%d,%d]: %w", from, to, err)
	}
	if err := conn.QueryRow(ctx, opsProofOpQuery, from, to).Scan(&w.Ops); err != nil {
		return w, fmt.Errorf("clickhouse: ops_by_source proof op keys [%d,%d]: %w", from, to, err)
	}
	if err := conn.QueryRow(ctx, opsProofProjQuery, from, to).Scan(&w.ProjTxs, &w.ProjOps); err != nil {
		return w, fmt.Errorf("clickhouse: ops_by_source proof projection keys [%d,%d]: %w", from, to, err)
	}
	return w, nil
}

// OpsBySourceMVsExist reports whether both feeding views exist, the condition for a proof through ledger N to hold
// for every ledger after it too.
func OpsBySourceMVsExist(ctx context.Context, addr string) (bool, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	var n uint64
	const q = `SELECT count() FROM system.tables WHERE database = 'stellar' AND name IN ('ops_by_source_ops_mv', 'ops_by_source_tx_mv')`
	if err := conn.QueryRow(ctx, q).Scan(&n); err != nil {
		return false, fmt.Errorf("clickhouse: ops_by_source views: %w", err)
	}
	return n == 2, nil
}

// SetOpsBySourceProofWatermark records a genesis-to-thru proof; a value at or below the stored one is a no-op.
func SetOpsBySourceProofWatermark(ctx context.Context, addr string, thru uint32) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stored, err := entryWatermarkOn(ctx, conn, OpsBySourceProofName)
	if err != nil {
		return err
	}
	if thru <= stored {
		return nil
	}
	const q = `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`
	if err := conn.Exec(ctx, q, OpsBySourceProofName, thru); err != nil {
		return fmt.Errorf("clickhouse: set %s proof watermark %d: %w", OpsBySourceProofName, thru, err)
	}
	return nil
}
