package clickhouse

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Protocol-analytics reads (ADR-0038): a protocol's footprint aggregated from contract_events,
// scoped to the registry's contract-id set (never user input, so the IN-list binds directly).
// Windowed variants take sinceLedger (0 = all-time). The raw readers are the FALLBACK; serving is
// contract_events_daily (*Fast). Raw cost is the contract_id bloom skip-index, not FINAL: a BUSY
// contract touches nearly every granule, a QUIET one is pruned and FINAL re-expands it. Do not add
// explorerScanSettings here (max_threads = 4 was ~2x slower for identical I/O); the bound applied
// is protocolRawScanRowCeiling.

// protocolRawScanRowCeiling refuses a raw scan the ExplorerReader connection could not finish anyway (30 s at ~18.6 M rows/s fits ~558 M
// rows); 600 M is a tripwire. read_overflow_mode='throw' REFUSES rather than truncating: callers degrade honestly on error, a LIMIT would
// serve a short answer as complete.
const protocolRawScanRowCeiling = ` SETTINGS max_rows_to_read = 600000000, read_overflow_mode = 'throw'`

// ProtocolEventTypeCount is one (event symbol, count) row of a protocol's event-type distribution.
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

// LakeTipLedger returns the highest ledger_seq in the lake (cheap: small ledgers table), to derive a recent-window cutoff.
func (r *ExplorerReader) LakeTipLedger(ctx context.Context) (uint32, error) {
	var tip uint32
	if err := r.conn.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledgers`).Scan(&tip); err != nil {
		return 0, fmt.Errorf("clickhouse: lake tip: %w", err)
	}
	return tip, nil
}

// lakeWatermarkGapWindow bounds how far below the raw max LakeWatermark looks for a hole: the live sink only drops ledgers near the tip
// (healed by the ~10-minute ch-live-catchup timer), and genesis-to-tip each refresh would hit the substrateWindow memory cap. 10,000
// ledgers (~14h) far exceeds ADR-0041 Decision 3's 1h page threshold.
const lakeWatermarkGapWindow = 10_000

// LakeWatermark returns the lake's CONTIGUOUS captured tip and its close time (surfaced as `as_of_ledger`); the raw max(ledger_seq) can point past a live-sink drop.
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

// ProtocolEventBreakdown returns the event-type distribution (topic[0] symbol to count) for a protocol's contracts, descending; sinceLedger>0 bounds to a recent window, 0 is all-time.
func (r *ExplorerReader) ProtocolEventBreakdown(ctx context.Context, contractIDs []string, sinceLedger uint32) ([]ProtocolEventTypeCount, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	// Group by topic[0]'s denormalized symbol. Where topic[0] is not a Symbol the lake leaves
	// topic_0_sym empty, so also carry raw topic[1] and topic[0] XDR for scanEventBreakdown to
	// recover the name (Soroswap keeps it in topic[1]; Phoenix's topic[1] is a per-field name that
	// must not be split on). The if() splits only the empty bucket.
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

// protocolEventBreakdownQuery builds the raw breakdown query, split out so its shape (FINAL, ledger bound, row ceiling) is unit-testable.
// FINAL is REQUIRED: contract_events is ReplacingMergeTree(ingested_at) with large unmerged duplicates (+45% on a busy contract); LIMIT 1
// BY cannot replace it (ingested_at is 1 s, no same-second tie-break).
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

// effectiveEventName resolves the display label: topic_0_sym; else topic[1] as a Symbol under a String-marker topic[0] (Soroswap;
// Symbol-ONLY so it cannot match Phoenix's String field names); else topic[0] as Symbol or String (Phoenix). Returns "" when none yields
// a name (folded into "untyped" upstream).
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

// ProtocolDailyActivity returns ascending daily event counts from sinceLedger (>0 required), complete days only so the partial current day does not render as a phantom cliff.
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

// ProtocolContractActivity returns per-contract event counts and last-seen from sinceLedger (>0 required, an all-time scan blows the 30 s budget), descending, complete days only.
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

// contract_events_daily fast paths. The pre-aggregation (deploy/clickhouse/tier1_schema.sql) holds
// per-day uniqCombined(17) states: uniqExact's unbounded hash set blew the merge memory budget.
// uniqCombined still dedups the natural key (ledger_seq, tx_hash, op_index, event_index), avoiding
// a Summing MV's retry overcount, at ~0.1-0.5% error. Callers probe DailyActivityAvailable and
// fall back to the raw readers.

// DailyActivityAvailable reports whether the pre-aggregation exists with rows; definitive=true only when the server ANSWERED, so a transport error, deadline or empty table must not be cached.
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

// ProtocolDailyActivityFast is ProtocolDailyActivity over the pre-aggregation; sinceDay bounds the window at day grain.
func (r *ExplorerReader) ProtocolDailyActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolDailyPoint, error) {
	return memoProtocolDaily(&r.protocolDaily, "protocolDailyActivityFast", contractIDs, sinceDay, func() ([]ProtocolDailyPoint, error) {
		return r.protocolDailyActivityFast(ctx, contractIDs, sinceDay)
	})
}

func (r *ExplorerReader) protocolDailyActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolDailyPoint, error) {
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

// ProtocolContractActivityFast is ProtocolContractActivity over the pre-aggregation at day grain, replacing the raw FINAL scan that blew the 2 GiB per-query limit; counts match within the rollup's uniqCombined error.
func (r *ExplorerReader) ProtocolContractActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolContractActivity, error) {
	return memoProtocolDaily(&r.protocolDaily, "protocolContractActivityFast", contractIDs, sinceDay, func() ([]ProtocolContractActivity, error) {
		return r.protocolContractActivityFast(ctx, contractIDs, sinceDay)
	})
}

func (r *ExplorerReader) protocolContractActivityFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolContractActivity, error) {
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

// ProtocolEventBreakdownFast is ProtocolEventBreakdown over the pre-aggregation; t1_xdr/t0_xdr carry the raw topics for name recovery (effectiveEventName).
func (r *ExplorerReader) ProtocolEventBreakdownFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolEventTypeCount, error) {
	return memoProtocolDaily(&r.protocolDaily, "protocolEventBreakdownFast", contractIDs, sinceDay, func() ([]ProtocolEventTypeCount, error) {
		return r.protocolEventBreakdownFast(ctx, contractIDs, sinceDay)
	})
}

func (r *ExplorerReader) protocolEventBreakdownFast(ctx context.Context, contractIDs []string, sinceDay time.Time) ([]ProtocolEventTypeCount, error) {
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

// protocolDailyMaxAge bounds how long a backfilled past day can go unseen; the reads stop before today, so they otherwise change only
// when the UTC day turns.
const (
	protocolDailyMaxAge  = 2 * time.Hour
	protocolDailyMemoMax = 512
)

type protocolDailyEntry struct {
	v   any
	day string
	at  time.Time
}

type protocolDailyMemo struct {
	mu  sync.Mutex
	m   map[[32]byte]protocolDailyEntry
	now func() time.Time
}

// memoProtocolDaily answers read from the memo while it is from today (UTC) and under protocolDailyMaxAge. The key is order-free in
// contractIDs, since each read binds them as an IN-list. Errors are not memoised.
func memoProtocolDaily[T any](m *protocolDailyMemo, kind string, contractIDs []string, sinceDay time.Time, read func() ([]T, error)) ([]T, error) {
	if len(contractIDs) == 0 {
		return nil, nil
	}
	ids := slices.Clone(contractIDs)
	slices.Sort(ids)
	key := sha256.Sum256([]byte(kind + "\x00" + sinceDay.UTC().Format(time.RFC3339) + "\x00" + strings.Join(ids, ",")))
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	today := now.UTC().Format(time.DateOnly)
	m.mu.Lock()
	e, ok := m.m[key]
	m.mu.Unlock()
	if ok && e.day == today && now.Sub(e.at) < protocolDailyMaxAge {
		return slices.Clone(e.v.([]T)), nil
	}
	v, err := read()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.m == nil || len(m.m) >= protocolDailyMemoMax {
		m.m = make(map[[32]byte]protocolDailyEntry)
	}
	m.m[key] = protocolDailyEntry{v: slices.Clone(v), day: today, at: now}
	m.mu.Unlock()
	return v, nil
}
