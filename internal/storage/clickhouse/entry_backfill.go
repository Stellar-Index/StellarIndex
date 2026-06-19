package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// SnapshotEntryRow builds a ledger_entry_changes row from a live snapshot entry
// (Post-only), populating owner/asset/balance with the SAME helpers the indexer
// uses for its own writes — so account-state + asset-holder + supply reads work
// identically against backfilled rows. Keyed on the entry's real LedgerKey +
// LastModifiedLedgerSeq (so the readers' key_xdr lookups match and newest-wins
// ordering stays correct). change_type="state", op_index=-1. ok=false on a
// marshal error (skip the one entry, never abort the backfill).
func SnapshotEntryRow(post *xdr.LedgerEntry, closeTime time.Time) (LedgerEntryChangeRow, bool) {
	key, err := post.LedgerKey()
	if err != nil {
		return LedgerEntryChangeRow{}, false
	}
	keyB64, err := xdr.MarshalBase64(key)
	if err != nil {
		return LedgerEntryChangeRow{}, false
	}
	entryB64, err := xdr.MarshalBase64(post)
	if err != nil {
		return LedgerEntryChangeRow{}, false
	}
	owner, asset := ownerAndAsset(key)
	return LedgerEntryChangeRow{
		LedgerSeq:  uint32(post.LastModifiedLedgerSeq), //nolint:gosec // ledger seq fits uint32
		CloseTime:  closeTime,
		OpIndex:    -1,
		ChangeType: "state",
		EntryType:  entryTypeName(post.Data.Type),
		KeyXDR:     keyB64,
		EntryXDR:   entryB64,
		AccountID:  owner,
		Asset:      asset,
		Balance:    entryBalance(*post),
	}, true
}

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
// throttle is a pause between chunks — keep it non-zero for large backfills so
// the insert + the ledger_entries_current MV don't spike the live serving CH.
func InsertEntryChanges(ctx context.Context, addr string, rows []LedgerEntryChangeRow, throttle time.Duration) (int, error) {
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
		if err := insertEntryChunk(ctx, conn, rows[start:end]); err != nil {
			return written, err
		}
		written += end - start
		if throttle > 0 && end < len(rows) {
			select {
			case <-time.After(throttle):
			case <-ctx.Done():
				return written, ctx.Err()
			}
		}
	}
	return written, nil
}

// insertEntryChunk sends one prepared batch of entry-change rows.
func insertEntryChunk(ctx context.Context, conn clickhouse.Conn, chunk []LedgerEntryChangeRow) error {
	b, err := conn.PrepareBatch(ctx,
		"INSERT INTO stellar.ledger_entry_changes (ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, account_id, asset, balance)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare entry-changes batch: %w", err)
	}
	for _, r := range chunk {
		if err := b.Append(r.LedgerSeq, r.CloseTime, r.TxHash, r.OpIndex, r.ChangeIndex,
			r.ChangeType, r.EntryType, r.KeyXDR, r.EntryXDR, r.AccountID, r.Asset, r.Balance); err != nil {
			return fmt.Errorf("clickhouse: append entry-change: %w", err)
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("clickhouse: send entry-changes batch: %w", err)
	}
	return nil
}
