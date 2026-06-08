package clickhouse

import (
	"context"
	"fmt"
	"math/big"
)

// supplyFlowsDDL is the canonical stellar.supply_flows definition (kept in sync
// with deploy/clickhouse/tier1_schema.sql). Decode-at-ingest supply events with
// the i128 amount already decoded, so per-token supply is a pure SQL sum with no
// read-time XDR decode and no rollup refresh. ORDER BY contract_id first for
// fast per-token reads; the (ledger,tx,op,event) suffix is the event identity so
// re-ingest is idempotent under ReplacingMergeTree.
const supplyFlowsDDL = `
	CREATE TABLE IF NOT EXISTS stellar.supply_flows (
		contract_id  String,
		ledger_seq   UInt32,
		close_time   DateTime('UTC'),
		tx_hash      String,
		op_index     UInt32,
		event_index  UInt32,
		kind         LowCardinality(String),
		amount       Int128,
		ingested_at  DateTime DEFAULT now()
	) ENGINE = ReplacingMergeTree(ingested_at)
	PARTITION BY intDiv(ledger_seq, 1000000)
	ORDER BY (contract_id, ledger_seq, tx_hash, op_index, event_index)`

// EnsureSupplyFlowsTable creates stellar.supply_flows if absent. Idempotent;
// called at dual-sink / backfill / seed startup so the decode-at-ingest write
// path never races a missing table.
func EnsureSupplyFlowsTable(ctx context.Context, addr string) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec(ctx, supplyFlowsDDL); err != nil {
		return fmt.Errorf("clickhouse: ensure supply_flows: %w", err)
	}
	return nil
}

// WriteSupplyFlows batch-inserts decoded supply-flow rows into
// stellar.supply_flows. Used by the one-time history seed (decode existing CH
// contract_events → supply_flows); the live path writes via Sink.Flush.
// Idempotent under ReplacingMergeTree (re-seeding replaces by event identity).
func WriteSupplyFlows(ctx context.Context, addr string, rows []SupplyFlowRow) error {
	if len(rows) == 0 {
		return nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	batch, err := conn.PrepareBatch(ctx, `
		INSERT INTO stellar.supply_flows
		(contract_id, ledger_seq, close_time, tx_hash, op_index, event_index, kind, amount)`)
	if err != nil {
		return fmt.Errorf("clickhouse: prepare supply_flows seed batch: %w", err)
	}
	for _, r := range rows {
		amt := r.Amount
		if amt == nil {
			amt = big.NewInt(0)
		}
		if err := batch.Append(r.ContractID, r.LedgerSeq, r.CloseTime, r.TxHash, r.OpIndex, r.EventIndex, r.Kind, amt); err != nil {
			return fmt.Errorf("clickhouse: append supply_flow seed %s/%s/%d/%d: %w", r.ContractID, r.TxHash, r.OpIndex, r.EventIndex, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("clickhouse: send supply_flows seed batch: %w", err)
	}
	return nil
}

// TokenSupply is one token's supply, summed live from supply_flows.
type TokenSupply struct {
	ContractID string
	Total      *big.Int // mint − burn − clawback
	Mint       *big.Int
	Burn       *big.Int
	Clawback   *big.Int
	FlowCount  uint64
}

// SupplyForContract returns a token's current supply by summing its
// supply_flows directly — always current (the dual-sink feeds the table in real
// time), no rollup refresh. FINAL dedups the ReplacingMergeTree parts for the
// contract's (small) key range. Sums are taken as Int256 to avoid overflow when
// Σmint alone exceeds i128, then returned as *big.Int (ADR-0003). A token with
// no flows returns zeros (Total=0), not an error.
func SupplyForContract(ctx context.Context, addr, contractID string) (TokenSupply, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return TokenSupply{}, err
	}
	defer func() { _ = conn.Close() }()

	const q = `
		SELECT
			toString(sum(toInt256(if(kind = 'mint', amount, toInt128(0))))) AS mint,
			toString(sum(toInt256(if(kind = 'burn', amount, toInt128(0))))) AS burn,
			toString(sum(toInt256(if(kind = 'clawback', amount, toInt128(0))))) AS clawback,
			count() AS flows
		FROM stellar.supply_flows FINAL
		WHERE contract_id = ?`
	var mintS, burnS, clawbackS string
	var flows uint64
	if err := conn.QueryRow(ctx, q, contractID).Scan(&mintS, &burnS, &clawbackS, &flows); err != nil {
		return TokenSupply{}, fmt.Errorf("clickhouse: supply for %s: %w", contractID, err)
	}
	mint := mustBig(mintS)
	burn := mustBig(burnS)
	clawback := mustBig(clawbackS)
	total := new(big.Int).Sub(mint, new(big.Int).Add(burn, clawback))
	return TokenSupply{
		ContractID: contractID,
		Total:      total,
		Mint:       mint,
		Burn:       burn,
		Clawback:   clawback,
		FlowCount:  flows,
	}, nil
}

// mustBig parses a base-10 integer string into a *big.Int, returning 0 on an
// empty/invalid value (CH sum() of an empty set yields "0").
func mustBig(s string) *big.Int {
	if s == "" {
		return big.NewInt(0)
	}
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return big.NewInt(0)
	}
	return n
}
