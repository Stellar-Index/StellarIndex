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
//
// ── What "contract-scoped" actually costs (measured on r1, 2026-09-03) ──
//
// These three raw readers are the FALLBACK path. The serving path is the
// contract_events_daily pre-aggregation below (*Fast); the raw readers run
// only when that table is empty (a lake restored ahead of its rollup) or a
// fast read errored. Their cost, cold, `use_query_condition_cache = 0`, over
// the caller's real 90-day window (tip 64,249,968 → sinceLedger 62,694,768):
//
//	                        BUSY contract (125M ev/90d)   QUIET contract (200 ev/90d)
//	EventBreakdown          58,460 ms / 1.09 B / 373 GiB   3,622 ms / 47.0 M / 17.2 GiB
//	DailyActivity           18,279 ms / 1.09 B / 117 GiB
//	ContractActivity        20,111 ms / 1.09 B / 117 GiB   1,585 ms / 47.0 M / 6.05 GiB
//
// The reason is the skip index, not FINAL. `contract_id` is a bloom_filter
// skip index over a sorting key of (ledger_seq, tx_hash, op_index,
// event_index), so a BUSY contract touches nearly every granule in the
// window: 176,887 PK-selected → 132,830 after the bloom, a 1.33x prune.
// FINAL's PrimaryKeyExpand then re-expands 132,830 → 132,830, i.e. it costs
// NOTHING on a busy contract, because there was nothing left to add back.
// (On a QUIET contract the numbers invert: the bloom prunes 132,833 → 1,031,
// a 129x win, and FINAL re-expands that to 5,693 — the TxOutcomesByHash
// trap, 5.5x, on a read that is 1.5 s in absolute terms.)
//
// Two things this is NOT, both measured before being ruled out:
//
//   - It is NOT fixed by passing the caller's upper bound. The caller holds
//     the lake tip, but `AND ledger_seq <= tip` moves granule selection
//     132,830 → 132,829. The scan already ends at the tip; there is no
//     unbounded top end to close.
//   - It is NOT fixed by explorerScanSettings. Pinning max_threads = 4 on
//     the breakdown measured 105,649 ms vs 58,460 ms unpinned, for 373.33
//     vs 373.41 GiB — nearly 2x SLOWER for identical I/O. The 40x-bytes
//     fan-out that clause exists for (explorer_scan_settings_test.go) is a
//     ledger_entries_current part-layout effect and does not apply here.
//     Do not "fix" these queries by adding it.
//
// What IS applied is protocolRawScanRowCeiling — see its doc.

// protocolRawScanRowCeiling refuses a raw protocol-analytics scan that the
// ExplorerReader connection could not have served anyway, instead of letting
// it burn the lake for 30 s and then die.
//
// The connection pins `max_execution_time: 30` and ReadTimeout 30 s
// (NewExplorerReaderAuth). The slowest of the three reads is the breakdown,
// measured at 1.09 B rows in 58,460 ms = 18.6 M rows/s on an IDLE r1 — so 30 s
// buys at most ~558 M rows, and a busy protocol's 1.09 B-row window is roughly
// 2x more than the connection can ever finish. Today that read is killed at
// the 30 s mark having already decompressed ~190 GiB at 86 threads, three of
// them concurrently per page build (enrichProtocolAnalytics runs the series,
// breakdown and roster fills in parallel) — 605 GiB and ~250 threads spent to
// produce nothing. That is the "57s / 3.2B-row scans starved the customer API"
// event recorded on ProtocolContractActivityFast below.
//
// 600 M is that 558 M measured budget rounded up: a tripwire against the
// impossible, not a policy bound on which protocols get analytics. Measured
// effect on the busy contract's breakdown: 58,460 ms / 373 GiB → 179 ms / 0 B.
// The quiet and mid-tier windows (47 M and 508 M rows) are under it and are
// unaffected.
//
// read_overflow_mode='throw' REFUSES; it does not truncate. That distinction
// is the whole point — every caller (fillProtocolSeries / fillProtocolBreakdown
// / fillProtocolContractActivity) already degrades honestly on error, marking
// the view's analytics status, whereas a LIMIT would have served a silently
// short answer as if it were complete. Same posture and same server code (158)
// as recentOperationsCursorRowCeiling in explorer_reader.go.
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
	args := []any{contractIDs}
	if sinceLedger > 0 {
		args = append(args, sinceLedger)
	}
	rows, err := r.conn.Query(ctx, protocolEventBreakdownQuery(sinceLedger > 0), args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol event breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// Aggregate by effective event name (see effectiveEventName). Rows whose
	// name can't be recovered from any topic are dropped here — protocols.go's
	// reconcile folds them into the "untyped" remainder against the total.
	return scanEventBreakdown(rows)
}

