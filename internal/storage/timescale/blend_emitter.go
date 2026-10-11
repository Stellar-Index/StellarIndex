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
// String values match the blend_emitter_events.event_kind CHECK
// constraint (migration 0095) and the Event* constants in
// internal/sources/blend_emitter/events.go.
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

// BlendEmitterDistributeEvent is one observed `distribute` event —
// one BLND emission to a backstop pool.
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

// BlendEmitterRecipient is one (address, amount) pair from a `drop`
// event's variable-length recipient list.
type BlendEmitterRecipient struct {
	Address string
	Amount  canonical.Amount
}

// BlendEmitterDropEvent is one observed `drop` event. The writer fans Recipients out to one row
// each, with a 0-based recipient_index (slice order) keeping the rows distinct.
type BlendEmitterDropEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	Recipients      []BlendEmitterRecipient
}

// BlendEmitterSwapConfigEvent is one observed `q_swap` / `swap`
// event — the Emitter queuing or executing a change of which
// backstop (and backstop token) it targets.
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

// insertBlendEmitterEventQuery is shared by every writer; the three event kinds differ only in
// which columns are NULL.
//
// Generation-guarded corrective upsert: a corrected re-derive lands in place when its generation
// is >= the stored one, so a live gen-0 replay never reverts it. The drop fan-out issues one Exec
// per recipient with a distinct recipient_index (a conflict-key part), so no intra-batch dedup.
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

// blendEmitterExecer is satisfied by both *sql.DB and *sql.Tx, so the
// single-row writers and InsertBlendEmitterDrop's per-recipient fan-out
// loop can share one INSERT path.
type blendEmitterExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// execBlendEmitterRow issues one blend_emitter_events INSERT.
// recipientIndex is always 0 for distribute/q_swap/swap; the drop
// fan-out loop passes 0..N-1.
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

// InsertBlendEmitterDistribute appends one `distribute` row, idempotent on the table PK. A replay
// rewrites the row only when the incoming derive_generation is >= the stored one, so a stale
// replay cannot revert a later correction.
//
// It rejects empty ContractID/TxHash/BackstopID and a non-positive Amount before touching the DB;
// the decoder enforces these too, but the writer double-checks so malformed input cannot land.
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

// InsertBlendEmitterDrop appends one `drop` event as one row per recipient (recipient_index =
// slice position). A single transaction prevents a partial fan-out; each row is idempotent under
// the same generation-guarded upsert as InsertBlendEmitterDistribute.
//
// It rejects empty ContractID/TxHash, an empty recipient list, and per-recipient empty addresses
// or non-positive amounts.
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

// InsertBlendEmitterSwapConfig appends one `q_swap` or `swap` row, with the same PK and
// generation-guarded upsert as the other writers.
//
// It rejects empty ContractID/TxHash/NewBackstop/NewBackstopToken and any other Kind. An
// UnlockTime that is zero or outside the timestamptz range writes SQL NULL rather than erroring.
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
