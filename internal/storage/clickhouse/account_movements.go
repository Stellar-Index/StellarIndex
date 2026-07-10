package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// AccountMovementDirection discriminates which side of a two-party
// classic-asset movement one stellar.account_movements row represents
// (ADR-0048 D2).
type AccountMovementDirection string

const (
	AccountMovementSent     AccountMovementDirection = "sent"
	AccountMovementReceived AccountMovementDirection = "received"
	AccountMovementSelf     AccountMovementDirection = "self"
)

// AccountMovement is the storage-local, PRE-FAN-OUT shape of one
// ADR-0047-reconstructed classic movement. It deliberately mirrors
// classicmovements.Movement's fields rather than importing that type:
// internal/storage/ sits BELOW internal/sources/ in the repo's import
// direction (scripts/ci/lint-imports.sh's L/storage-below-compute
// rule forbids new storage->sources edges), the same reason
// timescale.ClassicMovementRow (the table this replaces, ADR-0048)
// doesn't import classicmovements either. The caller
// (stellarindex-ops classic-movements-backfill) converts.
type AccountMovement struct {
	MovementKind    string
	Provenance      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	LegIndex        uint32
	Asset           string
	Amount          *big.Int

	// FromAddress/ToAddress: "" means "not a real G-account for this
	// leg" (a claimable balance's escrow, a liquidity pool), NOT
	// "unknown" — every classicmovements decode path either resolves
	// a side or leaves it empty on purpose. See FanOutAccountMovement.
	FromAddress string
	ToAddress   string

	// Attributes is the kind-specific remainder, written straight to
	// migration 0105's `attributes` shape (as a JSON string here) —
	// same convention as timescale.ClassicMovementRow.Attributes.
	Attributes map[string]any
}

// AccountMovementRow is one stellar.account_movements row — the
// feed-shaped, per-participant fan-out of an AccountMovement
// (ADR-0048 D2). Address is the row's OWN participant; Counterparty
// is the other side, when known.
type AccountMovementRow struct {
	Address         string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	LegIndex        uint32
	Direction       AccountMovementDirection
	MovementKind    string
	Provenance      string
	Asset           string
	Counterparty    string
	Amount          *big.Int
	Attributes      map[string]any
}

// FanOutAccountMovement expands one reconstructed movement into its
// stellar.account_movements row(s) — ADR-0048 D2's "two rows per
// movement, one per participant, direction discriminator" rule, with
// two documented exceptions driven directly by
// internal/sources/classicmovements' decode semantics:
//
//   - FromAddress == ToAddress (both non-empty): a degenerate
//     self-payment (Stellar allows a payment whose destination is its
//     own source). ONE row, direction=self — a sent+received pair
//     would otherwise be two rows IDENTICAL except for `direction`
//     for the very same address, which is redundant and would force
//     `direction` into the ORDER BY purely to avoid a false PK
//     collision between them.
//   - Exactly one of FromAddress/ToAddress is non-empty: the other
//     side isn't a real G-account for this leg (a claimable balance's
//     escrow at creation/claim/clawback time, or a liquidity pool's
//     leg). ONE row for the known side, Counterparty="". This is the
//     "acting side" rule (ADR-0048 D2 / the classic-movements-backfill
//     task doc): claimable_balance_create emits one 'sent' row for the
//     creator; claimable_balance_claim/clawback and each LP-withdraw
//     leg emit one 'received' row for the account; each LP-deposit leg
//     emits one 'sent' row for the depositor.
//
// Neither side known (both empty) is a defensive no-op (nil, zero
// rows) — every real classicmovements decode path populates at least
// one side; a caller hitting this indicates a decode-layer bug worth
// logging, not a legitimate zero-participant movement.
func FanOutAccountMovement(m AccountMovement) []AccountMovementRow {
	base := AccountMovementRow{
		Ledger:          m.Ledger,
		LedgerCloseTime: m.LedgerCloseTime,
		TxHash:          m.TxHash,
		OpIndex:         m.OpIndex,
		LegIndex:        m.LegIndex,
		MovementKind:    m.MovementKind,
		Provenance:      m.Provenance,
		Asset:           m.Asset,
		Amount:          m.Amount,
		Attributes:      m.Attributes,
	}
	switch {
	case m.FromAddress != "" && m.ToAddress != "" && m.FromAddress == m.ToAddress:
		row := base
		row.Address = m.FromAddress
		row.Direction = AccountMovementSelf
		return []AccountMovementRow{row}
	case m.FromAddress != "" && m.ToAddress != "":
		sent := base
		sent.Address = m.FromAddress
		sent.Direction = AccountMovementSent
		sent.Counterparty = m.ToAddress
		received := base
		received.Address = m.ToAddress
		received.Direction = AccountMovementReceived
		received.Counterparty = m.FromAddress
		return []AccountMovementRow{sent, received}
	case m.FromAddress != "":
		row := base
		row.Address = m.FromAddress
		row.Direction = AccountMovementSent
		return []AccountMovementRow{row}
	case m.ToAddress != "":
		row := base
		row.Address = m.ToAddress
		row.Direction = AccountMovementReceived
		return []AccountMovementRow{row}
	default:
		return nil
	}
}

