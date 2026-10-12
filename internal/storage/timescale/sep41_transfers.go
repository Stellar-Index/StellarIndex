package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
)

// SEP41TransferKind discriminates the audit-trail variants sep41_transfers owns; mint/burn/clawback belong to sep41_supply_events, not here.
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

// SEP41TransferRow mirrors migration 0047's column set.
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

// InsertSEP41TransferBatch persists rows via one multi-row INSERT whose ON CONFLICT is a derive_generation-guarded upsert: an equal-or-higher-generation replay corrects the row, a stale one is a no-op.
func (s *Store) InsertSEP41TransferBatch(ctx context.Context, rows []SEP41TransferRow) error {
	if len(rows) == 0 {
		return nil
	}
	if err := validateSEP41TransferRows("InsertSEP41TransferBatch", rows); err != nil {
		return err
	}

	// The upsert is ON CONFLICT DO UPDATE, and Postgres rejects one statement presenting the same
	// conflict key twice ("cannot affect row a second time"), which DO NOTHING would absorb.
	// Collapse intra-batch duplicates (last-wins) first.
	insertRows := dedupeSEP41TransferRows(rows)

	const ncols = 13
	var sb strings.Builder
	// Generation-guarded corrective upsert: a re-derive with a higher-or-
	// equal generation UPDATEs the decoded value columns (amount, event_kind,
	// addresses, …) in place; a live gen-0 replay can never revert a
	// correction.
	sb.WriteString(`
        INSERT INTO sep41_transfers (
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, event_kind,
            from_addr, to_addr,
            amount, live_until_ledger, authorized,
            derive_generation
        ) VALUES `)
	args := make([]any, 0, ncols*len(insertRows))
	for i := range insertRows {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * ncols
		fmt.Fprintf(&sb,
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11, base+12, base+13,
		)
		r := &insertRows[i]
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
			s.deriveGeneration,
		)
	}
	sb.WriteString(` ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index) DO UPDATE SET
            event_kind        = EXCLUDED.event_kind,
            from_addr         = EXCLUDED.from_addr,
            to_addr           = EXCLUDED.to_addr,
            amount            = EXCLUDED.amount,
            live_until_ledger = EXCLUDED.live_until_ledger,
            authorized        = EXCLUDED.authorized,
            derive_generation = EXCLUDED.derive_generation
          WHERE sep41_transfers.derive_generation <= EXCLUDED.derive_generation`)

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("timescale: InsertSEP41TransferBatch (%d rows): %w", len(insertRows), err)
	}
	return nil
}

// validateSEP41TransferRows is the pre-DB row contract for every sep41_transfers writer, so per-row and COPY paths agree. op names the
// caller.
func validateSEP41TransferRows(op string, rows []SEP41TransferRow) error {
	for i := range rows {
		r := &rows[i]
		if r.ContractID == "" {
			return fmt.Errorf("timescale: %s: row %d empty ContractID", op, i)
		}
		if r.TxHash == "" {
			return fmt.Errorf("timescale: %s: row %d empty TxHash", op, i)
		}
		if !r.Kind.IsValid() {
			return fmt.Errorf("timescale: %s: row %d invalid Kind %q", op, i, r.Kind)
		}
		// Mirrors sep41_transfers_amount_check exactly; nothing in the
		// database enforces it while 0174 is a no-op.
		if r.Amount != nil && r.Amount.Sign() < 0 {
			return fmt.Errorf("timescale: %s: row %d %s negative Amount %s", op, i, r.Kind, r.Amount)
		}
		if (r.Kind == SEP41Transfer || r.Kind == SEP41Approve) && r.Amount == nil {
			return fmt.Errorf("timescale: %s: row %d %s missing Amount", op, i, r.Kind)
		}
		if r.Kind == SEP41SetAuthorized && r.Authorized == nil {
			return fmt.Errorf("timescale: %s: row %d set_authorized missing Authorized", op, i)
		}
	}
	return nil
}

// sep41TransferConflictKey is the ON CONFLICT identity (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index);
// the time is normalised to UTC UnixNano so instants differing only in monotonic reading or location collapse.
type sep41TransferConflictKey struct {
	observedAtNanos int64
	contractID      string
	ledger          uint32
	txHash          string
	opIndex         uint32
	eventIndex      uint32
}

