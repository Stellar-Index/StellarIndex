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

// SEP41EventKind discriminates the three supply-affecting SEP-41
// event variants. Stable string values matching the migration's
// CHECK constraint and `internal/canonical/discovery`'s symbol
// names.
type SEP41EventKind string

const (
	SEP41EventMint     SEP41EventKind = "mint"
	SEP41EventBurn     SEP41EventKind = "burn"
	SEP41EventClawback SEP41EventKind = "clawback"
)

// IsValid reports whether k is one of the three supply-affecting
// kinds. Caller surfaces unknown kinds as a 400 / decode error
// (the observer in PR 2/4 enforces this at Decode time).
func (k SEP41EventKind) IsValid() bool {
	switch k {
	case SEP41EventMint, SEP41EventBurn, SEP41EventClawback:
		return true
	}
	return false
}

// SEP41SupplyEvent is one mint / burn / clawback event row.
// Mirrors the sep41_supply_events columns.
type SEP41SupplyEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the contract event's index within its operation —
	// the per-event discriminator added to the PK by migration 0057
	// (F-1324) so multiple supply events from one op don't collide.
	EventIndex   uint32
	ObservedAt   time.Time
	Kind         SEP41EventKind
	Amount       *big.Int
	Counterparty string // empty when not present (reserved for future variants)
}

// InsertSEP41SupplyEvent appends one event row, idempotent on the
// (contract_id, ledger, tx_hash, op_index, observed_at, event_kind,
// event_index) PK (event_index + event_kind added by migration 0057,
// F-1324, so multiple supply events from one op don't collide).
// Re-running the indexer over the same range writes the same
// rows; ON CONFLICT DO NOTHING keeps the running sum
// monotonically correct across replays.
//
// Defensive: rejects empty ContractID / TxHash / nil Amount /
// invalid Kind before touching the DB.
func (s *Store) InsertSEP41SupplyEvent(ctx context.Context, e SEP41SupplyEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertSEP41SupplyEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertSEP41SupplyEvent: TxHash is empty")
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent: invalid Kind %q (want mint/burn/clawback)", e.Kind)
	}
	if e.Amount == nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent: Amount is nil (contract=%s tx=%s)", e.ContractID, e.TxHash)
	}
	if e.Amount.Sign() < 0 {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent: negative Amount %s (event amounts are non-negative; kind discriminates direction)", e.Amount)
	}
	const q = `
        INSERT INTO sep41_supply_events (
            contract_id, ledger, tx_hash, op_index, observed_at,
            event_kind, event_index, amount, counterparty
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9
        )
        ON CONFLICT (contract_id, ledger, tx_hash, op_index, observed_at,
                     event_kind, event_index) DO NOTHING
    `
	var counterparty sql.NullString
	if e.Counterparty != "" {
		counterparty = sql.NullString{String: e.Counterparty, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.TxHash, int(e.OpIndex), e.ObservedAt.UTC(),
		string(e.Kind), int(e.EventIndex), e.Amount.String(), counterparty,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// SEP41NetMintAtOrBefore returns the running supply sum for
// `contractID` at-or-before `asOfLedger`:
//
//	Σ amount where event_kind='mint'
//	  − Σ amount where event_kind IN ('burn', 'clawback')
//
// per ADR-0011 Algorithm 3. Returns a non-nil *big.Int (zero is
// the answer for a contract with no supply-affecting events
// observed yet) on success.
func (s *Store) SEP41NetMintAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (*big.Int, error) {
	const q = `
        SELECT COALESCE(
            sum(CASE WHEN event_kind = 'mint' THEN amount ELSE -amount END),
            0
        )::text
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      <= $2
    `
	var raw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(asOfLedger)).Scan(&raw); err != nil {
		return nil, fmt.Errorf("timescale: SEP41NetMintAtOrBefore %s@%d: %w", contractID, asOfLedger, err)
	}
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: SEP41NetMintAtOrBefore: parse %q", raw)
	}
	return v, nil
}

// SEP41KindTotals carries the per-kind running sums for one
// SEP-41 contract. Used by the storage reader to surface the
// algorithm's three components separately rather than as the
// pre-netted [SEP41NetMintAtOrBefore]: ADR-0011 Algorithm 3's
// SEP41SupplyComponents tracks them distinct (operators want to
// see clawback volume separately from voluntary burns for
// compliance dashboards).
type SEP41KindTotals struct {
	Mint     *big.Int
	Burn     *big.Int
	Clawback *big.Int
}

