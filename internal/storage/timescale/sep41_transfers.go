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
// INSERT. On the full PK the ON CONFLICT arm is DO UPDATE guarded by
// derive_generation (INV-3 / migration 0110): a higher-or-equal-
// generation replay corrects the row in place, a stale
// lower-generation one is a no-op.
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

	// INV-3 (migration 0110): the batch upsert below is now ON CONFLICT DO
	// UPDATE, and Postgres rejects a single statement that presents the same
	// conflict key twice ("cannot affect row a second time") — which the old
	// DO NOTHING silently absorbed. Collapse intra-batch conflict-key
	// duplicates (last-wins) before building the statement.
	insertRows := dedupeSEP41TransferRows(rows)

	const ncols = 13
	var sb strings.Builder
	// Generation-guarded corrective upsert: a re-derive with a higher-or-
	// equal generation UPDATEs the decoded value columns (amount, event_kind,
	// addresses, …) in place; a live gen-0 replay can never revert a
	// correction. Replaces the old DO NOTHING no-op.
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

// sep41TransferConflictKey is the sep41_transfers ON CONFLICT identity
// (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index).
// The timestamp is normalised to a UTC UnixNano so two instants that differ
// only in monotonic-clock reading / location still collapse to one key (they
// land in the same timestamptz).
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

// dedupeSEP41TransferRows collapses rows that collide on the sep41_transfers
// conflict key, keeping the LAST copy of each key (the latest redelivery) in
// first-seen order. Required because the INV-3 batch upsert (migration 0110)
// uses ON CONFLICT DO UPDATE, which Postgres rejects when one statement
// presents the same conflict key twice; the old DO NOTHING tolerated it.
//
// Copy-on-write: the common case (no intra-batch duplicate) returns the input
// slice untouched and allocates nothing.
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

// sep41TransferLookbackLadder is the trailing-window ladder
// [Store.ListSEP41Transfers] walks before it falls back to the
// full-history read. Each rung is a `ledger_close_time >= now-D` floor;
// the first rung that fills the caller's page wins.
//
// Why a ladder instead of one unbounded query: sep41_transfers has no
// index that yields a single contract's rows in ledger_close_time DESC
// order — sep41_transfers_contract_{from,to}_idx (migration 0047) put
// the address column between contract_id and ledger_close_time, so a
// contract-only predicate cannot take time order from them, and the
// primary key leads with ledger_close_time — so an unbounded read has to materialise and sort
// EVERY row a busy contract owns in the newest uncompressed chunk before
// the LIMIT can take five of them. On r1 that chunk holds a month of the
// CAP-67 firehose, and for the USDC SAC (CCW67TSZ…, the busiest token
// contract and the OpenAPI parameter example) the planner picks
// `Seq Scan + Sort` over ~17M estimated rows: GET
// /v1/contracts/CCW67TSZ…/transfers?limit=5 burnt the whole 8s handler
// budget and 503'd, while a quiet contract answered from
// sep41_transfers_contract_from_idx in 0.19s — cost inverted with how
// interesting the contract is.
//
// A floor inside the recent uncompressed data changes the plan to an
// index scan on the hypertable's ledger_close_time index under an
// Incremental Sort, so the LIMIT stops early (r1 EXPLAIN ANALYZE, same
// contract: 0.34ms for limit=100 over the 1h rung, 0.97ms for limit=500
// over the 7d one).
//
// The invariant the rungs keep is that they stay narrow enough for
// chunk exclusion to leave only the newest uncompressed chunk or
// chunks, and 7d is the widest window measured on r1 to still plan
// onto an index scan — 90d plans straight back to the per-chunk
// `Seq Scan + Sort`, so widening the ladder buys no deeper cheap read,
// it only reintroduces the timeout one rung later. The rungs do NOT
// depend on fitting inside one chunk: migration 0047 declares 1-day
// chunks and compression after 7 days, so a tree-built deployment
// serves the 7d rung from up to eight chunks, the oldest of which may
// already be compressed. r1's 30-day chunk width — which happens to put
// all three rungs inside a single chunk — is drift from that migration,
// not the design.
//
// The short-circuit is safe because the read is time-ordered: every row
// a rung's floor excludes is strictly older than every row it keeps, so
// a rung that returns a full page returned exactly the newest page.
//
// Cost: a rung that fills its page stops at the LIMIT and is cheap. A
// rung that CANNOT fill it is not — for a contract the chunk statistics
// still call busy, the planner walks the whole window of the time index
// filtering on contract_id, and on r1 that walk measured 36ms at 1h,
// 2.2s at 24h and past a 9s statement timeout at 7d. The ladder as a
// whole is therefore bounded by [SEP41TransferLadderBudget]; see
// [Store.walkSEP41TransferLadder].
var sep41TransferLookbackLadder = []time.Duration{
	time.Hour,
	24 * time.Hour,
	7 * 24 * time.Hour,
}