func sep41TransferKeyOf(r *SEP41TransferRow) sep41TransferConflictKey {
	return sep41TransferConflictKey{
		observedAtNanos: r.ObservedAt.UTC().UnixNano(),
		contractID:      r.ContractID,
		ledger:          r.Ledger,
		txHash:          r.TxHash,
		opIndex:         r.OpIndex,
		eventIndex:      r.EventIndex,
	}
}

// dedupeSEP41TransferRows keeps the LAST copy of rows colliding on the conflict key, in first-seen order: ON CONFLICT DO UPDATE rejects a
// repeated key in one statement. Returns the input slice when there is no duplicate.
func dedupeSEP41TransferRows(rows []SEP41TransferRow) []SEP41TransferRow {
	firstDup := -1
	seen := make(map[sep41TransferConflictKey]int, len(rows))
	for i := range rows {
		k := sep41TransferKeyOf(&rows[i])
		if _, ok := seen[k]; ok {
			firstDup = i
			break
		}
		seen[k] = i
	}
	if firstDup < 0 {
		return rows // already unique — no allocation
	}
	out := make([]SEP41TransferRow, 0, len(rows))
	pos := make(map[sep41TransferConflictKey]int, len(rows))
	for i := range rows {
		k := sep41TransferKeyOf(&rows[i])
		if idx, ok := pos[k]; ok {
			out[idx] = rows[i] // last wins
			continue
		}
		pos[k] = len(out)
		out = append(out, rows[i])
	}
	return out
}

// InsertSEP41Transfer is the single-row convenience wrapper.
func (s *Store) InsertSEP41Transfer(ctx context.Context, r SEP41TransferRow) error {
	return s.InsertSEP41TransferBatch(ctx, []SEP41TransferRow{r})
}

// sep41TransferLookbackLadder: `ledger_close_time >= now-D` floors tried before the full read; the first rung to fill the page wins.
// A floor lets an Incremental Sort stop early; 7d is the widest that keeps it (90d seq-scans); see [SEP41TransferLadderBudget].
var sep41TransferLookbackLadder = []time.Duration{
	time.Hour,
	24 * time.Hour,
	7 * 24 * time.Hour,
}

// SEP41TransferLadderBudget bounds the WHOLE lookback ladder so it can never spend the caller's budget on the way to the fallback that has the rows.
const SEP41TransferLadderBudget = 3 * time.Second

// ListSEP41Transfers returns the most-recent N rows for a contract, optionally filtered by from_addr / to_addr; a page no lookback rung fills falls through to an unbounded read.
func (s *Store) ListSEP41Transfers(ctx context.Context, contractID, fromAddr, toAddr string, limit int) ([]SEP41TransferRow, error) {
	return s.listSEP41TransfersAt(ctx, contractID, fromAddr, toAddr, limit, time.Now(), SEP41TransferLadderBudget)
}

// listSEP41TransfersAt is [Store.ListSEP41Transfers] with the clock and ladder budget injected so window floors and cost bound are
// testable.
func (s *Store) listSEP41TransfersAt(ctx context.Context, contractID, fromAddr, toAddr string, limit int, now time.Time, ladderBudget time.Duration) ([]SEP41TransferRow, error) {
	if contractID == "" {
		return nil, errors.New("timescale: ListSEP41Transfers: empty contractID")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	rows, filled, err := s.walkSEP41TransferLadder(ctx, contractID, fromAddr, toAddr, limit, now, ladderBudget)
	if err != nil {
		return nil, err
	}
	if filled {
		return rows, nil
	}
	// Zero `since` = no window bound: the full-history fallback, issued
	// on the caller's own context — never on the ladder's, whose budget
	// is spent or beside the point by now.
	return s.listSEP41TransfersSince(ctx, contractID, fromAddr, toAddr, limit, time.Time{})
}

// walkSEP41TransferLadder runs the rungs under one budget: (rows, true, nil) for the first rung that fills the page; (nil, false, nil) =
// fall back to the unbounded read. Errors only for a real storage failure or the CALLER's own deadline or cancellation.
func (s *Store) walkSEP41TransferLadder(ctx context.Context, contractID, fromAddr, toAddr string, limit int, now time.Time, budget time.Duration) ([]SEP41TransferRow, bool, error) {
	ladderCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	for _, window := range sep41TransferLookbackLadder {
		if ladderCtx.Err() != nil {
			break // spent between rungs: a statement issued now is already dead
		}
		rows, err := s.listSEP41TransfersSince(ladderCtx, contractID, fromAddr, toAddr, limit, now.Add(-window))
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, err // the caller's deadline, not the ladder's
			}
			if ladderCtx.Err() != nil {
				break // the ladder's budget: fall back on the caller's remaining time
			}
			return nil, false, err
		}
		if len(rows) >= limit {
			return rows, true, nil
		}
	}
	return nil, false, nil
}