// protocolEventBreakdownQuery builds the raw event-type-distribution query.
// Split out (like contractEventsFilteredQuery / recentOperationsQuery) so the
// shape — FINAL, the ledger bound, the row ceiling — is unit-testable without
// a ClickHouse server; losing any of them is silent and expensive.
//
// FINAL is REQUIRED, and not for the theoretical reason the old comment gave.
// stellar.contract_events is ReplacingMergeTree(ingested_at) and its unmerged
// duplicates are enormous on r1: the busy contract measured 154,915,417 events
// with FINAL vs 224,616,719 without (+45%), and the quiet one 200 vs 388
// (+94%). Dropping FINAL would overstate every protocol's headline event count
// by tens of percent. (The DAT-10 tie-break argument applies here too —
// `ingested_at` is DateTime, one-second resolution, so an `ORDER BY
// ingested_at DESC LIMIT 1 BY` rewrite cannot break a same-second re-ingest
// tie — but the 45-94% overcount is the decisive fact, not the tie.)
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

// protocolDailyActivityQuery — FINAL and the row ceiling are load-bearing for
// the reasons on protocolEventBreakdownQuery / protocolRawScanRowCeiling.
const protocolDailyActivityQuery = `SELECT toString(toDate(close_time)) AS d, count() AS c
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract' AND ledger_seq >= ?
		  AND close_time < toStartOfDay(now())
		GROUP BY d ORDER BY d ASC` + protocolRawScanRowCeiling

// ProtocolDailyActivity returns daily contract-event counts for a protocol's
// contracts from sinceLedger forward (sinceLedger>0 required for performance —
// the caller passes tip − window). Ascending by date. Complete days only:
// the current (still-accumulating) day is excluded — its partial bucket
// renders as a phantom activity cliff on every daily chart (the UXP-16
// class, audit 2026-07-31). ProtocolEventBreakdown shares the same bound
// so sum(breakdown) keeps reconciling with the series-derived total.
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

// protocolContractActivityQuery — FINAL and the row ceiling are load-bearing
// for the reasons on protocolEventBreakdownQuery / protocolRawScanRowCeiling.
const protocolContractActivityQuery = `SELECT contract_id, count() AS c, max(close_time) AS last_seen
		FROM stellar.contract_events FINAL
		WHERE contract_id IN (?) AND event_type = 'contract' AND ledger_seq >= ?
		GROUP BY contract_id ORDER BY c DESC LIMIT 1000` + protocolRawScanRowCeiling

// ProtocolContractActivity returns per-contract event counts + last-seen for a
// protocol's roster, scoped to sinceLedger forward (>0 required — bounding by
// ledger_seq prunes partitions; an all-time scan over the 12B-row table blows
// the 30s read budget for active protocols). Descending by event count.
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
// (and has any rows) on this ClickHouse. definitive=true only when the
// server actually ANSWERED the question — the table is missing (a schema
// verdict, see isSchemaAbsent) or the probe found rows. A transport
// error / deadline / resource blip is NOT an answer (definitive=false):
// callers must not cache it, or one ClickHouse hiccup at first probe
// would latch the raw 12B-row scans for the process lifetime (the same
// class as C1-048; see schemaProbe for the founding precedent). An empty
// table is also non-definitive — it exists but hasn't been backfilled
// yet, and the probe is a LIMIT 1 read cheap enough to re-ask.
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

// ProtocolContractActivityFast is ProtocolContractActivity over the daily
// pre-aggregation — per-contract event counts (uniqCombined-merged) +
// last-seen at day grain. It REPLACES the raw `contract_events FINAL`
// per-contract scan, which merge-on-read of the 12.8B-row ReplacingMergeTree
// blew ClickHouse's 2 GiB per-query memory limit (Code 241) — the memory
// kill IS the certified-lake "unavailable" verdict on /v1/protocols/{name},
// and the 57s/3.2B-row scans starved the customer API (the CH-side twin of
// the volume_character regression). sinceDay bounds the window; last-seen is
// therefore day-precise, which is sufficient for the roster's "last active"
// column. Counts match the raw path's `count() … FINAL` (both dedup the
// natural key) within the daily rollup's ~0.1-0.5% uniqCombined error.
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
		  AND day < toDate(now())
		GROUP BY topic_0_sym, t1_xdr, t0_xdr ORDER BY c DESC LIMIT 200`
	rows, err := r.conn.Query(ctx, q, contractIDs, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: protocol event breakdown (fast): %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanEventBreakdown(rows)
}
