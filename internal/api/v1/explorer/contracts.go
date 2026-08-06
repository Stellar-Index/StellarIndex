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
			h.writeReadTimeout(w, r, "https://api.stellarindex.io/errors/contract-detail-timeout",
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
	for i, e := range rows {
		out.Events[i] = contractEventView(e)
	}
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
