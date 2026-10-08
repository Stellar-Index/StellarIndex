package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Protocol-analytics reads (ADR-0038): a protocol's footprint aggregated from contract_events,
// scoped to the registry's contract-id set (never user input, so the IN-list binds directly). All
// filter event_type='contract' and lean on the contract_id bloom skip-index. Windowed variants take
// sinceLedger (0 = all-time); bounding by ledger_seq prunes partitions.
// The raw readers are the FALLBACK; the serving path is contract_events_daily (*Fast), used when
// that table is empty or a fast read errors. Raw cost is the skip index, not FINAL: contract_id is
// a bloom over a (ledger_seq, tx_hash, op_index, event_index) key, so a BUSY contract (125M
// events/90d) touches nearly every granule and FINAL re-expands nothing (breakdown 58 s, 1.09 B
// rows). On a QUIET contract the bloom prunes 129x and FINAL re-expands it (the TxOutcomesByHash
// trap), still 1.5 s.
// Not fixed by an upper `ledger_seq <= tip` bound (the scan already ends at the tip), nor by
// explorerScanSettings: max_threads = 4 measured ~2x SLOWER for identical I/O, since the fan-out it
// targets is a ledger_entries_current part-layout effect. Do not add it. What is applied is
// protocolRawScanRowCeiling.

// protocolRawScanRowCeiling refuses a raw scan the ExplorerReader connection could not finish
// anyway (max_execution_time 30 s; the breakdown reads ~18.6 M rows/s idle, so ~558 M rows fit, and
// a busy protocol's window is 1.09 B). Without it the read decompresses ~190 GiB before the kill,
// three at once per page build.
// 600 M is that budget rounded up: a tripwire against the impossible, not a policy bound.
// read_overflow_mode='throw' REFUSES rather than truncating; callers already degrade honestly on
// error, whereas a LIMIT would serve a short answer as complete. Same posture as
// recentOperationsCursorRowCeiling (explorer_reader.go).
const protocolRawScanRowCeiling = ` SETTINGS max_rows_to_read = 600000000, read_overflow_mode = 'throw'`

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

// LakeTipLedger returns the highest ledger_seq in the lake (cheap: small ledgers table), to derive
// a recent-window cutoff.
func (r *ExplorerReader) LakeTipLedger(ctx context.Context) (uint32, error) {
	var tip uint32
	if err := r.conn.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledgers`).Scan(&tip); err != nil {
		return 0, fmt.Errorf("clickhouse: lake tip: %w", err)
	}
	return tip, nil
}

// lakeWatermarkGapWindow bounds how far below the raw max LakeWatermark looks for a hole. The live
// sink only drops ledgers near the tip, healed by the ~10-minute ch-live-catchup timer; certifying
// genesis-to-tip on every cached refresh would hit the memory cap described at substrateWindow.
// 10,000 ledgers (~14h) far exceeds the heal time and ADR-0041 Decision 3's 1h page threshold.
const lakeWatermarkGapWindow = 10_000

// LakeWatermark returns the lake's CONTIGUOUS captured tip (highest ledger with no hole within
// lakeWatermarkGapWindow below the raw max) and its close time (ADR-0041 Decision 4). Handlers
// surface it as `as_of_ledger` and compare the close time to now for `flags.stale`.
// The raw max(ledger_seq) can point PAST a live-sink drop, and a consumer would read `as_of_ledger`
// as "everything up to here is captured". contiguousWatermarkOn is the same query the projector
// clamps to; the API caches the result (lakeWatermarkTTL).
func (r *ExplorerReader) LakeWatermark(ctx context.Context) (uint32, time.Time, error) {
	var rawTip uint32
	if err := r.conn.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledgers`).Scan(&rawTip); err != nil {
		return 0, time.Time{}, fmt.Errorf("clickhouse: lake watermark: raw tip: %w", err)
	}
	if rawTip == 0 {
		return 0, time.Time{}, nil
	}
	from := uint32(1)
	if rawTip > lakeWatermarkGapWindow {
		from = rawTip - lakeWatermarkGapWindow
	}
	wm, err := contiguousWatermarkOn(ctx, r.conn, from)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("clickhouse: lake watermark: %w", err)
	}
	if wm == 0 {
		return 0, time.Time{}, nil
	}
	var closedAt time.Time
	const closedAtQ = `SELECT max(close_time) FROM stellar.ledgers WHERE ledger_seq = ?`
	if err := r.conn.QueryRow(ctx, closedAtQ, wm).Scan(&closedAt); err != nil {
		return 0, time.Time{}, fmt.Errorf("clickhouse: lake watermark: close time at %d: %w", wm, err)
	}
	return wm, closedAt, nil
}

