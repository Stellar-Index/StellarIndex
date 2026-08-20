package explorer

import (
	"context"
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
	// CoverageNote is an honest-degrade signal (mirrors AccountMovements'
	// coverage_note): non-empty when a NON-FATAL sub-read failed while
	// assembling this response, so a degraded answer — operations without
	// their per-op result codes, or a tx served without its contract events —
	// is distinguishable on the wire from a transaction that genuinely
	// produced none. Both `events` and each op's `result_code` are omitempty,
	// so absent this note a FAILED read is byte-identical to a real empty.
	// Absent = every sub-read succeeded and empty arrays/absent codes are real.
	CoverageNote string `json:"coverage_note,omitempty"`
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

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	tx, found, err := h.Reader.TransactionByHash(ctx, hash)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer TransactionByHash deadline exceeded", "hash", hash)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/tx-detail-timeout",
				"Transaction detail timed out")
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

	ops, err := h.Reader.OperationsByTx(ctx, tx.Seq, hash)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer OperationsByTx deadline exceeded", "hash", hash)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/tx-detail-timeout",
				"Transaction detail timed out")
			return
		}
		h.Logger.Error("explorer OperationsByTx failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	results, err := h.Reader.OperationResultsByTx(ctx, tx.Seq, hash)
	resultsPartial := false
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		// Non-fatal: serve ops without per-op result codes, but DISCLOSE it
		// (W1.2). result_code is omitempty, so a failed read is otherwise
		// byte-identical to a genuinely absent code.
		h.Logger.Error("explorer OperationResultsByTx failed", "err", err, "hash", hash)
		results = nil
		resultsPartial = true
	}
	events, err := h.Reader.EventsByTx(ctx, tx.Seq, hash)
	eventsPartial := false
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		// Non-fatal: serve the tx without its contract events, but DISCLOSE it
		// (W1.2). events is omitempty, so a failed read is otherwise
		// byte-identical to a tx that emitted none.
		h.Logger.Error("explorer EventsByTx failed", "err", err, "hash", hash)
		events = nil
		eventsPartial = true
	}

	h.WriteJSON(w, TxDetailView{
		TxSummaryView: txSummaryView(tx),
		Operations:    buildTxOpViews(ops, results, tx.Successful, tx.ResultCode),
		Events:        buildTxEventViews(events),
		CoverageNote:  txCoverageNote(resultsPartial, eventsPartial),
	}, false)
}

// txCoverageNote is the tx-detail feed's honest-degrade statement (mirrors
// movementsCoverageNote / AccountMovements' coverage_note): non-empty when a
// non-fatal sub-read failed, so a degraded response is distinguishable on the
// wire from a genuinely-empty one. Empty = every sub-read succeeded.
func txCoverageNote(resultsFailed, eventsFailed bool) string {
	switch {
	case resultsFailed && eventsFailed:
		return "per-operation result codes and contract events are temporarily unavailable for this " +
			"transaction; the operations shown carry no result_code and the events list is incomplete, not empty"
	case resultsFailed:
		return "per-operation result codes are temporarily unavailable for this transaction; the " +
			"operations shown carry no result_code, which is not a successful/zero result"
	case eventsFailed:
		return "contract events are temporarily unavailable for this transaction; the events list is " +
			"incomplete, not genuinely empty"
	}
	return ""
}

// buildTxOpViews decodes a transaction's operations, stamps each with its
// parent transaction's success (so a failed tx's operations are unambiguously
// marked, not masquerading as applied), and attaches each op's result code +
// human slug (when known).
func buildTxOpViews(ops []clickhouse.OpRow, results map[uint32]int32, txSuccessful bool, txResultCode int32) []OpView {
	out := make([]OpView, len(ops))
	// One immutable bool shared by every op — they all belong to this tx and
	// share its outcome.
	txOK := txSuccessful
	txResult := txResultName(txResultCode)
	for i, o := range ops {
		ov := opView(o)
		ov.TransactionSuccessful = &txOK
		ov.TransactionResult = txResult
		if code, ok := results[o.OpIndex]; ok {
			c := code
			ov.ResultCode = &c
			ov.Result = opResultName(code)
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
