package clickhouse

import (
	"context"
	"fmt"
)

// TokenSupplyRow is one token's materialized supply, derived from the lake's
// mint/burn/clawback flows: total = mint − burn − clawback (raw, token-decimal
// scaled — the explorer/API applies per-token decimals at display). Keyed by
// contract_id (SAC for classic assets, token contract for SEP-41) — a unique
// per-token identity. Amounts are decimal strings (ADR-0003: i128/sum-of-i128
// never truncated to a fixed int width).
type TokenSupplyRow struct {
	ContractID    string
	TotalSupply   string
	MintTotal     string
	BurnTotal     string
	ClawbackTotal string
	FlowCount     uint64
	LastLedger    uint32
}

// EnsureTokenSupplyTable creates stellar.token_supply if absent. ReplacingMergeTree
// keyed by contract_id so a fresh recompute (full or incremental) replaces the
// prior row at read time / on merge.
func EnsureTokenSupplyTable(ctx context.Context, addr string) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS stellar.token_supply (
			contract_id    String,
			total_supply   String,
			mint_total     String,
			burn_total     String,
			clawback_total String,
			flow_count     UInt64,
			last_ledger    UInt32,
			computed_at    DateTime DEFAULT now()
		) ENGINE = ReplacingMergeTree(computed_at)
		ORDER BY contract_id`)
}

// WriteTokenSupplies batch-inserts token supply rows into stellar.token_supply.
func WriteTokenSupplies(ctx context.Context, addr string, rows []TokenSupplyRow) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	batch, err := conn.PrepareBatch(ctx, `
		INSERT INTO stellar.token_supply
		(contract_id, total_supply, mint_total, burn_total, clawback_total, flow_count, last_ledger)`)
	if err != nil {
		return fmt.Errorf("clickhouse: prepare token_supply batch: %w", err)
	}
	for _, r := range rows {
		if err := batch.Append(r.ContractID, r.TotalSupply, r.MintTotal, r.BurnTotal,
			r.ClawbackTotal, r.FlowCount, r.LastLedger); err != nil {
			return fmt.Errorf("clickhouse: append token_supply row %s: %w", r.ContractID, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("clickhouse: send token_supply batch: %w", err)
	}
	return nil
}
