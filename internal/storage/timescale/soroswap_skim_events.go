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

// SoroswapSkimEvent is one soroswap_skim_events row: a single Soroswap pair-contract `skim` event.
//
// Amount0 / Amount1 are decimal-string i128 values (ADR-0003) stored verbatim into NUMERIC. To is
// the optional recipient strkey; empty means SQL NULL, since the current Soroswap WASM omits it.
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

// InsertSoroswapSkimEvent appends one skim event row, idempotent on the (ledger_close_time,
// ledger, tx_hash, op_index, event_index) PK. ON CONFLICT DO UPDATE guarded by derive_generation
// corrects the row on a higher-or-equal-generation replay and no-ops a stale lower one.
//
// It rejects empty ContractID / TxHash / Amount0 / Amount1 (the amount columns are NOT NULL, so
// empty is a caller bug) and a zero LedgerCloseTime, the partition column, which would route the
// row to a chunk Timescale will not open.
//
// TxHash auto-decodes a 64-char hex string because Soroswap events keep TxHash as a string while
// every other Soroban table takes raw bytes.
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

	// Generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the skimmed amounts (amount_0 / amount_1) lands
	// in place when its generation is >= the stored one; a live gen-0 replay
	// can never revert it.
	const q = `
        INSERT INTO soroswap_skim_events (
            ledger_close_time, ledger, tx_hash, op_index, event_index,
            contract_id, to_address, amount_0, amount_1,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9,
            $10
        )
        ON CONFLICT (ledger_close_time, ledger, tx_hash, op_index, event_index) DO UPDATE SET
            contract_id       = EXCLUDED.contract_id,
            to_address        = EXCLUDED.to_address,
            amount_0          = EXCLUDED.amount_0,
            amount_1          = EXCLUDED.amount_1,
            derive_generation = EXCLUDED.derive_generation
          WHERE soroswap_skim_events.derive_generation <= EXCLUDED.derive_generation
    `

	var toAddr sql.NullString
	if e.To != "" {
		toAddr = sql.NullString{String: e.To, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		e.LedgerCloseTime.UTC(), int(e.Ledger), e.TxHash, e.OpIndex, e.EventIndex,
		e.ContractID, toAddr, e.Amount0, e.Amount1,
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSoroswapSkimEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// SoroswapSkimSummary is the windowed skim activity summary over soroswap_skim_events. Skims and
// Pairs are window totals; amounts exist only per pair in ByPair, because token0 and token1 differ
// in token and decimals and a cross-pair sum would add unlike base units.
type SoroswapSkimSummary struct {
	Skims    int64
	Pairs    int64
	ByPair   []SoroswapSkimPairTotal
	LatestAt time.Time
}

// SoroswapSkimPairTotal is one pair's windowed skim total, in that pair's
// native token0 / token1 base units (i128/NUMERIC, never int64 — ADR-0003).
// Token0 / Token1 are "" when the pair is not yet in soroswap_pairs.
type SoroswapSkimPairTotal struct {
	ContractID string
	Token0     string
	Token1     string
	Skims      int64
	Amount0    canonical.Amount
	Amount1    canonical.Amount
}

// SoroswapSkimWindowStats reads the windowed skim summary (count, distinct pairs, per-pair
// token0/token1 excess), ByPair ordered by skim count descending. A skim claims pool balance above
// recorded reserves; it is not a trade and never feeds VWAP.
//
// Returns (nil, nil) when no skim exists in the window so the KPI is omitted. windowDays <= 0
// means 90.
func (s *Store) SoroswapSkimWindowStats(ctx context.Context, windowDays int) (*SoroswapSkimSummary, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	rows, err := s.db.QueryContext(ctx, `
		SELECT e.contract_id,
		       COALESCE(p.token0_strkey, ''),
		       COALESCE(p.token1_strkey, ''),
		       count(*),
		       COALESCE(sum(e.amount_0),0)::text,
		       COALESCE(sum(e.amount_1),0)::text,
		       max(e.ledger_close_time)
		  FROM soroswap_skim_events e
		  LEFT JOIN soroswap_pairs p ON p.pair_strkey = e.contract_id
		 WHERE e.ledger_close_time > now() - $1::interval
		 GROUP BY e.contract_id, p.token0_strkey, p.token1_strkey
		 ORDER BY count(*) DESC, e.contract_id`, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: SoroswapSkimWindowStats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out SoroswapSkimSummary
	for rows.Next() {
		var (
			pt     SoroswapSkimPairTotal
			latest sql.NullTime
		)
		if err := rows.Scan(&pt.ContractID, &pt.Token0, &pt.Token1, &pt.Skims, &pt.Amount0, &pt.Amount1, &latest); err != nil {
			return nil, fmt.Errorf("timescale: SoroswapSkimWindowStats scan: %w", err)
		}
		out.ByPair = append(out.ByPair, pt)
		out.Skims += pt.Skims
		if latest.Valid && latest.Time.After(out.LatestAt) {
			out.LatestAt = latest.Time.UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: SoroswapSkimWindowStats rows: %w", err)
	}
	if out.Skims == 0 {
		return nil, nil
	}
	out.Pairs = int64(len(out.ByPair))
	return &out, nil
}

// DecodeSoroswapTxHash maps SkimEvent.TxHash (64-char lowercase hex on the wire) to the raw 32
// bytes the bytea column accepts. It also passes a raw 32-byte value through unchanged (test
// fixtures) and rejects everything else.
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
