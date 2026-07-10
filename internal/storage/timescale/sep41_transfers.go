package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// SEP41TransferKind discriminates the four audit-trail variants
// the sep41_transfers hypertable owns. mint/burn/clawback are NOT
// here — they belong to sep41_supply_events (Algorithm 3).
type SEP41TransferKind string

const (
	SEP41Transfer      SEP41TransferKind = "transfer"
	SEP41Approve       SEP41TransferKind = "approve"
	SEP41SetAdmin      SEP41TransferKind = "set_admin"
	SEP41SetAuthorized SEP41TransferKind = "set_authorized"
)

func (k SEP41TransferKind) IsValid() bool {
	switch k {
	case SEP41Transfer, SEP41Approve, SEP41SetAdmin, SEP41SetAuthorized:
		return true
	}
	return false
}

// SEP41TransferRow mirrors migration 0047's column set. Nil/zero
// values flow to SQL NULL where non-applicable for the kind.
type SEP41TransferRow struct {
	ContractID      string
	Ledger          uint32
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	ObservedAt      time.Time
	Kind            SEP41TransferKind
	FromAddr        string
	ToAddr          string
	Amount          *big.Int
	LiveUntilLedger uint32
	Authorized      *bool
}

// InsertSEP41TransferBatch persists rows via a single multi-row
// INSERT. Idempotent on the full PK via ON CONFLICT DO NOTHING.
//
//nolint:gocognit,gocyclo // per-row validation + 12-col placeholder builder; linear.
func (s *Store) InsertSEP41TransferBatch(ctx context.Context, rows []SEP41TransferRow) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		r := &rows[i]
		if r.ContractID == "" {
			return fmt.Errorf("timescale: InsertSEP41TransferBatch: row %d empty ContractID", i)
		}
		if r.TxHash == "" {
			return fmt.Errorf("timescale: InsertSEP41TransferBatch: row %d empty TxHash", i)
		}
		if !r.Kind.IsValid() {
			return fmt.Errorf("timescale: InsertSEP41TransferBatch: row %d invalid Kind %q", i, r.Kind)
		}
		// Only value-bearing kinds require a non-negative Amount; SetAdmin /
		// SetAuthorized carry no amount (Authorized is checked separately below).
		//exhaustive:ignore
		switch r.Kind {
		case SEP41Transfer, SEP41Approve:
			if r.Amount == nil {
				return fmt.Errorf("timescale: InsertSEP41TransferBatch: row %d %s missing Amount", i, r.Kind)
			}
			if r.Amount.Sign() < 0 {
				return fmt.Errorf("timescale: InsertSEP41TransferBatch: row %d %s negative Amount %s", i, r.Kind, r.Amount)
			}
		}
		if r.Kind == SEP41SetAuthorized && r.Authorized == nil {
			return fmt.Errorf("timescale: InsertSEP41TransferBatch: row %d set_authorized missing Authorized", i)
		}
	}

	const ncols = 12
	var sb strings.Builder
	sb.WriteString(`
        INSERT INTO sep41_transfers (
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, event_kind,
            from_addr, to_addr,
            amount, live_until_ledger, authorized
        ) VALUES `)
	args := make([]any, 0, ncols*len(rows))
	for i := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * ncols
		fmt.Fprintf(&sb,
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11, base+12,
		)
		r := &rows[i]
		args = append(args,
			r.ObservedAt.UTC(),
			int64(r.Ledger),
			r.TxHash,
			int16(r.OpIndex),
			int16(r.EventIndex),
			r.ContractID,
			string(r.Kind),
			nullStrXfer(r.FromAddr),
			nullStrXfer(r.ToAddr),
			nullNumericFromBigXfer(r.Amount),
			nullU32Xfer(r.LiveUntilLedger, r.Kind == SEP41Approve),
			nullBoolXfer(r.Authorized),
		)
	}
	sb.WriteString(` ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index) DO NOTHING`)

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("timescale: InsertSEP41TransferBatch (%d rows): %w", len(rows), err)
	}
	return nil
}