// accountMovementsDDL is the canonical stellar.account_movements
// definition, kept in sync with deploy/clickhouse/tier1_schema.sql
// (that file's copy is the one applied to r1 —
// `clickhouse-client < deploy/clickhouse/tier1_schema.sql`, an
// operator step, see docs/operations/self-hosting.md §4.5 — and the
// one the integration-test harness loads). This Go-side copy exists
// so EnsureAccountMovementsTable can defensively create the table on
// a fresh/older ClickHouse before the first backfill write, the same
// belt-and-suspenders pattern supply_flows.go uses.
const accountMovementsDDL = `
	CREATE TABLE IF NOT EXISTS stellar.account_movements (
		address           String,
		ledger            UInt32,
		ledger_close_time DateTime64(0, 'UTC'),
		tx_hash           String,
		op_index          UInt32,
		leg_index         UInt32,
		direction         LowCardinality(String),
		movement_kind     LowCardinality(String),
		provenance        LowCardinality(String),
		asset             String,
		counterparty      String DEFAULT '',
		amount            Int128,
		attributes        String DEFAULT '{}',
		ingested_at       DateTime DEFAULT now()
	) ENGINE = ReplacingMergeTree(ingested_at)
	PARTITION BY intDiv(ledger, 1000000)
	ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction)`

// EnsureAccountMovementsTable creates stellar.account_movements if
// absent. Idempotent; classic-movements-backfill calls it at startup
// so the write path never races a missing table on a freshly deployed
// ClickHouse that hasn't had tier1_schema.sql re-applied yet.
func EnsureAccountMovementsTable(ctx context.Context, addr string) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec(ctx, accountMovementsDDL); err != nil {
		return fmt.Errorf("clickhouse: ensure account_movements: %w", err)
	}
	return nil
}

// accountMovementsInsertChunk is how many ROWS (post-fan-out, not
// input movements — most movements fan out to 2 rows) accumulate
// before one native INSERT batch is sent. A caller may hand
// InsertAccountMovements an entire backfill window's decoded movement
// set (the task's "fat batches, >=10k rows" target); chunking here
// bounds any single INSERT payload independently of how large a
// window's total row count grows.
const accountMovementsInsertChunk = 20_000

