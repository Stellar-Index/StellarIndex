package explorer

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// LedgerView is the wire shape for a ledger header (ADR-0038). Every stroop
// amount (total_coins, fee_pool, base_fee, base_reserve) is a decimal STRING,
// never a JSON number (ADR-0003). tx_count/op_count cover the whole tx set,
// failed txs included; soroban_event_count covers successful txs only.
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
	BaseFee           string `json:"base_fee"`
	BaseReserve       string `json:"base_reserve"`
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
		BaseFee:           strconv.FormatUint(uint64(l.BaseFee), 10),
		BaseReserve:       strconv.FormatUint(uint64(l.BaseReserve), 10),
	}
}

// TxSummaryView is the wire shape for a transaction summary (in ledger + tx
// listings). fee_charged/max_fee are stroops as decimal strings (ADR-0003).
// Memo is already decoded; memo_type carries the discriminant.
type TxSummaryView struct {
	Hash           string `json:"hash"`
	Ledger         uint32 `json:"ledger"`
	CloseTime      string `json:"close_time"`
	Index          uint32 `json:"index"`
	SourceAccount  string `json:"source_account"`
	FeeCharged     string `json:"fee_charged"`
	MaxFee         string `json:"max_fee"`
	OperationCount uint16 `json:"operation_count"`
	Successful     bool   `json:"successful"`
	ResultCode     int32  `json:"result_code"`
	// Result is the human-readable slug for ResultCode (e.g. "tx_success",
	// "tx_failed", "tx_insufficient_fee"), so a failed transaction states WHY
	// on the wire, not just via a bare integer. Always present.
	Result   string `json:"result"`
	MemoType string `json:"memo_type,omitempty"`
	Memo     string `json:"memo,omitempty"`
	// MemoBase64 is the lossless byte-for-byte copy of a MEMO_TEXT memo.
	// MEMO_TEXT is opaque XDR bytes, not guaranteed UTF-8 (exchange deposit
	// tokens are frequently binary/latin-1); encoding/json silently replaces
	// invalid bytes in Memo with U+FFFD, which breaks deposit-attribution
	// reconciliation with no signal that anything changed. Present only for
	// memo_type "text" — Memo stays the best-effort display value.
	MemoBase64 string `json:"memo_base64,omitempty"`
	// FeeBump is present on a fee-bump transaction: Hash is then the OUTER
	// hash, SourceAccount the inner tx's source, and MaxFee the fee payer's
	// bid — the bound FeeCharged is held to. Absent on a fee bump ingested
	// before the lake captured the outer layer; its Result still reads
	// tx_fee_bump_inner_success / tx_fee_bump_inner_failed.
	FeeBump *FeeBumpView `json:"fee_bump,omitempty"`
}

// FeeBumpView is a fee bump's outer layer and the inner transaction it wraps.
// The inner fields are absent when the result carried no inner result pair.
type FeeBumpView struct {
	FeeAccount      string `json:"fee_account"`
	InnerHash       string `json:"inner_hash,omitempty"`
	InnerMaxFee     string `json:"inner_max_fee"`
	InnerResultCode *int32 `json:"inner_result_code,omitempty"`
	InnerResult     string `json:"inner_result,omitempty"`
}

func txSummaryView(t clickhouse.TxSummary) TxSummaryView {
	v := txSummaryBase(t)
	if t.FeeAccount == "" {
		return v
	}
	fb := &FeeBumpView{FeeAccount: t.FeeAccount, InnerMaxFee: strconv.FormatInt(t.MaxFee, 10)}
	if t.InnerTxHash != "" {
		code := t.InnerResultCode
		fb.InnerHash = t.InnerTxHash
		fb.InnerResultCode = &code
		fb.InnerResult = xdrjson.TxResultName(code)
	}
	v.MaxFee = strconv.FormatInt(t.FeeBumpFee, 10)
	v.FeeBump = fb
	return v
}

func txSummaryBase(t clickhouse.TxSummary) TxSummaryView {
	v := TxSummaryView{
		Hash:           t.TxHash,
		Ledger:         t.Seq,
		CloseTime:      t.CloseTime.UTC().Format(time.RFC3339),
		Index:          t.TxIndex,
		SourceAccount:  t.SourceAccount,
		FeeCharged:     strconv.FormatInt(t.FeeCharged, 10),
		MaxFee:         strconv.FormatInt(t.MaxFee, 10),
		OperationCount: t.OperationCount,
		Successful:     t.Successful,
		ResultCode:     t.ResultCode,
		Result:         xdrjson.TxResultName(t.ResultCode),
		MemoType:       xdrjson.MemoTypeName(t.MemoType),
		Memo:           t.Memo,
	}
	if v.MemoType == "text" {
		v.MemoBase64 = base64.StdEncoding.EncodeToString([]byte(t.Memo))
	}
	return v
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
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer RecentLedgers deadline exceeded")
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/ledgers-timeout",
				"Ledgers listing timed out")
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
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer LedgerBySeq deadline exceeded", "seq", seq)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/ledger-detail-timeout",
				"Ledger detail timed out")
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
// Total is the ledger's exact transaction count (from the ledger header);
// Truncated is true when Total exceeds len(Transactions), so a consumer
// never has to guess whether the page-size cap cut off real data.
type LedgerTransactionsView struct {
	Ledger       uint32          `json:"ledger"`
	Transactions []TxSummaryView `json:"transactions"`
	Total        uint32          `json:"total"`
	Truncated    bool            `json:"truncated"`
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
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer LedgerTransactions deadline exceeded", "seq", seq)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/ledger-transactions-timeout",
				"Ledger transactions timed out")
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
	// Total/Truncated come from the ledger header, not the tx query, so a
	// header-read hiccup only loses this metadata (both fields stay zero)
	// rather than failing a request the transaction fetch already served.
	if hdr, found, herr := h.Reader.LedgerBySeq(ctx, seq); herr != nil {
		h.Logger.Warn("explorer LedgerBySeq (transactions total) failed", "err", herr, "seq", seq)
	} else if found {
		out.Total = hdr.TxCount
		out.Truncated = hdr.TxCount > uint32(len(rows))
	}
	h.WriteJSON(w, out, false)
}
