package explorer

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

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

// opTypeStatsCache holds the last computed op-type breakdown. On a refresh
// failure the caller serves the STALE value rather than blanking the panel —
// a slow aggregate should degrade to slightly-old numbers, never to nothing.
type opTypeStatsCache struct {
	mu    sync.Mutex
	stats []OpTypeStatV
	at    time.Time
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
	// ResultCode is the operation's XDR result code, populated only in the
	// per-transaction view (GET /v1/tx/{hash}); nil in the ledger op list.
	ResultCode *int32 `json:"result_code,omitempty"`
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
func (h *Handler) NetworkThroughput(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	windowDays := h.ParseWindowDays(r, 30)

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	buckets, err := h.Reader.NetworkThroughput(ctx, windowDays)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer NetworkThroughput failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := NetworkThroughputView{WindowDays: windowDays, Buckets: make([]ThroughputBucketV, len(buckets))}
	for i, b := range buckets {
		out.Buckets[i] = ThroughputBucketV{
			Day:     b.Day.UTC().Format("2006-01-02"),
			Ledgers: b.Ledgers, Txs: b.Txs, Ops: b.Ops, Events: b.Events,
			Partial: b.Partial,
		}
	}
	h.WriteJSON(w, out, false)
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
		cached, fresh := h.opTypeStats.get()
		switch {
		case fresh:
			out.OpTypeStats = cached
		default:
			stats, serr := h.Reader.OperationTypeStats(ctx, 0)
			if serr == nil {
				v := make([]OpTypeStatV, len(stats))
				for i, st := range stats {
					v[i] = OpTypeStatV{Type: normalizeLakeOpType(st.OpType), Count: st.Count}
				}
				out.OpTypeStats = v
				h.opTypeStats.put(v)
			} else {
				if !h.ClientAborted(r, serr) {
					h.Logger.Warn("explorer OperationTypeStats failed (serving last good)", "err", serr)
				}
				// Stale beats blank: a 24h aggregate that's a few minutes old
				// is still a truthful answer; an empty panel is not.
				out.OpTypeStats = cached
			}
		}
		h.opsDir.put(limit, out) // warm the cache with the assembled first page
	}
	h.WriteJSON(w, out, false)
}
