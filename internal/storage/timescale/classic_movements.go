// classic_movements.go: SUPERSEDED-BY-ADR-0048.
//
// ADR-0048 D2 (2026-07-10) amended ADR-0047 D1's storage target: the
// pre-P23 classic-movement archive is now ClickHouse-native
// (stellar.account_movements, internal/storage/clickhouse/
// account_movements.go), not this Postgres classic_movements table.
// Migration 0105 (which creates the table this file writes) stays
// APPLIED but intentionally UNPOPULATED — see migrations/README.md's
// 0105 row. `stellarindex-ops classic-movements-backfill` no longer
// calls anything in this file; it opens no Postgres connection at
// all (ADR-0048 D2: "no Postgres in the loop"). This file is kept
// in-tree, not deleted, pending a later cleanup migration that drops
// the table once the ClickHouse path is proven (ADR-0048 D2's own
// wording) — do NOT wire a new writer to it.
package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// ClassicMovementKind discriminates the ten classic-movement kinds
// (migration 0105). ADR-0047 Phase 1 (internal/sources/classicmovements)
// writes ClassicMovementPayment / ClassicMovementCreateAccount only;
// the other eight are admitted by the schema from day one so Phases
// 2-4 need no migration churn — see migration 0105's header.
//
// SUPERSEDED BY ADR-0048 (2026-07-10): see this file's package doc
// comment. Kept for reference / the eventual down-migration; no
// writer calls into this file anymore.
type ClassicMovementKind string

// The ten ADR-0047 D1 movement kinds, in the same order as migration
// 0103's movement_kind CHECK constraint.
const (
	ClassicMovementPayment                  ClassicMovementKind = "payment"
	ClassicMovementCreateAccount            ClassicMovementKind = "create_account"
	ClassicMovementPathPayment              ClassicMovementKind = "path_payment"
	ClassicMovementAccountMerge             ClassicMovementKind = "account_merge"
	ClassicMovementClawback                 ClassicMovementKind = "clawback"
	ClassicMovementClaimableBalanceCreate   ClassicMovementKind = "claimable_balance_create"
	ClassicMovementClaimableBalanceClaim    ClassicMovementKind = "claimable_balance_claim"
	ClassicMovementClaimableBalanceClawback ClassicMovementKind = "claimable_balance_clawback"
	ClassicMovementLiquidityPoolDeposit     ClassicMovementKind = "liquidity_pool_deposit"
	ClassicMovementLiquidityPoolWithdraw    ClassicMovementKind = "liquidity_pool_withdraw"
)

// IsValid reports whether k is one of the ten known movement kinds.
// Mirrors the CHECK constraint in migration 0105.
func (k ClassicMovementKind) IsValid() bool {
	switch k {
	case ClassicMovementPayment, ClassicMovementCreateAccount, ClassicMovementPathPayment,
		ClassicMovementAccountMerge, ClassicMovementClawback,
		ClassicMovementClaimableBalanceCreate, ClassicMovementClaimableBalanceClaim,
		ClassicMovementClaimableBalanceClawback,
		ClassicMovementLiquidityPoolDeposit, ClassicMovementLiquidityPoolWithdraw:
		return true
	}
	return false
}

// ClassicMovementProvenance discriminates how a classic_movements row
// was derived.
type ClassicMovementProvenance string

const (
	// ClassicMovementClassicDerived is every row this table has ever
	// held — reconstructed from the ClickHouse lake per ADR-0047.
	ClassicMovementClassicDerived ClassicMovementProvenance = "classic_derived"

	// ClassicMovementCAP67Event is RESERVED (ADR-0047 D1) for a
	// possible future normalization of post-P23 sep41_transfers rows
	// into this table. No writer emits it today.
	ClassicMovementCAP67Event ClassicMovementProvenance = "cap67_event"
)

// IsValid reports whether p is one of the two known provenance
// values. Mirrors the CHECK constraint in migration 0105.
func (p ClassicMovementProvenance) IsValid() bool {
	switch p {
	case ClassicMovementClassicDerived, ClassicMovementCAP67Event:
		return true
	}
	return false
}

