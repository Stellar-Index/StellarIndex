package explorer

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ContractEventView is one event in the contract-activity view.
type ContractEventView struct {
	Ledger     uint32 `json:"ledger"`
	CloseTime  string `json:"close_time"`
	TxHash     string `json:"tx_hash"`
	OpIndex    uint32 `json:"op_index"`
	EventIndex uint32 `json:"event_index"`
	EventType  string `json:"event_type"`
	Topic0     string `json:"topic_0,omitempty"`
	// Topics are human-readable renderings of topics[1:] (topic_0 is
	// the symbol above); Data renders the event payload. Display
	// format — lossy by design (S-016: rows read 'transfer' fifty
	// times with no amounts or parties).
	Topics []string `json:"topics,omitempty"`
	Data   string   `json:"data,omitempty"`
}

// ContractDetailView is the wire response for GET /v1/contracts/{contract_id}:
// the contract id + its most-recent events. NextCursor is the opaque keyset
// cursor for the next (older) page — the full row-identity composite
// (ledger, tx_hash, op_index, event_index) so a contract that emits many
// events in one ledger (across many txs) never loses rows across a page
// boundary. Echo it back as ?cursor=. Set only when a full page returned.
type ContractDetailView struct {
	ContractID string `json:"contract_id"`
	// Protocol names the registry protocol this contract belongs to
	// (blend, soroswap, …) when attribution is known (Pass-B CON-3:
	// a Blend pool page couldn't say it was Blend while the server
	// held the map).
	Protocol   string              `json:"protocol,omitempty"`
	Events     []ContractEventView `json:"events"`
	NextCursor string              `json:"next_cursor,omitempty"`
	// Directory is the curated third-party label for this contract
	// (directory.go) — display attribution, not verification. Omitted
	// when the contract isn't listed or no directory reader is wired.
	Directory *DirectoryInfoV `json:"directory,omitempty"`
	// Activity is the liveness card (page insight program unit 1):
	// lifetime bounds + a 30-day daily active-ledger series off the
	// contract-keyed index. Omitted when the index isn't provisioned.
	Activity *ContractActivityV `json:"activity,omitempty"`
	// Exists is false when the lake holds no evidence the contract was ever
	// deployed — no events, no activity, no instance entry or TTL row — so
	// "no such contract / not yet captured", 200 not 404 like the account
	// route. Absent when the instance read failed and nothing else decides it.
	Exists *bool `json:"exists,omitempty"`
	// TTL is the instance entry's liveness at the lake watermark. Absent
	// when no TTL row is held or the watermark is unavailable.
	TTL *ContractTTLV `json:"ttl,omitempty"`
}

// Wire values of ContractTTLV.State.
const (
	ttlStateLive     = "live"
	ttlStateArchived = "archived"
)

// ContractTTLV is the wire TTL state of a contract's instance entry. An
// archived instance cannot be invoked until it is restored.
type ContractTTLV struct {
	LiveUntil  uint32 `json:"live_until"`
	State      string `json:"state"`
	AsOfLedger uint32 `json:"as_of_ledger"`
}

// ContractActivityV is the wire liveness card.
type ContractActivityV struct {
	FirstSeen          string                 `json:"first_seen"`
	LastSeen           string                 `json:"last_seen"`
	ActiveLedgersTotal uint64                 `json:"active_ledgers_total"`
	Daily              []ContractActivityDayV `json:"daily"`
}

// ContractActivityDayV is one day of the activity series.
type ContractActivityDayV struct {
	Date          string `json:"date"`
	ActiveLedgers uint64 `json:"active_ledgers"`
}

