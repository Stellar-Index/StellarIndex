package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// entryBackfillChunk bounds each INSERT batch so a large backfill streams in
// fixed-memory chunks rather than one giant batch.
const entryBackfillChunk = 20_000

// InsertEntryChanges batch-inserts rows directly into
// stellar.ledger_entry_changes, bypassing the Sink's per-ledger commit-marker
// flow: it writes NO stellar.ledgers row, so it never advances the completeness
// watermark (which keys off a ledgers row meaning "this ledger is fully
// durable"). That makes it the correct path for backfilling historical /
// snapshot entries — e.g. the state-snapshot contract-code + instance backfill
// (DATA-TRUTH-PLAN G1) — into the append-log the WASM + account-state readers
// query (and which the ledger_entries_current MV folds into current state).
// Idempotent under the table's ReplacingMergeTree. Returns the number written.
func InsertEntryChanges(ctx context.Context, addr string, rows []LedgerEntryChangeRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
		// A finite ceiling — this is the cheap append path, not a heavy read.
		Settings:        clickhouse.Settings{"max_execution_time": 600},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return 0, fmt.Errorf("clickhouse: open %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Ping(ctx); err != nil {
		return 0, fmt.Errorf("clickhouse: ping %s: %w", addr, err)
	}

	written := 0
	for start := 0; start < len(rows); start += entryBackfillChunk {
		end := start + entryBackfillChunk
		if end > len(rows) {
			end = len(rows)
		}
		b, err := conn.PrepareBatch(ctx,
			"INSERT INTO stellar.ledger_entry_changes (ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, account_id, asset, balance)")
		if err != nil {
			return written, fmt.Errorf("clickhouse: prepare entry-changes batch: %w", err)
		}
		for _, r := range rows[start:end] {
			if err := b.Append(r.LedgerSeq, r.CloseTime, r.TxHash, r.OpIndex, r.ChangeIndex,
				r.ChangeType, r.EntryType, r.KeyXDR, r.EntryXDR, r.AccountID, r.Asset, r.Balance); err != nil {
				return written, fmt.Errorf("clickhouse: append entry-change: %w", err)
			}
		}
		if err := b.Send(); err != nil {
			return written, fmt.Errorf("clickhouse: send entry-changes batch: %w", err)
		}
		written += end - start
	}
	return written, nil
}