// ProtocolEventBreakdown returns the event-type distribution (topic[0] symbol to count) for a
// protocol's contracts, descending. sinceLedger>0 bounds to a recent window; 0 is all-time.
func (r *ExplorerReader) ProtocolEventBreakdown(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]ProtocolEventTypeCount, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	// Group by topic[0]'s denormalized symbol. Where topic[0] is not a Symbol the lake leaves
	// topic_0_sym empty, so also carry raw topic[1] and topic[0] XDR for scanEventBreakdown to
	// recover the name: Soroswap puts it in topic[1] ([String("SoroswapPair"), Symbol(name)]);
	// Phoenix emits topic[0] itself as a String and topic[1] is a per-field name we must not split
	// on. The if() splits only the empty bucket.
	args := []any{contractIDs}
	if sinceLedger > 0 {
		args = append(args, sinceLedger)
	}
	rows, err := r.conn.Query(ctx, protocolEventBreakdownQuery(sinceLedger > 0), args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol event breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// Aggregated by effectiveEventName; unrecoverable names are dropped here and protocols.go folds
	// them into "untyped".
	return scanEventBreakdown(rows)
}

// protocolEventBreakdownQuery builds the raw breakdown query, split out so the shape (FINAL, the
// ledger bound, the row ceiling) is unit-testable; losing any is silent and expensive.
// FINAL is REQUIRED: contract_events is ReplacingMergeTree(ingested_at) and unmerged duplicates are
// large (busy contract 224.6 M vs 154.9 M with FINAL, +45%; quiet +94%), so dropping it overstates
// every headline count. A LIMIT 1 BY rewrite would not help: ingested_at is DateTime (1 s), so it
// cannot break a same-second tie.
func protocolEventBreakdownQuery(windowed bool) string {
	q := `SELECT topic_0_sym,
		       if(topic_0_sym = '', topics_xdr[2], '') AS t1,
		       if(topic_0_sym = '', topics_xdr[1], '') AS t0,
		       count() AS c
		-- Complete days only: the daily activity series excludes the current
		-- (partial) day (UXP-16 phantom cliff), and EventsTotal is derived
		-- from that series — the breakdown must share the bound or
		-- sum(EventBreakdown) != EventsTotal breaks the reconcile.
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract'
		  AND close_time < toStartOfDay(now())`
	if windowed {
		q += ` AND ledger_seq >= ?`
	}
	return q + ` GROUP BY topic_0_sym, t1, t0 ORDER BY c DESC LIMIT 200` +
		protocolRawScanRowCeiling
}

// scanEventBreakdown folds (topic_0_sym, topic1_xdr, topic0_xdr, count) rows into named counts,
// shared by the raw and daily paths so name recovery is identical.
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

// effectiveEventName resolves the display label, in priority order:
//
//  1. topic_0_sym (topic[0] is a Symbol).
//  2. topic[1] as a Symbol, for a namespace-marker String topic[0] (Soroswap).
//     Symbol-ONLY so it cannot match Phoenix's String field names.
//  3. topic[0] as a Symbol or String (Phoenix action names).
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

// decodeTopicSymbol returns the Symbol in a base64-XDR ScVal; ok=false when empty or not a Symbol.
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

// decodeTopicName is decodeTopicSymbol that also accepts Strings: Phoenix topics have field names
// with spaces, which are not valid Symbols, so the whole contract uses Strings.
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

// FINAL and the row ceiling are load-bearing, see protocolEventBreakdownQuery and
// protocolRawScanRowCeiling.
const protocolDailyActivityQuery = `SELECT toString(toDate(close_time)) AS d, count() AS c
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract' AND ledger_seq >= ?
		  AND close_time < toStartOfDay(now())
		GROUP BY d ORDER BY d ASC` + protocolRawScanRowCeiling

// ProtocolDailyActivity returns daily event counts from sinceLedger forward (>0 required),
// ascending. Complete days only: the partial current day would render as a phantom cliff, and
// ProtocolEventBreakdown shares the bound so sum(breakdown) reconciles with the series total.
func (r *ExplorerReader) ProtocolDailyActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]ProtocolDailyPoint, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	rows, err := r.conn.Query(ctx, protocolDailyActivityQuery, contractIDs, sinceLedger)
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

