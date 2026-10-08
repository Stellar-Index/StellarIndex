// Package clickhouse is the Tier-1 raw-lake write path (ADR-0034): it buffers decoded
// ledger rows and flushes them to the `stellar.*` tables in native batches, mirroring
// deploy/clickhouse/tier1_schema.sql (minus the DEFAULT ingested_at column).
package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ---- Row types (1:1 with the Tier-1 schema, ingested_at omitted = DEFAULT) ----

// LedgerRow mirrors stellar.ledgers.
type LedgerRow struct {
	LedgerSeq               uint32
	CloseTime               time.Time
	LedgerHash              string
	PrevHash                string
	ProtocolVersion         uint32
	BucketListHash          string
	TxCount                 uint32
	OpCount                 uint32
	SorobanEventCount       uint32
	ClassicTradeEffectCount uint32
	TotalCoins              int64
	FeePool                 int64
	BaseFee                 uint32
	BaseReserve             uint32
}

// TransactionRow mirrors stellar.transactions.
type TransactionRow struct {
	LedgerSeq      uint32
	CloseTime      time.Time
	TxHash         string
	TxIndex        uint32
	SourceAccount  string
	FeeCharged     int64
	MaxFee         int64
	OperationCount uint16
	Successful     uint8
	ResultCode     int32
	MemoType       string
	Memo           string

	// Soroban resource metering (extract.go:extractSorobanMetering); 0 for classic txs.
	// DECLARED = submitter's bid, ACTUAL = fees core charged. Pubnet meta has no
	// actual-instructions field.
	SorobanInstructions   uint32 // declared CPU instruction bid
	SorobanDiskReadBytes  uint32 // declared
	SorobanWriteBytes     uint32 // declared
	SorobanReadEntries    uint16 // declared len(footprint.ReadOnly)
	SorobanWriteEntries   uint16 // declared len(footprint.ReadWrite)
	SorobanResourceFeeBid int64  // declared total resource-fee bid
	SorobanNonrefundFee   int64  // actual non-refundable resource fee charged
	SorobanRefundableFee  int64  // actual refundable resource fee charged
	SorobanRentFee        int64  // actual rent fee charged

	// Fee-bump outer layer (extract.go:extractFeeBump); ""/0 otherwise. On a fee bump
	// TxHash is the OUTER hash and SourceAccount/MaxFee are the inner tx's.
	InnerTxHash     string // inner tx hash
	FeeAccount      string // fee payer
	FeeBumpFee      int64  // fee payer's max-fee bid (the bound on FeeCharged)
	InnerResultCode int32  // inner TransactionResultCode
}

// OperationRow mirrors stellar.operations.
type OperationRow struct {
	LedgerSeq     uint32
	CloseTime     time.Time
	TxHash        string
	TxIndex       uint32
	OpIndex       uint32
	OpType        string
	SourceAccount string
	BodyXDR       string
}

// OperationResultRow mirrors stellar.operation_results.
type OperationResultRow struct {
	LedgerSeq  uint32
	TxHash     string
	OpIndex    uint32
	ResultCode int32
	ResultXDR  string
}

// OperationParticipantRow mirrors stellar.operation_participants: one row per
// (non-source account, operation) (ADR-0038).
type OperationParticipantRow struct {
	Account   string
	LedgerSeq uint32
	CloseTime time.Time
	TxHash    string
	TxIndex   uint32
	OpIndex   uint32
}

// ContractEventRow mirrors stellar.contract_events.
type ContractEventRow struct {
	LedgerSeq        uint32
	CloseTime        time.Time
	TxHash           string
	OpIndex          uint32
	EventIndex       uint32
	ContractID       string
	EventType        string
	TopicCount       uint8
	Topic0Sym        string
	TopicsXDR        []string
	DataXDR          string
	OpArgsXDR        []string
	InSuccessfulCall uint8
}

