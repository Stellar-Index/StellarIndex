package explorer

import (
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// AccountTransactionsView is the wire response for
// GET /v1/accounts/{g_strkey}/transactions. NextCursor is the opaque keyset
// cursor for the next (older) page — composite (ledger, tx_index) so an account
// that submits many txs in one ledger never loses rows across a page boundary.
type AccountTransactionsView struct {
	Account      string          `json:"account"`
	Transactions []TxSummaryView `json:"transactions"`
	NextCursor   string          `json:"next_cursor,omitempty"`
	// Scope documents the coverage: "all" = sourced + incoming/participant
	// activity (ADR-0038 Phase B — the participant index is wired). Incoming
	// coverage tracks the participant-index capture + backfill.
	Scope string `json:"scope"`
}

// AccountOperationsView is the wire response for
// GET /v1/accounts/{g_strkey}/operations. NextCursor is the opaque keyset cursor
// for the next (older) page — composite (ledger, tx_index, op_index).
type AccountOperationsView struct {
	Account    string   `json:"account"`
	Operations []OpView `json:"operations"`
	NextCursor string   `json:"next_cursor,omitempty"`
	Scope      string   `json:"scope"`
}

// accountScopeAll = sourced + incoming/participant activity (ADR-0038 Phase B;
// the participant index is wired). Incoming coverage tracks participant-index
// capture + backfill.
const accountScopeAll = "all"

// parseAccountStrkey validates the {g_strkey} path segment. ok=false (after a
// problem+json) on an invalid strkey.
func (h *Handler) parseAccountStrkey(w http.ResponseWriter, r *http.Request) (string, bool) {
	g := r.PathValue("g_strkey")
	if !canonical.IsAccountID(g) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-account",
			"Invalid account", http.StatusBadRequest,
			"the account must be a valid G-strkey")
		return "", false
	}
	return g, true
}

// AccountTransactions serves GET /v1/accounts/{g_strkey}/transactions —
// transactions involving the account (sourced + incoming/participant), newest
// first (scope: "all", ADR-0038 Phase B).
func (h *Handler) AccountTransactions(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, 50, 200)
	if !ok {
		return
	}
	cur, ok := h.parseExplorerCursor(w, r, 2) // (ledger, tx_index)
	if !ok {
		return
	}
	rows, err := h.Reader.AccountTransactions(r.Context(), g, limit, cur)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer AccountTransactions failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := AccountTransactionsView{Account: g, Scope: accountScopeAll, Transactions: make([]TxSummaryView, len(rows))}
	for i, t := range rows {
		out.Transactions[i] = txSummaryView(t)
	}
	if n := len(rows); n == limit {
		last := rows[n-1]
		out.NextCursor = encodeCursor(last.Seq, last.TxIndex)
	}
	h.WriteJSON(w, out, false)
}

// AccountOperations serves GET /v1/accounts/{g_strkey}/operations —
// operations involving the account (sourced + incoming/participant), decoded,
// newest first (scope: "all", ADR-0038 Phase B).
func (h *Handler) AccountOperations(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, 50, 200)
	if !ok {
		return
	}
	cur, ok := h.parseExplorerCursor(w, r, 3) // (ledger, tx_index, op_index)
	if !ok {
		return
	}
	rows, err := h.Reader.AccountOperations(r.Context(), g, limit, cur)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer AccountOperations failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := AccountOperationsView{Account: g, Scope: accountScopeAll, Operations: make([]OpView, len(rows))}
	for i, o := range rows {
		out.Operations[i] = opView(o)
	}
	if n := len(rows); n == limit {
		last := rows[n-1]
		out.NextCursor = encodeCursor(last.Seq, last.TxIndex, last.OpIndex)
	}
	h.WriteJSON(w, out, false)
}
