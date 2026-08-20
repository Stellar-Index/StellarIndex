package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// CopyMergeSEP41Transfers bulk-loads rows via the COPY protocol into a
// temp table, then merges with the same generation-guarded corrective
// upsert as the per-row path (InsertSEP41TransferBatch). Built for the
// 2026-07-05 full-history re-derive: multi-row INSERTs topped out
// near ~4 batches/s (every 12k-placeholder statement pays full
// parse/plan), which priced a ~700M-row rebuild in days. COPY + merge
// moves the same rows at bulk-load speed while carrying the row path's
// INV-3 semantics: derive_generation is COPY'd per row and the merge is
// the gen-guarded DO UPDATE, so a re-derive at a higher-or-equal
// generation corrects a wrong value in place and a stale lower-generation
// replay can never revert it (migration 0110). The old col lists omitted
// derive_generation — so bulk-loaded rows defaulted to generation 0 — and
// merged DO NOTHING, silently dropping the correction (TV-1/TV-3).
func (s *Store) CopyMergeSEP41Transfers(ctx context.Context, rows []SEP41TransferRow) error {
	if len(rows) == 0 {
		return nil
	}
	values := make([][]any, len(rows))
	for i := range rows {
		r := &rows[i]
		values[i] = []any{
			// tx_hash is a `bytea` column (migration 0047). pgx's binary
			// CopyFrom needs a []byte for bytea — unlike a plain Exec, it
			// won't wrap a string — so pass the hash's raw bytes explicitly.
			// []byte(string) is byte-identical to what the per-row
			// InsertSEP41TransferBatch path stores for the same TxHash.
			r.ObservedAt.UTC(), int64(r.Ledger), []byte(r.TxHash),
			int16(r.OpIndex), int16(r.EventIndex), r.ContractID,
			string(r.Kind), nullStrXfer(r.FromAddr), nullStrXfer(r.ToAddr),
			nullNumericFromBigXfer(r.Amount),
			nullU32Xfer(r.LiveUntilLedger, r.Kind == SEP41Approve),
			nullBoolXfer(r.Authorized),
			s.deriveGeneration,
		}
	}
	return s.copyMerge(ctx, "sep41_transfers",
		[]string{
			"ledger_close_time", "ledger", "tx_hash", "op_index", "event_index",
			"contract_id", "event_kind", "from_addr", "to_addr",
			"amount", "live_until_ledger", "authorized",
			"derive_generation",
		},
		"(ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index)",
		// Non-conflict value columns the corrective re-derive updates in place,
		// mirroring InsertSEP41TransferBatch's DO UPDATE SET (sep41_transfers.go).
		[]string{
			"event_kind", "from_addr", "to_addr",
			"amount", "live_until_ledger", "authorized",
			"derive_generation",
		},
		values)
}

// CopyMergeSEP41SupplyEvents is the sep41_supply_events sibling. It carries
// the same INV-3 generation-guarded corrective-upsert semantics as the
// per-row InsertSEP41SupplyEvent: derive_generation is COPY'd per row and
// the merge is the gen-guarded DO UPDATE (migration 0110), not the old
// generation-0 DO NOTHING (TV-1/TV-3).
func (s *Store) CopyMergeSEP41SupplyEvents(ctx context.Context, rows []SEP41SupplyEvent) error {
	if len(rows) == 0 {
		return nil
	}
	values := make([][]any, len(rows))
	for i := range rows {
		e := &rows[i]
		if e.Amount == nil {
			return fmt.Errorf("timescale: CopyMergeSEP41SupplyEvents: row %d: nil Amount", i)
		}
		values[i] = []any{
			e.ContractID, int64(e.Ledger), e.TxHash,
			int16(e.OpIndex), int16(e.EventIndex), e.ObservedAt.UTC(),
			string(e.Kind), e.Amount.String(),
			sql.NullString{String: e.Counterparty, Valid: e.Counterparty != ""},
			s.deriveGeneration,
		}
	}
	return s.copyMerge(ctx, "sep41_supply_events",
		[]string{
			"contract_id", "ledger", "tx_hash", "op_index", "event_index",
			"observed_at", "event_kind", "amount", "counterparty",
			"derive_generation",
		},
		"(contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index)",
		// Non-conflict value columns the corrective re-derive updates in place,
		// mirroring InsertSEP41SupplyEvent's DO UPDATE SET (sep41_supply_events.go).
		[]string{"amount", "counterparty", "derive_generation"},
		values)
}

