package explorer

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
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
	// CoverageNote is non-empty when a NON-FATAL sub-read failed (per-op result codes or contract
	// events). `events` and each op's `result_code` are omitempty, so without this note a failed
	// read is byte-identical to a real empty. Absent = every sub-read succeeded.
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
		if h.txReadFailed(w, r, ctx, err, "TransactionByHash", hash) {
			return
		}
		h.Logger.Error("explorer TransactionByHash failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	tip, stale, tipOK := h.lakeTip(ctx)
	if !found {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/tx-not-found",
			"Transaction not found", http.StatusNotFound, txNotFoundDetail(tip, stale, tipOK))
		return
	}
	// A fee bump found by its inner hash: its rows are keyed on the outer one.
	hash = tx.TxHash

	ops, err := h.Reader.OperationsByTx(ctx, tx.Seq, hash)
	if err != nil {
		if h.txReadFailed(w, r, ctx, err, "OperationsByTx", hash) {
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
		if h.txReadFailed(w, r, ctx, err, "OperationResultsByTx", hash) {
			return
		}
		// Non-fatal: serve ops without per-op result codes, but DISCLOSE it:
		// result_code is omitempty, so a failed read is otherwise
		// byte-identical to a genuinely absent code.
		h.Logger.Error("explorer OperationResultsByTx failed", "err", err, "hash", hash)
		results = nil
		resultsPartial = true
	}
	events, err := h.Reader.EventsByTx(ctx, tx.Seq, hash)
	eventsPartial := false
	if err != nil {
		if h.txReadFailed(w, r, ctx, err, "EventsByTx", hash) {
			return
		}
		// Non-fatal: serve the tx without its contract events, but DISCLOSE it:
		// events is omitempty, so a failed read is otherwise
		// byte-identical to a tx that emitted none.
		h.Logger.Error("explorer EventsByTx failed", "err", err, "hash", hash)
		events = nil
		eventsPartial = true
	}

	h.writeJSONAt(w, TxDetailView{
		TxSummaryView: txSummaryView(tx),
		Operations:    buildTxOpViews(ops, results, tx.Successful, tx.ResultCode),
		Events:        buildTxEventViews(events),
		CoverageNote:  txCoverageNote(resultsPartial, eventsPartial),
	}, stale, resultsPartial || eventsPartial, time.Time{})
}

// txNotFoundDetail keeps a behind-the-network lake from making a recent
// transaction's 404 indistinguishable from a hash that never existed.
func txNotFoundDetail(tip uint32, stale, tipOK bool) string {
	const base = "no transaction with that hash in the indexed range"
	switch {
	case !stale:
		return base
	case tipOK:
		return fmt.Sprintf("%s, which ends at ledger %d and is behind the network; a recent transaction may not be indexed yet", base, tip)
	default:
		return base + "; the index's freshness is unknown, so a recent transaction may not be indexed yet"
	}
}

// txReadFailed handles the two outcomes every TxDetail read shares: a client hangup writes
// nothing, and a blown budget answers retryable. The budget belongs to the whole request, so
// later sub-reads are doomed too, and a "partial" transaction assembled from failures would be a
// 200 read as fact; a deadline is therefore never the disclosable-partial class. Returns true
// when the response has been written.
func (h *Handler) txReadFailed(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, read, hash string) bool {
	if h.ClientAborted(r, err) {
		return true
	}
	if retryableColdMiss(ctx, err) {
		h.Logger.Warn("explorer "+read+" deadline exceeded", "hash", hash)
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/tx-detail-timeout",
			"Transaction detail timed out")
		return true
	}
	return false
}

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

// buildTxOpViews decodes a transaction's operations, stamps each with its parent transaction's
// success (so a failed tx's operations are not mistaken for applied), and attaches each op's
// result code and slug, plus the inner slug of an op_inner operation.
func buildTxOpViews(ops []clickhouse.OpRow, results map[uint32]clickhouse.OpResult, txSuccessful bool, txResultCode int32) []OpView {
	out := make([]OpView, len(ops))
	// One immutable bool shared by every op — they all belong to this tx and
	// share its outcome.
	txOK := txSuccessful
	txResult := xdrjson.TxResultName(txResultCode)
	for i, o := range ops {
		ov := opView(o)
		ov.TransactionSuccessful = &txOK
		ov.TransactionResult = txResult
		if res, ok := results[o.OpIndex]; ok {
			c := res.Code
			ov.ResultCode = &c
			ov.Result = xdrjson.OpResultName(c)
			ov.InnerResult, _ = xdrjson.OpInnerResultName(res.ResultXDR)
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
