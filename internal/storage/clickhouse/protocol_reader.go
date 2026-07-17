package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Protocol-analytics reads (ADR-0038 explorer / per-protocol pages). These
// aggregate a protocol's on-chain footprint generically from the certified
// lake's contract_events, scoped to the protocol's contract-id set (from the
// registry). Because the set comes from OUR registry — never user input — the
// IN-list is safe to bind directly. All reads filter event_type='contract'
// (drop diagnostic/system events) and lean on the contract_id bloom skip-index.
//
// Windowed variants take sinceLedger (0 = all-time): bounding by ledger_seq
// prunes partitions (PARTITION BY intDiv(ledger_seq,1e6)), which is what keeps
// the daily-activity + breakdown queries fast on the 12B-row table.

// ProtocolEventTypeCount is one (event symbol → count) row of a protocol's
// event-type distribution.
type ProtocolEventTypeCount struct {
	EventType string // topic[0] symbol (e.g. "swap", "deposit", "new_pair")
	Count     uint64
}

// ProtocolDailyPoint is one day of a protocol's event-activity series.
type ProtocolDailyPoint struct {
	Date   string // YYYY-MM-DD (UTC)
	Events uint64
}

// ProtocolContractActivity is per-contract rollup for a protocol's roster.
type ProtocolContractActivity struct {
	ContractID string
	Events     uint64
	LastSeen   time.Time
}

// LakeTipLedger returns the highest ledger_seq in the lake (cheap — small
// ledgers table). Used to derive a recent-window cutoff for the windowed
// protocol-analytics reads.
func (r *ExplorerReader) LakeTipLedger(ctx context.Context) (uint32, error) {
	var tip uint32
	if err := r.conn.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledgers`).Scan(&tip); err != nil {
		return 0, fmt.Errorf("clickhouse: lake tip: %w", err)
	}
	return tip, nil
}

// LakeWatermark returns the lake's captured tip — the highest ledger_seq in
// stellar.ledgers plus that ledger's close time (ADR-0041 Decision 4: lake
// reads carry their watermark). API handlers surface the ledger as
// `as_of_ledger` and compare the close time against now for the
// `flags.stale` signal. Cheap (small ledgers table), but the API layer still
// caches it (v1's lakeWatermarkTTL) so per-request reads never fan out to
// ClickHouse.
func (r *ExplorerReader) LakeWatermark(ctx context.Context) (uint32, time.Time, error) {
	var (
		tip      uint32
		closedAt time.Time
	)
	const q = `SELECT max(ledger_seq), max(close_time) FROM stellar.ledgers`
	if err := r.conn.QueryRow(ctx, q).Scan(&tip, &closedAt); err != nil {
		return 0, time.Time{}, fmt.Errorf("clickhouse: lake watermark: %w", err)
	}
	return tip, closedAt, nil
}

// ProtocolEventBreakdown returns the event-type distribution (topic[0] symbol →
// count) for a protocol's contracts. sinceLedger>0 bounds to a recent window;
// 0 is all-time. Descending by count.
func (r *ExplorerReader) ProtocolEventBreakdown(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]ProtocolEventTypeCount, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	// Group by topic[0]'s denormalized symbol. For events whose topic[0] is
	// NOT a Symbol — the lake leaves topic_0_sym empty — also carry the raw
	// topic[1] AND topic[0] XDR so scanEventBreakdown can recover the real
	// event name at read time:
	//   - Soroswap: [String("SoroswapPair"), Symbol(name)] — the event name
	//     (swap/sync/deposit/withdraw/skim) lives in topic[1].
	//   - Phoenix:  [String("swap"), String("<field>")] — the ACTION name is
	//     topic[0] itself, emitted as a String (its field names have spaces,
	//     so the whole contract uses ScvString topics), and topic[1] is a
	//     per-field name we do NOT want to split on.
	// The if() keeps Symbol-topic[0] events grouped by their symbol (t0/t1
	// coalesced away) and only splits the empty bucket by (t1, t0).
	q := `SELECT topic_0_sym,
		       if(topic_0_sym = '', topics_xdr[2], '') AS t1,
		       if(topic_0_sym = '', topics_xdr[1], '') AS t0,
		       count() AS c
		-- FINAL: stellar.contract_events is ReplacingMergeTree(ingested_at); an
		-- un-merged re-ingested event double-counts here until a merge (audit
		-- C2-12). Contract-scoped, so FINAL stays bounded.
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract'`
	args := []any{contractIDs}
	if sinceLedger > 0 {
		q += ` AND ledger_seq >= ?`
		args = append(args, sinceLedger)
	}
	q += ` GROUP BY topic_0_sym, t1, t0 ORDER BY c DESC LIMIT 200`
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol event breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// Aggregate by effective event name (see effectiveEventName). Rows whose
	// name can't be recovered from any topic are dropped here — protocols.go's
	// reconcile folds them into the "untyped" remainder against the total.
	return scanEventBreakdown(rows)
}