// SEP41TransferLadderBudget bounds the WHOLE lookback ladder — every
// rung of [sep41TransferLookbackLadder] together — so the ladder can
// never spend the caller's budget on the way to the fallback that has
// the rows. A rung still running when the budget runs out is abandoned,
// no wider rung is tried (a wider window is a superset scan of the one
// that just failed to finish), and the full-history read goes out on
// the caller's OWN remaining budget.
//
// Sized against the handler's 8s deadline
// (internal/api/v1/sep41_transfers.go, sep41TransfersReadTimeout — a
// test there pins this constant at no more than half of it) and the r1
// measurements in the ladder's doc comment: 3s lets the 1h and 24h
// rungs complete even under the pathological walk (36ms + 2.2s), cuts
// the 7d rung before it can reach the shape that ran past 9s, and
// leaves the fallback 5s — nearly four times the 1.3s a low-volume
// contract's fallback measured at limit=100 through
// sep41_transfers_contract_from_idx. The example contract fills its
// page at the first rung in 0.34ms, four orders of magnitude inside it.
const SEP41TransferLadderBudget = 3 * time.Second

// ListSEP41Transfers returns the most-recent N rows for a contract,
// optionally filtered by from_addr / to_addr. Powers GET
// /v1/contracts/{id}/transfers.
//
// Full history stays reachable: a contract whose page none of the
// [sep41TransferLookbackLadder] rungs can fill falls through to an
// unbounded read — the same query this method has always issued. That
// fallback is cheap for a genuinely low-volume contract, whose rows the
// planner reaches through sep41_transfers_contract_from_idx (1.3s at
// limit=100 on r1 for a contract with 21k rows in the newest chunk),
// but NOT for every contract that reaches it: a contract with a large
// history and fewer than `limit` rows in the widest rung still takes
// the same per-chunk sort that produced the timeout. The ladder
// mitigates the timeout class for contracts that are busy now; it does
// not eliminate it, and that residual class pays the ladder first, so
// its fallback runs on the caller's remaining time — at least 5s of the
// 8s it used to have. The root cause — no index yielding one contract's
// rows in ledger_close_time DESC order — is only removed by a
// (contract_id, ledger_close_time DESC) index together with an ORDER BY
// the compressed chunks can serve (the read also orders by op_index,
// which compress_orderby lacks, so a compressed chunk still sorts its
// whole segment); the index is heavy DDL on a
// hypertable of hundreds of millions of rows and belongs in its own
// migration with the by-hand CONCURRENTLY step r1 needs (migrations
// 0083 / 0106 set that convention).
func (s *Store) ListSEP41Transfers(ctx context.Context, contractID, fromAddr, toAddr string, limit int) ([]SEP41TransferRow, error) {
	return s.listSEP41TransfersAt(ctx, contractID, fromAddr, toAddr, limit, time.Now(), SEP41TransferLadderBudget)
}

// listSEP41TransfersAt is [Store.ListSEP41Transfers] with the wall-clock
// reference and the ladder budget threaded in so the window floors and
// the cost bound are deterministically testable.
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

// walkSEP41TransferLadder runs the rungs of [sep41TransferLookbackLadder]
// under one shared budget and reports (rows, true, nil) for the first
// rung that fills the page. (nil, false, nil) tells the caller to fall
// back to the unbounded read: either every rung came back short, or the
// budget ran out — a rung the database had not answered by then is
// abandoned, and no wider rung is tried. An error comes back only for a
// real storage failure or for the CALLER's own deadline or cancellation,
// which leaves nothing to fall back with.
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

