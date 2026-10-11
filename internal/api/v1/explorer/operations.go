package explorer

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// opsDirTTL is how long the cached /v1/operations first page counts as fresh (~two ledgers).
// Past the TTL the entry is still SERVED (flags.stale + its real as_of) while a detached
// refresh runs, so the TTL sets the freshness label rather than gating a blocking recompute.
const opsDirTTL = 10 * time.Second

// opsDirRefreshTimeout bounds one detached first-page rebuild. The read
// itself is a tail-window-bounded page plus the batched parent-transaction
// outcome read (its own txOutcomeStitchBudget), so 30s is contention
// headroom, not an expectation.
const opsDirRefreshTimeout = 30 * time.Second

// opsDirMaxLimit is the page size the cached entry holds. It matches the handler's ParseLimit
// ceiling, so the one warm entry serves any accepted `?limit=` by slicing its head.
const opsDirMaxLimit = 200

// opsDirFlightKey is the single-flight key. The cache holds exactly one
// entry (the max page), so the key is a constant — it exists only to
// reuse perKeyFlight.
const opsDirFlightKey = "ops_directory"

// opsDirCache is a tiny SWR cache for the /v1/operations directory FIRST page (no cursor), which
// is identical for every caller between ledgers but costs a DESC-LIMIT lake read plus the
// tx-outcome read.
//
// One entry holding the MAXIMUM page, sliced per request: keying by raw `?limit=` would let a
// caller sweeping the range buy a fresh lake read per value. Cursor pages are unique and are
// never cached. An expired entry is served while a single detached refresh rebuilds it; only a
// never-computed entry blocks. Zero value is ready to use.
type opsDirCache struct {
	mu    sync.Mutex
	entry opsDirEntry
	has   bool
	// flight collapses concurrent detached refreshes.
	flight perKeyFlight
}

type opsDirEntry struct {
	view     OperationsView
	cachedAt time.Time
}

// get returns the cached first-page view whenever one exists — INCLUDING
// past the TTL (fresh=false). Staleness is the caller's judgment, so a run
// of failed refreshes degrades to old-but-real data rather than to a
// blocking read on every hit.
func (c *opsDirCache) get() (e opsDirEntry, ok, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.has {
		return opsDirEntry{}, false, false
	}
	return c.entry, true, time.Since(c.entry.cachedAt) <= opsDirTTL
}

// put caches the assembled max-page first-page view.
func (c *opsDirCache) put(view OperationsView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry = opsDirEntry{view: view, cachedAt: time.Now()}
	c.has = true
}

// opTypeStatsTTL bounds the trailing-24h op-type breakdown. It is much longer than opsDirTTL
// because ledger-level precision on a 24-hour aggregate is meaningless and each recompute scans
// a day of the operations table.
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
	// TransactionSuccessful reports whether the PARENT transaction applied. A failed transaction
	// is still indexed and served, so every operation list must be able to mark it FAILED
	// (false: nothing it names actually moved). nil means the parent outcome was not read (a
	// degraded response disclosed via coverage_note), NOT "successful".
	TransactionSuccessful *bool `json:"transaction_successful,omitempty"`
	// TransactionResult is the slug for the parent transaction's result (e.g. "tx_failed"),
	// the WHY that pairs with TransactionSuccessful. Empty when the parent outcome was not read.
	TransactionResult string `json:"transaction_result,omitempty"`
	// ResultCode is the operation's OUTER XDR result code, populated only in the per-transaction
	// view; nil where not read.
	ResultCode *int32 `json:"result_code,omitempty"`
	// Result is the slug for ResultCode. For a failed transaction the authoritative reason is
	// the TRANSACTION-level result (TxSummaryView.Result); this is per-operation detail.
	Result string `json:"result,omitempty"`
	// InnerResult names an op_inner operation's op-type-specific outcome
	// (e.g. "payment_underfunded", "payment_success") — which operation of a
	// tx_failed transaction failed, and why. Empty when Result is not op_inner.
	InnerResult string `json:"inner_result,omitempty"`
}