// SEP41KindTotalsAtOrBefore returns the per-kind sums for
// `contractID` at-or-before `asOfLedger`. Single round-trip via
// SQL `SUM(...) FILTER (WHERE ...)` per kind.
//
// Each component is non-nil; zero is a valid answer for a
// contract with no events of that kind observed yet (e.g. a
// token that's never been clawed back returns Clawback=0).
func (s *Store) SEP41KindTotalsAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (SEP41KindTotals, error) {
	const q = `
        SELECT
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'mint'),     0)::text AS mint_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'burn'),     0)::text AS burn_total,
            COALESCE(sum(amount) FILTER (WHERE event_kind = 'clawback'), 0)::text AS clawback_total
          FROM sep41_supply_events
         WHERE contract_id = $1
           AND ledger      <= $2
    `
	var mintRaw, burnRaw, clawbackRaw string
	if err := s.db.QueryRowContext(ctx, q, contractID, int(asOfLedger)).Scan(&mintRaw, &burnRaw, &clawbackRaw); err != nil {
		return SEP41KindTotals{}, fmt.Errorf("timescale: SEP41KindTotalsAtOrBefore %s@%d: %w", contractID, asOfLedger, err)
	}
	mint, err := parseSEP41Numeric(mintRaw, "mint_total")
	if err != nil {
		return SEP41KindTotals{}, err
	}
	burn, err := parseSEP41Numeric(burnRaw, "burn_total")
	if err != nil {
		return SEP41KindTotals{}, err
	}
	clawback, err := parseSEP41Numeric(clawbackRaw, "clawback_total")
	if err != nil {
		return SEP41KindTotals{}, err
	}
	return SEP41KindTotals{Mint: mint, Burn: burn, Clawback: clawback}, nil
}

// MinSEP41ComponentLedger returns MAX(ledger) of sep41_supply_events
// rows for `contractID` at-or-before `asOfLedger`. There's only one
// component-feeding table for SEP-41 supply (event sums), so the
// "min across components" semantics reduce to just the table's max.
// Zero when the contract has no events yet — gate-skip signal.
// F-1236 (codex audit-2026-05-12).
func (s *Store) MinSEP41ComponentLedger(ctx context.Context, contractID string, asOfLedger uint32) (uint32, error) {
	const q = `
		SELECT COALESCE(MAX(ledger), 0)
		  FROM sep41_supply_events
		 WHERE contract_id = $1 AND ledger <= $2
	`
	var ledger uint32
	if err := s.db.QueryRowContext(ctx, q, contractID, int(asOfLedger)).Scan(&ledger); err != nil {
		return 0, fmt.Errorf("timescale: MinSEP41ComponentLedger %s@%d: %w", contractID, asOfLedger, err)
	}
	return ledger, nil
}

func parseSEP41Numeric(raw, label string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("timescale: parse %s %q", label, raw)
	}
	return v, nil
}

// InsertSEP41SupplyEventBatch persists rows via a single multi-row
// INSERT .. ON CONFLICT DO NOTHING (the batch sibling of
// InsertSEP41SupplyEvent — added for the 2026-07-05 full-history
// re-derive, where per-row round-trips capped writes at ~520/s).
// Rows are validated with the same rules as the single-row path.
func (s *Store) InsertSEP41SupplyEventBatch(ctx context.Context, rows []SEP41SupplyEvent) error {
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		e := &rows[i]
		if e.ContractID == "" {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d empty ContractID", i)
		}
		if e.TxHash == "" {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d empty TxHash", i)
		}
		if e.Amount == nil {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d nil Amount", i)
		}
		if !e.Kind.IsValid() {
			return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch: row %d invalid Kind %q", i, e.Kind)
		}
	}

	const ncols = 9
	var sb strings.Builder
	sb.WriteString(`
        INSERT INTO sep41_supply_events (
            contract_id, ledger, tx_hash, op_index, event_index,
            observed_at, event_kind, amount, counterparty
        ) VALUES `)
	args := make([]any, 0, ncols*len(rows))
	for i := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * ncols
		fmt.Fprintf(&sb,
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5,
			base+6, base+7, base+8, base+9,
		)
		e := &rows[i]
		args = append(args,
			e.ContractID,
			int64(e.Ledger),
			e.TxHash,
			int16(e.OpIndex),
			int16(e.EventIndex),
			e.ObservedAt.UTC(),
			string(e.Kind),
			e.Amount.String(),
			sql.NullString{String: e.Counterparty, Valid: e.Counterparty != ""},
		)
	}
	sb.WriteString(` ON CONFLICT (contract_id, ledger, tx_hash, op_index, observed_at, event_kind, event_index) DO NOTHING`)

	if _, err := s.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("timescale: InsertSEP41SupplyEventBatch (%d rows): %w", len(rows), err)
	}
	return nil
}