// sep41TransfersQuery builds one audit-trail read AND its bound args
// together — one function, so the placeholder numbering can never drift
// from the arg order as the optional predicates come and go. A zero
// `since` builds the unbounded (full-history) form.
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

// listSEP41TransfersSince runs one rung of the lookback ladder (or, for
// a zero `since`, the unbounded fallback) and maps its rows.
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

// sep41TransfersByAddressQuery assembles the UNION arm set for one
// direction filter (see ListSEP41TransfersByAddress's shape comment).
func sep41TransfersByAddressQuery(direction, cursorClause, orderBy string) (string, error) {
	const armCols = `
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, event_kind,
            from_addr, to_addr,
            amount::text AS amount, live_until_ledger, authorized`
	fromArm := `(SELECT` + armCols + `
        FROM sep41_transfers
        WHERE event_kind = 'transfer' AND ledger >= $1
          AND from_addr = $2 AND (to_addr IS DISTINCT FROM $2)` + cursorClause + orderBy + `)`
	toArm := `(SELECT` + armCols + `
        FROM sep41_transfers
        WHERE event_kind = 'transfer' AND ledger >= $1
          AND to_addr = $2 AND (from_addr IS DISTINCT FROM $2)` + cursorClause + orderBy + `)`
	selfArm := `(SELECT` + armCols + `
        FROM sep41_transfers
        WHERE event_kind = 'transfer' AND ledger >= $1
          AND from_addr = $2 AND to_addr = $2` + cursorClause + orderBy + `)`

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

//nolint:gocognit // linear: arm-select + cursor build + null-projecting row-scan loop, same shape as ListSEP41Transfers.
func (s *Store) ListSEP41TransfersByAddress(ctx context.Context, address string, limit int, cur SEP41TransferCursor, direction string, floorLedger uint32) ([]SEP41TransferRow, error) {
	if address == "" {
		return nil, errors.New("timescale: ListSEP41TransfersByAddress: empty address")
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
	}

	// QUERY SHAPE (site audit 2026-08-08): two index-friendly arms
	// UNION'd, not `from_addr = $2 OR to_addr = $2` — the same OR
	// disease account_trades.go documents. The OR can't ride either
	// address-leading partial index in output order, so on the 30/32
	// COMPRESSED sep41_transfers chunks (no btrees) the planner
	// decompress-scanned every chunk, blew the statement timeout, and
	// the movements handler soft-failed the tail — which is why busy
	// accounts' /movements silently stopped at the P23 boundary. Each
	// arm walks its own partial index (from_addr/to_addr, ledger DESC)
	// newest-first and stops after one page; the outer merge picks the
	// page. Direction filters collapse to arm selection: sent = the
	// from-arm alone, received = the to-arm alone, self = one from-arm
	// with to = from.
	// floorLedger is the DYNAMIC lower bound (exclusive semantics via
	// >=): the cap67 movements watermark + 1 once the lake-derived
	// archive (inventory #1) is following — the archive serves
	// everything at/below the watermark for ALL assets, so this tail
	// only needs (watermark, tip]. Never below the P23 boundary: the
	// classic_derived archive owns everything before it.
	if fl := MovementsFloor(); floorLedger < fl {
		floorLedger = fl
	}
	cursorClause := ""
	args := []any{int64(floorLedger), address}
	if cur.IsSet() {
		args = append(args, int64(cur.Ledger), cur.TxHash, int16(cur.OpIndex), int16(cur.EventIndex))
		cursorClause = " AND (ledger, tx_hash, op_index, event_index) < ($3, $4, $5, $6)"
	}
	args = append(args, limit)
	limitPh := fmt.Sprintf("$%d", len(args))
	orderBy := " ORDER BY ledger DESC, tx_hash DESC, op_index DESC, event_index DESC LIMIT " + limitPh

	q, qerr := sep41TransfersByAddressQuery(direction, cursorClause, orderBy)
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
