package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// BlendEmitterKind discriminates the four Blend Emitter events.
type BlendEmitterKind string

const (
	BlendEmitterDistribute BlendEmitterKind = "distribute"
	BlendEmitterDrop       BlendEmitterKind = "drop"
	BlendEmitterQSwap      BlendEmitterKind = "q_swap"
	BlendEmitterSwap       BlendEmitterKind = "swap"
)

// IsValid reports whether k is one of the four known kinds.
func (k BlendEmitterKind) IsValid() bool {
	switch k {
	case BlendEmitterDistribute, BlendEmitterDrop, BlendEmitterQSwap, BlendEmitterSwap:
		return true
	}
	return false
}

// BlendEmitterDistributeEvent is one observed `distribute` event — one BLND emission to a backstop pool.
type BlendEmitterDistributeEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	BackstopID      string
	Amount          canonical.Amount
}

// BlendEmitterRecipient is one (address, amount) pair from a `drop` event's variable-length recipient list.
type BlendEmitterRecipient struct {
	Address string
	Amount  canonical.Amount
}

// BlendEmitterDropEvent is one observed `drop` event.
type BlendEmitterDropEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	Recipients      []BlendEmitterRecipient
}

// BlendEmitterSwapConfigEvent is one `q_swap` / `swap` event: the Emitter queuing or executing a change of its target backstop and backstop token.
type BlendEmitterSwapConfigEvent struct {
	ContractID       string
	Ledger           uint32
	LedgerCloseTime  time.Time
	TxHash           string
	OpIndex          uint32
	EventIndex       uint32
	Kind             BlendEmitterKind // BlendEmitterQSwap or BlendEmitterSwap
	NewBackstop      string
	NewBackstopToken string
	// UnlockTime is the backstop-swap timelock, converted from the
	// contract's u64 Unix-seconds field. Zero / out-of-postgres-range
	// values write SQL NULL rather than erroring (same posture as
	// soroswap_router_swaps.DeadlineTS).
	UnlockTime time.Time
}

// insertBlendEmitterEventQuery is shared by all writers (kinds differ only in NULL columns); a generation-guarded corrective upsert:
// re-derives land in place at generation >= stored, so a gen-0 replay never reverts them; recipient_index is part of the conflict key.
const insertBlendEmitterEventQuery = `
    INSERT INTO blend_emitter_events (
        contract_id, ledger, ledger_close_time, tx_hash, op_index,
        event_index, event_kind, recipient_index,
        backstop_id, recipient, amount,
        new_backstop, new_backstop_token, unlock_time,
        derive_generation
    ) VALUES (
        $1, $2, $3, $4, $5,
        $6, $7, $8,
        $9, $10, $11,
        $12, $13, $14,
        $15
    )
    ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, event_index, recipient_index) DO UPDATE SET
        backstop_id        = EXCLUDED.backstop_id,
        recipient          = EXCLUDED.recipient,
        amount             = EXCLUDED.amount,
        new_backstop       = EXCLUDED.new_backstop,
        new_backstop_token = EXCLUDED.new_backstop_token,
        unlock_time        = EXCLUDED.unlock_time,
        derive_generation  = EXCLUDED.derive_generation
      WHERE blend_emitter_events.derive_generation <= EXCLUDED.derive_generation
`

// blendEmitterExecer is satisfied by *sql.DB and *sql.Tx so single-row writers and the drop fan-out share one INSERT path.
type blendEmitterExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// execBlendEmitterRow issues one blend_emitter_events INSERT; recipientIndex is 0 except in the drop fan-out (0..N-1).
func execBlendEmitterRow(
	ctx context.Context, x blendEmitterExecer,
	contractID string, ledger uint32, closeTime time.Time, txHash string, opIndex, eventIndex uint32,
	kind BlendEmitterKind, recipientIndex int,
	backstopID, recipient string, amount sql.NullString,
	newBackstop, newBackstopToken string, unlockTime sql.NullTime,
	deriveGeneration int64,
) error {
	if _, err := x.ExecContext(ctx, insertBlendEmitterEventQuery,
		contractID, int(ledger), closeTime.UTC(), txHash, int(opIndex),
		int(eventIndex), string(kind), recipientIndex,
		nullString(backstopID), nullString(recipient), amount,
		nullString(newBackstop), nullString(newBackstopToken), unlockTime,
		deriveGeneration,
	); err != nil {
		return fmt.Errorf("timescale: InsertBlendEmitterEvent %s@%d kind=%s: %w", contractID, ledger, kind, err)
	}
	return nil
}