// InsertSEP41Transfer is the single-row convenience wrapper.
func (s *Store) InsertSEP41Transfer(ctx context.Context, r SEP41TransferRow) error {
	return s.InsertSEP41TransferBatch(ctx, []SEP41TransferRow{r})
}

// ListSEP41Transfers returns the most-recent N rows for a contract,
// optionally filtered by from_addr / to_addr. Powers GET
// /v1/contracts/{id}/transfers.
//
//nolint:gocognit,gocyclo // linear query-build + row-scan loop.
func (s *Store) ListSEP41Transfers(ctx context.Context, contractID, fromAddr, toAddr string, limit int) ([]SEP41TransferRow, error) {
	if contractID == "" {
		return nil, errors.New("timescale: ListSEP41Transfers: empty contractID")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	var sb strings.Builder
	sb.WriteString(`
        SELECT
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, event_kind,
            from_addr, to_addr,
            amount::text, live_until_ledger, authorized
        FROM sep41_transfers
        WHERE contract_id = $1
    `)
	args := []any{contractID}
	if fromAddr != "" {
		args = append(args, fromAddr)
		fmt.Fprintf(&sb, " AND from_addr = $%d", len(args))
	}
	if toAddr != "" {
		args = append(args, toAddr)
		fmt.Fprintf(&sb, " AND to_addr = $%d", len(args))
	}
	args = append(args, limit)
	fmt.Fprintf(&sb,
		" ORDER BY ledger_close_time DESC, ledger DESC, op_index DESC LIMIT $%d",
		len(args),
	)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListSEP41Transfers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SEP41TransferRow
	for rows.Next() {
		var (
			r          SEP41TransferRow
			ledger     int64
			opIdx      int16
			eventIdx   int16
			kind       string
			fromNull   sql.NullString
			toNull     sql.NullString
			amountStr  sql.NullString
			liveNull   sql.NullInt32
			authorized sql.NullBool
		)
		if err := rows.Scan(
			&r.ObservedAt, &ledger, &r.TxHash, &opIdx, &eventIdx,
			&r.ContractID, &kind,
			&fromNull, &toNull,
			&amountStr, &liveNull, &authorized,
		); err != nil {
			return nil, fmt.Errorf("timescale: ListSEP41Transfers scan: %w", err)
		}
		r.Ledger = uint32(ledger)
		r.OpIndex = uint32(opIdx)
		r.EventIndex = uint32(eventIdx)
		r.Kind = SEP41TransferKind(kind)
		if fromNull.Valid {
			r.FromAddr = fromNull.String
		}
		if toNull.Valid {
			r.ToAddr = toNull.String
		}
		if amountStr.Valid {
			v, ok := new(big.Int).SetString(amountStr.String, 10)
			if !ok {
				return nil, fmt.Errorf("timescale: ListSEP41Transfers: parse amount %q", amountStr.String)
			}
			r.Amount = v
		}
		if liveNull.Valid {
			r.LiveUntilLedger = uint32(liveNull.Int32)
		}
		if authorized.Valid {
			b := authorized.Bool
			r.Authorized = &b
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SEP41MovementsFloorLedger is ADR-0048 D5's non-overlap boundary for
// the /v1/accounts/{g}/movements merge: ClickHouse's
// stellar.account_movements (the pre-P23 classic-movement archive,
// internal/storage/clickhouse/account_movements.go) is hard-clamped
// BELOW this ledger by its only writer
// (`stellarindex-ops classic-movements-backfill`); ListSEP41TransfersByAddress
// floors its own query at-or-above it, so the two stores' contributions
// to the merged feed can never overlap.
//
// Same VALUE as internal/sources/classicmovements.P23StartLedger, not
// the same CONSTANT: internal/storage sits below internal/sources in
// the repo's import direction (scripts/ci/lint-imports.sh's
// L/storage-below-compute rule forbids a storage->sources edge, test
// files included), so this package can't import that one to avoid
// duplicating the literal. internal/api/v1/explorer/movements_test.go's
// TestP23BoundaryConstantsAgree is the executable assertion that keeps
// the two from silently drifting — it CAN import both (api sits above
// both layers).
const SEP41MovementsFloorLedger uint32 = 58_762_517

// SEP41TransferCursor is the keyset position for
// ListSEP41TransfersByAddress pagination (ADR-0048 D5) — descending
// (ledger, tx_hash, op_index, event_index), generalizing
// ListSEP41Transfers' per-contract natural-key order across contracts
// for the address-scoped read. Zero value (Ledger==0) means "from the
// newest" (first page) — same IsSet/Ledger==0 sentinel convention as
// clickhouse.ExplorerCursor / clickhouse.AccountMovementCursor.
type SEP41TransferCursor struct {
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
}

// IsSet reports whether the cursor points past the newest row (a
// continuation page, not the first).
func (c SEP41TransferCursor) IsSet() bool { return c.Ledger > 0 }

// ListSEP41TransfersByAddress returns one address's SEP-41 'transfer'
// history — both sides (from_addr = address OR to_addr = address) —
// newest first, keyset-paged by the composite (ledger, tx_hash,
// op_index, event_index) cursor. ADR-0048 D5: this is the Postgres
// "recent tail" half of the unified GET /v1/accounts/{g}/movements
// feed; internal/api/v1/explorer/movements.go merges it with
// ClickHouse's stellar.account_movements (the pre-P23 archive) —
// SEP41MovementsFloorLedger's doc comment has the non-overlap
// argument.
//
// Scope, deliberately narrower than ListSEP41Transfers:
//   - event_kind = 'transfer' only — approve/set_admin/set_authorized
//     don't move an asset amount, so they aren't "movements".
//   - ledger >= SEP41MovementsFloorLedger. Below the P23 boundary, any
//     transfer of a CLASSIC asset already has a
//     stellar.account_movements row (ADR-0047); a pure Soroban-native
//     SEP-41 token transfer below the boundary is real activity this
//     scope doesn't surface via this feed yet — a documented gap (see
//     the OpenAPI description for GET /accounts/{g_strkey}/movements),
//     not a bug.
//
// direction, when non-empty, must be "sent"/"received"/"self"
// (mirroring clickhouse.AccountMovementDirection, which this package
// can't import — see SEP41MovementsFloorLedger's doc comment on the
// import-direction rule) and is evaluated against `address`: "sent" =
// from_addr=address (and to_addr != address), "received" = the
// reverse, "self" = from_addr=address AND to_addr=address. No
// per-contract asset filter here — resolving a token contract_id to
// the CANONICAL asset id CH's account_movements.asset column holds is
// a per-row lookup the caller (movements.go) already does for
// display, so it applies any ?asset= filter itself, POST-fetch, on
// the resolved name; this keeps the two merge-side queries' asset
// semantics honestly asymmetric (documented) rather than silently
// wrong.
//
//nolint:gocognit,gocyclo // linear query-build (four optional clauses) + row-scan loop, same shape as ListSEP41Transfers.
func (s *Store) ListSEP41TransfersByAddress(ctx context.Context, address string, limit int, cur SEP41TransferCursor, direction string) ([]SEP41TransferRow, error) {
	if address == "" {
		return nil, errors.New("timescale: ListSEP41TransfersByAddress: empty address")
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
	}

	var sb strings.Builder
	sb.WriteString(`
        SELECT
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, event_kind,
            from_addr, to_addr,
            amount::text, live_until_ledger, authorized
        FROM sep41_transfers
        WHERE event_kind = 'transfer'
          AND ledger >= $1
          AND (from_addr = $2 OR to_addr = $2)
    `)
	args := []any{int64(SEP41MovementsFloorLedger), address}
	switch direction {
	case "sent":
		sb.WriteString(" AND from_addr = $2 AND (to_addr IS DISTINCT FROM $2)")
	case "received":
		sb.WriteString(" AND to_addr = $2 AND (from_addr IS DISTINCT FROM $2)")
	case "self":
		sb.WriteString(" AND from_addr = $2 AND to_addr = $2")
	case "":
		// no direction filter
	default:
		return nil, fmt.Errorf("timescale: ListSEP41TransfersByAddress: invalid direction %q", direction)
	}
	if cur.IsSet() {
		args = append(args, int64(cur.Ledger), cur.TxHash, int16(cur.OpIndex), int16(cur.EventIndex))
		fmt.Fprintf(&sb, " AND (ledger, tx_hash, op_index, event_index) < ($%d, $%d, $%d, $%d)",
			len(args)-3, len(args)-2, len(args)-1, len(args))
	}
	args = append(args, limit)
	fmt.Fprintf(&sb, " ORDER BY ledger DESC, tx_hash DESC, op_index DESC, event_index DESC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListSEP41TransfersByAddress(%s): %w", address, err)
	}
	defer func() { _ = rows.Close() }()

	var out []SEP41TransferRow
	for rows.Next() {
		var (
			r          SEP41TransferRow
			ledger     int64
			opIdx      int16
			eventIdx   int16
			kind       string
			fromNull   sql.NullString
			toNull     sql.NullString
			amountStr  sql.NullString
			liveNull   sql.NullInt32
			authorized sql.NullBool
		)
		if err := rows.Scan(
			&r.ObservedAt, &ledger, &r.TxHash, &opIdx, &eventIdx,
			&r.ContractID, &kind,
			&fromNull, &toNull,
			&amountStr, &liveNull, &authorized,
		); err != nil {
			return nil, fmt.Errorf("timescale: ListSEP41TransfersByAddress scan: %w", err)
		}
		r.Ledger = uint32(ledger)
		r.OpIndex = uint32(opIdx)
		r.EventIndex = uint32(eventIdx)
		r.Kind = SEP41TransferKind(kind)
		if fromNull.Valid {
			r.FromAddr = fromNull.String
		}
		if toNull.Valid {
			r.ToAddr = toNull.String
		}
		if amountStr.Valid {
			v, ok := new(big.Int).SetString(amountStr.String, 10)
			if !ok {
				return nil, fmt.Errorf("timescale: ListSEP41TransfersByAddress: parse amount %q", amountStr.String)
			}
			r.Amount = v
		}
		if liveNull.Valid {
			r.LiveUntilLedger = uint32(liveNull.Int32)
		}
		if authorized.Valid {
			b := authorized.Bool
			r.Authorized = &b
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountSEP41TransfersInRange returns the row count in [from, to].
func (s *Store) CountSEP41TransfersInRange(ctx context.Context, from, to uint32) (int64, error) {
	if to < from {
		return 0, errors.New("timescale: CountSEP41TransfersInRange: to < from")
	}
	const q = `SELECT count(*) FROM sep41_transfers WHERE ledger BETWEEN $1 AND $2`
	var n int64
	if err := s.db.QueryRowContext(ctx, q, int64(from), int64(to)).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: CountSEP41TransfersInRange [%d,%d]: %w", from, to, err)
	}
	return n, nil
}

func nullStrXfer(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func nullNumericFromBigXfer(amt *big.Int) sql.NullString {
	if amt == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: amt.String(), Valid: true}
}

func nullU32Xfer(v uint32, applicable bool) sql.NullInt32 {
	if !applicable {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(v), Valid: true} //nolint:gosec // u32 -> int32 reinterpret.
}

func nullBoolXfer(b *bool) sql.NullBool {
	if b == nil {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: *b, Valid: true}
}