// InsertAccountMovements fans out + batch-inserts movements into
// stellar.account_movements. Retry-safe: ReplacingMergeTree absorbs a
// duplicate re-send of an already-written window (the same idempotent
// re-derivation guarantee as every other ADR-0034 lake/serving
// writer) — a caller that fails partway through a multi-chunk send
// can simply retry the whole batch. Returns the number of ROWS sent
// (not deduped — unlike Postgres's ON CONFLICT ... RETURNING, a
// ClickHouse INSERT doesn't observe how many rows survive merge-time
// dedup; "landed" isn't directly measurable here the way
// BatchInsertClassicMovements' return value was).
func InsertAccountMovements(ctx context.Context, addr string, movements []AccountMovement) (int64, error) {
	if len(movements) == 0 {
		return 0, nil
	}
	var rows []AccountMovementRow
	for _, m := range movements {
		rows = append(rows, FanOutAccountMovement(m)...)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// Deterministic order (matches the table's own ORDER BY key) —
	// unlike the retired Postgres writer this isn't for row-lock
	// ordering (ClickHouse inserts don't take row locks), just for
	// reproducible batches in tests and logs.
	sortAccountMovementRows(rows)

	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	var written int64
	for i := 0; i < len(rows); i += accountMovementsInsertChunk {
		end := i + accountMovementsInsertChunk
		if end > len(rows) {
			end = len(rows)
		}
		if err := insertAccountMovementChunk(ctx, conn, rows[i:end]); err != nil {
			return written, fmt.Errorf("clickhouse: InsertAccountMovements: chunk [%d,%d): %w", i, end, err)
		}
		written += int64(end - i)
	}
	return written, nil
}

// insertAccountMovementChunk sends one native batch (<=
// accountMovementsInsertChunk rows).
func insertAccountMovementChunk(ctx context.Context, conn driver.Conn, rows []AccountMovementRow) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.account_movements
		(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		 movement_kind, provenance, asset, counterparty, amount, attributes)`)
	if err != nil {
		return fmt.Errorf("prepare account_movements batch: %w", err)
	}
	for _, r := range rows {
		amt := r.Amount
		if amt == nil {
			amt = big.NewInt(0)
		}
		attrs, aerr := marshalAccountMovementAttributes(r.Attributes)
		if aerr != nil {
			return fmt.Errorf("marshal attributes %s/%s/%d/%d: %w", r.Address, r.TxHash, r.OpIndex, r.LegIndex, aerr)
		}
		if err := batch.Append(
			r.Address, r.Ledger, r.LedgerCloseTime, r.TxHash, r.OpIndex, r.LegIndex, string(r.Direction),
			r.MovementKind, r.Provenance, r.Asset, r.Counterparty, amt, attrs,
		); err != nil {
			return fmt.Errorf("append %s/%s/%d/%d/%s: %w", r.Address, r.TxHash, r.OpIndex, r.LegIndex, r.Direction, err)
		}
	}
	return wrapSend(batch.Send(), "account_movements")
}

// marshalAccountMovementAttributes renders Attributes as the string
// to pass through to the driver — '{}' for nil/empty (matching the
// column DEFAULT), json.Marshal's output otherwise. Same convention
// as timescale's marshalClassicMovementAttributes.
func marshalAccountMovementAttributes(attrs map[string]any) (string, error) {
	if len(attrs) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return "", fmt.Errorf("marshal attributes: %w", err)
	}
	return string(b), nil
}

// sortAccountMovementRows sorts by the table's exact ORDER BY key
// (address, ledger, tx_hash, op_index, leg_index, direction).
func sortAccountMovementRows(rows []AccountMovementRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		if a.Ledger != b.Ledger {
			return a.Ledger < b.Ledger
		}
		if a.TxHash != b.TxHash {
			return a.TxHash < b.TxHash
		}
		if a.OpIndex != b.OpIndex {
			return a.OpIndex < b.OpIndex
		}
		if a.LegIndex != b.LegIndex {
			return a.LegIndex < b.LegIndex
		}
		return a.Direction < b.Direction
	})
}

// openAccountMovementsWrite dials ClickHouse for
// InsertAccountMovements' batch INSERTs — the cheap-append write
// class (a finite execution ceiling), same shape as
// openParticipantWrite, kept as its own opener per this package's
// per-writer-file convention (participant_backfill.go, sink.go each
// define their own).
func openAccountMovementsWrite(ctx context.Context, addr string) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
		Settings: clickhouse.Settings{
			"max_execution_time": 300,
		},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open write %s: %w", addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping write %s: %w", addr, err)
	}
	return conn, nil
}

// MaxAccountMovementLedger returns the highest ledger already present
// in stellar.account_movements within [from,to] inclusive — the
// ClickHouse-native resume point classic-movements-backfill uses in
// place of a Postgres-persisted cursor (ADR-0048 D2: "no Postgres in
// the loop"). found=false when nothing has been written in-range yet.
//
// No FINAL: max() over duplicate ReplacingMergeTree parts is correct
// without dedup (a duplicate row shares the same ledger value), so
// this stays a cheap, un-deduped read even over a large window.
func MaxAccountMovementLedger(ctx context.Context, addr string, from, to uint32) (ledger uint32, found bool, err error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()
	var cnt, hi uint64
	if err := conn.QueryRow(ctx,
		`SELECT toUInt64(count()), toUInt64(max(ledger)) FROM stellar.account_movements WHERE ledger BETWEEN ? AND ?`,
		from, to).Scan(&cnt, &hi); err != nil {
		return 0, false, fmt.Errorf("clickhouse: max account_movements ledger [%d,%d]: %w", from, to, err)
	}
	if cnt == 0 {
		return 0, false, nil
	}
	return uint32(hi), true, nil
}

// FindClaimableBalanceCreate looks up a previously-written
// claimable_balance_create movement's asset/amount/creator by its
// balance_id — the ADR-0048 D2 ClickHouse-native replacement for the
// retired Postgres timescale.Store.FindClaimableBalanceCreate lookup
// (ADR-0047 Phase 3's cross-window correlation fallback tier; see
// classicmovements/dispatcher_adapter.go's Decoder doc for the full
// three-tier resolution: in-run index, this lookup, then unresolved).
//
// found=false, err=nil means no matching create row exists YET for
// this balance_id in what's been backfilled to ClickHouse so far — a
// genuine ADR-0047 D4 recognizable-incompleteness signal (the create
// may be outside the range backfilled so far, or — rarely —
// same-ledger ordering noise), never a query failure. Callers must
// count + log this, never guess an amount.
//
// No index backs the JSON attributes' balance_id — this scans the
// claimable_balance_create subset of stellar.account_movements (a
// LowCardinality equality prunes columns but not primary-key
// granules, since movement_kind isn't in ORDER BY). Same "acceptable
// for a rare fallback path" caveat as the retired Postgres version
// (see its doc comment for the identical reasoning); if this becomes
// a hot path in production (a WARNING-level unresolved-count spike),
// a dedicated balance_id column + bloom skip-index is the fix, not
// pre-optimizing here.
func FindClaimableBalanceCreate(ctx context.Context, addr, balanceIDHex string) (asset string, amount *big.Int, createdBy string, found bool, err error) {
	if balanceIDHex == "" {
		return "", nil, "", false, fmt.Errorf("clickhouse: FindClaimableBalanceCreate: balanceIDHex is empty")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return "", nil, "", false, err
	}
	defer func() { _ = conn.Close() }()

	const q = `
		SELECT asset, amount, address
		FROM stellar.account_movements
		WHERE movement_kind = 'claimable_balance_create'
		  AND JSONExtractString(attributes, 'balance_id') = ?
		LIMIT 1`
	rows, qerr := conn.Query(ctx, q, balanceIDHex)
	if qerr != nil {
		return "", nil, "", false, fmt.Errorf("clickhouse: FindClaimableBalanceCreate(%s): %w", balanceIDHex, qerr)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", nil, "", false, rows.Err()
	}
	var amt *big.Int
	if err := rows.Scan(&asset, &amt, &createdBy); err != nil {
		return "", nil, "", false, fmt.Errorf("clickhouse: scan FindClaimableBalanceCreate(%s): %w", balanceIDHex, err)
	}
	if amt == nil {
		amt = big.NewInt(0)
	}
	return asset, amt, createdBy, true, nil
}

// AccountMovementVerifyCounts maps movement_kind -> the number of
// DISTINCT movements (not rows — a two-participant movement is 2 rows
// sharing one (tx_hash, op_index, leg_index) identity) currently in
// stellar.account_movements for a ledger window.
type AccountMovementVerifyCounts map[string]uint64

// VerifyAccountMovementsWindow recounts [from,to] from
// stellar.account_movements, grouped by movement_kind, collapsing
// each movement's 1-2 fan-out rows back to one count via
// uniqExact(tx_hash, op_index, leg_index) — tx_hash is unique
// network-wide (stellar.tx_hash_index's ORDER BY tx_hash precedent),
// so this needs no `ledger` in the tuple beyond the WHERE-scope.
//
// This is classic-movements-backfill's -verify mode: a cheap,
// window-scoped reconciliation of "ops decoded this run" against
// "movements now visible in ClickHouse" (ADR-0047 D4 applied to the
// CH write target) — NOT the full ADR-0033 substrate/recognition/
// projection machinery, which doesn't apply to a historical-only,
// non-projected write path like this one.
//
// No FINAL: uniqExact over duplicate ReplacingMergeTree parts is
// still exact (an identical duplicate row is the identical tuple, so
// it doesn't inflate the distinct count) — the same reasoning
// StreamClassicOps' NO-FINAL note documents for this table family.
func VerifyAccountMovementsWindow(ctx context.Context, addr string, from, to uint32) (AccountMovementVerifyCounts, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, `
		SELECT movement_kind, uniqExact(tx_hash, op_index, leg_index) AS n
		FROM stellar.account_movements
		WHERE ledger BETWEEN ? AND ?
		GROUP BY movement_kind`, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: verify account_movements window [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	out := AccountMovementVerifyCounts{}
	for rows.Next() {
		var kind string
		var n uint64
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, fmt.Errorf("clickhouse: scan verify row [%d,%d]: %w", from, to, err)
		}
		out[kind] = n
	}
	return out, rows.Err()
}