// LedgerEntryChangeRow mirrors stellar.ledger_entry_changes.
type LedgerEntryChangeRow struct {
	LedgerSeq   uint32
	CloseTime   time.Time
	TxHash      string
	OpIndex     int32 // -1 for fee-meta / tx-level
	ChangeIndex uint32
	ChangeType  string
	EntryType   string
	KeyXDR      string
	EntryXDR    string
	// AccountID is the owning account G-strkey for account-owned entries, "" otherwise;
	// the queryable owner column for explorer reads (ADR-0038).
	AccountID string
	// Asset is the canonical asset id (CODE-ISSUER / native / pool:<hex>) for trustlines.
	Asset string
	// Balance is the stroop balance for account and trustline entries (i64 in XDR), 0
	// otherwise, so holder reads sort and aggregate in SQL without decoding XDR.
	Balance int64
	// IntraLedgerSeq is the change's position in the ledger-wide canonical walk, in PHASE
	// order, not per-tx: (1) every tx's fee changes, (2) every tx's apply-phase meta,
	// (3) post-apply fee changes (Soroban refunds), (4) evicted keys. Mirrors the SDK's
	// ingest.LedgerChangeReader and dispatcher walkLedgerEntryChanges. It is folded into
	// ledger_entries_current's RMT version (ledger_seq<<32 | intra_ledger_seq) so FINAL
	// keeps the LAST same-ledger change; change_index cannot serve (per-tx counter).
	// Seed rows stamp seedIntraLedgerSeq. Positions are comparable only within the same
	// dispatcher.EntryWalkVersion: a bump renumbers every ledger, so the partition must be
	// dropped before re-ingest (migration 0120). DEFAULT 0 in the lake.
	IntraLedgerSeq uint32
}

// SupplyFlowRow mirrors stellar.supply_flows: one decoded mint/burn/clawback event.
// The i128 magnitude is decoded at ingest as a *big.Int (ADR-0003), so per-token
// supply is a pure SQL sum. The event identity is the ORDER BY key, so re-inserts
// are idempotent.
type SupplyFlowRow struct {
	ContractID string
	LedgerSeq  uint32
	CloseTime  time.Time
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Kind       string // "mint" | "burn" | "clawback"
	Amount     *big.Int
}

// LedgerExtract is all rows from one LedgerCloseMeta.
type LedgerExtract struct {
	Ledger       LedgerRow
	Txs          []TransactionRow
	Ops          []OperationRow
	Results      []OperationResultRow
	Participants []OperationParticipantRow
	Events       []ContractEventRow
	Changes      []LedgerEntryChangeRow
	SupplyFlows  []SupplyFlowRow

	// TxReadErrors / TxEventReadErrors count txs not fully read (malformed, or events
	// unreadable under an unsupported meta version). In-memory only: the ledger is still
	// written to keep the lake contiguous, but its Events undercount. The indexer meters
	// them on stellarindex_ch_live_sink_read_undercount_total.
	TxReadErrors      int
	TxEventReadErrors int

	// EntryMetaUnsupported counts txs whose entry-change walk was skipped for an
	// unhandled TransactionMeta version: the ledger's Changes undercount and read as
	// "nothing happened". In-memory only; unreachable on production input today.
	EntryMetaUnsupported int

	// SorobanFeeMetaUnsupported counts Soroban txs whose charged-fee columns are written
	// as 0, indistinguishable from a zero charge. In-memory only.
	SorobanFeeMetaUnsupported int

	// EvictedKeysUnreadable is 1 when the evicted-keys list was unreadable: each evicted
	// entry's last write stays current. In-memory only.
	EvictedKeysUnreadable int

	// EntryChangesUnencodable counts changes that could not be re-marshalled and wrote no
	// row (each still took its intra_ledger_seq position). Unreachable on decoded input.
	EntryChangesUnencodable int
}

// ErrBufferFull is returned by [Sink.Add] when the buffer is at maxBufferLedgers and
// flushes keep failing (sustained ClickHouse outage). The extract is DROPPED to cap
// heap; callers count it as a bounded drop, not an error, and the ch-live-catchup
// gap scan heals it.
var ErrBufferFull = errors.New("clickhouse: sink buffer full — extract dropped (bounded-drop, heals via ch-live-catchup)")

// Sink buffers extracts and flushes them to ClickHouse in batches. Not safe
// for concurrent use by multiple goroutines — give each backfill worker its
// own Sink (ClickHouse handles concurrent connections well).
type Sink struct {
	conn             driver.Conn
	flushEvery       int
	maxBufferLedgers int // hard cap on buffered ledgers; 0 = unbounded (backfill).
	// onFlushed, when set, is told how many ledgers every successful Flush
	// made durable — whichever path (Add's inline flush, Flush, Close) ran it.
	onFlushed func(ledgers int)

	ledgers      []LedgerRow
	txs          []TransactionRow
	ops          []OperationRow
	results      []OperationResultRow
	participants []OperationParticipantRow
	events       []ContractEventRow
	changes      []LedgerEntryChangeRow
	supplyFlows  []SupplyFlowRow
}

// SetMaxBufferLedgers caps buffered ledgers before [Add] drops with [ErrBufferFull].
// 0 (default) is unbounded, right for backfill Sinks whose caller retries the same
// range; the LiveSink sets a cap because live ingest never stops feeding it.
func (s *Sink) SetMaxBufferLedgers(n int) { s.maxBufferLedgers = n }

