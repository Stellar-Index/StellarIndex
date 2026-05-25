package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/RatesEngine/rates-engine/internal/sources/sorobanevents"
)

// InsertSorobanEventsBatch persists a slice of raw Soroban event
// rows into the `soroban_events` hypertable (migration 0041,
// ADR-0029). Idempotent on the (ledger, tx_hash, op_index,
// event_index) PK via `ON CONFLICT DO NOTHING` — replays /
// retries / overlapping backfill chunks are a no-op for already-
// captured rows.
//
// Single multi-row INSERT statement. For a typical batch size of
// 1000 rows × 15 columns this is one round-trip to Postgres, which
// is what the [sorobanevents.AsyncSink] depends on for throughput.
//
// An empty batch returns nil — the caller's flush ticker hits an
// idle period; that's not an error.
//
// Defensive bounds-check: every row must carry a non-empty TxHash
// + ContractID + Topic0XDR + BodyXDR. The capture function should
// never produce malformed rows, but a single bad row in a 1000-row
// batch would abort the whole transaction on COMMIT — surfacing
// the validation here keeps one rogue event from sinking 999 good
// ones.
func (s *Store) InsertSorobanEventsBatch(ctx context.Context, rows []sorobanevents.Row) error {
	if len(rows) == 0 {
		return nil
	}

	// Per-row validation. Cheap — keeps a transient malformed row
	// from poisoning the batch.
	for i := range rows {
		r := &rows[i]
		if len(r.TxHash) != 32 {
			return fmt.Errorf("timescale: InsertSorobanEventsBatch: row %d TxHash len %d, want 32", i, len(r.TxHash))
		}
		if r.ContractID == "" {
			return fmt.Errorf("timescale: InsertSorobanEventsBatch: row %d empty ContractID", i)
		}
		if len(r.ContractIDHex) != 32 {
			return fmt.Errorf("timescale: InsertSorobanEventsBatch: row %d ContractIDHex len %d, want 32", i, len(r.ContractIDHex))
		}
		if len(r.Topic0XDR) == 0 {
			return fmt.Errorf("timescale: InsertSorobanEventsBatch: row %d empty Topic0XDR", i)
		}
		if len(r.BodyXDR) == 0 {
			return fmt.Errorf("timescale: InsertSorobanEventsBatch: row %d empty BodyXDR", i)
		}
	}

	// Build the multi-row VALUES clause + arg slice. 15 columns ×
	// N rows.
	const cols = 15
	var sb strings.Builder
	sb.WriteString(`
        INSERT INTO soroban_events (
            ledger, ledger_close_time, tx_hash, op_index, event_index,
            contract_id, contract_id_hex,
            topic_count, topic_0_sym,
            topic_0_xdr, topic_1_xdr, topic_2_xdr, topic_3_xdr,
            body_xdr, op_args_xdr
        ) VALUES `)
	args := make([]any, 0, cols*len(rows))
	for i := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * cols
		fmt.Fprintf(&sb,
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5,
			base+6, base+7,
			base+8, base+9,
			base+10, base+11, base+12, base+13,
			base+14, base+15,
		)
		r := &rows[i]
		args = append(args,
			int64(r.Ledger),
			r.LedgerCloseTime,
			r.TxHash,
			int16(r.OpIndex),
			int16(r.EventIndex),
			r.ContractID,
			r.ContractIDHex,
			int16(r.TopicCount),
			nullString(r.Topic0Sym),
			r.Topic0XDR,
			nullBytes(r.Topic1XDR),
			nullBytes(r.Topic2XDR),
			nullBytes(r.Topic3XDR),
			r.BodyXDR,
			nullBytes(r.OpArgsXDR),
		)
	}
	sb.WriteString(` ON CONFLICT (ledger, tx_hash, op_index, event_index) DO NOTHING`)

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("timescale: InsertSorobanEventsBatch (%d rows): %w", len(rows), err)
	}
	return nil
}

// CountSorobanEventsInRange returns the row count in the ledger
// range [from, to] inclusive. Test + diagnostic helper — not on
// the hot path. Used by the integration test to assert
// "100 events walked → 100 rows landed".
func (s *Store) CountSorobanEventsInRange(ctx context.Context, from, to uint32) (int64, error) {
	if to < from {
		return 0, errors.New("timescale: CountSorobanEventsInRange: to < from")
	}
	const q = `SELECT count(*) FROM soroban_events WHERE ledger BETWEEN $1 AND $2`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, int64(from), int64(to)).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: CountSorobanEventsInRange [%d,%d]: %w", from, to, err)
	}
	return n, nil
}

// nullString maps an empty string to SQL NULL and any other value
// to a populated sql.NullString. Mirrors `nullNumeric` in
// cctp_events.go.
func nullString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// nullBytes maps a nil byte slice to SQL NULL and any other value
// to the bytes verbatim. Postgres' bytea column accepts the []byte
// form natively via lib/pq's driver.
func nullBytes(v []byte) any {
	if v == nil {
		return nil
	}
	return v
}