// opViewLight is the directory summary shape: identity + op type WITHOUT decoding the XDR body.
// Decoding would force reading the large body_xdr column over the whole table; per-op fields
// live on the per-ledger view and /v1/tx/{hash}.
func opViewLight(o clickhouse.OpRow) OpView {
	return OpView{
		Ledger:        o.Seq,
		CloseTime:     o.CloseTime.UTC().Format(time.RFC3339),
		TxHash:        o.TxHash,
		TxIndex:       o.TxIndex,
		OpIndex:       o.OpIndex,
		SourceAccount: o.SourceAccount,
		Type:          xdrjson.OpTypeNameFromEnumString(o.OpType),
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

// opsOutcomeCoverageNote is served when the parent-transaction outcome read fails: operations
// go out without transaction_successful, and this note says absence means UNKNOWN, not success.
const opsOutcomeCoverageNote = "transaction outcomes are temporarily unavailable; operations shown without " +
	"transaction_successful are of UNKNOWN outcome and may belong to a FAILED transaction, not applied"

// txOutcomeStitchBudget bounds the detached parent-transaction outcome read.
// See stampTxOutcomes for how it is sized (r1-measured worst case × ~16).
const txOutcomeStitchBudget = time.Second

// opRowTxKeys returns the distinct ledgers and tx hashes of a page of operation rows, the two
// IN-lists for a batched TxOutcomesByHash read.
//
// The ledger SET, not a [lo,hi] span: a sparse account's page can straddle millions of ledgers,
// and a span predicate scales the outcome read with idleness rather than page size (see
// TxOutcomesByHash).
func opRowTxKeys(rows []clickhouse.OpRow) (ledgers []uint32, hashes []string) {
	if len(rows) == 0 {
		return nil, nil
	}
	seenLedger := make(map[uint32]struct{}, len(rows))
	seenHash := make(map[string]struct{}, len(rows))
	for _, o := range rows {
		if _, ok := seenLedger[o.Seq]; !ok {
			seenLedger[o.Seq] = struct{}{}
			ledgers = append(ledgers, o.Seq)
		}
		if _, ok := seenHash[o.TxHash]; !ok {
			seenHash[o.TxHash] = struct{}{}
			hashes = append(hashes, o.TxHash)
		}
	}
	return ledgers, hashes
}

// stitchTxOutcomes stamps each op view with its parent transaction's outcome so failed
// transactions' operations are marked FAILED on list views. An op whose tx hash is absent from
// the map keeps nil TransactionSuccessful: absence is UNKNOWN, never "successful".
func stitchTxOutcomes(ops []OpView, outcomes map[string]clickhouse.TxOutcome) {
	for i := range ops {
		o, ok := outcomes[ops[i].TxHash]
		if !ok {
			continue
		}
		s := o.Successful
		ops[i].TransactionSuccessful = &s
		ops[i].TransactionResult = xdrjson.TxResultName(o.ResultCode)
	}
}

// stampTxOutcomes stitches parent-transaction outcomes onto the built views. It returns a
// coverage note (empty on success) rather than failing: a transient read blip degrades to
// "outcome unknown", not to a misleading unmarked op.
func (h *Handler) stampTxOutcomes(ctx context.Context, ops []OpView, rows []clickhouse.OpRow) string {
	ledgers, hashes := opRowTxKeys(rows)
	if len(hashes) == 0 {
		return ""
	}
	// Detached budget: this read runs LAST, after the ops-by-account scan, which for a long-idle
	// account can exhaust the request deadline while the outcome read itself is cheap.
	// WithoutCancel keeps tracing values without inheriting the spent deadline. 1s is ~16x the
	// measured cold worst case; beyond it ClickHouse is degraded and shedding to the honest
	// coverage note beats holding an unauthenticated client longer.
	octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txOutcomeStitchBudget)
	defer cancel()
	outcomes, err := h.Reader.TxOutcomesByHash(octx, ledgers, hashes)
	if err != nil {
		// Non-fatal: serve the operations with UNKNOWN outcome and disclose it
		// (a client abort lands here too, in which case the response is moot).
		h.Logger.Warn("explorer TxOutcomesByHash failed (serving ops with unknown outcome)", "err", err)
		return opsOutcomeCoverageNote
	}
	stitchTxOutcomes(ops, outcomes)
	return ""
}

// resolveOpTypeStats returns the trailing-24h op-type breakdown, serving the STALE value (nil on
// a cold process, so the omitempty panel is dropped) while a DETACHED single-flight refresh runs.
// Minutes-old numbers are still truthful for a 24-hour aggregate; recomputing inline would share
// the directory's request budget and drag the whole page into 503 every refresh.
func (h *Handler) resolveOpTypeStats() (stats []OpTypeStatV, fresh bool) {
	cached, fresh := h.opTypeStats.get()
	if fresh {
		return cached, true
	}
	h.refreshOpTypeStats() //nolint:contextcheck // intentional detach — the aggregate must never share a request deadline (see refreshOpTypeStats)
	return cached, false   // stale (or nil on a cold process — panel appears next request)
}

// PrewarmOpTypeStats primes the op-type breakdown so a cold process never serves /v1/operations
// without the panel. The API's prewarm loop calls it at opTypeStatsTTL cadence; it kicks the same
// detached single-flight refresh as the request path, and a still-fresh panel is a no-op.
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
		// End the flight from a defer so a panic cannot wedge the
		// op-type panel forever — see perKeyFlight.end.
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-op-type-stats-refresh", rec)
				err = errRefreshPanicked
			}
			h.opTypeStats.flight.end("stats", fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), opTypeStatsRefreshTimeout)
		defer cancel()
		stats, err := h.Reader.OperationTypeStats(rctx, 0)
		obs.ObserveExplorerSWRRefresh("op_type_stats", start, err)
		if err != nil {
			// Keep the previous value — the panel degrades to slightly-old
			// numbers, never to nothing.
			h.Logger.Warn("explorer OperationTypeStats detached refresh failed (serving last good)", "err", err)
			return
		}
		v := make([]OpTypeStatV, len(stats))
		for i, st := range stats {
			v[i] = OpTypeStatV{Type: xdrjson.OpTypeNameFromEnumString(st.OpType), Count: st.Count}
		}
		h.opTypeStats.put(v)
	}()
}