// sep41TransfersQuery builds one audit-trail read and its args together so placeholder numbering cannot drift from arg order.
// A zero `since` builds the unbounded (full-history) form.
func sep41TransfersQuery(contractID, fromAddr, toAddr string, limit int, since time.Time) (string, []any) {
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
	if !since.IsZero() {
		args = append(args, since.UTC())
		fmt.Fprintf(&sb, " AND ledger_close_time >= $%d", len(args))
	}
	args = append(args, limit)
	fmt.Fprintf(&sb,
		" ORDER BY ledger_close_time DESC, ledger DESC, op_index DESC LIMIT $%d",
		len(args),
	)
	return sb.String(), args
}

// listSEP41TransfersSince runs one ladder rung (zero `since` = the unbounded fallback) and maps its rows.
//
//nolint:gocognit,gocyclo // linear null-projecting row-scan loop.
func (s *Store) listSEP41TransfersSince(ctx context.Context, contractID, fromAddr, toAddr string, limit int, since time.Time) ([]SEP41TransferRow, error) {
	q, args := sep41TransfersQuery(contractID, fromAddr, toAddr, limit, since)

	rows, err := s.db.QueryContext(ctx, q, args...)
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

// SEP41MovementsFloorLedger is the non-overlap boundary for the /v1/accounts/{g}/movements merge: ClickHouse's pre-P23 account_movements is clamped below this ledger by its only writer.
const SEP41MovementsFloorLedger uint32 = 58_762_517

// SEP41TransferCursor is the descending keyset position (ledger, tx_hash, op_index, event_index) for ListSEP41TransfersByAddress; the zero value (Ledger==0) means the first page.
type SEP41TransferCursor struct {
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
}

// IsSet reports whether the cursor points past the newest row (a continuation page, not the first).
func (c SEP41TransferCursor) IsSet() bool { return c.Ledger > 0 }

// cursorIndex16 clamps a client cursor index to the smallint column: a wrap to negative would skip the rest of that tx.
func cursorIndex16(v uint32) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	return int16(v)
}

// ListSEP41TransfersByAddress returns one address's SEP-41 'transfer' history, both sides, newest
// first, keyset-paged by (ledger, tx_hash, op_index, event_index). It is the Postgres recent tail
// of GET /v1/accounts/{g}/movements (ADR-0048 D5), merged in explorer/movements.go with
// ClickHouse's stellar.account_movements; see SEP41MovementsFloorLedger for non-overlap.
//
// Scope: event_kind = 'transfer' and ledger >= SEP41MovementsFloorLedger (below P23 a classic
// transfer is already in account_movements; a pure Soroban-native one is a documented gap).
//
// direction, when non-empty, is "sent"/"received"/"self" relative to `address`. contractID, when
// non-empty, filters every arm BEFORE the LIMIT so an ?asset= page is filled from matching rows.

