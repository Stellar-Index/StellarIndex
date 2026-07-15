package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

// CreditAnalyticsSummary is the windowed sorocredit activity summary the
// /v1/protocols/sorocredit bespoke block is built from — the READ side of
// the four credit_* tables the projector fills. All volume fields are
// i128/NUMERIC (canonical.Amount, never int64 — ADR-0003); counts are
// exact row / distinct-key tallies.
//
// SettlementVolume is the summed SCHEDULED-settlement amount (the on-wire
// "Liquidation" event — a recurring keeper settlement, NOT distress; see
// InsertCreditSettlement and migration 0090). It must NEVER be surfaced as
// a liquidation / risk signal.
type CreditAnalyticsSummary struct {
	// PositionsOpened is the count of NewCollateralContract events in the
	// window (one child Collateral-<uuid> contract per opened position).
	PositionsOpened int64
	// OpenPositions is the count of positions opened in the window whose
	// collateral child has no observed withdrawal (cash-out) in the window
	// — a window-scoped proxy for still-open positions, not an all-time
	// live-position count (the served tier is retention-scoped).
	OpenPositions int64
	// UniqueUsers is the count of distinct position owners (G-addresses) in
	// the window.
	UniqueUsers int64

	Statements      int64
	StatementVolume canonical.Amount

	// Settlements + SettlementVolume are SCHEDULED settlements — recurring
	// keeper settlements of published statements, NOT distressed
	// liquidations. Label accordingly wherever surfaced.
	Settlements      int64
	SettlementVolume canonical.Amount

	Withdrawals      int64
	WithdrawalVolume canonical.Amount

	// LatestActivity is the most recent ledger_close_time across all four
	// credit_* tables in the window (a freshness signal).
	LatestActivity time.Time
}

// HasActivity reports whether any credit_* table had a row in the window —
// the gate the bespoke block uses to omit an all-empty panel.
func (a *CreditAnalyticsSummary) HasActivity() bool {
	return a != nil && (a.PositionsOpened > 0 || a.Statements > 0 ||
		a.Settlements > 0 || a.Withdrawals > 0)
}

// CreditWindowAnalytics reads the windowed sorocredit activity summary
// (positions / statements / SCHEDULED settlements / withdrawals) from the
// four credit_* hypertables. Volumes are i128/NUMERIC preserved as
// canonical.Amount (never int64 — ADR-0003).
//
// Empty-safe: returns (nil, nil) when no credit_* row exists in the window,
// so the bespoke block omits the panel cleanly (r1's credit_* tables are
// empty until the sorocredit projector-replay runs post-deploy).
// windowDays <= 0 is treated as 90.
func (s *Store) CreditWindowAnalytics(ctx context.Context, windowDays int) (*CreditAnalyticsSummary, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	var (
		out          CreditAnalyticsSummary
		posLatest    sql.NullTime
		stmtLatest   sql.NullTime
		settleLatest sql.NullTime
		wdLatest     sql.NullTime
	)

	// Positions + open-position proxy + distinct owners. The NOT EXISTS is
	// window-scoped on both sides so it stays an index-only probe of
	// credit_events (credit_events_collateral_ts_idx) and the semantics are
	// honestly "opened AND not withdrawn within the window".
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE NOT EXISTS (
		           SELECT 1 FROM credit_events e
		            WHERE e.event_type = 'withdrawal'
		              AND e.collateral_contract = p.collateral_contract
		              AND e.ledger_close_time > now() - $1::interval)),
		       count(DISTINCT owner),
		       max(ledger_close_time)
		  FROM credit_positions p
		 WHERE p.ledger_close_time > now() - $1::interval`, since).
		Scan(&out.PositionsOpened, &out.OpenPositions, &out.UniqueUsers, &posLatest); err != nil {
		return nil, fmt.Errorf("timescale: CreditWindowAnalytics positions: %w", err)
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(amount),0)::text, max(ledger_close_time)
		  FROM credit_statements WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&out.Statements, &out.StatementVolume, &stmtLatest); err != nil {
		return nil, fmt.Errorf("timescale: CreditWindowAnalytics statements: %w", err)
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(settled_amount),0)::text, max(ledger_close_time)
		  FROM credit_settlements WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&out.Settlements, &out.SettlementVolume, &settleLatest); err != nil {
		return nil, fmt.Errorf("timescale: CreditWindowAnalytics settlements: %w", err)
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE event_type = 'withdrawal'),
		       COALESCE(sum(amount) FILTER (WHERE event_type = 'withdrawal'),0)::text,
		       max(ledger_close_time) FILTER (WHERE event_type = 'withdrawal')
		  FROM credit_events WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&out.Withdrawals, &out.WithdrawalVolume, &wdLatest); err != nil {
		return nil, fmt.Errorf("timescale: CreditWindowAnalytics withdrawals: %w", err)
	}

	for _, t := range []sql.NullTime{posLatest, stmtLatest, settleLatest, wdLatest} {
		if t.Valid && t.Time.After(out.LatestActivity) {
			out.LatestActivity = t.Time.UTC()
		}
	}

	if !out.HasActivity() {
		return nil, nil
	}
	return &out, nil
}