// scanEventBreakdown aggregates (topic_0_sym, topic1_xdr, topic0_xdr, count)
// rows into named event-type counts — shared by the raw-scan and daily-preagg
// paths so the topic name-recovery behaves identically on both.
func scanEventBreakdown(rows driver.Rows) ([]ProtocolEventTypeCount, error) {
	byName := make(map[string]uint64)
	for rows.Next() {
		var sym, t1, t0 string
		var c uint64
		if err := rows.Scan(&sym, &t1, &t0, &c); err != nil {
			return nil, fmt.Errorf("clickhouse: scan event breakdown: %w", err)
		}
		name := effectiveEventName(sym, t1, t0)
		if name == "" {
			continue
		}
		byName[name] += c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ProtocolEventTypeCount, 0, len(byName))
	for name, c := range byName {
		out = append(out, ProtocolEventTypeCount{EventType: name, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

// effectiveEventName resolves the display label for a contract event from its
// denormalized topic[0] Symbol plus the raw topic[1]/topic[0] XDR (populated
// only when topic[0] isn't a Symbol). Priority, chosen so no currently-labeled
// event regresses:
//
//  1. topic_0_sym — topic[0] is a Symbol (comet "POOL", aquarius "trade", …).
//  2. topic[1] decoded as a Symbol — topic[0] is a namespace-marker String and
//     the event name lives in topic[1] (Soroswap: [String("SoroswapPair"),
//     Symbol("swap")]). Symbol-ONLY on purpose: it must NOT match Phoenix's
//     String field names, so those fall through to (3).
//  3. topic[0] decoded as a Symbol OR String — topic[0] IS the action name but
//     was emitted as a non-Symbol scval (Phoenix: [String("swap"),
//     String("<field>")]). This is the generalization of the Soroswap
//     special-case: it labels phoenix swap/provide_liquidity/… instead of
//     dropping them into "untyped".
//
// Returns "" when no topic yields a name (folded into "untyped" upstream).
func effectiveEventName(topic0Sym, topic1XDR, topic0XDR string) string {
	if topic0Sym != "" {
		return topic0Sym
	}
	if dec, ok := decodeTopicSymbol(topic1XDR); ok {
		return dec
	}
	if dec, ok := decodeTopicName(topic0XDR); ok {
		return dec
	}
	return ""
}

// decodeTopicSymbol decodes a base64-XDR ScVal and returns its Symbol string,
// ok=false when empty or not a Symbol. Recovers the event name from topic[1]
// for protocols whose topic[0] is a non-Symbol marker (Soroswap).
func decodeTopicSymbol(b64 string) (string, bool) {
	if b64 == "" {
		return "", false
	}
	var v xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(b64, &v); err != nil {
		return "", false
	}
	if s, ok := v.GetSym(); ok {
		return string(s), true
	}
	return "", false
}

// decodeTopicName decodes a base64-XDR ScVal topic and returns its name when it
// is a Symbol OR a String, ok=false otherwise. Unlike decodeTopicSymbol
// (Symbol-only) it also accepts Strings, because some protocols emit their
// event/action name as an ScvString rather than an ScvSymbol — Phoenix's
// topics carry field names with spaces (e.g. "actual received amount"), which
// aren't valid Symbols, so the whole contract uses Strings, topic[0] included.
func decodeTopicName(b64 string) (string, bool) {
	if b64 == "" {
		return "", false
	}
	var v xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(b64, &v); err != nil {
		return "", false
	}
	if s, ok := v.GetSym(); ok {
		return string(s), true
	}
	if s, ok := v.GetStr(); ok {
		return string(s), true
	}
	return "", false
}

// ProtocolDailyActivity returns daily contract-event counts for a protocol's
// contracts from sinceLedger forward (sinceLedger>0 required for performance —
// the caller passes tip − window). Ascending by date.
func (r *ExplorerReader) ProtocolDailyActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]ProtocolDailyPoint, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT toString(toDate(close_time)) AS d, count() AS c
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract' AND ledger_seq >= ?
		GROUP BY d ORDER BY d ASC`
	rows, err := r.conn.Query(ctx, q, contractIDs, sinceLedger)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol daily activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProtocolDailyPoint
	for rows.Next() {
		var p ProtocolDailyPoint
		if err := rows.Scan(&p.Date, &p.Events); err != nil {
			return nil, fmt.Errorf("clickhouse: scan daily activity: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProtocolContractActivity returns per-contract event counts + last-seen for a
// protocol's roster, scoped to sinceLedger forward (>0 required — bounding by
// ledger_seq prunes partitions; an all-time scan over the 12B-row table blows
// the 30s read budget for active protocols). Descending by event count.
func (r *ExplorerReader) ProtocolContractActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]ProtocolContractActivity, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT contract_id, count() AS c, max(close_time) AS last_seen
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract' AND ledger_seq >= ?
		GROUP BY contract_id ORDER BY c DESC LIMIT 1000`
	rows, err := r.conn.Query(ctx, q, contractIDs, sinceLedger)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol contract activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProtocolContractActivity
	for rows.Next() {
		var a ProtocolContractActivity
		if err := rows.Scan(&a.ContractID, &a.Events, &a.LastSeen); err != nil {
			return nil, fmt.Errorf("clickhouse: scan contract activity: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── contract_events_daily fast paths (BACKLOG #43) ──────────────────────
//
// The daily pre-aggregation (deploy/clickhouse/tier1_schema.sql,
// contract_events_daily + its MV) collapses the ~15s raw scans behind
// /v1/protocols/{name} into millisecond reads over per-day uniqCombined(17)
// states (docs/architecture/contract-events-daily-redesign.md — replaced
// uniqExact 2026-07-09 after its unbounded per-state hash set blew the
// ClickHouse merge memory budget on r1). uniqCombined still dedups a
// group's natural key (ledger_seq, tx_hash, op_index, event_index) — so a
// Summing MV's live-sink-retry / ch-rebuild-re-derive overcount risk is
// still avoided — but in bounded memory, at the cost of ~0.1-0.5% count
// error (imperceptible: the explorer only ever renders these numbers
// compact-formatted, e.g. "1.2M events"). Callers probe
// DailyActivityAvailable once and fall back to the raw readers when
// the table hasn't been created/backfilled on a deployment yet.

// DailyActivityAvailable reports whether the pre-aggregation exists
// (and has any rows) on this ClickHouse.
func (r *ExplorerReader) DailyActivityAvailable(ctx context.Context) bool {
	rows, err := r.conn.Query(ctx,
		`SELECT 1 FROM stellar.contract_events_daily LIMIT 1`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	return rows.Next()
}

// ProtocolDailyActivityFast is ProtocolDailyActivity over the daily
// pre-aggregation. sinceDay bounds the window (callers convert their
// ledger window to days; day granularity is the table's grain).
func (r *ExplorerReader) ProtocolDailyActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolDailyPoint, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT toString(day) AS d, uniqCombinedMerge(17)(events) AS c
		FROM stellar.contract_events_daily
		WHERE contract_id IN (?) AND event_type = 'contract' AND day >= ?
		GROUP BY day ORDER BY day ASC`
	rows, err := r.conn.Query(ctx, q, contractIDs, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol daily activity (fast): %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProtocolDailyPoint
	for rows.Next() {
		var p ProtocolDailyPoint
		if err := rows.Scan(&p.Date, &p.Events); err != nil {
			return nil, fmt.Errorf("clickhouse: scan daily activity (fast): %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProtocolEventBreakdownFast is ProtocolEventBreakdown over the daily
// pre-aggregation, preserving the topic name-recovery for events whose
// topic[0] isn't a Symbol (the t1_xdr / t0_xdr columns carry the raw
// topic[1] / topic[0] XDR — see effectiveEventName).
func (r *ExplorerReader) ProtocolEventBreakdownFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolEventTypeCount, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT topic_0_sym, t1_xdr, t0_xdr, toUInt64(uniqCombinedMerge(17)(events)) AS c
		FROM stellar.contract_events_daily
		WHERE contract_id IN (?) AND event_type = 'contract' AND day >= ?
		GROUP BY topic_0_sym, t1_xdr, t0_xdr ORDER BY c DESC LIMIT 200`
	rows, err := r.conn.Query(ctx, q, contractIDs, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol event breakdown (fast): %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanEventBreakdown(rows)
}
