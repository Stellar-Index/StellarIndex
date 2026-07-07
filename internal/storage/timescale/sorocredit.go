package timescale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// This file is the served-tier writer for the sorocredit source (an
// unbranded consumer-USDC credit / CDP protocol; see
// internal/sources/sorocredit). Four hypertables, migration 0090:
//
//	credit_positions    ← NewCollateralContract  (one row per opened position)
//	credit_statements   ← StatementPublished     (periodic per-position statement)
//	credit_settlements  ← "Liquidation"          (SCHEDULED settlement — NOT distress)
//	credit_events       ← Withdrawal + config    (event_type-discriminated)
//
// All amounts are decimal-i128 strings handed verbatim to NUMERIC
// columns (ADR-0003 — never int64). The projector (ADR-0031/0032) is the
// sole writer; the pipeline sink converts sorocredit.Event → these
// storage structs (row types live HERE, not imported from the source
// package, so storage keeps its no-upward-import boundary — the cctp /
// rozo pattern).

// CreditPosition is one credit_positions row — a position opened by a
// NewCollateralContract event. CollateralContract is the per-user
// Collateral-<uuid> child contract this event deploys.
type CreditPosition struct {
	CollateralContract string
	PositionUUID       string
	PositionName       string
	Owner              string
	Ledger             uint32
	LedgerCloseTime    time.Time
	TxHash             string
	OpIndex            int
	EventIndex         int
}

// CreditStatement is one credit_statements row — a periodic per-position
// statement (StatementPublished). Amount is a decimal i128 string.
type CreditStatement struct {
	StatementUUID      string
	PositionUUID       string
	CollateralContract string
	Amount             string
	StatementTime      time.Time
	Ledger             uint32
	LedgerCloseTime    time.Time
	TxHash             string
	OpIndex            int
	EventIndex         int
}

// CreditSettlement is one credit_settlements row — a SCHEDULED settlement
// (decoded from the on-wire "Liquidation" event; NOT a distressed
// liquidation, see internal/sources/sorocredit). DebtAsset / SettledAmount
// are the primary (USDC) leg; empty → SQL NULL. Attributes holds the full
// event body.
type CreditSettlement struct {
	CollateralContract string
	PositionUUID       string
	StatementUUID      string
	SettlerAccount     string
	DebtAsset          string // "" → NULL
	SettledAmount      string // decimal i128; "" → NULL
	Attributes         map[string]any
	Ledger             uint32
	LedgerCloseTime    time.Time
	TxHash             string
	OpIndex            int
	EventIndex         int
}

// CreditEvent is one credit_events row — a Withdrawal or config event,
// discriminated by EventType. Promoted columns vary by type ("" → NULL).
type CreditEvent struct {
	EventType          string
	CollateralContract string // "" → NULL
	Asset              string // "" → NULL
	Account            string // "" → NULL
	Amount             string // decimal i128; "" → NULL
	Attributes         map[string]any
	Ledger             uint32
	LedgerCloseTime    time.Time
	TxHash             string
	OpIndex            int
	EventIndex         int
}

// creditAttrs marshals a sorocredit event Attributes map to jsonb,
// defaulting to an empty object.
func creditAttrs(attrs map[string]any) ([]byte, error) {
	if len(attrs) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("timescale: marshal sorocredit attributes: %w", err)
	}
	return b, nil
}

// InsertCreditPosition appends one opened-position row. Idempotent on the PK.
func (s *Store) InsertCreditPosition(ctx context.Context, e CreditPosition) error {
	if e.CollateralContract == "" {
		return errors.New("timescale: InsertCreditPosition: CollateralContract is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertCreditPosition: TxHash is empty")
	}
	const q = `
        INSERT INTO credit_positions (
            collateral_contract, position_uuid, position_name, owner,
            ledger, ledger_close_time, tx_hash, op_index, event_index
        ) VALUES (
            $1, $2, $3, $4,
            $5, $6, $7, $8, $9
        )
        ON CONFLICT (ledger_close_time, collateral_contract, ledger, tx_hash, op_index, event_index) DO NOTHING
    `
	_, err := s.db.ExecContext(ctx, q,
		e.CollateralContract, e.PositionUUID, e.PositionName, e.Owner,
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, e.OpIndex, e.EventIndex,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertCreditPosition %s@%d: %w", e.CollateralContract, e.Ledger, err)
	}
	return nil
}