// ContractDetail serves GET /v1/contracts/{contract_id} — a contract's
// recent on-chain event activity (uses the contract_id bloom skip-index).
// SEP-41 transfer detail lives at the sibling /v1/contracts/{id}/transfers.
func (h *Handler) ContractDetail(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	cid := r.PathValue("contract_id")
	if !canonical.IsContractID(cid) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-contract-id",
			"Invalid contract id", http.StatusBadRequest,
			"the contract id must be a valid C-strkey")
		return
	}
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}
	cur, ok := h.parseContractEventsCursor(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	// First page (no cursor): served through the shared contract-detail
	// SWR cache, computed once at the max page size and sliced — a busy
	// contract's scan cannot fit the request deadline, and dying with the
	// request left the cache permanently cold (route-sweep 2026-07-30).
	// Cursor pages stay inline: they are unique per cursor (caching them
	// would just churn the bounded cache) and their PK range is narrower.
	var (
		rows     []clickhouse.ContractActivityRow
		asOf     time.Time
		degraded bool
		err      error
	)
	if !cur.IsSet() {
		rows, asOf, degraded, err = h.contractEventsFirstPageCached(ctx, cid, limit)
	} else {
		rows, err = h.Reader.ContractEventsRecent(ctx, cid, limit, cur)
	}
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer ContractEventsRecent deadline/saturation", "contract", cid, "err", err)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/contract-detail-timeout",
				"Contract detail timed out")
			return
		}
		h.Logger.Error("explorer ContractEventsRecent failed", "err", err, "contract", cid)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := ContractDetailView{ContractID: cid, Events: make([]ContractEventView, len(rows))}
	out.Protocol = h.contractAttribution(ctx)[cid]
	out.Directory = h.directoryFor(ctx, cid)
	out.Activity = h.contractActivityCard(ctx, cid)
	for i, e := range rows {
		out.Events[i] = contractEventView(e)
	}
	h.setContractLiveness(ctx, &out)
	// Only emit a cursor on a full page — a short page is the last page, so a
	// cursor there just costs the client one empty round-trip.
	if n := len(rows); n == limit {
		last := rows[n-1]
		out.NextCursor = fmt.Sprintf("%d.%s.%d.%d", last.Seq, last.TxHash, last.OpIndex, last.EventIndex)
	}
	if !cur.IsSet() {
		h.writeJSONAt(w, out, degraded, asOf)
		return
	}
	h.WriteJSON(w, out, false)
}

// contractActivityCard is the 30-day liveness card, nil when the activity
// index is unprovisioned, errors, or holds nothing for the contract.
func (h *Handler) contractActivityCard(ctx context.Context, cid string) *ContractActivityV {
	act, ok, err := h.Reader.ContractActivitySummaryFor(ctx, cid, 30)
	if err != nil || !ok || act.ActiveLedgersTotal == 0 {
		return nil
	}
	av := &ContractActivityV{
		FirstSeen:          act.FirstSeen.UTC().Format(time.RFC3339),
		LastSeen:           act.LastSeen.UTC().Format(time.RFC3339),
		ActiveLedgersTotal: act.ActiveLedgersTotal,
	}
	for _, d := range act.Daily {
		av.Daily = append(av.Daily, ContractActivityDayV{Date: d.Date.UTC().Format("2006-01-02"), ActiveLedgers: d.ActiveLedgers})
	}
	return av
}

// setContractLiveness fills Exists and TTL. Exists is claimed false only on
// a successful instance read that found nothing and no other lake evidence.
func (h *Handler) setContractLiveness(ctx context.Context, out *ContractDetailView) {
	st, ok := h.contractInstanceState(ctx, out.ContractID)
	if ok {
		out.TTL = h.contractTTL(ctx, st.LiveUntil)
	}
	var exists bool
	switch {
	case len(out.Events) > 0 || out.Activity != nil || st.Known:
		exists = true
	case h.IsKnownSAC != nil && h.IsKnownSAC(out.ContractID):
		exists = true
	case !ok:
		return
	}
	out.Exists = &exists
}

