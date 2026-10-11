package explorer

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
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
	ListAccountTrades(ctx context.Context, address string, limit int, cur timescale.AccountTradesCursor) ([]timescale.AccountTradeRow, time.Time, error)
}

// accountTradesDefaultLimit / accountTradesMaxLimit — the pagination
// contract (mirrors the sibling explorer listings).
const (
	accountTradesDefaultLimit = 50
	accountTradesMaxLimit     = 200
)

// accountTradesScopeNote states which trades CAN appear here, so an empty page is never read as
// "this address never traded anywhere". Accounts are attributed via the taker/maker columns.
const accountTradesScopeNote = "On-chain trades where this address is recorded as the acting account " +
	"(taker) or the resting sdex offer owner (maker). Coverage: sdex, soroswap, aquarius, phoenix, comet " +
	"(soroswap records the taker for trades from 2026-07-30 onward; earlier soroswap rows carry none). " +
	"Off-chain CEX/FX trades carry no Stellar account, so they cannot appear here."

// AccountTradeEntry is one row in the wire response. Amounts and usd_volume are decimal strings;
// usd_volume is absent when unknown, never "0". There is deliberately no price field: `trades`
// stores none and deriving one needs per-asset decimals, so raw quote/base would be a wrong
// number with a right-looking name.
type AccountTradeEntry struct {
	Ts          string `json:"ts"`
	Source      string `json:"source"`
	BaseAsset   string `json:"base_asset"`
	QuoteAsset  string `json:"quote_asset"`
	BaseAmount  string `json:"base_amount"`
	QuoteAmount string `json:"quote_amount"`
	// BaseDecimals / QuoteDecimals scale each amount to whole units;
	// omitted when the asset's scale is unknown.
	BaseDecimals  *int   `json:"base_decimals,omitempty"`
	QuoteDecimals *int   `json:"quote_decimals,omitempty"`
	USDVolume     string `json:"usd_volume,omitempty"`
	TxHash        string `json:"tx_hash"`
	Ledger        uint32 `json:"ledger"`
	OpIndex       uint32 `json:"op_index"`
	Role          string `json:"role"`
	Counterparty  string `json:"counterparty,omitempty"`
	RoutedVia     string `json:"routed_via,omitempty"`
	// Signer is the transaction source account (fee-payer / initiator)
	// behind an AMM/Soroban swap — the human/EOA a router or contract
	// call hides behind the pool `taker`. Empty for non-AMM trades or
	// ones the signer sweeper has not yet tagged (migration 0150).
	Signer string `json:"signer,omitempty"`
}

// AccountTradesView is the wire response for GET
// /v1/accounts/{g_strkey}/trades.
type AccountTradesView struct {
	Account    string              `json:"account"`
	Trades     []AccountTradeEntry `json:"trades"`
	NextCursor string              `json:"next_cursor,omitempty"`
	Note       string              `json:"note"`
}

// accountTradesGate caps concurrent ListAccountTrades reads at four of the 25-connection serving
// pool: enough for a few genuine callers, too few to starve other Postgres-backed endpoints.
// accountTradesCallers holds each caller to one slot so one caller's parallel slow scans cannot
// shed everyone else.
var (
	accountTradesGate    = make(chan struct{}, 4)
	accountTradesCallers = callerSlots{held: map[string]struct{}{}}
)

// accountTradesGateWait is how long a request waits for a slot before
// shedding. Short by design — the point is to shed, not to queue.
const accountTradesGateWait = 750 * time.Millisecond

// callerSlots admits at most one in-flight request per caller key.
type callerSlots struct {
	mu   sync.Mutex
	held map[string]struct{}
}

func (c *callerSlots) acquire(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, busy := c.held[key]; busy {
		return false
	}
	c.held[key] = struct{}{}
	return true
}

func (c *callerSlots) release(key string) {
	c.mu.Lock()
	delete(c.held, key)
	c.mu.Unlock()
}

// encodeAccountTradesCursor renders the last served row's keyset position as the opaque cursor
// "<ts unixnano>.<ledger>.<tx_hash>.<op_index>". Dots are safe because tx_hash is 64-char hex.
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

// tradesBusy writes the retryable 503 shed when the account-trades gate is full. The scan never
// started, so this is "busy, retry", not a timeout; same 503 and type URL, only the detail differs.
func (h *Handler) tradesBusy(w http.ResponseWriter, r *http.Request) {
	h.WriteProblem(w, r,
		"https://api.stellarindex.io/errors/account-trades-timeout",
		"Account trades busy", http.StatusServiceUnavailable,
		"too many per-account trades scans are already in flight (at most one per caller); "+
			"this scan was NOT started on this request — retry in a moment")
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

	// Bound concurrent scans: the read takes ~10s on cold chunks (taker/maker are not compression
	// segmentby keys), so unbounded callers would pin the serving pool and starve other endpoints.
	// Acquire a slot or shed a retryable 503; never queue past the read budget.
	caller := middleware.RateLimitCallerKey(r)
	if !accountTradesCallers.acquire(caller) {
		h.tradesBusy(w, r)
		return
	}
	defer accountTradesCallers.release(caller)
	select {
	case accountTradesGate <- struct{}{}:
		defer func() { <-accountTradesGate }()
	case <-time.After(accountTradesGateWait):
		h.tradesBusy(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	rows, horizon, err := h.Trades.ListAccountTrades(ctx, g, limit, cur)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer AccountTrades deadline exceeded", "account", g)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-trades-timeout",
				"Account trades timed out")
			return
		}
		h.Logger.Error("explorer AccountTrades failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	note := accountTradesScopeNote
	if !horizon.IsZero() && horizon.Year() > 1971 {
		// Compression-horizon floor: trades older than the oldest uncompressed chunk are not
		// searchable per account, so say so rather than serve a silently partial "all time" list.
		note += " Showing trades since " + horizon.UTC().Format("2006-01-02") +
			"; older trade history is not yet searchable per-account and is being rebuilt."
	}
	out := AccountTradesView{
		Account: g,
		Trades:  make([]AccountTradeEntry, len(rows)),
		Note:    note,
	}
	dctx, dcancel := context.WithTimeout(ctx, movementDecimalsBudget)
	defer dcancel()
	scale := h.assetScales(dctx)
	for i, t := range rows {
		out.Trades[i] = accountTradeEntryView(t)
		out.Trades[i].BaseDecimals = scale(t.BaseAsset)
		out.Trades[i].QuoteDecimals = scale(t.QuoteAsset)
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
		Signer:       t.Signer,
	}
}
