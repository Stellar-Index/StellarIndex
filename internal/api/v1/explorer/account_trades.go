package explorer

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// This file serves GET /v1/accounts/{g_strkey}/trades — the address's
// historic trades out of the Postgres `trades` hypertable. Sibling of
// movements.go / positions.go: same package, same Handler, same
// response-writing seams, same x-stability: experimental posture.

// TradesReader is the narrow Postgres read seam this endpoint needs.
// *timescale.Store satisfies it. Nil disables the endpoint (503), same
// degrade pattern as PositionsReader.
type TradesReader interface {
	ListAccountTrades(ctx context.Context, address string, limit int, cur timescale.AccountTradesCursor) ([]timescale.AccountTradeRow, error)
}

// accountTradesDefaultLimit / accountTradesMaxLimit — the pagination
// contract (mirrors the sibling explorer listings).
const (
	accountTradesDefaultLimit = 50
	accountTradesMaxLimit     = 200
)

// accountTradesScopeNote is the static honesty note every response
// carries: it states exactly which trades CAN appear here, so an empty
// page is never mistaken for "this address never traded anywhere".
// The `trades` table attributes accounts via its taker/maker columns —
// see timescale/account_trades.go's file header for the evidence.
const accountTradesScopeNote = "On-chain trades where this address is recorded as the acting account " +
	"(taker) or the resting sdex offer owner (maker). Coverage: sdex, aquarius, phoenix, comet. " +
	"Soroswap swaps do not yet record the acting account and off-chain CEX/FX trades carry no " +
	"Stellar account, so neither can appear here."

// AccountTradeEntry is one row in the wire response. Amounts and
// usd_volume are decimal strings (ADR-0003); usd_volume is absent when
// the stored value is NULL (unknown), never "0". There is deliberately
// NO price field: `trades` stores no price column and deriving one
// needs per-asset decimals — serving quote/base raw would be a wrong
// number with a right-looking name.
type AccountTradeEntry struct {
	Ts           string `json:"ts"`
	Source       string `json:"source"`
	BaseAsset    string `json:"base_asset"`
	QuoteAsset   string `json:"quote_asset"`
	BaseAmount   string `json:"base_amount"`
	QuoteAmount  string `json:"quote_amount"`
	USDVolume    string `json:"usd_volume,omitempty"`
	TxHash       string `json:"tx_hash"`
	Ledger       uint32 `json:"ledger"`
	OpIndex      uint32 `json:"op_index"`
	Role         string `json:"role"`
	Counterparty string `json:"counterparty,omitempty"`
	RoutedVia    string `json:"routed_via,omitempty"`
}

// AccountTradesView is the wire response for GET
// /v1/accounts/{g_strkey}/trades.
type AccountTradesView struct {
	Account    string              `json:"account"`
	Trades     []AccountTradeEntry `json:"trades"`
	NextCursor string              `json:"next_cursor,omitempty"`
	Note       string              `json:"note"`
}

// encodeAccountTradesCursor renders the keyset position of the last
// served row as the opaque `?cursor=` value:
// "<ts unixnano>.<ledger>.<tx_hash>.<op_index>" — dotted with the
// tx_hash segment third (safe: tx_hash is fixed 64-char hex, never
// contains '.'), mirroring the movements cursor convention.
func encodeAccountTradesCursor(r timescale.AccountTradeRow) string {
	return strconv.FormatInt(r.Ts.UTC().UnixNano(), 10) + "." +
		strconv.FormatUint(uint64(r.Ledger), 10) + "." +
		r.TxHash + "." +
		strconv.FormatUint(uint64(r.OpIndex), 10)
}

// parseAccountTradesCursor decodes the opaque `?cursor=`. ok=false
// (after a problem+json) on a malformed value.
func (h *Handler) parseAccountTradesCursor(w http.ResponseWriter, r *http.Request) (timescale.AccountTradesCursor, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return timescale.AccountTradesCursor{}, true
	}
	bad := func() (timescale.AccountTradesCursor, bool) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return timescale.AccountTradesCursor{}, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 {
		return bad()
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || nanos <= 0 {
		return bad()
	}
	ledger, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return bad()
	}
	if parts[2] == "" {
		return bad()
	}
	opIdx, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return bad()
	}
	return timescale.AccountTradesCursor{
		Ts:      time.Unix(0, nanos).UTC(),
		Ledger:  uint32(ledger),
		TxHash:  parts[2],
		OpIndex: uint32(opIdx),
	}, true
}

// tradesUnavailable writes the 503 for this endpoint — its hard
// dependency is the Postgres trades reader, not the ClickHouse lake.
func (h *Handler) tradesUnavailable(w http.ResponseWriter, r *http.Request) {
	h.WriteProblem(w, r,
		"https://api.stellarindex.io/errors/explorer-unavailable",
		"Explorer unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the Postgres trades reader.")
}

// AccountTrades serves GET /v1/accounts/{g_strkey}/trades — the
// address's historic trades (taker or maker side), newest first,
// keyset-paged by the opaque (ts, ledger, tx_hash, op_index) cursor.
func (h *Handler) AccountTrades(w http.ResponseWriter, r *http.Request) {
	if h.Trades == nil {
		h.tradesUnavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, accountTradesDefaultLimit, accountTradesMaxLimit)
	if !ok {
		return
	}
	cur, ok := h.parseAccountTradesCursor(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	rows, err := h.Trades.ListAccountTrades(ctx, g, limit, cur)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if readTimedOut(ctx, err) {
			h.Logger.Warn("explorer AccountTrades deadline exceeded", "account", g)
			h.writeReadTimeout(w, r, "https://api.stellarindex.io/errors/account-trades-timeout",
				"Account trades timed out")
			return
		}
		h.Logger.Error("explorer AccountTrades failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	out := AccountTradesView{
		Account: g,
		Trades:  make([]AccountTradeEntry, len(rows)),
		Note:    accountTradesScopeNote,
	}
	for i, t := range rows {
		out.Trades[i] = accountTradeEntryView(t)
	}
	if len(rows) == limit {
		out.NextCursor = encodeAccountTradesCursor(rows[len(rows)-1])
	}
	h.WriteJSON(w, out, false)
}

// accountTradeEntryView renders one reader row as its wire shape.
func accountTradeEntryView(t timescale.AccountTradeRow) AccountTradeEntry {
	return AccountTradeEntry{
		Ts:           t.Ts.UTC().Format(time.RFC3339),
		Source:       t.Source,
		BaseAsset:    t.BaseAsset,
		QuoteAsset:   t.QuoteAsset,
		BaseAmount:   t.BaseAmount,
		QuoteAmount:  t.QuoteAmount,
		USDVolume:    t.USDVolume,
		TxHash:       t.TxHash,
		Ledger:       t.Ledger,
		OpIndex:      t.OpIndex,
		Role:         t.Role,
		Counterparty: t.Counterparty,
		RoutedVia:    t.RoutedVia,
	}
}