// OperationsView is the wire response for GET /v1/operations and GET /v1/ledgers/{seq}/operations.
// The per-ledger form sets Ledger with no cursor/stats; the directory form has Ledger 0,
// NextCursor and OpTypeStats.
type OperationsView struct {
	Ledger      uint32        `json:"ledger"`
	Operations  []OpView      `json:"operations"`
	NextCursor  string        `json:"next_cursor,omitempty"`
	OpTypeStats []OpTypeStatV `json:"op_type_stats,omitempty"`
	// CoverageNote is non-empty when the parent-transaction outcome read failed
	// while assembling this page, so operations without transaction_successful
	// are of UNKNOWN outcome rather than known-applied (opsOutcomeCoverageNote).
	CoverageNote string `json:"coverage_note,omitempty"`
	// Total and Truncated are set only on the per-ledger form: the ledger header's exact
	// operation count vs len(Operations). Zero/false on the directory, which pages instead.
	Total     uint32 `json:"total,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// opTypeStatsStale records that OpTypeStats was stale or absent when the
	// page was assembled, so a cached copy still serves as degraded.
	opTypeStatsStale bool
}

// OpTypeStatV is one op-type's count in the trailing-24h window.
type OpTypeStatV struct {
	Type  string `json:"type"`
	Count int64  `json:"count"`
}

// Operations serves GET /v1/operations: the network-wide recent-operations directory, newest
// first, keyset-paged via ?cursor= (composite ledger.tx_index.op_index), plus op_type_stats.
func (h *Handler) Operations(w http.ResponseWriter, r *http.Request) {
	// A ledger-scoped read has its own route and consistency contract
	// (ADR-0018); serving it here as a directory page would answer the wrong question.
	if r.URL.Query().Has("ledger") {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid parameter", http.StatusBadRequest,
			"ledger is not a /v1/operations parameter; use GET /v1/ledgers/{seq}/operations")
		return
	}
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	h.operationsDirectory(w, r)
}

// LedgerOperations serves GET /v1/ledgers/{seq}/operations.
func (h *Handler) LedgerOperations(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	seq, ok := h.parseLedgerSeq(w, r)
	if !ok {
		return
	}
	h.ledgerOperations(w, r, seq)
}

// ledgerOperations is one ledger's operations, decoded (partition-pruned).
func (h *Handler) ledgerOperations(w http.ResponseWriter, r *http.Request, seq uint32) {
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
	decodedBytes := 0
	for i, o := range rows {
		// ParseLimit bounds ROW count, but each op's XDR body is attacker-influenced in size
		// (Soroban footprint/auth can run to tens of KB), so response BYTES need their own cap.
		// Past the budget, rows are served UNDECODED (opViewLight), never dropped, so the
		// operation count always matches len(rows).
		if decodedBytes+len(o.BodyXDR) > operationsResponseByteBudget {
			out.Operations[i] = opViewLight(o)
			continue
		}
		decodedBytes += len(o.BodyXDR)
		out.Operations[i] = opView(o)
	}
	out.CoverageNote = h.stampTxOutcomes(ctx, out.Operations, rows)
	// len(rows)==limit can't distinguish "exactly limit ops" from "truncated", so read the
	// ledger header's exact op count. A header-read failure only loses this metadata.
	hdr, found, herr := h.Reader.LedgerBySeq(ctx, seq)
	if herr != nil {
		h.Logger.Warn("explorer LedgerBySeq (operations total) failed", "err", herr, "seq", seq)
	} else if found {
		out.Total = hdr.OpCount
		out.Truncated = hdr.OpCount > uint32(len(rows))
	}
	_, stale, _ := h.lakeTip(ctx)
	h.writeJSONAt(w, out, stale, out.CoverageNote != "" || herr != nil, time.Time{})
}

// operationsResponseByteBudget is a conservative, fail-closed ceiling on the raw XDR bytes one
// per-ledger response will decode. The right figure depends on proxy limits not visible from
// this repo, so it is a placeholder rather than a tuned value.
const operationsResponseByteBudget = 8 << 20 // 8 MiB

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
	// End-of-day chain state from the day's last ledger. FeePool and TotalCoins are XLM stroops
	// as decimal strings: total_coins is far past 2^53, so a JSON number would lose precision.
	// fee_pool is CUMULATIVE: daily fee burn is the delta between consecutive complete days,
	// minus FeePoolAdjustment.
	FeePool         string `json:"fee_pool"`
	TotalCoins      string `json:"total_coins"`
	ProtocolVersion uint32 `json:"protocol_version"`
	// FeePoolAdjustment is the stroops this day's fee_pool changed by
	// outside any transaction fee (a protocol upgrade crediting the pool).
	FeePoolAdjustment string `json:"fee_pool_adjustment,omitempty"`
	// Partial marks a bucket not covering a whole UTC day (only today, still accumulating).
	// Clients should render it distinctly and exclude it from window totals.
	Partial bool `json:"partial,omitempty"`
}

// knownFeePoolAdjustments lists fee_pool credits that no transaction paid.
// Each is keyed on its UTC day AND the protocol upgrade applied that day, so
// a network whose upgrade fell on another day (testnet, futurenet) never
// matches.
var knownFeePoolAdjustments = []struct {
	day                    string
	fromProtocol, protocol uint32
	stroops                int64
}{
	// Pubnet's P24 upgrade (ledger 59,501,299) credited the pool directly;
	// total_coins did not change.
	{day: "2025-10-22", fromProtocol: 23, protocol: 24, stroops: 31_879_035},
}

// feePoolAdjustment returns the non-fee fee_pool change for day as a stroop
// string, or "" when there is none.
func feePoolAdjustment(day string, prevProtocol, protocol uint32) string {
	for _, a := range knownFeePoolAdjustments {
		if a.day == day && a.fromProtocol == prevProtocol && a.protocol == protocol {
			return strconv.FormatInt(a.stroops, 10)
		}
	}
	return ""
}

// NetworkThroughput serves GET /v1/network/throughput: daily ledger / transaction / operation /
// Soroban-event counts over the trailing `?window_days=` (default 30, max 365), ascending.
//
// Snapshot-served: the year-window FINAL scan runs DETACHED and prewarmed, and this handler only
// slices the warm entry (fresh, or STALE with flags.stale + its real as_of). Only a
// never-computed process can time out, and its detached scan keeps running so the retry lands
// warm. See network_throughput_cache.go.
func (h *Handler) NetworkThroughput(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	windowDays, ok := h.ParseWindowDays(w, r, 30)
	if !ok {
		return
	}

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
	// `partial` is READ from the cached bucket, never recomputed against wall-clock "today": the
	// reader derives it from the query's own max(close_time). A server-clock recompute would
	// silently clear a still-incomplete bucket for a stale cached entry or a peer whose clock
	// already rolled the day.
	out := NetworkThroughputView{WindowDays: windowDays, Buckets: make([]ThroughputBucketV, len(buckets))}
	for i, b := range buckets {
		out.Buckets[i] = ThroughputBucketV{
			Day:     b.Day.UTC().Format("2006-01-02"),
			Ledgers: b.Ledgers, Txs: b.Txs, Ops: b.Ops, Events: b.Events,
			FeePool:         strconv.FormatInt(b.FeePool, 10),
			TotalCoins:      strconv.FormatInt(b.TotalCoins, 10),
			ProtocolVersion: b.ProtocolVersion,
			Partial:         b.Partial,
		}
		if i > 0 {
			out.Buckets[i].FeePoolAdjustment = feePoolAdjustment(out.Buckets[i].Day, buckets[i-1].ProtocolVersion, b.ProtocolVersion)
		}
	}
	h.writeJSONAt(w, out, degraded, degraded, asOf)
}

// operationsDirectory serves GET /v1/operations: network-wide
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
	opTypes, ok := h.parseOpTypes(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	if len(opTypes) > 0 {
		out, err := h.buildOperationsOfType(ctx, limit, cur, opTypes)
		if err != nil {
			h.writeOperationsPageError(ctx, w, r, err)
			return
		}
		_, stale, _ := h.lakeTip(ctx)
		h.writeJSONAt(w, out, stale, out.CoverageNote != "", time.Time{})
		return
	}

	// The first page (no cursor) is the hot path, identical for every caller between ledgers.
	// Serve it from the SWR cache, sliced to the caller's limit; a never-computed cache is
	// single-flighted through opsDirCached so a burst shares ONE lake read.
	if !cur.IsSet() {
		view, asOf, degraded, err := h.opsDirCached(ctx)
		if err != nil {
			if h.ClientAborted(r, err) {
				return
			}
			if retryableColdMiss(ctx, err) {
				h.Logger.Warn("explorer operations directory deadline/saturation on cold cache", "err", err)
				h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/operations-timeout",
					"Operations query timed out")
				return
			}
			h.Logger.Error("explorer operations directory failed", "err", err)
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
				"Internal error", http.StatusInternalServerError, "")
			return
		}
		h.writeJSONAt(w, sliceOperationsView(view, limit), degraded, degraded || view.opTypeStatsStale || view.CoverageNote != "", asOf)
		return
	}

	// Cursor pages are unique per request and skip the op-type stats, so they are built inline.
	out, err := h.buildOperationsDirectory(ctx, limit, cur)
	if err != nil {
		h.writeOperationsPageError(ctx, w, r, err)
		return
	}
	_, stale, _ := h.lakeTip(ctx)
	h.writeJSONAt(w, out, stale, out.CoverageNote != "", time.Time{})
}

// writeOperationsPageError answers a failed uncached /v1/operations page.
func (h *Handler) writeOperationsPageError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	if h.ClientAborted(r, err) {
		return
	}
	// A cursor so deep that the bounded read hits its row ceiling is the CALLER's fault, and an
	// identical retry is refused identically, so it must not be reported as retryable. The cursor
	// is a publicly mintable dotted decimal.
	if errors.Is(err, clickhouse.ErrOperationsCursorTooDeep) {
		h.Logger.Warn("explorer RecentOperations refused a too-deep cursor")
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/cursor-too-deep",
			"Cursor too deep", http.StatusBadRequest,
			"the supplied cursor would require scanning an unbounded portion of the table; page forward from a more recent cursor, or use /v1/ledgers/{seq}/operations to address a specific ledger")
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
}

// opsDirCached serves the cached max-page first-page view. A stale entry is served
// (degraded=true) while a detached refresh runs; a never-computed cache single-flights the fill
// and waits up to ctx's own deadline, so concurrent first requests share ONE detached lake read
// (unshared inline reads would let a limit sweep defeat the cache on its first pass).
func (h *Handler) opsDirCached(ctx context.Context) (view OperationsView, asOf time.Time, degraded bool, err error) {
	if e, hit, fresh := h.opsDir.get(); hit {
		if !fresh {
			h.refreshOpsDirectory() //nolint:contextcheck // intentional detach — see refreshOpsDirectory
		}
		return e.view, e.cachedAt, !fresh, nil
	}
	// Stone-cold: wait for the detached compute, bounded by OUR deadline
	// only — the compute itself is not (see refreshAssetHolders).
	fl := h.refreshOpsDirectory() //nolint:contextcheck // intentional detach — a request that times out must not kill the fill
	select {
	case <-fl.done:
		if e, hit, _ := h.opsDir.get(); hit {
			return e.view, e.cachedAt, false, nil
		}
		if fl.err != nil {
			return OperationsView{}, time.Time{}, false, fl.err
		}
		return OperationsView{}, time.Time{}, false, errRefreshFailed
	case <-ctx.Done():
		return OperationsView{}, time.Time{}, false, ctx.Err()
	}
}

// sliceOperationsView truncates a max-limit page to the caller's limit. NextCursor is recomputed
// from the last retained row: the ceiling page's cursor points past opsDirMaxLimit rows and
// would skip every row between the requested limit and the ceiling.
func sliceOperationsView(v OperationsView, limit int) OperationsView {
	if limit <= 0 || limit >= len(v.Operations) {
		return v
	}
	out := v
	out.Operations = v.Operations[:limit]
	last := out.Operations[limit-1]
	out.NextCursor = encodeCursor(last.Ledger, last.TxIndex, last.OpIndex)
	return out
}

// buildOperationsDirectory assembles one directory page. It is the SINGLE assembler behind the
// request path and the detached refresh, so a cached page never differs in shape from a fresh one.
func (h *Handler) buildOperationsDirectory(ctx context.Context, limit int, cur clickhouse.ExplorerCursor) (OperationsView, error) {
	rows, err := h.Reader.RecentOperations(ctx, limit, cur)
	if err != nil {
		return OperationsView{}, err
	}
	out := h.lightOperationsView(ctx, rows, limit)
	// Op-type stats are best-effort context and only attached to the first page.
	if !cur.IsSet() {
		var fresh bool
		out.OpTypeStats, fresh = h.resolveOpTypeStats() //nolint:contextcheck // intentional detach — the 24h aggregate must never share a request deadline (see resolveOpTypeStats)
		out.opTypeStatsStale = !fresh
	}
	return out, nil
}

// lightOperationsView builds a directory page's summary views, stamps their
// parent-transaction outcomes, and sets next_cursor when the page is full.
func (h *Handler) lightOperationsView(ctx context.Context, rows []clickhouse.OpRow, limit int) OperationsView {
	out := OperationsView{Operations: make([]OpView, len(rows))}
	for i, o := range rows {
		out.Operations[i] = opViewLight(o) // directory = summary; body fields on the detail views
	}
	out.CoverageNote = h.stampTxOutcomes(ctx, out.Operations, rows)
	if n := len(rows); n == limit {
		last := rows[n-1]
		out.NextCursor = encodeCursor(last.Seq, last.TxIndex, last.OpIndex)
	}
	return out
}

// buildOperationsOfType assembles one type-filtered directory page. The read
// scans a bounded ledger span, so a short page still continues below that
// span (cursor `<floor>.0.0`) until the scan reaches genesis.
func (h *Handler) buildOperationsOfType(ctx context.Context, limit int, cur clickhouse.ExplorerCursor, opTypes []string) (OperationsView, error) {
	page, err := h.Reader.RecentOperationsOfType(ctx, limit, cur, opTypes)
	if err != nil {
		return OperationsView{}, err
	}
	out := h.lightOperationsView(ctx, page.Rows, limit)
	if out.NextCursor == "" && page.ScannedFrom > 0 {
		out.NextCursor = encodeCursor(page.ScannedFrom, 0, 0)
	}
	return out, nil
}

// parseOpTypes reads `?type=` (comma-separated and/or repeated snake_case op
// types) into the lake's op_type enum strings, deduplicated. Absent → nil.
func (h *Handler) parseOpTypes(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range r.URL.Query()["type"] {
		for _, name := range strings.Split(raw, ",") {
			enum, ok := xdrjson.OpTypeEnumString(strings.TrimSpace(name))
			if !ok {
				h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
					"Invalid parameter", http.StatusBadRequest,
					"type must be one or more snake_case operation types (e.g. payment,manage_sell_offer)")
				return nil, false
			}
			if !seen[enum] {
				seen[enum] = true
				out = append(out, enum)
			}
		}
	}
	return out, true
}

// refreshOpsDirectory kicks ONE detached rebuild of the max-page first page (a no-op while a
// flight is up) and returns the flight to optionally wait on. Detached because a rebuild bound
// to the request deadline would die with the request and the next visitor would pay the same wait.
//
// Bounded by the shared refresh gate under its own class; on saturation we skip and keep serving
// the stale entry, never queue.
func (h *Handler) refreshOpsDirectory() *keyFlight {
	fl, owner := h.opsDir.flight.begin(opsDirFlightKey)
	if !owner {
		return fl
	}
	gate := h.detachedGate()
	if !gate.TryAcquireClass("ops_directory") {
		h.opsDir.flight.end(opsDirFlightKey, fl, errRefreshSaturated)
		return fl
	}
	go func() {
		defer gate.ReleaseClass("ops_directory")
		// End the flight from a defer so a panic cannot wedge this cache
		// forever — see perKeyFlight.end.
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-ops-directory-refresh", rec)
				err = errRefreshPanicked
			}
			h.opsDir.flight.end(opsDirFlightKey, fl, err)
		}()
		start := time.Now()
		rctx, cancel := context.WithTimeout(context.Background(), opsDirRefreshTimeout)
		defer cancel()
		out, err := h.buildOperationsDirectory(rctx, opsDirMaxLimit, clickhouse.ExplorerCursor{})
		obs.ObserveExplorerSWRRefresh("ops_directory", start, err)
		if err != nil {
			// Keep the previous entry — old-but-real beats blank, and the
			// served response already discloses it via flags.stale.
			h.Logger.Warn("explorer operations directory detached refresh failed (serving last good)", "err", err)
			return
		}
		h.opsDir.put(out)
	}()
	return fl
}
