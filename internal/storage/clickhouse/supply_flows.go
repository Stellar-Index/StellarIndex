package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// supplyFlowsDDL is the stellar.supply_flows definition (sync with
// deploy/clickhouse/tier1_schema.sql). The i128 amount is decoded at ingest, so supply
// is a pure SQL sum. ORDER BY leads with contract_id for per-token reads; the
// (ledger,tx,op,event) suffix is the event identity, so re-ingest is idempotent.
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

// EnsureSupplyFlowsTable creates stellar.supply_flows if absent (idempotent), so the
// write path never races a missing table.
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

// WriteSupplyFlows batch-inserts rows for the one-time history seed; the live path
// writes via Sink.Flush. Idempotent under ReplacingMergeTree (event identity).
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

// SorobanGenesisLedger is the protocol-20 activation ledger on pubnet. The Postgres
// SEP-41 supply observer only covers [SorobanGenesisLedger, tip]; a classic asset's
// SAC-wrapper mint history below it comes from the lake's post-P23 (CAP-67) replay and
// is summed as the pre-genesis opening balance (TokenSupplyBelowLedger).
const SorobanGenesisLedger uint32 = 50457424

// TokenSupply is one token's supply, summed live from supply_flows.
type TokenSupply struct {
	ContractID string
	Total      *big.Int // mint − burn − clawback
	Mint       *big.Int
	Burn       *big.Int
	Clawback   *big.Int
	FlowCount  uint64

	// Incomplete is true when Total < 0: burns+clawbacks exceed mints, impossible for a
	// real token. It means this contract's supply_flows are INCOMPLETELY SEEDED (e.g.
	// pre-Soroban SAC-wrapper mints not yet replayed), not that supply is negative.
	// Consumers MUST treat it as UNAVAILABLE (omit or refuse), never serve it (ADR-0003;
	// migration 0005 enforces supply >= 0) and never clamp it to 0, which would read as
	// "fully burned". Mirrors [supply.SEP41Computer.Compute]'s ErrNegativeTotalSupply.
	Incomplete bool
}

// supplySumQuery sums a contract's supply_flows. FINAL dedups the RMT parts for the
// contract's small contract_id-ordered range; sums are Int256 because Σmint alone can
// exceed i128, then returned as *big.Int (ADR-0003). No flows scans to zeros.
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

// supplySumBelowLedgerQuery is supplySumQuery with `ledger_seq < ?`: the pre-Soroban
// opening-balance slice for the SEP-41 baseline seed. Int256 for the same reason.
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
		// A negative net total is impossible for a real token: flag it so consumers refuse
		// it (see the Incomplete field doc).
		Incomplete: total.Sign() < 0,
	}
}

// SupplyReader is a persistent ClickHouse connection serving per-token supply on the
// API hot path; construct once, reuse, Close at shutdown.
type SupplyReader struct {
	conn driver.Conn
}

// NewSupplyReader dials ClickHouse with a request-sized pool and pings it, using the
// environment's identity ([chAuth]).
func NewSupplyReader(ctx context.Context, addr string) (*SupplyReader, error) {
	return NewSupplyReaderAuth(ctx, addr, "", "")
}

