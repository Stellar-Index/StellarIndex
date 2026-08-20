package explorer

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// opsDirTTL bounds how stale the cached /v1/operations directory first page can
// be. The directory changes each ledger (~5s close time), so a few seconds keeps
// it within ~one ledger while absorbing repeated hits under real traffic.
const opsDirTTL = 3 * time.Second

// opsDirCache is a tiny TTL cache for the network-wide /v1/operations directory
// FIRST page (no cursor). That page is identical for every caller between
// ledgers, but assembling it is a ~300ms multi-column DESC-LIMIT read over the
// 24B-row lake plus the 24h op-type aggregation. Caching the assembled view for
// opsDirTTL makes the endpoint effectively free once traffic is concurrent.
// Keyed by limit; cursor pages are unique + cheaper (they skip the stats) so
// they're never cached. Zero value is ready to use (map lazily created).
type opsDirCache struct {
	mu      sync.Mutex
	entries map[int]opsDirEntry
}

type opsDirEntry struct {
	view    OperationsView
	expires time.Time
}

// get returns the cached first-page view for limit if still fresh.
func (c *opsDirCache) get(limit int) (OperationsView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[limit]
	if !ok || time.Now().After(e.expires) {
		return OperationsView{}, false
	}
	return e.view, true
}

// put caches the assembled first-page view for limit.
func (c *opsDirCache) put(limit int, view OperationsView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[int]opsDirEntry)
	}
	c.entries[limit] = opsDirEntry{view: view, expires: time.Now().Add(opsDirTTL)}
}

// opTypeStatsTTL bounds the trailing-24h op-type breakdown. It is deliberately
// MUCH longer than opsDirTTL: the directory listing changes every ledger (~5s),
// but this aggregate summarises a 24-HOUR window, so 3s precision on it was
// meaningless — and expensive, since each recompute scans a day of a 34B-row
// table. 5 minutes is still ~288x finer than the window it describes.
const opTypeStatsTTL = 5 * time.Minute

// opTypeStatsRefreshTimeout bounds one detached op-type aggregation — a
// FINAL GROUP BY over ~24h of the operations table, comfortably inside a
// minute in the background even under load.
const opTypeStatsRefreshTimeout = time.Minute

// opTypeStatsCache holds the last computed op-type breakdown. On a refresh
// failure the caller serves the STALE value rather than blanking the panel —
// a slow aggregate should degrade to slightly-old numbers, never to nothing.
type opTypeStatsCache struct {
	mu    sync.Mutex
	stats []OpTypeStatV
	at    time.Time
	// flight collapses concurrent detached refreshes into one aggregation.
	flight perKeyFlight
}

// get returns the cached breakdown and whether it is still fresh. The stats
// are returned even when stale so the caller can fall back to them on error.
func (c *opTypeStatsCache) get() (stats []OpTypeStatV, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats, c.stats != nil && time.Since(c.at) < opTypeStatsTTL
}

func (c *opTypeStatsCache) put(stats []OpTypeStatV) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats, c.at = stats, time.Now()
}

