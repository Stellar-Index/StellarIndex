package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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

// SorobanGenesisLedger is the protocol-20 (Soroban) activation ledger on
// pubnet — the boundary between the pre-Soroban classic era and the Soroban
// era. SEP-41 / SAC-wrapper contract events only exist at or above it; the
// Postgres SEP-41 supply observer (sep41_supply_events) therefore populates
// only [SorobanGenesisLedger, tip]. A classic asset's SAC-wrapper mint history
// that predates Soroban lives BELOW this ledger — captured in the ClickHouse
// lake's stellar.supply_flows via the post-P23 (CAP-67) replay — and is summed
// as the pre-genesis opening balance (TokenSupplyBelowLedger) that the
// aggregator seeds into sep41_supply_rollup (migration 0088, incident
// 2026-07-06).
const SorobanGenesisLedger uint32 = 50457424

// TokenSupply is one token's supply, summed live from supply_flows.
type TokenSupply struct {
	ContractID string
	Total      *big.Int // mint − burn − clawback
	Mint       *big.Int
	Burn       *big.Int
	Clawback   *big.Int
	FlowCount  uint64

	// Incomplete is true when Total < 0 — Σ(burn+clawback) exceeds Σmint.
	// That is physically impossible for a real token (you cannot burn more
	// than was ever minted); it means this contract's supply_flows are
	// INCOMPLETELY SEEDED in the lake — e.g. pre-Soroban SAC-wrapper mints
	// not yet CAP-67-replayed, so only the burn side is captured — NOT that
	// supply is genuinely negative. Serving Total as a supply figure would
	// publish a physically-impossible negative (ADR-0003; migration 0005
	// enforces total_supply/circulating_supply >= 0), so both API consumers
	// (/v1/assets/{id} F2 fallback and /v1/assets/{id}/supply) MUST treat an
	// Incomplete supply as UNAVAILABLE — omit or refuse — rather than serve
	// it, or clamp it to a bare 0 (which would read as a real "fully burned"
	// supply and understate the token). Mirrors how
	// [supply.SEP41Computer.Compute] REFUSES a negative total via
	// ErrNegativeTotalSupply / ErrNegativeTotalMissingBaseline instead of
	// publishing it.
	Incomplete bool
}

// supplySumQuery sums a contract's supply_flows. FINAL dedups the
// ReplacingMergeTree parts for the contract's (small, contract_id-ordered) key
// range; sums are Int256 to avoid overflow when Σmint alone exceeds i128, then
// returned as *big.Int (ADR-0003). A contract with no flows scans to zeros.
const supplySumQuery = `
	SELECT
		toString(sum(toInt256(if(kind = 'mint', amount, toInt128(0))))) AS mint,
		toString(sum(toInt256(if(kind = 'burn', amount, toInt128(0))))) AS burn,
		toString(sum(toInt256(if(kind = 'clawback', amount, toInt128(0))))) AS clawback,
		count() AS flows
	FROM stellar.supply_flows FINAL
	WHERE contract_id = ?`

func querySupply(ctx context.Context, conn driver.Conn, contractID string) (TokenSupply, error) {
	var mintS, burnS, clawbackS string
	var flows uint64
	if err := conn.QueryRow(ctx, supplySumQuery, contractID).Scan(&mintS, &burnS, &clawbackS, &flows); err != nil {
		return TokenSupply{}, fmt.Errorf("clickhouse: supply for %s: %w", contractID, err)
	}
	return assembleTokenSupply(contractID, mintS, burnS, clawbackS, flows), nil
}

// supplySumBelowLedgerQuery sums a contract's supply_flows STRICTLY BELOW a
// ledger. Identical shape to supplySumQuery with a `ledger_seq < ?` upper
// bound — the pre-genesis (pre-Soroban) opening-balance slice the SEP-41
// baseline seed reads (migration 0088). Int256 accumulation for the same
// overflow reason.
const supplySumBelowLedgerQuery = `
	SELECT
		toString(sum(toInt256(if(kind = 'mint', amount, toInt128(0))))) AS mint,
		toString(sum(toInt256(if(kind = 'burn', amount, toInt128(0))))) AS burn,
		toString(sum(toInt256(if(kind = 'clawback', amount, toInt128(0))))) AS clawback,
		count() AS flows
	FROM stellar.supply_flows FINAL
	WHERE contract_id = ? AND ledger_seq < ?`

func querySupplyBelowLedger(ctx context.Context, conn driver.Conn, contractID string, ledgerExclusive uint32) (TokenSupply, error) {
	var mintS, burnS, clawbackS string
	var flows uint64
	if err := conn.QueryRow(ctx, supplySumBelowLedgerQuery, contractID, ledgerExclusive).Scan(&mintS, &burnS, &clawbackS, &flows); err != nil {
		return TokenSupply{}, fmt.Errorf("clickhouse: supply for %s below ledger %d: %w", contractID, ledgerExclusive, err)
	}
	return assembleTokenSupply(contractID, mintS, burnS, clawbackS, flows), nil
}

