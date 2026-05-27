package v1

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// SEP41TransfersReader is the seam the handler reads through.
// timescale.Store satisfies it via ListSEP41Transfers.
type SEP41TransfersReader interface {
	ListSEP41Transfers(ctx context.Context, contractID, fromAddr, toAddr string, limit int) ([]timescale.SEP41TransferRow, error)
}

// SEP41TransferEntry is one row in the wire response. Amount is a
// string (not a JSON number) per ADR-0003 — i128 values exceed
// IEEE 754 double precision above 2^53.
type SEP41TransferEntry struct {
	Ledger          uint32 `json:"ledger"`
	LedgerCloseTime string `json:"ledger_close_time"`
	TxHash          string `json:"tx_hash"`
	OpIndex         uint32 `json:"op_index"`
	EventIndex      uint32 `json:"event_index"`
	Kind            string `json:"event_kind"`
	From            string `json:"from,omitempty"`
	To              string `json:"to,omitempty"`
	Amount          string `json:"amount,omitempty"`
	LiveUntilLedger uint32 `json:"live_until_ledger,omitempty"`
	Authorized      *bool  `json:"authorized,omitempty"`
}

type SEP41TransfersResponse struct {
	ContractID string               `json:"contract_id"`
	Count      int                  `json:"count"`
	Limit      int                  `json:"limit"`
	From       string               `json:"from,omitempty"`
	To         string               `json:"to,omitempty"`
	Transfers  []SEP41TransferEntry `json:"transfers"`
}

// handleSEP41Transfers serves GET
// /v1/contracts/{contract_id}/transfers[?from=&to=&limit=].
//
// F-0021 closure (audit-2026-05-26): unlocks per-account net-
// position queries — the Stellar moat feature CG/CMC structurally
// cannot offer.
func (s *Server) handleSEP41Transfers(w http.ResponseWriter, r *http.Request) {
	if s.sep41Transfers == nil {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/sep41-transfers-unavailable",
			"SEP-41 transfers unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the sep41 transfers reader yet.")
		return
	}
	contractID := r.PathValue("contract_id")
	if contractID == "" {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/invalid-contract-id",
			"Invalid contract_id", http.StatusBadRequest,
			"contract_id path segment is required")
		return
	}

	fromAddr := r.URL.Query().Get("from")
	toAddr := r.URL.Query().Get("to")

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeProblem(w, r,
				"https://api.ratesengine.net/errors/invalid-limit",
				"Invalid limit", http.StatusBadRequest,
				"limit must be 1-500")
			return
		}
		limit = n
	}

	listCtx, listCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer listCancel()

	rows, err := s.sep41Transfers.ListSEP41Transfers(listCtx, contractID, fromAddr, toAddr, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(listCtx, err) {
			s.logger.Warn("ListSEP41Transfers deadline exceeded",
				"contract_id", contractID, "from", fromAddr, "to", toAddr, "limit", limit)
			writeProblem(w, r,
				"https://api.ratesengine.net/errors/sep41-transfers-timeout",
				"SEP-41 transfers timed out", http.StatusServiceUnavailable,
				"the per-contract scan didn't return in 8s; retry shortly.")
			return
		}
		if transientStorageErr(err) {
			s.logger.Warn("sep41 transfers list: transient storage error",
				"contract_id", contractID, "err", err)
			writeProblem(w, r,
				"https://api.ratesengine.net/errors/sep41-transfers-transient",
				"SEP-41 transfers temporarily unavailable", http.StatusServiceUnavailable,
				"the storage layer hit a transient error; retry shortly.")
			return
		}
		s.logger.Warn("sep41 transfers list",
			"contract_id", contractID, "from", fromAddr, "to", toAddr, "err", err)
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/sep41-transfers-error",
			"SEP-41 transfers failed", http.StatusInternalServerError,
			"Storage layer returned an error.")
		return
	}

	entries := make([]SEP41TransferEntry, 0, len(rows))
	for _, row := range rows {
		e := SEP41TransferEntry{
			Ledger:          row.Ledger,
			LedgerCloseTime: row.ObservedAt.UTC().Format(time.RFC3339Nano),
			TxHash:          row.TxHash,
			OpIndex:         row.OpIndex,
			EventIndex:      row.EventIndex,
			Kind:            string(row.Kind),
			From:            row.FromAddr,
			To:              row.ToAddr,
			LiveUntilLedger: row.LiveUntilLedger,
			Authorized:      row.Authorized,
		}
		if row.Amount != nil {
			e.Amount = row.Amount.String()
		}
		entries = append(entries, e)
	}

	resp := SEP41TransfersResponse{
		ContractID: contractID,
		Count:      len(entries),
		Limit:      limit,
		From:       fromAddr,
		To:         toAddr,
		Transfers:  entries,
	}
	writeJSON(w, resp, Flags{})
}
