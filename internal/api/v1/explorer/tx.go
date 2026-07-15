package explorer

import (
	"net/http"
	"regexp"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// txHashRe matches a Stellar transaction hash: 64 lowercase hex chars (the lake
// stores hashes hex-encoded). Upper-case is normalised before matching.
var txHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TxEventView is a contract event emitted within a transaction (tx-detail view).
type TxEventView struct {
	OpIndex    uint32 `json:"op_index"`
	EventIndex uint32 `json:"event_index"`
	ContractID string `json:"contract_id"`
	EventType  string `json:"event_type"`
	Topic0     string `json:"topic_0,omitempty"`
}

// TxDetailView is the wire response for GET /v1/tx/{hash}: the transaction
// summary, its decoded operations (each with its result code), and the contract
// events it emitted.
type TxDetailView struct {
	TxSummaryView
	Operations []OpView      `json:"operations"`
	Events     []TxEventView `json:"events,omitempty"`
}

// TxDetail serves GET /v1/tx/{hash}. The hash lookup uses the tx_hash
// skip-index on stellar.transactions; once the ledger is known, operations /
// results / events are ledger-scoped (partition-pruned, fast).
func (h *Handler) TxDetail(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	hash := normalizeHexHash(r.PathValue("hash"))
	if !txHashRe.MatchString(hash) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-tx-hash",
			"Invalid transaction hash", http.StatusBadRequest,
			"the hash must be 64 hexadecimal characters")
		return
	}

	tx, found, err := h.Reader.TransactionByHash(r.Context(), hash)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer TransactionByHash failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !found {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/tx-not-found",
			"Transaction not found", http.StatusNotFound,
			"no transaction with that hash in the indexed range")
		return
	}

	ops, err := h.Reader.OperationsByTx(r.Context(), tx.Seq, hash)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer OperationsByTx failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	results, err := h.Reader.OperationResultsByTx(r.Context(), tx.Seq, hash)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		results = nil // non-fatal: serve ops without per-op result codes
	}
	events, err := h.Reader.EventsByTx(r.Context(), tx.Seq, hash)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		events = nil // non-fatal
	}

	h.WriteJSON(w, TxDetailView{
		TxSummaryView: txSummaryView(tx),
		Operations:    buildTxOpViews(ops, results),
		Events:        buildTxEventViews(events),
	}, false)
}

// buildTxOpViews decodes a transaction's operations and attaches each op's
// result code (when known).
func buildTxOpViews(ops []clickhouse.OpRow, results map[uint32]int32) []OpView {
	out := make([]OpView, len(ops))
	for i, o := range ops {
		ov := opView(o)
		if code, ok := results[o.OpIndex]; ok {
			c := code
			ov.ResultCode = &c
		}
		out[i] = ov
	}
	return out
}

func buildTxEventViews(events []clickhouse.EventSummary) []TxEventView {
	if len(events) == 0 {
		return nil
	}
	out := make([]TxEventView, len(events))
	for i, e := range events {
		out[i] = txEventView(e)
	}
	return out
}

func txEventView(e clickhouse.EventSummary) TxEventView {
	return TxEventView{
		OpIndex:    e.OpIndex,
		EventIndex: e.EventIndex,
		ContractID: e.ContractID,
		EventType:  e.EventType,
		Topic0:     e.Topic0Sym,
	}
}

// normalizeHexHash lowercases an incoming hash so 64-hex matching is
// case-insensitive (some clients upper-case hashes).
func normalizeHexHash(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'F' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
