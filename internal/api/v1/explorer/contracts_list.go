package explorer

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ledgersPerDay is the approximate Stellar ledger cadence (≈5s close time →
// 17,280/day). Used to translate a `?days=` window into a sinceLedger floor
// so the contract aggregates stay primary-key-range-scoped.
const ledgersPerDay = 17_280

// ContractDirectoryEntry is one row of GET /v1/contracts.
type ContractDirectoryEntry struct {
	ContractID string `json:"contract_id"`
	Events     int64  `json:"events"`
	LastLedger uint32 `json:"last_ledger"`
	LastSeen   string `json:"last_seen"`
	// Protocol is the owning protocol (e.g. "blend", "soroswap") when the
	// contract is in the protocol_contracts registry — the attribution hinge.
	// Omitted for unattributed contracts.
	Protocol string `json:"protocol,omitempty"`
}

// ContractsDirectoryView is the wire response for GET /v1/contracts.
type ContractsDirectoryView struct {
	WindowDays  int                      `json:"window_days"`
	SinceLedger uint32                   `json:"since_ledger"`
	Contracts   []ContractDirectoryEntry `json:"contracts"`
}

// ContractsList serves GET /v1/contracts — the contracts directory:
// the most active contracts (by emitted-event count) over a recent window,
// each tagged with its owning protocol where known. `?days=` sets the window
// (default 30, max 365); `?limit=` the row count (default 100, max 500).
func (h *Handler) ContractsList(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 || n > 365 {
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-window",
				"Invalid days", http.StatusBadRequest, "days must be an integer in [1, 365]")
			return
		}
		days = n
	}
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	since := h.windowFloorLedger(ctx, days)

	rows, err := h.Reader.RecentContracts(ctx, limit, since)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer RecentContracts failed", "err", err, "since", since)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	attribution := h.contractAttribution(ctx)
	out := ContractsDirectoryView{
		WindowDays:  days,
		SinceLedger: since,
		Contracts:   make([]ContractDirectoryEntry, len(rows)),
	}
	for i, c := range rows {
		out.Contracts[i] = ContractDirectoryEntry{
			ContractID: c.ContractID,
			Events:     c.Events,
			LastLedger: c.LastLedger,
			LastSeen:   c.LastSeen.UTC().Format(time.RFC3339),
			Protocol:   attribution[c.ContractID],
		}
	}
	h.WriteJSON(w, out, false)
}

// ContractEdge is one edge of a contract's interaction map.
type ContractEdge struct {
	ContractID string `json:"contract_id"`
	SharedTxs  int64  `json:"shared_txs"`
	Protocol   string `json:"protocol,omitempty"`
}

// ContractInteractionsView is the wire response for
// GET /v1/contracts/{contract_id}/interactions.
type ContractInteractionsView struct {
	ContractID   string         `json:"contract_id"`
	WindowDays   int            `json:"window_days"`
	SinceLedger  uint32         `json:"since_ledger"`
	Interactions []ContractEdge `json:"interactions"`
}

// ContractInteractions serves GET /v1/contracts/{contract_id}/interactions
// — the contract interaction map: other contracts that emitted events in the
// same transactions as this one (a proxy for cross-contract calls, since
// Soroban sub-invocations nest within one tx), ranked by shared-tx count and
// tagged with their owning protocol where known.
func (h *Handler) ContractInteractions(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	cid := r.PathValue("contract_id")
	// Validate up front (P2/C3-9, audit-2026-07-16): a malformed
	// contract_id otherwise reaches ClickHouse before any 400. IsContractID
	// is the same C-strkey validator the sibling contract endpoints
	// (contracts.go ContractDetail, wasm_view.go ContractWasm) enforce.
	if !canonical.IsContractID(cid) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-contract-id",
			"Invalid contract id", http.StatusBadRequest,
			"the contract id must be a valid C-strkey")
		return
	}
	days := 90
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 || n > 365 {
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-window",
				"Invalid days", http.StatusBadRequest, "days must be an integer in [1, 365]")
			return
		}
		days = n
	}
	limit, ok := h.ParseLimit(w, r, 50, 200)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	since := h.windowFloorLedger(ctx, days)

	edges, err := h.Reader.ContractInteractions(ctx, cid, limit, since)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer ContractInteractions failed", "err", err, "contract", cid)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	attribution := h.contractAttribution(ctx)
	out := ContractInteractionsView{
		ContractID:   cid,
		WindowDays:   days,
		SinceLedger:  since,
		Interactions: make([]ContractEdge, len(edges)),
	}
	for i, e := range edges {
		out.Interactions[i] = ContractEdge{
			ContractID: e.ContractID,
			SharedTxs:  e.SharedTxs,
			Protocol:   attribution[e.ContractID],
		}
	}
	h.WriteJSON(w, out, false)
}

// ContractCodeVersionV is one entry in a contract's code-upgrade timeline.
type ContractCodeVersionV struct {
	Ledger    uint32 `json:"ledger"`
	CloseTime string `json:"close_time"`
	WasmHash  string `json:"wasm_hash"`
}

// ContractCodeHistoryView is the wire response for
// GET /v1/contracts/{contract_id}/code-history.
type ContractCodeHistoryView struct {
	ContractID string                 `json:"contract_id"`
	Versions   []ContractCodeVersionV `json:"versions"`
}

// ContractCodeHistory serves GET /v1/contracts/{contract_id}/code-history
// — the contract's WASM-hash timeline ("change over time"): each distinct
// executable the contract instance has pointed at, chronologically, so an
// in-place upgrade shows as a new version. Empty when the instance isn't in the
// captured ledger window (fills with the Phase-C backfill).
func (h *Handler) ContractCodeHistory(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	cid := r.PathValue("contract_id")
	// Validate up front (P2/C3-9, audit-2026-07-16) — see ContractInteractions.
	if !canonical.IsContractID(cid) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-contract-id",
			"Invalid contract id", http.StatusBadRequest,
			"the contract id must be a valid C-strkey")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	versions, err := h.Reader.ContractCodeHistory(ctx, cid)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer ContractCodeHistory failed", "err", err, "contract", cid)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := ContractCodeHistoryView{ContractID: cid, Versions: make([]ContractCodeVersionV, len(versions))}
	for i, v := range versions {
		out.Versions[i] = ContractCodeVersionV{Ledger: v.Ledger, CloseTime: v.CloseTime.UTC().Format(time.RFC3339), WasmHash: v.WasmHash}
	}
	h.WriteJSON(w, out, false)
}

// windowFloorLedger returns the ledger sequence `days` days before the tip,
// or 0 when the tip is unknown / the window reaches past genesis. Keeps the
// contract aggregates scoped to a primary-key range rather than a full scan.
func (h *Handler) windowFloorLedger(ctx context.Context, days int) uint32 {
	tip, err := h.Reader.RecentLedgers(ctx, 1, 0)
	if err != nil || len(tip) == 0 {
		return 0
	}
	span := uint32(days * ledgersPerDay) //nolint:gosec // days is clamped to [1,365]
	if span >= tip[0].Seq {
		return 0
	}
	return tip[0].Seq - span
}

// contractAttribution loads the contract_id → protocol map (best-effort —
// a registry read failure degrades to no attribution, never a request error).
func (h *Handler) contractAttribution(ctx context.Context) map[string]string {
	if h.ProtocolContracts == nil {
		return map[string]string{}
	}
	idx, err := h.ProtocolContracts.ProtocolContractIndex(ctx)
	if err != nil {
		h.Logger.Warn("contract attribution index read failed", "err", err)
		return map[string]string{}
	}
	return idx
}