// OpView is the wire shape for a decoded operation. Type is the snake_case op
// type; Fields holds the decoded, human-readable body (empty for not-yet-decoded
// types, in which case RawXDR carries the original base64 so nothing is lost).
type OpView struct {
	Ledger        uint32         `json:"ledger"`
	CloseTime     string         `json:"close_time"`
	TxHash        string         `json:"tx_hash"`
	TxIndex       uint32         `json:"tx_index"`
	OpIndex       uint32         `json:"op_index"`
	Type          string         `json:"type"`
	SourceAccount string         `json:"source_account,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
	RawXDR        string         `json:"raw_xdr,omitempty"`
	// TransactionSuccessful reports whether the operation's PARENT transaction
	// applied. It is the authoritative honesty signal: a failed transaction is
	// still indexed and served (an on-chain, fee-charged, permanent record), so
	// every surface that lists an operation — including public account history —
	// must be able to mark it FAILED rather than let it masquerade as a real
	// interaction. false ⇒ the transaction did not apply; nothing this operation
	// names actually moved. Populated on every list view (account history, the
	// ledger op list, the /v1/operations directory) and the per-tx view; a nil
	// value means the parent outcome was not read (a degraded response, disclosed
	// via the view's coverage_note), NOT "successful".
	TransactionSuccessful *bool `json:"transaction_successful,omitempty"`
	// ResultCode is the operation's OUTER XDR result code (from
	// stellar.operation_results), populated in the per-transaction view (GET
	// /v1/tx/{hash}); nil where not read.
	ResultCode *int32 `json:"result_code,omitempty"`
	// Result is the human-readable slug for ResultCode (e.g. "op_inner",
	// "op_bad_auth", "op_no_source_account"). For a failed transaction the
	// authoritative reason is the TRANSACTION-level result (see
	// TxSummaryView.Result / GET /v1/tx/{hash}); this is per-operation structural
	// detail. Empty when ResultCode is nil.
	Result string `json:"result,omitempty"`
}

// opViewLight is the summary shape for the network-wide operations directory:
// the identity + op type (from the lake's op_type column), WITHOUT decoding the
// XDR body. The directory omits `fields`/`raw_xdr` on purpose — decoding every
// op's body meant reading the large body_xdr column over a 24B-row table
// (~600ms); the per-op fields live on the per-ledger view + /v1/tx/{hash}.
func opViewLight(o clickhouse.OpRow) OpView {
	return OpView{
		Ledger:        o.Seq,
		CloseTime:     o.CloseTime.UTC().Format(time.RFC3339),
		TxHash:        o.TxHash,
		TxIndex:       o.TxIndex,
		OpIndex:       o.OpIndex,
		SourceAccount: o.SourceAccount,
		Type:          normalizeLakeOpType(o.OpType),
	}
}

// opView decodes an operation row's XDR body into the wire shape. On decode
// failure it degrades to the lake's (normalised) op type + the raw body, so a
// single malformed/unknown op never fails the response.
func opView(o clickhouse.OpRow) OpView {
	v := OpView{
		Ledger:        o.Seq,
		CloseTime:     o.CloseTime.UTC().Format(time.RFC3339),
		TxHash:        o.TxHash,
		TxIndex:       o.TxIndex,
		OpIndex:       o.OpIndex,
		SourceAccount: o.SourceAccount,
	}
	d, err := xdrjson.DecodeOperationBody(o.BodyXDR)
	if err != nil {
		v.Type = normalizeLakeOpType(o.OpType)
		v.RawXDR = o.BodyXDR
		return v
	}
	v.Type = d.Type
	if len(d.Fields) > 0 {
		v.Fields = d.Fields
	}
	if d.RawXDR != "" {
		v.RawXDR = d.RawXDR
	}
	return v
}

// normalizeLakeOpType turns the lake's "OperationTypeManageSellOffer" into a
// best-effort lowercase fallback ("managesselloffer") for the decode-error path
// only — the happy path uses xdrjson's controlled snake_case vocabulary.
func normalizeLakeOpType(s string) string {
	return strings.ToLower(strings.TrimPrefix(s, "OperationType"))
}

// resolveOpTypeStats returns the trailing-24h op-type breakdown from the
// 5-minute cache, serving the STALE value (or, on a cold process, omitting
// the panel — `op_type_stats` is omitempty) while a DETACHED single-flight
// refresh runs. For a 24-hour aggregate, numbers a few minutes old are
// still truthful, whereas an empty panel is not — and recomputing INLINE on
// the request context was the failure (route-sweep 2026-07-29): the
// day-window FINAL GROUP BY shared the directory's 8s budget and dragged
// the whole /v1/operations page into its 503 class every 5 minutes.
func (h *Handler) resolveOpTypeStats(_ context.Context, _ *http.Request) []OpTypeStatV {
	cached, fresh := h.opTypeStats.get()
	if fresh {
		return cached
	}
	h.refreshOpTypeStats() //nolint:contextcheck // intentional detach — the aggregate must never share a request deadline (see refreshOpTypeStats)
	return cached          // stale (or nil on a cold process — panel appears next request)
}

// PrewarmOpTypeStats primes the trailing-24h op-type breakdown so a cold
// process never serves /v1/operations without the panel (pre-prewarm, the
// FIRST directory hit after every deploy got `op_type_stats` omitted —
// absence as the cold-start default rather than the rare exception).
// Called from the API's 5-minute prewarm loop
// (cmd/stellarindex-api/main.go), which matches opTypeStatsTTL, so the
// panel stays permanently fresh. Kicks the same detached single-flight
// refresh the request path uses; a still-fresh panel is a no-op. The
// aggregate itself measured ~70ms warm on r1 under replay load
// (2026-07-31) — the refresh's 1-minute budget is contention headroom.
func (h *Handler) PrewarmOpTypeStats(ctx context.Context) {
	if h.Reader == nil || ctx.Err() != nil {
		return
	}
	if _, fresh := h.opTypeStats.get(); fresh {
		return
	}
	h.refreshOpTypeStats() //nolint:contextcheck // intentional detach — prewarm kicks the same background compute
}

// refreshOpTypeStats kicks ONE detached op-type aggregation (no-op when a
// flight is already up). Detached: the aggregate must never share a
// request's deadline (see resolveOpTypeStats).
func (h *Handler) refreshOpTypeStats() {
	fl, owner := h.opTypeStats.flight.begin("stats")
	if !owner {
		return
	}
	go func() {
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), opTypeStatsRefreshTimeout)
		defer cancel()
		stats, err := h.Reader.OperationTypeStats(rctx, 0)
		obs.ObserveExplorerSWRRefresh("op_type_stats", start, err)
		if err != nil {
			// Keep the previous value — the panel degrades to slightly-old
			// numbers, never to nothing.
			h.Logger.Warn("explorer OperationTypeStats detached refresh failed (serving last good)", "err", err)
			h.opTypeStats.flight.end("stats", fl, err)
			return
		}
		v := make([]OpTypeStatV, len(stats))
		for i, st := range stats {
			v[i] = OpTypeStatV{Type: normalizeLakeOpType(st.OpType), Count: st.Count}
		}
		h.opTypeStats.put(v)
		h.opTypeStats.flight.end("stats", fl, nil)
	}()
}

// OperationsView is the wire response for GET /v1/operations.
//
// Two shapes on one route: with ?ledger=<seq> it's that ledger's ops
// (Ledger set, no cursor/stats); without it it's the network-wide
// recent-operations directory (Ledger 0, NextCursor for paging, and
// OpTypeStats — the trailing-24h per-type breakdown).
type OperationsView struct {
	Ledger      uint32        `json:"ledger"`
	Operations  []OpView      `json:"operations"`
	NextCursor  string        `json:"next_cursor,omitempty"`
	OpTypeStats []OpTypeStatV `json:"op_type_stats,omitempty"`
}

// OpTypeStatV is one op-type's count in the trailing-24h window.
type OpTypeStatV struct {
	Type  string `json:"type"`
	Count int64  `json:"count"`
}

// Operations serves GET /v1/operations.
//
//   - ?ledger=<seq>: that ledger's operations, decoded (partition-pruned).
//   - no ?ledger: the network-wide recent-operations DIRECTORY — newest
//     first, keyset-paged via ?cursor=<opaque> (echo back next_cursor;
//     composite ledger.tx_index.op_index), plus op_type_stats (per-type
//     counts over the trailing ~24h of ledgers).
func (h *Handler) Operations(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	seq, ok := h.parseUint32Query(w, r, "ledger")
	if !ok {
		return
	}
	if seq == 0 {
		h.operationsDirectory(w, r)
		return
	}
	limit, ok := h.ParseLimit(w, r, 500, 2000)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	rows, err := h.Reader.OperationsByLedger(ctx, seq, limit)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer OperationsByLedger deadline exceeded", "seq", seq)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/operations-timeout",
				"Operations query timed out")
			return
		}
		h.Logger.Error("explorer OperationsByLedger failed", "err", err, "seq", seq)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := OperationsView{Ledger: seq, Operations: make([]OpView, len(rows))}
	for i, o := range rows {
		out.Operations[i] = opView(o)
	}
	h.WriteJSON(w, out, false)
}

// NetworkThroughputView is the wire response for GET
// /v1/network/throughput — a daily time-series of network counts.
type NetworkThroughputView struct {
	WindowDays int                 `json:"window_days"`
	Buckets    []ThroughputBucketV `json:"buckets"`
}

type ThroughputBucketV struct {
	Day     string `json:"day"`
	Ledgers int64  `json:"ledgers"`
	Txs     int64  `json:"txs"`
	Ops     int64  `json:"ops"`
	Events  int64  `json:"events"`
	// End-of-day chain state from the day's last ledger. FeePool and
	// TotalCoins are XLM stroops as decimal strings — total_coins is
	// ~117× past 2^53, so a JSON number would silently lose precision
	// (ADR-0003). fee_pool is CUMULATIVE: daily fee burn is the delta
	// between consecutive complete days.
	FeePool         string `json:"fee_pool"`
	TotalCoins      string `json:"total_coins"`
	ProtocolVersion uint32 `json:"protocol_version"`
	// Partial is true for a bucket that does not cover a whole UTC day — in
	// practice only today, still accumulating. Clients should render it
	// distinctly and exclude it from window totals; every other bucket is a
	// complete day (the window is day-aligned).
	Partial bool `json:"partial,omitempty"`
}

// NetworkThroughput serves GET /v1/network/throughput — daily
// ledger / transaction / operation / Soroban-event counts over the
// trailing `?window_days=` (default 30, max 365), ascending by day.
// The time-series companion to the /v1/network/stats snapshot.
//
// Snapshot-served (§2.6b, 2026-08-13): the underlying year-window FINAL
// scan runs DETACHED and prewarmed, and this handler only ever slices the
// warm entry — fresh, or STALE with flags.stale + its real as_of while a
// single-flight rescan runs. Only a never-computed process can time out
// here, and its detached scan keeps running so the retry lands warm. See
// network_throughput_cache.go.
func (h *Handler) NetworkThroughput(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	windowDays := h.ParseWindowDays(r, 30)

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	buckets, asOf, degraded, err := h.networkThroughputCached(ctx, windowDays)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer NetworkThroughput deadline/saturation on cold series", "window_days", windowDays, "err", err)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/network-throughput-timeout",
				"Network throughput timed out")
			return
		}
		h.Logger.Error("explorer NetworkThroughput failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	// `partial` is decided HERE, not read from the cached bucket: only a
	// bucket covering today is still accumulating, and an entry computed
	// before UTC midnight would otherwise keep flagging yesterday — by
	// then a complete day — as partial.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := NetworkThroughputView{WindowDays: windowDays, Buckets: make([]ThroughputBucketV, len(buckets))}
	for i, b := range buckets {
		out.Buckets[i] = ThroughputBucketV{
			Day:     b.Day.UTC().Format("2006-01-02"),
			Ledgers: b.Ledgers, Txs: b.Txs, Ops: b.Ops, Events: b.Events,
			FeePool:         strconv.FormatInt(b.FeePool, 10),
			TotalCoins:      strconv.FormatInt(b.TotalCoins, 10),
			ProtocolVersion: b.ProtocolVersion,
			Partial:         !b.Day.UTC().Before(today),
		}
	}
	h.writeJSONAt(w, out, degraded, asOf)
}

// operationsDirectory serves the no-ledger path: network-wide
// recent operations (keyset-paged) + the trailing-24h op-type stats.
func (h *Handler) operationsDirectory(w http.ResponseWriter, r *http.Request) {
	limit, ok := h.ParseLimit(w, r, 50, 200)
	if !ok {
		return
	}
	cur, ok := h.parseExplorerCursor(w, r, 3) // (ledger, tx_index, op_index)
	if !ok {
		return
	}
	// The first page (no cursor) is the hot, cacheable path — same for every
	// caller between ledgers. Serve it from the short-TTL cache when warm.
	firstPage := !cur.IsSet()
	if firstPage {
		if view, hit := h.opsDir.get(limit); hit {
			h.WriteJSON(w, view, false)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	rows, err := h.Reader.RecentOperations(ctx, limit, cur)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer RecentOperations deadline exceeded")
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/operations-timeout",
				"Operations query timed out")
			return
		}
		h.Logger.Error("explorer RecentOperations failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := OperationsView{Operations: make([]OpView, len(rows))}
	for i, o := range rows {
		out.Operations[i] = opViewLight(o) // directory = summary; body fields on the detail views
	}
	if n := len(rows); n == limit {
		last := rows[n-1]
		out.NextCursor = encodeCursor(last.Seq, last.TxIndex, last.OpIndex)
	}
	// Op-type stats are best-effort context — a failure here shouldn't
	// fail the listing (only attached on the first page to keep paging
	// responses lean).
	if firstPage {
		out.OpTypeStats = h.resolveOpTypeStats(ctx, r)
		h.opsDir.put(limit, out) // warm the cache with the assembled first page
	}
	h.WriteJSON(w, out, false)
}