// ClassicMovementRow is one classic_movements row (migration 0105).
type ClassicMovementRow struct {
	Kind            ClassicMovementKind
	Provenance      ClassicMovementProvenance
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	LegIndex        uint32
	Asset           string
	Amount          canonical.Amount
	FromAddress     string // "" -> NULL
	ToAddress       string // "" -> NULL

	// Attributes is the kind-specific jsonb remainder (migration
	// 0105's `attributes` column, DEFAULT '{}'::jsonb). nil/empty
	// marshals to '{}' — same convention as cctp_events.Attributes /
	// aquarius_rewards.Attributes.
	Attributes map[string]any
}

// InsertClassicMovement appends one row to classic_movements.
// Idempotent on the (ledger_close_time, ledger, tx_hash, op_index,
// leg_index) PK — re-running a backfill window is a no-op
// (ON CONFLICT DO NOTHING).
func (s *Store) InsertClassicMovement(ctx context.Context, m ClassicMovementRow) error {
	if err := validateClassicMovementRow(m); err != nil {
		return fmt.Errorf("timescale: InsertClassicMovement: %w", err)
	}
	provenance := m.Provenance
	if provenance == "" {
		provenance = ClassicMovementClassicDerived
	}
	attrs, aerr := marshalClassicMovementAttributes(m.Attributes)
	if aerr != nil {
		return fmt.Errorf("timescale: InsertClassicMovement: %w", aerr)
	}

	const q = `
        INSERT INTO classic_movements (
            movement_kind, provenance, ledger, ledger_close_time, tx_hash,
            op_index, leg_index, asset, amount, from_address, to_address,
            attributes
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
        )
        ON CONFLICT (ledger_close_time, ledger, tx_hash, op_index, leg_index) DO NOTHING
    `
	_, err := s.db.ExecContext(ctx, q,
		string(m.Kind), string(provenance), int(m.Ledger), m.LedgerCloseTime.UTC(), m.TxHash,
		int(m.OpIndex), int(m.LegIndex), m.Asset, m.Amount,
		nullString(m.FromAddress), nullString(m.ToAddress),
		attrs,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertClassicMovement %s@%d tx=%s op=%d: %w",
			m.Kind, m.Ledger, m.TxHash, m.OpIndex, err)
	}
	return nil
}

// BatchInsertClassicMovements multi-row inserts rows, ON CONFLICT DO
// NOTHING, and returns the count that actually landed (excludes
// duplicates from a re-run over an already-written window). Mirrors
// BatchInsertTrades' shape: one multi-row INSERT rather than N
// round-trips, with a deterministic pre-sort by the PK's conflict
// key so two concurrent batches (should this ever run with more than
// one writer) take row locks in the same order.
func (s *Store) BatchInsertClassicMovements(ctx context.Context, rows []ClassicMovementRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	for i := range rows {
		if err := validateClassicMovementRow(rows[i]); err != nil {
			return 0, fmt.Errorf("timescale: BatchInsertClassicMovements: row %d: %w", i, err)
		}
		if rows[i].Provenance == "" {
			rows[i].Provenance = ClassicMovementClassicDerived
		}
	}
	sortClassicMovementsByConflictKey(rows)

	const colsPerRow = 12
	args := make([]any, 0, len(rows)*colsPerRow)
	valuesParts := make([]string, 0, len(rows))
	for i, m := range rows {
		base := i*colsPerRow + 1
		valuesParts = append(valuesParts, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10, base+11,
		))
		attrs, aerr := marshalClassicMovementAttributes(m.Attributes)
		if aerr != nil {
			return 0, fmt.Errorf("timescale: BatchInsertClassicMovements: row %d: %w", i, aerr)
		}
		args = append(args,
			string(m.Kind), string(m.Provenance), int(m.Ledger), m.LedgerCloseTime.UTC(), m.TxHash,
			int(m.OpIndex), int(m.LegIndex), m.Asset, m.Amount,
			nullString(m.FromAddress), nullString(m.ToAddress),
			attrs,
		)
	}

	//nolint:gosec // G201: VALUES placeholders constructed only from compile-time format string.
	query := fmt.Sprintf(`
        WITH ins AS (
            INSERT INTO classic_movements (
                movement_kind, provenance, ledger, ledger_close_time, tx_hash,
                op_index, leg_index, asset, amount, from_address, to_address,
                attributes
            ) VALUES %s
            ON CONFLICT (ledger_close_time, ledger, tx_hash, op_index, leg_index) DO NOTHING
            RETURNING 1
        )
        SELECT count(*) FROM ins
    `, strings.Join(valuesParts, ", "))

	var landed int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&landed); err != nil {
		return 0, fmt.Errorf("timescale: BatchInsertClassicMovements: %w", err)
	}
	return landed, nil
}

