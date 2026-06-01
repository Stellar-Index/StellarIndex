package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// SoroswapRouterSwap is one soroswap_router_swaps row — a single
// observed router invocation (one call to
// `swap_exact_tokens_for_tokens` / `swap_tokens_for_exact_tokens`).
// Mirrors migration 0049's columns; sister to canonical.Trade rows
// in the `trades` hypertable which hold the per-pair leg-level
// records emitted by the per-pair contracts the router walks.
//
// Identity per Stellar's per-op uniqueness: (ledger, tx_hash,
// op_index). Multiple router invocations in the same tx are
// theoretically possible (a contract calling the router twice
// inside one InvokeContract) but op_index disambiguates.
//
// AmountIn / AmountOut are decimal-string numerics (i128 →
// *big.Int → string per ADR-0003). Path is the hop sequence of
// raw token C-strkeys (≥ 2 by router precondition).
type SoroswapRouterSwap struct {
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32

	ContractID   string
	FunctionName string // 'swap_exact_tokens_for_tokens' | 'swap_tokens_for_exact_tokens'

	OpSource string // op source strkey (G… or muxed)
	TxSource string // tx source strkey

	Recipient string
	Path      []string

	AmountIn   string
	AmountOut  string
	DeadlineTS *time.Time
}

// InsertSoroswapRouterSwap appends one soroswap_router_swaps row,
// idempotent on (ledger_close_time, ledger, tx_hash, op_index).
// Defensive: rejects empty PK columns + empty function name + empty
// path before touching the DB.
func (s *Store) InsertSoroswapRouterSwap(ctx context.Context, e SoroswapRouterSwap) error {
	if e.TxHash == "" {
		return errors.New("timescale: InsertSoroswapRouterSwap: TxHash is empty")
	}
	if e.ContractID == "" {
		return errors.New("timescale: InsertSoroswapRouterSwap: ContractID is empty")
	}
	if e.FunctionName == "" {
		return errors.New("timescale: InsertSoroswapRouterSwap: FunctionName is empty")
	}
	if e.Recipient == "" {
		return errors.New("timescale: InsertSoroswapRouterSwap: Recipient is empty")
	}
	if len(e.Path) < 2 {
		return fmt.Errorf("timescale: InsertSoroswapRouterSwap: Path must have >= 2 hops, got %d", len(e.Path))
	}
	if e.AmountIn == "" || e.AmountOut == "" {
		return errors.New("timescale: InsertSoroswapRouterSwap: AmountIn/AmountOut required")
	}

	const q = `
        INSERT INTO soroswap_router_swaps (
            ledger, ledger_close_time, tx_hash, op_index,
            contract_id, function_name,
            op_source, tx_source,
            recipient, path,
            amount_in, amount_out, deadline_ts
        ) VALUES (
            $1, $2, $3, $4,
            $5, $6,
            $7, $8,
            $9, $10,
            $11, $12, $13
        )
        ON CONFLICT (ledger_close_time, ledger, tx_hash, op_index) DO NOTHING
    `
	var deadline interface{}
	if e.DeadlineTS != nil {
		deadline = e.DeadlineTS.UTC()
	}
	_, err := s.db.ExecContext(ctx, q,
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, int(e.OpIndex),
		e.ContractID, e.FunctionName,
		nullableString(e.OpSource), nullableString(e.TxSource),
		e.Recipient, pq.Array(e.Path),
		e.AmountIn, e.AmountOut, deadline,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSoroswapRouterSwap %s@%d: %w", e.TxHash, e.Ledger, err)
	}
	return nil
}

// nullableString returns nil for empty strings so the DB row carries
// SQL NULL rather than an empty-string literal. Matches the migration's
// nullable `op_source` / `tx_source` columns.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
