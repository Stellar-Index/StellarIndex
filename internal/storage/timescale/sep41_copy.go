package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CopyMergeSEP41Transfers bulk-loads rows via the COPY protocol into a
// temp table, then merges with ON CONFLICT DO NOTHING. Built for the
// 2026-07-05 full-history re-derive: multi-row INSERTs topped out
// near ~4 batches/s (every 12k-placeholder statement pays full
// parse/plan), which priced a ~700M-row rebuild in days. COPY + merge
// moves the same rows at bulk-load speed while keeping the idempotent
// conflict semantics of the row path.
func (s *Store) CopyMergeSEP41Transfers(ctx context.Context, rows []SEP41TransferRow) error {
	if len(rows) == 0 {
		return nil
	}
	return s.copyMerge(ctx, "sep41_transfers",
		[]string{
			"ledger_close_time", "ledger", "tx_hash", "op_index", "event_index",
			"contract_id", "event_kind", "from_addr", "to_addr",
			"amount", "live_until_ledger", "authorized",
		},
		"(ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index)",
		len(rows),
		func(st *sql.Stmt) error {
			for i := range rows {
				r := &rows[i]
				if _, err := st.ExecContext(ctx,
					r.ObservedAt.UTC(), int64(r.Ledger), r.TxHash,
					int16(r.OpIndex), int16(r.EventIndex), r.ContractID,
					string(r.Kind), nullStrXfer(r.FromAddr), nullStrXfer(r.ToAddr),
					nullNumericFromBigXfer(r.Amount),
					nullU32Xfer(r.LiveUntilLedger, r.Kind == SEP41Approve),
					nullBoolXfer(r.Authorized),
				); err != nil {
					return fmt.Errorf("copy row %d: %w", i, err)
				}
			}
			return nil
		})
}

// CopyMergeSEP41SupplyEvents is the sep41_supply_events sibling.
func (s *Store) CopyMergeSEP41SupplyEvents(ctx context.Context, rows []SEP41SupplyEvent) error {
	if len(rows) == 0 {
		return nil
	}
	return s.copyMerge(ctx, "sep41_supply_events",
		[]string{
			"contract_id", "ledger", "tx_hash", "op_index", "event_index",
			"observed_at", "event_kind", "amount", "counterparty",
		},
		"(contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index)",
		len(rows),
		func(st *sql.Stmt) error {
			for i := range rows {
				e := &rows[i]
				if e.Amount == nil {
					return fmt.Errorf("copy row %d: nil Amount", i)
				}
				if _, err := st.ExecContext(ctx,
					e.ContractID, int64(e.Ledger), e.TxHash,
					int16(e.OpIndex), int16(e.EventIndex), e.ObservedAt.UTC(),
					string(e.Kind), e.Amount.String(),
					sql.NullString{String: e.Counterparty, Valid: e.Counterparty != ""},
				); err != nil {
					return fmt.Errorf("copy row %d: %w", i, err)
				}
			}
			return nil
		})
}

// copyMerge runs COPY into an ON COMMIT DROP temp table shaped like
// target, then INSERT..SELECT..ON CONFLICT DO NOTHING, in one txn.
// The temp table drops the hypertable's constraints/indexes, so COPY
// streams at wire speed; the merge pays index cost once per row like
// any insert, without per-statement parse overhead.
func (s *Store) copyMerge(ctx context.Context, target string, cols []string, conflict string, n int, feed func(*sql.Stmt) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: copyMerge %s: begin: %w", target, err)
	}
	defer func() { _ = tx.Rollback() }()

	tmp := "copy_merge_" + target
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		"CREATE TEMP TABLE %s (LIKE %s INCLUDING DEFAULTS) ON COMMIT DROP", tmp, target)); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: temp: %w", target, err)
	}
	colList := strings.Join(cols, ", ")
	if err := runCopy(ctx, tx, fmt.Sprintf("COPY %s (%s) FROM STDIN", tmp, colList), feed); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: %w", target, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		"INSERT INTO %s (%s) SELECT %s FROM %s ON CONFLICT %s DO NOTHING",
		target, colList, colList, tmp, conflict)); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: merge %d rows: %w", target, n, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: copyMerge %s: commit: %w", target, err)
	}
	return nil
}

// runCopy prepares a COPY..FROM STDIN statement, feeds rows, flushes,
// and closes — the flush (empty Exec) must precede Close, and both
// errors are surfaced.
func runCopy(ctx context.Context, tx *sql.Tx, stmt string, feed func(*sql.Stmt) error) (err error) {
	st, perr := tx.PrepareContext(ctx, stmt)
	if perr != nil {
		return fmt.Errorf("prepare copy: %w", perr)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close copy: %w", cerr)
		}
	}()
	if ferr := feed(st); ferr != nil {
		return ferr
	}
	if _, xerr := st.ExecContext(ctx); xerr != nil { // flush COPY buffer
		return fmt.Errorf("flush copy: %w", xerr)
	}
	return nil
}