// BufferedLedgers reports how many ledgers are currently buffered (unflushed).
func (s *Sink) BufferedLedgers() int { return len(s.ledgers) }

// Open dials ClickHouse at addr, pings, and fails if any table or column Flush
// writes to is missing. flushEvery is the ledger count that triggers a Flush.
func Open(ctx context.Context, addr string, flushEvery int) (*Sink, error) {
	if flushEvery <= 0 {
		flushEvery = 2000
	}
	// Identity from the environment; see ops_auth.go.
	auth, err := chAuth()
	if err != nil {
		return nil, err
	}
	return openSink(ctx, &clickhouse.Options{
		Addr: []string{addr},
		Auth: auth,
		Settings: clickhouse.Settings{
			// max_execution_time is a time limit (0 = unlimited, which lets a heavy FINAL read
			// wedge CH). This is the write path, so a finite ceiling is a safety net; read-path
			// caps live on openRead in gate.go.
			"max_execution_time": 300,
		},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Hour,
	}, flushEvery)
}

// openSink dials, pings, and refuses a target missing any table or column Flush
// writes to, so a mis-pointed endpoint fails at startup.
func openSink(ctx context.Context, opts *clickhouse.Options, flushEvery int) (*Sink, error) {
	addr := strings.Join(opts.Addr, ",")
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open %s: %w", addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping %s: %w", addr, err)
	}
	if err := checkSchema(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: %s: %w", addr, err)
	}
	return &Sink{conn: conn, flushEvery: flushEvery}, nil
}

// sinkTables is every stellar.* table Flush inserts into.
var sinkTables = []string{
	"transactions", "operations", "operation_results", "operation_participants",
	"contract_events", "ledger_entry_changes", "supply_flows", "ledgers",
}

// sinkColumns are columns Flush writes that tier1_schema.sql alone may not create;
// older databases need the additive ALTERs in deploy/clickhouse.
var sinkColumns = map[string][]string{
	"transactions": {
		"soroban_instructions", "soroban_disk_read_bytes", "soroban_write_bytes",
		"soroban_read_entries", "soroban_write_entries", "soroban_resource_fee_bid",
		"soroban_nonrefundable_fee", "soroban_refundable_fee", "soroban_rent_fee",
		"inner_tx_hash", "fee_account", "fee_bump_fee", "inner_result_code",
	},
	"ledger_entry_changes": {"intra_ledger_seq"},
}

