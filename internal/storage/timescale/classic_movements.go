package timescale

import (
	"context"
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

	const q = `
        INSERT INTO classic_movements (
            movement_kind, provenance, ledger, ledger_close_time, tx_hash,
            op_index, leg_index, asset, amount, from_address, to_address
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
        )
        ON CONFLICT (ledger_close_time, ledger, tx_hash, op_index, leg_index) DO NOTHING
    `
	_, err := s.db.ExecContext(ctx, q,
		string(m.Kind), string(provenance), int(m.Ledger), m.LedgerCloseTime.UTC(), m.TxHash,
		int(m.OpIndex), int(m.LegIndex), m.Asset, m.Amount,
		nullString(m.FromAddress), nullString(m.ToAddress),
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

	const colsPerRow = 11
	args := make([]any, 0, len(rows)*colsPerRow)
	valuesParts := make([]string, 0, len(rows))
	for i, m := range rows {
		base := i*colsPerRow + 1
		valuesParts = append(valuesParts, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10,
		))
		args = append(args,
			string(m.Kind), string(m.Provenance), int(m.Ledger), m.LedgerCloseTime.UTC(), m.TxHash,
			int(m.OpIndex), int(m.LegIndex), m.Asset, m.Amount,
			nullString(m.FromAddress), nullString(m.ToAddress),
		)
	}

	//nolint:gosec // G201: VALUES placeholders constructed only from compile-time format string.
	query := fmt.Sprintf(`
        WITH ins AS (
            INSERT INTO classic_movements (
                movement_kind, provenance, ledger, ledger_close_time, tx_hash,
                op_index, leg_index, asset, amount, from_address, to_address
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