// InsertCreditStatement appends one published-statement row. Idempotent on the PK.
func (s *Store) InsertCreditStatement(ctx context.Context, e CreditStatement) error {
	if e.StatementUUID == "" {
		return errors.New("timescale: InsertCreditStatement: StatementUUID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertCreditStatement: TxHash is empty")
	}
	const q = `
        INSERT INTO credit_statements (
            statement_uuid, position_uuid, collateral_contract,
            amount, statement_time,
            ledger, ledger_close_time, tx_hash, op_index, event_index
        ) VALUES (
            $1, $2, $3,
            $4::numeric, $5,
            $6, $7, $8, $9, $10
        )
        ON CONFLICT (ledger_close_time, statement_uuid, ledger, tx_hash, op_index, event_index) DO NOTHING
    `
	_, err := s.db.ExecContext(ctx, q,
		e.StatementUUID, e.PositionUUID, e.CollateralContract,
		nullNumeric(e.Amount), e.StatementTime.UTC(),
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, e.OpIndex, e.EventIndex,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertCreditStatement %s@%d: %w", e.StatementUUID, e.Ledger, err)
	}
	return nil
}

// InsertCreditSettlement appends one SCHEDULED-SETTLEMENT row (decoded
// from the on-wire "Liquidation" event — recurring keeper settlement, NOT
// a distressed liquidation). Idempotent on the PK.
func (s *Store) InsertCreditSettlement(ctx context.Context, e CreditSettlement) error {
	if e.CollateralContract == "" {
		return errors.New("timescale: InsertCreditSettlement: CollateralContract is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertCreditSettlement: TxHash is empty")
	}
	attrs, err := creditAttrs(e.Attributes)
	if err != nil {
		return err
	}
	const q = `
        INSERT INTO credit_settlements (
            collateral_contract, position_uuid, statement_uuid,
            settler_account, debt_asset, settled_amount, attributes,
            ledger, ledger_close_time, tx_hash, op_index, event_index
        ) VALUES (
            $1, $2, $3,
            $4, $5, $6::numeric, $7,
            $8, $9, $10, $11, $12
        )
        ON CONFLICT (ledger_close_time, position_uuid, statement_uuid, ledger, tx_hash, op_index, event_index) DO NOTHING
    `
	_, err = s.db.ExecContext(ctx, q,
		e.CollateralContract, e.PositionUUID, e.StatementUUID,
		e.SettlerAccount, nullString(e.DebtAsset), nullNumeric(e.SettledAmount), attrs,
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, e.OpIndex, e.EventIndex,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertCreditSettlement %s@%d: %w", e.PositionUUID, e.Ledger, err)
	}
	return nil
}

// InsertCreditEvent appends one Withdrawal / config event row into the
// catch-all credit_events table, discriminated by EventType. Idempotent
// on the PK.
func (s *Store) InsertCreditEvent(ctx context.Context, e CreditEvent) error {
	if e.EventType == "" {
		return errors.New("timescale: InsertCreditEvent: EventType is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertCreditEvent: TxHash is empty")
	}
	attrs, err := creditAttrs(e.Attributes)
	if err != nil {
		return err
	}
	const q = `
        INSERT INTO credit_events (
            event_type, collateral_contract, asset, account, amount, attributes,
            ledger, ledger_close_time, tx_hash, op_index, event_index
        ) VALUES (
            $1, $2, $3, $4, $5::numeric, $6,
            $7, $8, $9, $10, $11
        )
        ON CONFLICT (ledger_close_time, event_type, ledger, tx_hash, op_index, event_index) DO NOTHING
    `
	_, err = s.db.ExecContext(ctx, q,
		e.EventType, nullString(e.CollateralContract), nullString(e.Asset),
		nullString(e.Account), nullNumeric(e.Amount), attrs,
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, e.OpIndex, e.EventIndex,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertCreditEvent %s@%d: %w", e.EventType, e.Ledger, err)
	}
	return nil
}