// checkSchema returns an error naming every sinkTables entry absent from the
// stellar database, then every sinkColumns column absent from its table.
func checkSchema(ctx context.Context, conn driver.Conn) error {
	have, err := stellarNames(ctx, conn, `SELECT name FROM system.tables WHERE database = 'stellar'`)
	if err != nil {
		return err
	}
	var missing []string
	for _, name := range sinkTables {
		if !have[name] {
			missing = append(missing, "stellar."+name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("schema check: missing table(s) %s (apply deploy/clickhouse/tier1_schema.sql)", strings.Join(missing, ", "))
	}
	cols, err := stellarNames(ctx, conn, `SELECT concat(table, '.', name) FROM system.columns WHERE database = 'stellar' AND table IN ('transactions', 'ledger_entry_changes')`)
	if err != nil {
		return err
	}
	for table, names := range sinkColumns {
		for _, name := range names {
			if !cols[table+"."+name] {
				missing = append(missing, "stellar."+table+"."+name)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("schema check: missing column(s) %s (apply the ALTERs in deploy/clickhouse/transactions_soroban_metering.sql, transactions_fee_bump.sql and ledger_entries_current_intra_ledger_seq.sql)", strings.Join(missing, ", "))
	}
	return nil
}

// stellarNames runs a single-String-column query and returns the values as a set.
func stellarNames(ctx context.Context, conn driver.Conn, query string) (map[string]bool, error) {
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("schema check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	have := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("schema check: scan: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema check: %w", err)
	}
	return have, nil
}

// Add buffers one ledger's extract and flushes at the ledger threshold; any error
// other than [ErrBufferFull] comes from that inline flush (e stays buffered). If a
// finite cap is set and the buffer is AT it, the NEWEST extract is dropped (O(1),
// keeps the slices intact) and the ch-live-catchup gap scan refills it: bounded heap
// beats unbounded growth on the shared host.
func (s *Sink) Add(ctx context.Context, e LedgerExtract) error {
	if s.maxBufferLedgers > 0 && len(s.ledgers) >= s.maxBufferLedgers {
		return ErrBufferFull
	}
	s.ledgers = append(s.ledgers, e.Ledger)
	s.txs = append(s.txs, e.Txs...)
	s.ops = append(s.ops, e.Ops...)
	s.results = append(s.results, e.Results...)
	s.participants = append(s.participants, e.Participants...)
	s.events = append(s.events, e.Events...)
	s.changes = append(s.changes, e.Changes...)
	s.supplyFlows = append(s.supplyFlows, e.SupplyFlows...)
	if len(s.ledgers) >= s.flushEvery {
		return s.Flush(ctx)
	}
	return nil
}

// Flush sends one native batch per table, then clears the buffers. A partial failure
// keeps them so the caller retries the range (idempotent under ReplacingMergeTree).
// ORDERING IS LOAD-BEARING: stellar.ledgers is flushed LAST as the per-ledger commit
// marker (the INSERTs are not transactional). The projector's ContiguousWatermark
// (ADR-0041) relies on it never passing a half-written ledger.
func (s *Sink) Flush(ctx context.Context) error {
	n := len(s.ledgers)
	if n == 0 {
		return nil
	}
	if err := s.flushTxs(ctx); err != nil {
		return err
	}
	if err := s.flushOps(ctx); err != nil {
		return err
	}
	if err := s.flushResults(ctx); err != nil {
		return err
	}
	if err := s.flushParticipants(ctx); err != nil {
		return err
	}
	if err := s.flushEvents(ctx); err != nil {
		return err
	}
	if err := s.flushChanges(ctx); err != nil {
		return err
	}
	if err := s.flushSupplyFlows(ctx); err != nil {
		return err
	}
	// ledgers LAST: the commit marker (see Flush).
	if err := s.flushLedgers(ctx); err != nil {
		return err
	}
	s.reset()
	if s.onFlushed != nil {
		s.onFlushed(n)
	}
	return nil
}

func (s *Sink) reset() {
	s.ledgers = s.ledgers[:0]
	s.txs = s.txs[:0]
	s.ops = s.ops[:0]
	s.results = s.results[:0]
	s.participants = s.participants[:0]
	s.events = s.events[:0]
	s.changes = s.changes[:0]
	s.supplyFlows = s.supplyFlows[:0]
}

// Close flushes any remaining rows and closes the connection.
func (s *Sink) Close(ctx context.Context) error {
	ferr := s.Flush(ctx)
	cerr := s.conn.Close()
	if ferr != nil {
		return ferr
	}
	return cerr
}

func (s *Sink) flushLedgers(ctx context.Context) error {
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version, bucket_list_hash, tx_count, op_count, soroban_event_count, classic_trade_effect_count, total_coins, fee_pool, base_fee, base_reserve)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare ledgers: %w", err)
	}
	for _, r := range s.ledgers {
		if err := b.Append(r.LedgerSeq, r.CloseTime, r.LedgerHash, r.PrevHash, r.ProtocolVersion, r.BucketListHash, r.TxCount, r.OpCount, r.SorobanEventCount, r.ClassicTradeEffectCount, r.TotalCoins, r.FeePool, r.BaseFee, r.BaseReserve); err != nil {
			return fmt.Errorf("clickhouse: append ledger %d: %w", r.LedgerSeq, err)
		}
	}
	return wrapSend(b.Send(), "ledgers")
}

func (s *Sink) flushTxs(ctx context.Context) error {
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.transactions (ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo, soroban_instructions, soroban_disk_read_bytes, soroban_write_bytes, soroban_read_entries, soroban_write_entries, soroban_resource_fee_bid, soroban_nonrefundable_fee, soroban_refundable_fee, soroban_rent_fee, inner_tx_hash, fee_account, fee_bump_fee, inner_result_code)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare transactions: %w", err)
	}
	for _, r := range s.txs {
		if err := b.Append(r.LedgerSeq, r.CloseTime, r.TxHash, r.TxIndex, r.SourceAccount, r.FeeCharged, r.MaxFee, r.OperationCount, r.Successful, r.ResultCode, r.MemoType, r.Memo,
			r.SorobanInstructions, r.SorobanDiskReadBytes, r.SorobanWriteBytes, r.SorobanReadEntries, r.SorobanWriteEntries, r.SorobanResourceFeeBid, r.SorobanNonrefundFee, r.SorobanRefundableFee, r.SorobanRentFee,
			r.InnerTxHash, r.FeeAccount, r.FeeBumpFee, r.InnerResultCode); err != nil {
			return fmt.Errorf("clickhouse: append tx %s: %w", r.TxHash, err)
		}
	}
	return wrapSend(b.Send(), "transactions")
}

func (s *Sink) flushOps(ctx context.Context) error {
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.operations (ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare operations: %w", err)
	}
	for _, r := range s.ops {
		if err := b.Append(r.LedgerSeq, r.CloseTime, r.TxHash, r.TxIndex, r.OpIndex, r.OpType, r.SourceAccount, r.BodyXDR); err != nil {
			return fmt.Errorf("clickhouse: append op %s/%d: %w", r.TxHash, r.OpIndex, err)
		}
	}
	return wrapSend(b.Send(), "operations")
}

func (s *Sink) flushResults(ctx context.Context) error {
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.operation_results (ledger_seq, tx_hash, op_index, result_code, result_xdr)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare operation_results: %w", err)
	}
	for _, r := range s.results {
		if err := b.Append(r.LedgerSeq, r.TxHash, r.OpIndex, r.ResultCode, r.ResultXDR); err != nil {
			return fmt.Errorf("clickhouse: append result %s/%d: %w", r.TxHash, r.OpIndex, err)
		}
	}
	return wrapSend(b.Send(), "operation_results")
}