func assembleTokenSupply(contractID, mintS, burnS, clawbackS string, flows uint64) TokenSupply {
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
		// A negative net total is impossible for a real token — flag it so
		// serving consumers refuse it rather than publish a negative supply
		// (see the Incomplete field doc). Non-negative → false, unchanged.
		Incomplete: total.Sign() < 0,
	}
}

// SupplyForContract returns a token's current supply by summing its
// supply_flows directly — always current (the dual-sink feeds the table in real
// time), no rollup refresh. Opens a connection per call; for a hot path (the
// API) hold a [SupplyReader] instead.
func SupplyForContract(ctx context.Context, addr, contractID string) (TokenSupply, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return TokenSupply{}, err
	}
	defer func() { _ = conn.Close() }()
	return querySupply(ctx, conn, contractID)
}

// SupplyReader is a persistent ClickHouse connection for serving per-token
// supply from supply_flows on a request hot path (the API). Construct once at
// startup, reuse across requests, Close at shutdown.
type SupplyReader struct {
	conn driver.Conn
}

// NewSupplyReader dials ClickHouse with a request-sized pool and pings it,
// authenticating as the ops-batch user when STELLARINDEX_CLICKHOUSE_OPS_USER/
// _PASSWORD are set (ops_auth.go) and otherwise as CH's unauthenticated
// `default` user — the pre-ADR-0048-D4 behavior. Non-API callers keep using
// this constructor unchanged.
func NewSupplyReader(ctx context.Context, addr string) (*SupplyReader, error) {
	// Ops-batch identity from the environment (2026-08-28 r1 incident;
	// see ops_auth.go) — CH `default` user when unset.
	auth, err := opsAuth()
	if err != nil {
		return nil, err
	}
	return NewSupplyReaderAuth(ctx, addr, auth.Username, auth.Password)
}

// NewSupplyReaderAuth is [NewSupplyReader] with an explicit CH
// username/password — ADR-0048 D4's serving-isolation profile, same
// rationale as clickhouse.NewExplorerReaderAuth (see that function's doc
// comment). The API binary wires GET /v1/assets/{id}/supply's reader through
// this constructor with `storage.clickhouse_serving_user` /
// `clickhouse_serving_password_env`.
func NewSupplyReaderAuth(ctx context.Context, addr, username, password string) (*SupplyReader, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:            []string{addr},
		Auth:            clickhouse.Auth{Database: "stellar", Username: username, Password: password},
		Settings:        clickhouse.Settings{"max_execution_time": 30},
		DialTimeout:     10 * time.Second,
		ReadTimeout:     30 * time.Second,
		MaxOpenConns:    8,
		MaxIdleConns:    4,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open supply reader %s: %w", addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping supply reader %s: %w", addr, err)
	}
	return &SupplyReader{conn: conn}, nil
}

// TokenSupply returns a contract's live supply (Σmint − Σburn − Σclawback).
func (r *SupplyReader) TokenSupply(ctx context.Context, contractID string) (TokenSupply, error) {
	return querySupply(ctx, r.conn, contractID)
}

// TokenSupplyBelowLedger returns a contract's supply summed STRICTLY BELOW
// ledgerExclusive (Σmint − Σburn − Σclawback over ledger_seq < ledgerExclusive).
// The SEP-41 genesis-baseline seed calls it with [SorobanGenesisLedger] to read
// the pre-Soroban opening balance the Postgres observer never captured
// (migration 0088, incident 2026-07-06). The pre-Soroban rows are
// REPLAY-DERIVED — a post-P23 core synthesized the CAP-67 unified asset events
// for classic history (legitimate but core-version-dependent, ADR-0033).
func (r *SupplyReader) TokenSupplyBelowLedger(ctx context.Context, contractID string, ledgerExclusive uint32) (TokenSupply, error) {
	return querySupplyBelowLedger(ctx, r.conn, contractID, ledgerExclusive)
}

// NativeTotalCoins returns XLM's total supply (in stroops, 7 decimals) and the
// ledger it was read from — the ledger header's total_coins, which is the
// authoritative native supply (XLM is not minted/burned via SAC mint/burn
// events, so it has no supply_flows). Reads the latest ledger.
func (r *SupplyReader) NativeTotalCoins(ctx context.Context) (totalCoins int64, ledger uint32, err error) {
	const q = `SELECT total_coins, ledger_seq FROM stellar.ledgers ORDER BY ledger_seq DESC LIMIT 1`
	if err := r.conn.QueryRow(ctx, q).Scan(&totalCoins, &ledger); err != nil {
		return 0, 0, fmt.Errorf("clickhouse: native total_coins: %w", err)
	}
	return totalCoins, ledger, nil
}

// Close releases the connection pool.
func (r *SupplyReader) Close() error { return r.conn.Close() }

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