// sep41TransfersByAddressQuery assembles the UNION arm set for one
// direction filter (see ListSEP41TransfersByAddress's shape comment).
func sep41TransfersByAddressQuery(direction, filterClause, orderBy string) (string, error) {
	const armCols = `
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, event_kind,
            from_addr, to_addr,
            amount::text AS amount, live_until_ledger, authorized`
	fromArm := `(SELECT` + armCols + `
        FROM sep41_transfers
        WHERE event_kind = 'transfer' AND ledger >= $1
          AND from_addr = $2 AND (to_addr IS DISTINCT FROM $2)` + filterClause + orderBy + `)`
	toArm := `(SELECT` + armCols + `
        FROM sep41_transfers
        WHERE event_kind = 'transfer' AND ledger >= $1
          AND to_addr = $2 AND (from_addr IS DISTINCT FROM $2)` + filterClause + orderBy + `)`
	selfArm := `(SELECT` + armCols + `
        FROM sep41_transfers
        WHERE event_kind = 'transfer' AND ledger >= $1
          AND from_addr = $2 AND to_addr = $2` + filterClause + orderBy + `)`

	var arms []string
	switch direction {
	case "sent":
		arms = []string{fromArm}
	case "received":
		arms = []string{toArm}
	case "self":
		arms = []string{selfArm}
	case "":
		arms = []string{fromArm, toArm, selfArm}
	default:
		return "", fmt.Errorf("timescale: ListSEP41TransfersByAddress: invalid direction %q", direction)
	}
	return `
        SELECT ledger_close_time, ledger, tx_hash, op_index, event_index,
               contract_id, event_kind, from_addr, to_addr,
               amount, live_until_ledger, authorized
        FROM (` + strings.Join(arms, " UNION ALL ") + `) u` + orderBy, nil
}

// sep41ByAddressArgs builds the positional args every arm shares ($1 floor, $2 address, optional contract/cursor binds, LIMIT)
// and the per-arm filter clause that references them.
func sep41ByAddressArgs(floorLedger uint32, address, contractID string, cur SEP41TransferCursor, limit int) (args []any, filterClause, limitPh string) {
	args = []any{int64(floorLedger), address}
	if contractID != "" {
		args = append(args, contractID)
		filterClause = fmt.Sprintf(" AND contract_id = $%d", len(args))
	}
	if cur.IsSet() {
		n := len(args)
		args = append(args, int64(cur.Ledger), cur.TxHash, cursorIndex16(cur.OpIndex), cursorIndex16(cur.EventIndex))
		filterClause += fmt.Sprintf(" AND (ledger, tx_hash, op_index, event_index) < ($%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4)
	}
	args = append(args, limit)
	return args, filterClause, fmt.Sprintf("$%d", len(args))
}

//nolint:gocognit // linear: arm-select + cursor build + null-projecting row-scan loop, same shape as ListSEP41Transfers.
func (s *Store) ListSEP41TransfersByAddress(ctx context.Context, address string, limit int, cur SEP41TransferCursor, direction, contractID string, floorLedger uint32) ([]SEP41TransferRow, error) {
	if address == "" {
		return nil, errors.New("timescale: ListSEP41TransfersByAddress: empty address")
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
	}

	// QUERY SHAPE: two index-friendly arms UNION'd, not `from_addr = $2 OR to_addr = $2` (the OR
	// problem account_trades.go documents). The OR cannot ride either address-leading partial
	// index in output order, so on COMPRESSED chunks (no btrees) the planner decompress-scans
	// every chunk and hits the statement timeout. Each arm walks its own partial index
	// (from_addr/to_addr, ledger DESC) and stops after one page; the outer merge picks the page.
	// Direction filters collapse to arm selection: sent = from-arm, received = to-arm, self = one
	// from-arm with to = from.
	//
	// floorLedger is the DYNAMIC lower bound (>=): the cap67 movements watermark + 1 once the
	// lake-derived archive is following, since the archive serves everything at/below the
	// watermark for ALL assets. Never below the P23 boundary: the classic_derived archive owns
	// everything before it.
	if fl := MovementsFloor(); floorLedger < fl {
		floorLedger = fl
	}
	args, filterClause, limitPh := sep41ByAddressArgs(floorLedger, address, contractID, cur, limit)
	orderBy := " ORDER BY ledger DESC, tx_hash DESC, op_index DESC, event_index DESC LIMIT " + limitPh

	q, qerr := sep41TransfersByAddressQuery(direction, filterClause, orderBy)
	if qerr != nil {
		return nil, qerr
	}
	var sb strings.Builder
	sb.WriteString(q)

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