// NewSupplyReaderAuth is [NewSupplyReader] with an explicit CH username/password
// (ADR-0048 D4 serving isolation; see NewExplorerReaderAuth). Both empty resolves the
// environment's identity.
func NewSupplyReaderAuth(ctx context.Context, addr, username, password string) (*SupplyReader, error) {
	auth, err := authOrEnv(username, password)
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:            []string{addr},
		Auth:            auth,
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

// supplySumByContractsQuery is [supplySumQuery] for a SET of contracts in ONE read.
// `contract_id` LEADS the ORDER BY, so the IN-list is a set of primary-key prefix
// ranges and GROUP BY cardinality is bounded by the list; FINAL's setup cost is paid
// once, not N times. Int256 and string projection as in [supplySumQuery] (ADR-0003).
// Contracts with no flows are ABSENT; callers must not read a missing key as zero
// supply (see [SupplyReader.TokenSupplyForContracts]).
const supplySumByContractsQuery = `
	SELECT
		contract_id,
		toString(sum(toInt256(if(kind = 'mint', amount, toInt128(0))))) AS mint,
		toString(sum(toInt256(if(kind = 'burn', amount, toInt128(0))))) AS burn,
		toString(sum(toInt256(if(kind = 'clawback', amount, toInt128(0))))) AS clawback,
		count() AS flows
	FROM stellar.supply_flows FINAL
	WHERE contract_id IN (?)
	GROUP BY contract_id`

// TokenSupplyForContracts sums [SupplyReader.TokenSupply] for many contracts in one
// read, keyed by contract id.
//
// A contract with NO flows is OMITTED, not returned as a zero [TokenSupply]: zero
// claims "no supply", absence claims nothing, and a caller treating a missing key as
// zero would publish a fully-burned supply for every token the lake has not seen.
// Incomplete carries through per contract; callers keep refusing those.
func (r *SupplyReader) TokenSupplyForContracts(ctx context.Context, contractIDs []string) (map[string]TokenSupply, error) {
	if len(contractIDs) == 0 {
		return map[string]TokenSupply{}, nil
	}
	rows, err := r.conn.Query(ctx, supplySumByContractsQuery, contractIDs)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: supply for %d contracts: %w", len(contractIDs), err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]TokenSupply, len(contractIDs))
	for rows.Next() {
		var contractID, mintS, burnS, clawbackS string
		var flows uint64
		if err := rows.Scan(&contractID, &mintS, &burnS, &clawbackS, &flows); err != nil {
			return nil, fmt.Errorf("clickhouse: scan bulk supply row: %w", err)
		}
		out[contractID] = assembleTokenSupply(contractID, mintS, burnS, clawbackS, flows)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: bulk supply stream: %w", err)
	}
	return out, nil
}

// TokenSupplyBelowLedger returns a contract's supply over ledger_seq <
// ledgerExclusive. The SEP-41 genesis-baseline seed calls it with
// [SorobanGenesisLedger]. Pre-Soroban rows are REPLAY-DERIVED (a post-P23 core
// synthesized CAP-67 events for classic history; core-version-dependent, ADR-0033).
func (r *SupplyReader) TokenSupplyBelowLedger(ctx context.Context, contractID string, ledgerExclusive uint32) (TokenSupply, error) {
	return querySupplyBelowLedger(ctx, r.conn, contractID, ledgerExclusive)
}

// NativeTotalCoins returns XLM's total supply (stroops) and its ledger from the
// latest ledger header's total_coins; XLM has no supply_flows (no SAC mint/burn).
func (r *SupplyReader) NativeTotalCoins(ctx context.Context) (totalCoins int64, ledger uint32, err error) {
	const q = `SELECT total_coins, ledger_seq FROM stellar.ledgers ORDER BY ledger_seq DESC LIMIT 1`
	if err := r.conn.QueryRow(ctx, q).Scan(&totalCoins, &ledger); err != nil {
		return 0, 0, fmt.Errorf("clickhouse: native total_coins: %w", err)
	}
	return totalCoins, ledger, nil
}

// Close releases the connection pool.
func (r *SupplyReader) Close() error { return r.conn.Close() }

// mustBig parses a base-10 integer string, returning 0 for empty/invalid (CH sum() of
// an empty set yields "0").
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

// SupplyFlowDay is one contract's NET supply change over one UTC day. It is a DELTA,
// never a level: the level is the running total of every delta from the first flow
// ([SupplyReader.DailySupplyFlowsForContracts]); a level built from a windowed slice
// of an append-only log would be the window's turnover.
type SupplyFlowDay struct {
	ContractID string
	// Day is the UTC midnight the flows were bucketed into.
	Day time.Time
	// Net is Σmint − Σ(burn + clawback); negative on net-redemption days (a real reading).
	Net *big.Int
	// Mint, Burn and Clawback are the day's unsigned per-kind sums; an unknown kind is in
	// Flows only.
	Mint, Burn, Clawback *big.Int
	// Flows counts the events behind Net; zero Net with Flows > 0 means mints and burns
	// cancelled, whereas a day with no flows has no row.
	Flows uint64
}

// supplyFlowsDailyByContractsQuery buckets a contract set's flows by UTC close day.
// `contract_id` leads the ORDER BY, so the IN-list is primary-key prefix ranges (as
// [supplySumByContractsQuery]). The sign comes from an explicit multiIf over the three
// DDL kinds, not `if(kind = 'mint', +, -)`: a fourth kind would silently SUBTRACT under
// the two-armed form, whereas here it adds zero and `flows` still counts it. Int256,
// stringified (ADR-0003).
const supplyFlowsDailyByContractsQuery = `
	SELECT
		contract_id,
		toStartOfDay(close_time) AS day,
		toString(sum(multiIf(
			kind = 'mint',                    toInt256(amount),
			kind IN ('burn', 'clawback'),    -toInt256(amount),
			toInt256(0)))) AS net,
		toString(sumIf(toInt256(amount), kind = 'mint'))     AS mint,
		toString(sumIf(toInt256(amount), kind = 'burn'))     AS burn,
		toString(sumIf(toInt256(amount), kind = 'clawback')) AS clawback,
		count() AS flows
	FROM stellar.supply_flows FINAL
	WHERE contract_id IN (?)
	GROUP BY contract_id, day
	ORDER BY contract_id ASC, day ASC`

// DailySupplyFlowsForContracts returns every day on which any named contract had a
// supply flow, with that day's net change, ascending by (contract, day).
//
// The WHOLE history, never a window: a supply LEVEL is the running total of every
// flow, so callers cumulate from the first flow and window the CUMULATED series.
// Cumulating from zero inside a window would publish its turnover as supply.
//
// A day with no flows has no row, and that means the supply did not change: the log
// records every event that could move it. This is arithmetic, not the licence to carry
// a PRICE forward across an oracle's silence.
//
// A contract with no flows is ABSENT, not zero ([SupplyReader.TokenSupplyForContracts]).
// Callers must check the running total for a NEGATIVE excursion ([TokenSupply.Incomplete]):
// refuse the series, never clamp it.
func (r *SupplyReader) DailySupplyFlowsForContracts(ctx context.Context, contractIDs []string) ([]SupplyFlowDay, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	rows, err := r.conn.Query(ctx, supplyFlowsDailyByContractsQuery, contractIDs)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: daily supply flows for %d contracts: %w", len(contractIDs), err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]SupplyFlowDay, 0, 512)
	for rows.Next() {
		var (
			contractID string
			day        time.Time
			netS       string
			mintS      string
			burnS      string
			clawbackS  string
			flows      uint64
		)
		if err := rows.Scan(&contractID, &day, &netS, &mintS, &burnS, &clawbackS, &flows); err != nil {
			return nil, fmt.Errorf("clickhouse: scan daily supply flow row: %w", err)
		}
		out = append(out, SupplyFlowDay{
			ContractID: contractID,
			Day:        day.UTC(),
			Net:        mustBig(netS),
			Mint:       mustBig(mintS),
			Burn:       mustBig(burnS),
			Clawback:   mustBig(clawbackS),
			Flows:      flows,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: daily supply flow stream: %w", err)
	}
	return out, nil
}