func (s *Sink) flushParticipants(ctx context.Context) error {
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.operation_participants (account, ledger_seq, close_time, tx_hash, tx_index, op_index)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare operation_participants: %w", err)
	}
	for _, r := range s.participants {
		if err := b.Append(r.Account, r.LedgerSeq, r.CloseTime, r.TxHash, r.TxIndex, r.OpIndex); err != nil {
			return fmt.Errorf("clickhouse: append participant %s/%s/%d: %w", r.Account, r.TxHash, r.OpIndex, err)
		}
	}
	return wrapSend(b.Send(), "operation_participants")
}

func (s *Sink) flushEvents(ctx context.Context) error {
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.contract_events (ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type, topic_count, topic_0_sym, topics_xdr, data_xdr, op_args_xdr, in_successful_call)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare contract_events: %w", err)
	}
	for _, r := range s.events {
		if err := b.Append(r.LedgerSeq, r.CloseTime, r.TxHash, r.OpIndex, r.EventIndex, r.ContractID, r.EventType, r.TopicCount, r.Topic0Sym, r.TopicsXDR, r.DataXDR, r.OpArgsXDR, r.InSuccessfulCall); err != nil {
			return fmt.Errorf("clickhouse: append event %s/%d/%d: %w", r.TxHash, r.OpIndex, r.EventIndex, err)
		}
	}
	return wrapSend(b.Send(), "contract_events")
}

// flushChanges writes stellar.ledger_entry_changes, a live table populated by
// extractEntryChanges and read by StreamEntryChanges.
func (s *Sink) flushChanges(ctx context.Context) error {
	if len(s.changes) == 0 {
		return nil // No entry changes in this batch (e.g. a ledger of pure fee bumps).
	}
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.ledger_entry_changes (ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, account_id, asset, balance, intra_ledger_seq)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare ledger_entry_changes: %w", err)
	}
	for _, r := range s.changes {
		if err := b.Append(r.LedgerSeq, r.CloseTime, r.TxHash, r.OpIndex, r.ChangeIndex, r.ChangeType, r.EntryType, r.KeyXDR, r.EntryXDR, r.AccountID, r.Asset, r.Balance, r.IntraLedgerSeq); err != nil {
			return fmt.Errorf("clickhouse: append change %s/%d/%d: %w", r.TxHash, r.OpIndex, r.ChangeIndex, err)
		}
	}
	return wrapSend(b.Send(), "ledger_entry_changes")
}

func (s *Sink) flushSupplyFlows(ctx context.Context) error {
	if len(s.supplyFlows) == 0 {
		return nil
	}
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO stellar.supply_flows (contract_id, ledger_seq, close_time, tx_hash, op_index, event_index, kind, amount)")
	if err != nil {
		return fmt.Errorf("clickhouse: prepare supply_flows: %w", err)
	}
	for _, r := range s.supplyFlows {
		amt := r.Amount
		if amt == nil {
			amt = big.NewInt(0)
		}
		if err := b.Append(r.ContractID, r.LedgerSeq, r.CloseTime, r.TxHash, r.OpIndex, r.EventIndex, r.Kind, amt); err != nil {
			return fmt.Errorf("clickhouse: append supply_flow %s/%s/%d/%d: %w", r.ContractID, r.TxHash, r.OpIndex, r.EventIndex, err)
		}
	}
	return wrapSend(b.Send(), "supply_flows")
}

func wrapSend(err error, table string) error {
	if err != nil {
		return fmt.Errorf("clickhouse: send %s batch: %w", table, err)
	}
	return nil
}