// FINAL and the row ceiling are load-bearing (see protocolEventBreakdownQuery). Complete days only,
// so the roster counts do not wobble down on re-read as the current day accumulates.
const protocolContractActivityQuery = `SELECT contract_id, count() AS c, max(close_time) AS last_seen
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract' AND ledger_seq >= ?
		  AND close_time < toStartOfDay(now())
		GROUP BY contract_id ORDER BY c DESC LIMIT 1000` + protocolRawScanRowCeiling

// ProtocolContractActivity returns per-contract event counts and last-seen for the roster from
// sinceLedger forward (>0 required: an all-time scan blows the 30 s budget), descending, complete
// days only.
func (r *ExplorerReader) ProtocolContractActivity(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]ProtocolContractActivity, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	rows, err := r.conn.Query(ctx, protocolContractActivityQuery, contractIDs, sinceLedger)
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

// contract_events_daily fast paths. The pre-aggregation (deploy/clickhouse/tier1_schema.sql)
// replaces the ~15 s raw scans with per-day uniqCombined(17) states: uniqExact's unbounded hash set
// blew the merge memory budget. uniqCombined still dedups the natural key (ledger_seq, tx_hash,
// op_index, event_index), avoiding a Summing MV's retry/re-derive overcount, in bounded memory at
// ~0.1-0.5% error. Callers probe DailyActivityAvailable and fall back to the raw readers.

// DailyActivityAvailable reports whether the pre-aggregation exists with rows. definitive=true only
// when the server ANSWERED: table missing (isSchemaAbsent) or rows found. A transport error,
// deadline or blip is not an answer; callers must not cache it, or one hiccup would latch the raw
// scans for the process lifetime (see schemaProbe). An empty table is non-definitive too: not
// backfilled yet, and the LIMIT 1 probe is cheap to repeat.
func (r *ExplorerReader) DailyActivityAvailable(ctx context.Context) (available, definitive bool) {
	rows, err := r.conn.Query(ctx,
		`SELECT 1 FROM stellar.contract_events_daily LIMIT 1`)
	if err != nil {
		if isSchemaAbsent(err) {
			return false, true
		}
		return false, false
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return true, true
	}
	return false, false
}

// ProtocolDailyActivityFast is ProtocolDailyActivity over the pre-aggregation; sinceDay bounds the
// window at day grain.
func (r *ExplorerReader) ProtocolDailyActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolDailyPoint, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT toString(day) AS d, uniqCombinedMerge(17)(events) AS c
		FROM stellar.contract_events_daily
		WHERE contract_id IN (?) AND event_type = 'contract' AND day >= ?
		  AND day < toDate(now())
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

// ProtocolContractActivityFast is ProtocolContractActivity over the pre-aggregation, at day grain
// (last-seen is day-precise). It replaces the raw FINAL per-contract scan, whose merge-on-read blew
// the 2 GiB per-query limit (Code 241), which is the "unavailable" verdict on /v1/protocols/{name}.
// Counts match the raw path within the rollup's uniqCombined error.
func (r *ExplorerReader) ProtocolContractActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolContractActivity, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT contract_id,
		       toUInt64(uniqCombinedMerge(17)(events)) AS c,
		       toDateTime(max(day)) AS last_seen
		FROM stellar.contract_events_daily
		WHERE contract_id IN (?) AND event_type = 'contract' AND day >= ?
		  AND day < toDate(now())
		GROUP BY contract_id ORDER BY c DESC LIMIT 1000`
	rows, err := r.conn.Query(ctx, q, contractIDs, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol contract activity (fast): %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProtocolContractActivity
	for rows.Next() {
		var a ProtocolContractActivity
		if err := rows.Scan(&a.ContractID, &a.Events, &a.LastSeen); err != nil {
			return nil, fmt.Errorf("clickhouse: scan contract activity (fast): %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ProtocolEventBreakdownFast is ProtocolEventBreakdown over the pre-aggregation; t1_xdr/t0_xdr
// carry the raw topics for name recovery (effectiveEventName).
func (r *ExplorerReader) ProtocolEventBreakdownFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolEventTypeCount, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT topic_0_sym, t1_xdr, t0_xdr, toUInt64(uniqCombinedMerge(17)(events)) AS c
		FROM stellar.contract_events_daily
		WHERE contract_id IN (?) AND event_type = 'contract' AND day >= ?
		  AND day < toDate(now())
		GROUP BY topic_0_sym, t1_xdr, t0_xdr ORDER BY c DESC LIMIT 200`
	rows, err := r.conn.Query(ctx, q, contractIDs, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol event breakdown (fast): %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanEventBreakdown(rows)
}
