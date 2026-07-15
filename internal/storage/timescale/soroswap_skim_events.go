package timescale

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SoroswapSkimEvent is one soroswap_skim_events row — a single
// observed Soroswap pair-contract `skim` event (migration 0042).
//
// Amount0 / Amount1 are decimal-string i128 values per ADR-0003;
// stored verbatim into the NUMERIC columns by the postgres driver.
// To is the strkey of the optional recipient (empty string → SQL
// NULL, matching the today-Soroswap WASM that omits a `to` field;
// see decode.SkimFields godoc).
type SoroswapSkimEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          []byte // 32-byte raw hash; hex strings auto-decoded
	OpIndex         int16
	EventIndex      int16
	To              string // "" → NULL
	Amount0         string // decimal i128
	Amount1         string // decimal i128
}

// InsertSoroswapSkimEvent appends one Soroswap skim event row,
// idempotent on the (ledger_close_time, ledger, tx_hash, op_index,
// event_index) PK. Re-running the indexer or a backfill over the
// same range writes the same rows; ON CONFLICT DO NOTHING makes the
// replay a no-op.
//
// Defensive: rejects empty ContractID / TxHash / Amount0 / Amount1
// and a zero LedgerCloseTime (the partition column — NULL/zero would
// route the row to a chunk Timescale won't open). The amount columns
// are NOT NULL in the migration; passing an empty string here is a
// caller bug, not a missing-field case.
//
// TxHash auto-decodes a 64-char hex string for callers that still
// hand the row through with the hex form — every other Soroban
// table accepts raw bytes, but Soroswap's TradeEvent / SkimEvent
// keep TxHash as a string field, so we centralise the parse here.
func (s *Store) InsertSoroswapSkimEvent(ctx context.Context, e SoroswapSkimEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertSoroswapSkimEvent: ContractID is empty")
	}
	if len(e.TxHash) == 0 {
		return errors.New("timescale: InsertSoroswapSkimEvent: TxHash is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertSoroswapSkimEvent: zero LedgerCloseTime (contract=%s ledger=%d)", e.ContractID, e.Ledger)
	}
	if e.Amount0 == "" {
		return fmt.Errorf("timescale: InsertSoroswapSkimEvent: Amount0 is empty (contract=%s ledger=%d)", e.ContractID, e.Ledger)
	}
	if e.Amount1 == "" {
		return fmt.Errorf("timescale: InsertSoroswapSkimEvent: Amount1 is empty (contract=%s ledger=%d)", e.ContractID, e.Ledger)
	}

	const q = `
        INSERT INTO soroswap_skim_events (
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, to_address, amount_0, amount_1
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9
        )
        ON CONFLICT (ledger_close_time, ledger, tx_hash, op_index, event_index) DO NOTHING
    `

	var toAddr sql.NullString
	if e.To != "" {
		toAddr = sql.NullString{String: e.To, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		e.LedgerCloseTime.UTC(), int(e.Ledger), e.TxHash, e.OpIndex, e.EventIndex,
		e.ContractID, toAddr, e.Amount0, e.Amount1,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSoroswapSkimEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// SoroswapSkimSummary is the windowed Soroswap skim activity summary — the
// READ side of soroswap_skim_events (migration 0043). Amount0 / Amount1 are
// the summed token0 / token1 excess claimed above pool reserves, in native
// token base units (i128/NUMERIC, never int64 — ADR-0003).
type SoroswapSkimSummary struct {
	Skims    int64
	Amount0  canonical.Amount
	Amount1  canonical.Amount
	Pairs    int64
	LatestAt time.Time
}

// SoroswapSkimWindowStats reads the windowed Soroswap skim summary (count +
// token0/token1 skimmed excess + distinct pairs) from soroswap_skim_events.
// Skim is the caller-initiated claim of pool balance above recorded reserves
// (rare — single-digit rows/day expected); it is not a trade and never feeds
// VWAP. Amounts are native token base units, preserved as canonical.Amount
// (ADR-0003).
//
// Empty-safe: returns (nil, nil) when no skim exists in the window, so the
// bespoke KPI is omitted cleanly. windowDays <= 0 is treated as 90.
func (s *Store) SoroswapSkimWindowStats(ctx context.Context, windowDays int) (*SoroswapSkimSummary, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	var (
		out    SoroswapSkimSummary
		latest sql.NullTime
	)
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*),
		       COALESCE(sum(amount_0),0)::text,
		       COALESCE(sum(amount_1),0)::text,
		       count(DISTINCT contract_id),
		       max(ledger_close_time)
		  FROM soroswap_skim_events WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&out.Skims, &out.Amount0, &out.Amount1, &out.Pairs, &latest); err != nil {
		return nil, fmt.Errorf("timescale: SoroswapSkimWindowStats: %w", err)
	}
	if out.Skims == 0 {
		return nil, nil
	}
	if latest.Valid {
		out.LatestAt = latest.Time.UTC()
	}
	return &out, nil
}

// DecodeSoroswapTxHash maps the SkimEvent.TxHash string (which comes
// from the events.Event JSON shape — 64-char lowercase hex) to the
// raw 32-byte form the bytea column accepts. Exposed for callers
// (pipeline sink + integration tests) that own the projection from
// the SkimEvent value into a SoroswapSkimEvent row.
//
// Accepts the raw 32-byte form unchanged (some test fixtures hand a
// `[]byte` directly). Accepts a 64-character hex string as the
// production wire form. Rejects everything else.
func DecodeSoroswapTxHash(s string) ([]byte, error) {
	if len(s) == 32 {
		// Raw bytes round-trip — accepted for symmetry with
		// soroban_events which stores TxHash bytea unconditionally.
		return []byte(s), nil
	}
	if len(s) != 64 {
		return nil, fmt.Errorf("timescale: tx_hash must be 32 raw bytes or 64-char hex; got len=%d", len(s))
	}
	b, err := hex.DecodeString(strings.ToLower(s))
	if err != nil {
		return nil, fmt.Errorf("timescale: tx_hash hex decode: %w", err)
	}
	return b, nil
}
