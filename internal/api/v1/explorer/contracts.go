package explorer

import (
	"net/http"
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
// cursor for the next (older) page — composite (ledger, op_index, event_index)
// so a contract that emits many events in one ledger never loses rows across a
// page boundary. Echo it back as ?cursor=. Set only when a full page returned.
type ContractDetailView struct {
	ContractID string `json:"contract_id"`
	// Protocol names the registry protocol this contract belongs to
	// (blend, soroswap, …) when attribution is known (Pass-B CON-3:
	// a Blend pool page couldn't say it was Blend while the server
	// held the map).
	Protocol   string              `json:"protocol,omitempty"`
	Events     []ContractEventView `json:"events"`
	NextCursor string              `json:"next_cursor,omitempty"`
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
	cur, ok := h.parseExplorerCursor(w, r, 3) // (ledger, op_index, event_index)
	if !ok {
		return
	}
	rows, err := h.Reader.ContractEventsRecent(r.Context(), cid, limit, cur)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer ContractEventsRecent failed", "err", err, "contract", cid)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := ContractDetailView{ContractID: cid, Events: make([]ContractEventView, len(rows))}
	out.Protocol = h.contractAttribution(r.Context())[cid]
	for i, e := range rows {
		out.Events[i] = contractEventView(e)
	}
	// Only emit a cursor on a full page — a short page is the last page, so a
	// cursor there just costs the client one empty round-trip.
	if n := len(rows); n == limit {
		last := rows[n-1]
		out.NextCursor = encodeCursor(last.Seq, last.OpIndex, last.EventIndex)
	}
	h.WriteJSON(w, out, false)
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