// copyMerge streams rows into an ON COMMIT DROP temp table shaped like
// target, then INSERT..SELECT..ON CONFLICT DO UPDATE (the INV-3
// generation-guarded corrective upsert), in one txn. The temp table drops
// the hypertable's constraints/indexes, so COPY streams at wire speed; the
// merge pays index cost once per row like any insert, without per-statement
// parse overhead. updateCols are the non-conflict value columns the merge
// corrects on conflict (derive_generation must be among them so the guard
// column advances).
//
// The COPY itself runs through pgx's native CopyFrom (the binary COPY
// protocol) rather than database/sql's per-row statement Exec: the pgx
// stdlib driver — unlike lib/pq — does not implement the driver-level
// `COPY … FROM STDIN` prepared-statement buffering, so we reach the pgx
// connection via conn.Raw and drive the whole load/merge as one pgx-native
// transaction on a single pooled connection.
func (s *Store) copyMerge(ctx context.Context, target string, cols []string, conflict string, updateCols []string, values [][]any) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("timescale: copyMerge %s: conn: %w", target, err)
	}
	defer func() { _ = conn.Close() }()

	return conn.Raw(func(driverConn any) error {
		stdlibConn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			// Guards against a silent driver swap: CopyFrom needs the pgx
			// connection, so fail loudly rather than skip the bulk load.
			return fmt.Errorf("timescale: copyMerge %s: driver conn %T is not *stdlib.Conn", target, driverConn)
		}
		return copyMergeOnConn(ctx, stdlibConn.Conn(), target, cols, conflict, updateCols, values)
	})
}

// copyMergeOnConn runs the create-temp / COPY / merge / commit sequence on
// one pgx connection inside a single pgx-native transaction. Split from
// copyMerge so the conn.Raw closure stays a thin unwrap.
func copyMergeOnConn(ctx context.Context, pgxConn *pgx.Conn, target string, cols []string, conflict string, updateCols []string, values [][]any) (err error) {
	tx, err := pgxConn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("timescale: copyMerge %s: begin: %w", target, err)
	}
	// Rollback is a no-op once Commit has run (returns ErrTxClosed, ignored).
	defer func() { _ = tx.Rollback(ctx) }()

	tmp := "copy_merge_" + target
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		"CREATE TEMP TABLE %s (LIKE %s INCLUDING DEFAULTS) ON COMMIT DROP", tmp, target)); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: temp: %w", target, err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{tmp}, cols, pgx.CopyFromRows(values)); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: copy %d rows: %w", target, len(values), err)
	}
	colList := strings.Join(cols, ", ")
	if _, err := tx.Exec(ctx, copyMergeUpsertSQL(target, tmp, colList, conflict, updateCols)); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: merge %d rows: %w", target, len(values), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: commit: %w", target, err)
	}
	return nil
}

// copyMergeUpsertSQL builds the INSERT..SELECT..ON CONFLICT DO UPDATE that
// promotes COPY'd temp-table rows into target with the same INV-3
// generation-guarded corrective-upsert semantics as the per-row writers
// (sep41_transfers.go / sep41_supply_events.go): a re-derive at a
// higher-or-equal derive_generation UPDATEs the value columns in place; a
// stale lower-generation replay can never revert a correction. Extracted as
// a pure builder so the generation guard is unit-testable without a DB.
func copyMergeUpsertSQL(target, tmp, colList, conflict string, updateCols []string) string {
	set := make([]string, len(updateCols))
	for i, c := range updateCols {
		set[i] = c + " = EXCLUDED." + c
	}
	return fmt.Sprintf(
		"INSERT INTO %s (%s) SELECT %s FROM %s ON CONFLICT %s DO UPDATE SET %s "+
			"WHERE %s.derive_generation <= EXCLUDED.derive_generation",
		target, colList, colList, tmp, conflict, strings.Join(set, ", "), target)
}
