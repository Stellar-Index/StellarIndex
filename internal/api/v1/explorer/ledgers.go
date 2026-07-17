package explorer

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// LedgerView is the wire shape for a ledger header (ADR-0038). total_coins and
// fee_pool are XLM stroops as decimal STRINGS — they exceed 2^53 so a JSON
// number would lose precision (ADR-0003).
type LedgerView struct {
	Sequence          uint32 `json:"sequence"`
	CloseTime         string `json:"close_time"`
	Hash              string `json:"hash"`
	PrevHash          string `json:"prev_hash"`
	ProtocolVersion   uint32 `json:"protocol_version"`
	TxCount           uint32 `json:"tx_count"`
	OpCount           uint32 `json:"op_count"`
	SorobanEventCount uint32 `json:"soroban_event_count"`
	TotalCoins        string `json:"total_coins"`
	FeePool           string `json:"fee_pool"`
	BaseFee           uint32 `json:"base_fee"`
	BaseReserve       uint32 `json:"base_reserve"`
}

func ledgerView(l clickhouse.LedgerHeader) LedgerView {
	return LedgerView{
		Sequence:          l.Seq,
		CloseTime:         l.CloseTime.UTC().Format(time.RFC3339),
		Hash:              l.LedgerHash,
		PrevHash:          l.PrevHash,
		ProtocolVersion:   l.ProtocolVersion,
		TxCount:           l.TxCount,
		OpCount:           l.OpCount,
		SorobanEventCount: l.SorobanEventCount,
		TotalCoins:        strconv.FormatInt(l.TotalCoins, 10),
		FeePool:           strconv.FormatInt(l.FeePool, 10),
		BaseFee:           l.BaseFee,
		BaseReserve:       l.BaseReserve,
	}
}

// TxSummaryView is the wire shape for a transaction summary (in ledger + tx
// listings). fee_charged/max_fee fit a JSON number (they're capped well below
// 2^53). Memo is already decoded; memo_type carries the discriminant.
type TxSummaryView struct {
	Hash           string `json:"hash"`
	Ledger         uint32 `json:"ledger"`
	CloseTime      string `json:"close_time"`
	Index          uint32 `json:"index"`
	SourceAccount  string `json:"source_account"`
	FeeCharged     int64  `json:"fee_charged"`
	MaxFee         int64  `json:"max_fee"`
	OperationCount uint16 `json:"operation_count"`
	Successful     bool   `json:"successful"`
	ResultCode     int32  `json:"result_code"`
	MemoType       string `json:"memo_type,omitempty"`
	Memo           string `json:"memo,omitempty"`
}

func txSummaryView(t clickhouse.TxSummary) TxSummaryView {
	return TxSummaryView{
		Hash:           t.TxHash,
		Ledger:         t.Seq,
		CloseTime:      t.CloseTime.UTC().Format(time.RFC3339),
		Index:          t.TxIndex,
		SourceAccount:  t.SourceAccount,
		FeeCharged:     t.FeeCharged,
		MaxFee:         t.MaxFee,
		OperationCount: t.OperationCount,
		Successful:     t.Successful,
		ResultCode:     t.ResultCode,
		MemoType:       xdrjson.MemoTypeName(t.MemoType),
		Memo:           t.Memo,
	}
}

// LedgersListView is the wire response for GET /v1/ledgers. NextBefore is the
// keyset cursor for the next (older) page: re-request with ?before=<NextBefore>.
type LedgersListView struct {
	Ledgers    []LedgerView `json:"ledgers"`
	NextBefore uint32       `json:"next_before,omitempty"`
}

// LedgersList serves GET /v1/ledgers — recent ledgers, descending, keyset
// paged via ?before=<seq> & ?limit=.
func (h *Handler) LedgersList(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	limit, ok := h.ParseLimit(w, r, 50, 200)
	if !ok {
		return
	}
	before, ok := h.parseUint32Query(w, r, "before")
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	rows, err := h.Reader.RecentLedgers(ctx, limit, before)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer RecentLedgers failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := LedgersListView{Ledgers: make([]LedgerView, len(rows))}
	for i, l := range rows {
		out.Ledgers[i] = ledgerView(l)
	}
	if n := len(rows); n > 0 {
		out.NextBefore = rows[n-1].Seq
	}
	h.WriteJSON(w, out, false)
}

// parseLedgerSeq parses the {seq} path segment as a uint32. ok=false (after a
// problem+json) on a malformed value.
func (h *Handler) parseLedgerSeq(w http.ResponseWriter, r *http.Request) (uint32, bool) {
	raw := r.PathValue("seq")
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-ledger",
			"Invalid ledger sequence", http.StatusBadRequest,
			"the ledger path segment must be a non-negative 32-bit integer")
		return 0, false
	}
	return uint32(n), true
}

// LedgerDetail serves GET /v1/ledgers/{seq}.
func (h *Handler) LedgerDetail(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	seq, ok := h.parseLedgerSeq(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	l, found, err := h.Reader.LedgerBySeq(ctx, seq)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer LedgerBySeq failed", "err", err, "seq", seq)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !found {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/ledger-not-found",
			"Ledger not found", http.StatusNotFound,
			"ledger "+strconv.FormatUint(uint64(seq), 10)+" is not in the indexed range")
		return
	}
	h.WriteJSON(w, ledgerView(l), false)
}

// LedgerTransactionsView is the wire response for GET /v1/ledgers/{seq}/transactions.
type LedgerTransactionsView struct {
	Ledger       uint32          `json:"ledger"`
	Transactions []TxSummaryView `json:"transactions"`
}

// LedgerTransactions serves GET /v1/ledgers/{seq}/transactions.
func (h *Handler) LedgerTransactions(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	seq, ok := h.parseLedgerSeq(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, 200, 1000)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	rows, err := h.Reader.LedgerTransactions(ctx, seq, limit)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer LedgerTransactions failed", "err", err, "seq", seq)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := LedgerTransactionsView{Ledger: seq, Transactions: make([]TxSummaryView, len(rows))}
	for i, t := range rows {
		out.Transactions[i] = txSummaryView(t)
	}
	h.WriteJSON(w, out, false)
}