// InsertBlendEmitterDistribute appends one `distribute` row, idempotent on the PK; a replay rewrites only when derive_generation >= stored, and empty ContractID/TxHash/BackstopID or a non-positive Amount is rejected before the DB.
func (s *Store) InsertBlendEmitterDistribute(ctx context.Context, e BlendEmitterDistributeEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertBlendEmitterDistribute: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertBlendEmitterDistribute: TxHash is empty")
	}
	if e.BackstopID == "" {
		return errors.New("timescale: InsertBlendEmitterDistribute: BackstopID is empty")
	}
	if e.Amount.Sign() <= 0 {
		return fmt.Errorf("timescale: InsertBlendEmitterDistribute: Amount must be > 0 (got %s)", e.Amount)
	}
	return execBlendEmitterRow(ctx, s.db,
		e.ContractID, e.Ledger, e.LedgerCloseTime, e.TxHash, e.OpIndex, e.EventIndex,
		BlendEmitterDistribute, 0,
		e.BackstopID, "", sql.NullString{String: e.Amount.String(), Valid: true},
		"", "", sql.NullTime{},
		s.deriveGeneration,
	)
}

// InsertBlendEmitterDrop appends one `drop` event as one row per recipient (recipient_index = slice position).
func (s *Store) InsertBlendEmitterDrop(ctx context.Context, e BlendEmitterDropEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertBlendEmitterDrop: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertBlendEmitterDrop: TxHash is empty")
	}
	if len(e.Recipients) == 0 {
		return errors.New("timescale: InsertBlendEmitterDrop: no recipients")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertBlendEmitterDrop begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, r := range e.Recipients {
		if r.Address == "" {
			return fmt.Errorf("timescale: InsertBlendEmitterDrop: recipient[%d] address is empty", i)
		}
		if r.Amount.Sign() <= 0 {
			return fmt.Errorf("timescale: InsertBlendEmitterDrop: recipient[%d] amount must be > 0 (got %s)", i, r.Amount)
		}
		if err := execBlendEmitterRow(ctx, tx,
			e.ContractID, e.Ledger, e.LedgerCloseTime, e.TxHash, e.OpIndex, e.EventIndex,
			BlendEmitterDrop, i,
			"", r.Address, sql.NullString{String: r.Amount.String(), Valid: true},
			"", "", sql.NullTime{},
			s.deriveGeneration,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertBlendEmitterDrop commit: %w", err)
	}
	return nil
}

// InsertBlendEmitterSwapConfig appends one `q_swap` or `swap` row, with the same PK and generation-guarded upsert as the other writers.
func (s *Store) InsertBlendEmitterSwapConfig(ctx context.Context, e BlendEmitterSwapConfigEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertBlendEmitterSwapConfig: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertBlendEmitterSwapConfig: TxHash is empty")
	}
	if e.Kind != BlendEmitterQSwap && e.Kind != BlendEmitterSwap {
		return fmt.Errorf("timescale: InsertBlendEmitterSwapConfig: invalid Kind %q", e.Kind)
	}
	if e.NewBackstop == "" {
		return errors.New("timescale: InsertBlendEmitterSwapConfig: NewBackstop is empty")
	}
	if e.NewBackstopToken == "" {
		return errors.New("timescale: InsertBlendEmitterSwapConfig: NewBackstopToken is empty")
	}

	var unlockTime sql.NullTime
	if !e.UnlockTime.IsZero() && pgTimestamptzRepresentable(e.UnlockTime) {
		unlockTime = sql.NullTime{Time: e.UnlockTime.UTC(), Valid: true}
	}

	return execBlendEmitterRow(ctx, s.db,
		e.ContractID, e.Ledger, e.LedgerCloseTime, e.TxHash, e.OpIndex, e.EventIndex,
		e.Kind, 0,
		"", "", sql.NullString{},
		e.NewBackstop, e.NewBackstopToken, unlockTime,
		s.deriveGeneration,
	)
}
