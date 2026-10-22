package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// supplyFlowsDDL is the stellar.supply_flows definition (sync with deploy/clickhouse/tier1_schema.sql). The i128 amount is decoded at
// ingest, so supply is a pure SQL sum; the (ledger,tx,op,event) ORDER BY suffix is the event identity, so re-ingest is idempotent.
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

// EnsureSupplyFlowsTable creates stellar.supply_flows if absent (idempotent), so the write path never races a missing table.
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

// WriteSupplyFlows batch-inserts rows for the one-time history seed (the live path writes via Sink.Flush); idempotent under ReplacingMergeTree on event identity.
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

// SorobanGenesisLedger is the protocol-20 activation ledger on pubnet; supply below it comes from the lake's post-P23 CAP-67 replay (TokenSupplyBelowLedger) as the opening balance.
const SorobanGenesisLedger uint32 = 50457424

// TokenSupply is one token's supply, summed live from supply_flows.
type TokenSupply struct {
	ContractID string
	Total      *big.Int // mint − burn − clawback
	Mint       *big.Int
	Burn       *big.Int
	Clawback   *big.Int
	FlowCount  uint64

	// Incomplete is true when Total < 0: burns+clawbacks exceed mints, impossible for a real
	// token, so this contract's supply_flows are INCOMPLETELY SEEDED. Consumers MUST treat it as
	// UNAVAILABLE (omit or refuse) and never clamp it to 0, which would read as "fully burned".
	// Mirrors [supply.SEP41Computer.Compute]'s ErrNegativeTotalSupply.
	Incomplete bool
}

// supplySumBelowLedgerQuery sums one contract's supply_flows below a ledger: the pre-Soroban opening-balance slice for the SEP-41
// baseline seed. Sums are Int256 because Σmint alone can exceed i128, then returned as *big.Int (ADR-0003).
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

// SupplyReader is a persistent ClickHouse connection serving per-token supply on the API hot path; construct once, reuse, Close at shutdown.
type SupplyReader struct {
	conn  driver.Conn
	bases settledSupplyMemo
	now   func() time.Time
}

// NewSupplyReader dials ClickHouse with a request-sized pool and pings it, using the environment's identity (chAuth).
func NewSupplyReader(ctx context.Context, addr string) (*SupplyReader, error) {
	return NewSupplyReaderAuth(ctx, addr, "", "")
}

// NewSupplyReaderAuth is NewSupplyReader with an explicit CH username/password (ADR-0048 D4 serving isolation); both empty resolves the environment's identity.
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

// TokenSupply returns a contract's live supply (Σmint − Σburn − Σclawback), reading only the flows above its memoised settled base.
func (r *SupplyReader) TokenSupply(ctx context.Context, contractID string) (TokenSupply, error) {
	sums, err := r.flowSumsForContracts(ctx, []string{contractID})
	if err != nil {
		return TokenSupply{}, err
	}
	return sums[contractID].supply(contractID), nil
}

// supplySumByContractsQuery sums supply_flows for a SET of contracts in ONE read, over a ledger range the caller appends: `contract_id`
// LEADS the ORDER BY, so the IN-list is primary-key prefix ranges and FINAL's setup cost is paid once. Int256 and string projection as
// in [supplySumBelowLedgerQuery] (ADR-0003). ledger_seq is in the dedup key, so FINAL never merges a row across a range split. Contracts with no
// flows in the range are ABSENT.
const supplySumByContractsQuery = `
	SELECT
		contract_id,
		toString(sum(toInt256(if(kind = 'mint', amount, toInt128(0))))) AS mint,
		toString(sum(toInt256(if(kind = 'burn', amount, toInt128(0))))) AS burn,
		toString(sum(toInt256(if(kind = 'clawback', amount, toInt128(0))))) AS clawback,
		count() AS flows
	FROM stellar.supply_flows FINAL
	WHERE contract_id IN (?) AND ledger_seq `

// supplySettledQuery is the ledger a base sum stops below: supplySettledMargin under the newest flow, rounded down, and never above the
// lake tip, so a stray far-future row cannot pull live flows into a base. Both max() reads are answered from part metadata.
const supplySettledQuery = `SELECT toUInt32(greatest(intDiv(toInt64(least(max(ledger_seq),
		ifNull((SELECT max(ledger_seq) FROM stellar.ledgers), 0))) - ` + supplySettledMargin + `, 100000) * 100000, 0))
	FROM stellar.supply_flows`

const (
	// supplySettledMargin keeps the split well below the live writer (~14h of ledgers).
	supplySettledMargin = "10000"
	// supplyBaseMaxAge bounds how long a flow written late below the split (a replay) can go unseen.
	supplyBaseMaxAge  = 6 * time.Hour
	supplyBaseMemoMax = 8192
)

// flowSums is a contract's supply_flows sums over a ledger range; all fields are set.
type flowSums struct {
	mint, burn, clawback *big.Int
	flows                uint64
}

func parseFlowSums(mintS, burnS, clawbackS string, flows uint64) flowSums {
	return flowSums{mint: mustBig(mintS), burn: mustBig(burnS), clawback: mustBig(clawbackS), flows: flows}
}

func (a flowSums) plus(b flowSums) flowSums {
	return flowSums{
		mint:     new(big.Int).Add(a.mint, b.mint),
		burn:     new(big.Int).Add(a.burn, b.burn),
		clawback: new(big.Int).Add(a.clawback, b.clawback),
		flows:    a.flows + b.flows,
	}
}

func (a flowSums) supply(contractID string) TokenSupply {
	return assembleTokenSupply(contractID, a.mint.String(), a.burn.String(), a.clawback.String(), a.flows)
}