// contractInstanceState reads the instance evidence through the shared
// contract-detail SWR cache. ok=false when the read failed.
func (h *Handler) contractInstanceState(ctx context.Context, cid string) (clickhouse.ContractInstanceState, bool) {
	v, _, _, err := h.contractDetailCached(ctx, "inst:"+cid, func(rctx context.Context) (any, error) {
		return h.Reader.ContractInstanceState(rctx, cid)
	})
	if err != nil {
		h.Logger.Warn("explorer ContractInstanceState failed", "contract", cid, "err", err)
		return clickhouse.ContractInstanceState{}, false
	}
	st, ok := v.(clickhouse.ContractInstanceState)
	return st, ok
}

// contractTTL judges the instance's live_until against the lake watermark —
// the same ledger every lake-backed view is stamped with. Nil when there is
// no TTL row or no watermark to judge it against.
func (h *Handler) contractTTL(ctx context.Context, liveUntil uint32) *ContractTTLV {
	if liveUntil == 0 || h.LakeWatermark == nil {
		return nil
	}
	tip, _, ok := h.LakeWatermark(ctx)
	if !ok {
		return nil
	}
	var state string
	switch clickhouse.TTLVerdictAt(liveUntil, tip) {
	case clickhouse.TTLLive:
		state = ttlStateLive
	case clickhouse.TTLArchived:
		state = ttlStateArchived
	default:
		return nil
	}
	return &ContractTTLV{LiveUntil: liveUntil, State: state, AsOfLedger: tip}
}

// parseContractEventsCursor decodes the opaque `?cursor=` for the contract
// activity feed — dotted-decimal with the tx_hash segment second
// ("63000000.<64-hex>.0.2"), mirroring parseMovementCursor. The tx_hash
// segment is required: the 3-part (ledger, op_index, event_index) tuple is
// not unique (single-op txs all tie at 0.0), and paging on it permanently
// skipped tied rows at page boundaries (cold audit 2026-08-03). Old 3-part
// cursors are rejected as invalid — they are short-lived client echoes, and
// resuming them exactly is impossible without the tx discriminator anyway.
func (h *Handler) parseContractEventsCursor(w http.ResponseWriter, r *http.Request) (clickhouse.ContractEventsCursor, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return clickhouse.ContractEventsCursor{}, true
	}
	bad := func() (clickhouse.ContractEventsCursor, bool) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return clickhouse.ContractEventsCursor{}, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 {
		return bad()
	}
	ledger, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || ledger == 0 {
		return bad()
	}
	txHash := parts[1]
	if txHash == "" {
		return bad()
	}
	opIdx, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return bad()
	}
	evIdx, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return bad()
	}
	return clickhouse.ContractEventsCursor{
		Ledger:     uint32(ledger),
		TxHash:     txHash,
		OpIndex:    uint32(opIdx),
		EventIndex: uint32(evIdx),
	}, true
}

func contractEventView(e clickhouse.ContractActivityRow) ContractEventView {
	return ContractEventView{
		Ledger:     e.Seq,
		CloseTime:  e.CloseTime.UTC().Format(time.RFC3339),
		TxHash:     e.TxHash,
		OpIndex:    e.OpIndex,
		EventIndex: e.EventIndex,
		EventType:  e.EventType,
		Topic0:     e.Topic0Sym,
		Topics:     e.TopicsDisplay,
		Data:       e.DataDisplay,
	}
}

// contractEventsFirstPageCached serves the contract's first activity page
// through the shared contract-detail SWR cache, computing at the max page
// size and slicing to the request's limit (see ContractDetail).
func (h *Handler) contractEventsFirstPageCached(ctx context.Context, cid string, limit int) ([]clickhouse.ContractActivityRow, time.Time, bool, error) {
	v, asOf, degraded, err := h.contractDetailCached(ctx, "ev:"+cid, func(rctx context.Context) (any, error) {
		full, cerr := h.Reader.ContractEventsRecent(rctx, cid, 500, clickhouse.ContractEventsCursor{})
		if cerr != nil {
			return nil, cerr
		}
		return full, nil
	})
	if err != nil {
		return nil, time.Time{}, false, err
	}
	rows, _ := v.([]clickhouse.ContractActivityRow)
	if limit < len(rows) {
		rows = rows[:limit]
	}
	return rows, asOf, degraded, nil
}