// FindClaimableBalanceCreate looks up a previously-written
// 'claimable_balance_create' row's asset/amount/creator by its
// balance_id (matched against `attributes->>'balance_id'`) — ADR-0047
// Phase 3's second-pass correlation fallback for a
// classicmovements.Decoder whose in-run BalanceId index didn't have
// the create (see that package's Decoder doc: most commonly, the
// create landed in an earlier, already-completed backfill
// invocation over a different ledger range).
//
// found=false, err=nil means "no matching create row exists in
// classic_movements" — a genuine ADR-0047 D4 recognizable-
// incompleteness signal (the create may be outside the range this
// table has been backfilled over yet, or — rarely — same-ledger
// ordering noise per classicmovements/doc.go), NOT a query failure.
// Callers must count + log this, never guess an amount.
//
// No index currently backs `attributes->>'balance_id'` — this scans
// the claimable_balance_create subset of classic_movements (cheap
// relative to the whole table via the movement_kind check, but not
// O(1)). Acceptable for a rare fallback path; if it becomes a hot
// path in production (a WARNING-level unresolved-count spike), add a
// partial expression index in a follow-up migration rather than
// pre-optimizing here.
func (s *Store) FindClaimableBalanceCreate(ctx context.Context, balanceIDHex string) (asset string, amount canonical.Amount, createdBy string, found bool, err error) {
	if balanceIDHex == "" {
		return "", canonical.Amount{}, "", false, errors.New("timescale: FindClaimableBalanceCreate: balanceIDHex is empty")
	}
	const q = `
        SELECT asset, amount, COALESCE(from_address, '')
        FROM classic_movements
        WHERE movement_kind = 'claimable_balance_create'
          AND attributes ->> 'balance_id' = $1
        LIMIT 1
    `
	row := s.db.QueryRowContext(ctx, q, balanceIDHex)
	var amt canonical.Amount
	if scanErr := row.Scan(&asset, &amt, &createdBy); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return "", canonical.Amount{}, "", false, nil
		}
		return "", canonical.Amount{}, "", false, fmt.Errorf("timescale: FindClaimableBalanceCreate: %w", scanErr)
	}
	return asset, amt, createdBy, true, nil
}

// marshalClassicMovementAttributes renders m.Attributes as the jsonb
// bytes to pass through to the driver — '{}' for nil/empty (matching
// the column DEFAULT), json.Marshal's output otherwise. Same
// convention as cctp_events.go / aquarius_rewards.go.
func marshalClassicMovementAttributes(attrs map[string]any) ([]byte, error) {
	if len(attrs) == 0 {
		return []byte("{}"), nil
	}
	marshaled, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("marshal attributes: %w", err)
	}
	return marshaled, nil
}

// validateClassicMovementRow rejects rows that would violate
// migration 0105's CHECK constraints before they reach Postgres —
// cheap, and produces a much more actionable error than a generic
// pq CHECK-violation message.
func validateClassicMovementRow(m ClassicMovementRow) error {
	if !m.Kind.IsValid() {
		return fmt.Errorf("invalid Kind %q", m.Kind)
	}
	if m.Provenance != "" && !m.Provenance.IsValid() {
		return fmt.Errorf("invalid Provenance %q", m.Provenance)
	}
	if m.TxHash == "" {
		return errors.New("TxHash is empty")
	}
	if m.Asset == "" {
		return errors.New("asset is empty")
	}
	if m.Amount.Sign() < 0 {
		return fmt.Errorf("amount must be >= 0 (got %s)", m.Amount)
	}
	return nil
}

// sortClassicMovementsByConflictKey sorts by the exact PK conflict
// key (ledger_close_time, ledger, tx_hash, op_index, leg_index) —
// same deadlock-avoidance rationale as trades.go's
// sortTradesByConflictKey.
func sortClassicMovementsByConflictKey(rows []ClassicMovementRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if !a.LedgerCloseTime.Equal(b.LedgerCloseTime) {
			return a.LedgerCloseTime.Before(b.LedgerCloseTime)
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
		return a.LegIndex < b.LegIndex
	})
}