// settledBase is a contract's sums strictly below settled, as read at at.
type settledBase struct {
	settled uint32
	sums    flowSums
	at      time.Time
}

type settledSupplyMemo struct {
	mu sync.Mutex
	m  map[string]settledBase
}

func (m *settledSupplyMemo) get(id string, now time.Time) (settledBase, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[id]
	return b, ok && now.Sub(b.at) < supplyBaseMaxAge
}

func (m *settledSupplyMemo) put(id string, b settledBase) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.m[id]; m.m == nil || (!held && len(m.m) >= supplyBaseMemoMax) {
		m.m = make(map[string]settledBase)
	}
	m.m[id] = b
}

// TokenSupplyForContracts sums TokenSupply for many contracts, keyed by contract id; a contract with NO flows is OMITTED, never zero
// (zero would publish a fully-burned supply), and Incomplete carries through. Each contract's sum below a settled ledger is memoised
// for supplyBaseMaxAge, so a refresh re-reads only the flows above it: the full FINAL sum of a busy SAC is hundreds of millions of rows.
func (r *SupplyReader) TokenSupplyForContracts(ctx context.Context, contractIDs []string) (map[string]TokenSupply, error) {
	sums, err := r.flowSumsForContracts(ctx, contractIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TokenSupply, len(sums))
	for id, sum := range sums {
		if sum.flows > 0 {
			out[id] = sum.supply(id)
		}
	}
	return out, nil
}

// flowSumsForContracts is every contract's all-time sums: its memoised settled base plus the flows above it, zero when it has none.
func (r *SupplyReader) flowSumsForContracts(ctx context.Context, contractIDs []string) (map[string]flowSums, error) {
	groups, err := r.settledBases(ctx, contractIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]flowSums, len(contractIDs))
	for settled, bases := range groups {
		ids := make([]string, 0, len(bases))
		for id := range bases {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		recent, err := r.sumFlows(ctx, ids, ">= ?", settled)
		if err != nil {
			return nil, err
		}
		for id, sum := range bases {
			if d, ok := recent[id]; ok {
				sum = sum.plus(d)
			}
			out[id] = sum
		}
	}
	return out, nil
}

// settledBases groups each contract's base sums by the settled ledger they stop below, reading and memoising the ones not memoised.
func (r *SupplyReader) settledBases(ctx context.Context, contractIDs []string) (map[uint32]map[string]flowSums, error) {
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	groups := map[uint32]map[string]flowSums{}
	add := func(id string, b settledBase) {
		if groups[b.settled] == nil {
			groups[b.settled] = map[string]flowSums{}
		}
		groups[b.settled][id] = b.sums
	}
	var cold []string
	for _, id := range contractIDs {
		if b, ok := r.bases.get(id, now); ok {
			add(id, b)
		} else {
			cold = append(cold, id)
		}
	}
	if len(cold) == 0 {
		return groups, nil
	}
	var settled uint32
	if err := r.conn.QueryRow(ctx, supplySettledQuery).Scan(&settled); err != nil {
		return nil, fmt.Errorf("clickhouse: supply settled ledger: %w", err)
	}
	below, err := r.sumFlows(ctx, cold, "< ?", settled)
	if err != nil {
		return nil, err
	}
	for _, id := range cold {
		b := settledBase{settled: settled, sums: below[id], at: now}
		if b.sums.mint == nil {
			b.sums = parseFlowSums("0", "0", "0", 0)
		} else {
			// Only real bases are kept: an empty one costs nothing to re-read, and request-chosen ids must not evict them.
			r.bases.put(id, b)
		}
		add(id, b)
	}
	return groups, nil
}

func (r *SupplyReader) sumFlows(ctx context.Context, ids []string, ledgerCond string, ledger uint32) (map[string]flowSums, error) {
	rows, err := r.conn.Query(ctx, supplySumByContractsQuery+ledgerCond+`
	GROUP BY contract_id`, ids, ledger)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: supply for %d contracts: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]flowSums, len(ids))
	for rows.Next() {
		var id, m, b, c string
		var f uint64
		if err := rows.Scan(&id, &m, &b, &c, &f); err != nil {
			return nil, fmt.Errorf("clickhouse: scan bulk supply row: %w", err)
		}
		out[id] = parseFlowSums(m, b, c, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: bulk supply stream: %w", err)
	}
	return out, nil
}

// TokenSupplyBelowLedger returns a contract's supply over ledger_seq < ledgerExclusive; pre-Soroban rows are REPLAY-DERIVED (core-version-dependent, ADR-0033).
func (r *SupplyReader) TokenSupplyBelowLedger(ctx context.Context, contractID string, ledgerExclusive uint32) (TokenSupply, error) {
	return querySupplyBelowLedger(ctx, r.conn, contractID, ledgerExclusive)
}

// NativeTotalCoins returns XLM's total supply (stroops) and its ledger from the latest header's total_coins; XLM has no supply_flows.
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

// SupplyFlowDay is one contract's NET supply change over one UTC day: a DELTA, never a level (a level is the running total from the first flow).
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

// supplyFlowsDailyByContractsQuery buckets a contract set's flows by UTC close day. The sign comes from an explicit multiIf over the
// three DDL kinds, not `if(kind = 'mint', +, -)`: a fourth kind would silently SUBTRACT under the two-armed form, whereas here it adds
// zero and `flows` still counts it. Int256, stringified (ADR-0003).
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

// DailySupplyFlowsForContracts returns every day with a supply flow per contract, ascending, over the WHOLE history (callers cumulate then window); a contract with no flows is ABSENT, and a NEGATIVE running total must be refused, never clamped.
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
