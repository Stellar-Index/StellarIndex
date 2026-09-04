package v1

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// HistoryReader is the storage-side interface for /v1/history
// lookups.
type HistoryReader interface {
	// TradesInRange returns trades for pair whose close-time is in
	// [from, to). Ordered chronologically (ts ASC). Used by the
	// aggregation endpoints (/v1/vwap, /v1/twap, /v1/ohlc) which
	// consume the whole window at once.
	//
	// When the window holds more than `limit` trades, implementations
	// MUST keep the NEWEST `limit` rows (and still return them ts ASC),
	// not the oldest: a truncated aggregate has to run up to the window
	// end or a busy 24h VWAP reports a price from hours ago (F-1319).
	// The handlers' `truncated` flag is documented against this
	// guarantee — see VWAPResult.Truncated.
	TradesInRange(ctx context.Context, pair canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error)

	// TradesInRangeAfter is TradesInRange with a full-PK cursor.
	// Rows are filtered to (ts, ledger, tx_hash, op_index, source)
	// > (afterTs, afterLedger, afterTxHash, afterOpIndex, afterSource).
	// afterTs = zero time disables the cursor.
	//
	// Full PK as the cursor (not just ts+ledger) avoids dropping
	// rows when a page break falls mid-ledger — multiple trades
	// can share (ts, ledger) on high-volume ledgers.
	TradesInRangeAfter(
		ctx context.Context,
		pair canonical.Pair,
		from, to, afterTs time.Time,
		afterLedger uint32,
		afterTxHash, afterSource string,
		afterOpIndex uint32,
		limit int,
	) ([]canonical.Trade, error)

	// HistoryPoints returns every CLOSED bucket from the requested
	// granularity's CAGG (prices_1m / prices_15m / prices_1h / etc.)
	// for the pair, ordered chronologically. Used by /v1/history/
	// since-inception to serve the full historical series.
	//
	// granularity must be one of the canonical strings: "1m", "15m",
	// "1h", "4h", "1d", "1w", "1mo". The handler translates the
	// query-param string into this argument; unknown granularities
	// return [ErrUnknownGranularity].
	//
	// limit clamps the row count; 0 = unbounded. Empty slice + nil
	// error when the pair has no closed buckets yet.
	HistoryPoints(ctx context.Context, pair canonical.Pair, granularity string, limit int) ([]HistoryPoint, error)

	// HistoryPointsInRange is [HistoryPoints] with an explicit
	// [from, to) bucket bound. Same closed-bucket guard, same
	// granularity validation, same limit semantics. `from` zero
	// disables the lower bound; `to` zero disables the upper bound.
	//
	// Used by /v1/chart to serve a rolling-window series (timeframe
	// → from = now-tf, to = now). Per ADR-0020.
	HistoryPointsInRange(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int) ([]HistoryPoint, error)

	// TWAPPointsInRange is the time-weighted-average sibling of
	// [HistoryPointsInRange], reading the twap_<granularity> CAGG
	// (migration 0081) instead of prices_<granularity>. Same [from, to)
	// window, closed-bucket guard, and `[]HistoryPoint` shape (the VWAP
	// field carries the TWAP value). granularity is 1h or 1d — the only
	// two grains with a TWAP CAGG; other values return
	// [ErrUnknownGranularity] (handler → 400). Backs
	// /v1/chart?price_type=twap.
	TWAPPointsInRange(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int) ([]HistoryPoint, error)

	// OHLCSeries returns chronologically-ordered OHLC bars from the
	// CAGG matching the requested interval, in [from, to). Bucket is
	// the START of each window; window end = bucket + interval.
	// `interval` is one of: "1m", "5m", "15m", "30m", "1h", "4h",
	// "1d", "1w". Unknown intervals return [ErrUnknownGranularity].
	// `limit` clamps the row count (0 = unbounded). Empty slice +
	// nil error when no closed buckets exist in the window. The
	// storage-side impl re-buckets a finer-grain CAGG when the
	// interval doesn't have a native CAGG (5m / 30m / 4h).
	//
	// Used by /v1/ohlc's multi-bar mode (F-0071, CG/CMC parity).
	OHLCSeries(ctx context.Context, pair canonical.Pair, interval string, from, to time.Time, limit int) ([]OHLCSeriesBar, error)

	// LatestTradePerSource returns the most-recent trade FROM EACH
	// source that has ever recorded a trade on `pair`. Empty slice +
	// nil error when the pair has no trades at all.
	//
	// Optional sourceFilter ("" = no filter) restricts the result to
	// a single source — equivalent to "latest trade for the pair on
	// venue X", returning a 0- or 1-element slice. The filter is
	// applied at the SQL layer so a per-source query is cheap.
	//
	// This is the storage-side primitive for the ADR-0018 Surface 3
	// `/v1/observations` endpoint. The production impl is a
	// `DISTINCT ON (source) … WHERE base=$1 AND quote=$2
	// ORDER BY source, ts DESC` scan with no time bound.
	//
	// COST, re-measured on r1 2026-08-03 (this comment was wrong in
	// BOTH directions before): `trades_pair_source_ts_idx` (migration
	// 0037) DOES exist, and Timescale plans this as a Merge Append of
	// per-chunk SkipScans over the compressed index — 49 ms execution
	// across 249 chunks for native/fiat:USD, 289 ms for the heaviest
	// pair measured, 47 ms to prove a novel pair empty. An earlier
	// revision claimed the index was never created and that the scan
	// "probes every chunk — multiple seconds"; both were false, and
	// the deferred multi-GB index build recorded here as the "durable
	// root-cause follow-up" is work that is already deployed. Do not
	// re-schedule it. Note Postgres PLANNING (40-210 ms over 249
	// chunks) can exceed execution on a novel key, since the planner
	// re-plans across the wide chunk set.
	//
	// [CachedHistoryReader] still SWR-caches this method (#29) to
	// keep the status page's poll off the database entirely.
	LatestTradePerSource(ctx context.Context, pair canonical.Pair, sourceFilter string) ([]canonical.Trade, error)
}

// HistoryPoint is one (timestamp, price, optional usd-volume) row
// from a CAGG, returned by [HistoryReader.HistoryPoints]. The wire
// shape (`{t, p, v_usd?}`) is the OpenAPI HistoryPoint schema; the
// reader returns rich types and the handler does the marshalling.
type HistoryPoint struct {
	Bucket    time.Time
	VWAP      string  // NUMERIC text — pass-through, no float round-trip
	VolumeUSD *string // null when the bucket's underlying trades had no usd_volume
}

// ErrUnknownGranularity is what HistoryReader.HistoryPoints returns
// when the granularity arg isn't one of the seven canonical values.
// Handler translates to HTTP 400 problem+json.
var ErrUnknownGranularity = fmt.Errorf("unknown granularity")

// TradeRow is the wire shape for /v1/history entries.
//
// Numeric amounts ship as decimal strings (ADR-0003). Price is a
// pre-computed decimal for consumer convenience — the storage layer
// never persists a derived price, so we compute at response time.
type TradeRow struct {
	Source      string    `json:"source"`
	Ledger      uint32    `json:"ledger"`
	TxHash      string    `json:"tx_hash"`
	OpIndex     uint32    `json:"op_index"`
	Timestamp   time.Time `json:"ts"`
	BaseAsset   string    `json:"base_asset"`
	QuoteAsset  string    `json:"quote_asset"`
	BaseAmount  string    `json:"base_amount"`
	QuoteAmount string    `json:"quote_amount"`
	Price       string    `json:"price"` // quote/base as decimal
	// BaseDecimals / QuoteDecimals are the smallest-unit scale for each
	// side's amount: divide base_amount by 10^base_decimals (and quote by
	// 10^quote_decimals) to get whole-asset units.
	//
	// The scale is a property of the ROW'S SOURCE, not of the asset, and
	// a single page MIXES BOTH:
	//
	//   on-chain rows (sdex, soroswap, aquarius, phoenix, comet) carry
	//     the asset's own scale — 7 for native/classic, the token
	//     contract's declared decimals() for Soroban tokens;
	//   CEX rows (binance, kraken, bitstamp, coinbase) carry 8
	//     REGARDLESS of the pair — the external normalisation scale, not
	//     anything about the asset.
	//
	// Never substitute a constant. A reader that assumed 7 per-asset
	// mis-scaled real rows in production once already, and the error is
	// PAYLOAD-UNDETECTABLE: `price` is quote/base and therefore
	// scale-invariant, so nothing in the response looks wrong.
	//
	// Populated on /v1/history; omitted (0) on /v1/observations, whose
	// rows carry no per-side scale.
	BaseDecimals  int `json:"base_decimals,omitempty"`
	QuoteDecimals int `json:"quote_decimals,omitempty"`
	// RoutedVia is the router/aggregator whose same-tx invocation
	// drove this trade (routers.name, e.g. "soroswap-router").
	// Omitted for direct trades — and for very recent routed trades
	// the attribution sweeper (1-min cadence) hasn't tagged yet.
	RoutedVia string `json:"routed_via,omitempty"`
}

// tradeRowFrom converts canonical.Trade → wire shape. Price is
// computed at `decimals` fractional digits (default 10 — generous
// enough for sub-stroop precision without being absurd).
func tradeRowFrom(t canonical.Trade, decimals int) TradeRow {
	if decimals <= 0 {
		decimals = 10
	}
	return TradeRow{
		Source:      t.Source,
		Ledger:      t.Ledger,
		TxHash:      t.TxHash,
		OpIndex:     t.OpIndex,
		Timestamp:   t.Timestamp,
		BaseAsset:   t.Pair.Base.String(),
		QuoteAsset:  t.Pair.Quote.String(),
		BaseAmount:  t.BaseAmount.String(),
		QuoteAmount: t.QuoteAmount.String(),
		Price:       priceRatioDecimal(t, decimals),
		RoutedVia:   t.RoutedVia,
	}
}

// normalizeTradeRowPrices rewrites each row's Price from the RAW
// quote_amount/base_amount ratio (what tradeRowFrom / priceRatioDecimal
// render) to the dex-nonstandard-decimals-corrected price (M2). rows and
// trades are parallel slices; base/quote are the request pair's legs, resolved
// ONCE against the nonstandard_decimals_assets guard table (the SAME source
// every normalized endpoint uses — /v1/vwap, main /v1/price, …).
//
// BYTE-IDENTICAL no-op — rows left exactly as tradeRowFrom rendered them —
// when neither leg is a confirmed non-7-decimals asset (baseDec == quoteDec,
// the common case), keeping the wire response unchanged for standard tokens.
// BaseAmount / QuoteAmount are deliberately left as raw smallest-unit integers
// (the on-chain truth); only the convenience Price field is corrected, exactly
// as /v1/history and /v1/observations agree.
func (s *Server) normalizeTradeRowPrices(rows []TradeRow, trades []canonical.Trade, base, quote canonical.Asset) {
	baseDec := aggregate.ResolveDecimals(s.nonstandardDecimals, base)
	quoteDec := aggregate.ResolveDecimals(s.nonstandardDecimals, quote)
	if baseDec == quoteDec {
		return
	}
	for i := range rows {
		b := trades[i].BaseAmount.BigInt()
		if b.Sign() == 0 {
			continue
		}
		raw := new(big.Rat).SetFrac(trades[i].QuoteAmount.BigInt(), b)
		rows[i].Price = ratToDecimal(aggregate.AdjustPrice(raw, baseDec, quoteDec), 10)
	}
}

// ─── Handler ──────────────────────────────────────────────────────

// handleHistory serves GET /v1/history?base=<id>&quote=<id>&from=<rfc3339>&to=<rfc3339>&limit=<int>.
//
// Defaults:
//   - from: to - 1h (1-hour window rolling back from `to`)
//   - to:   now
//   - limit: 1000 (server clamps to ≤ 10000)
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) { //nolint:funlen // option parsing + 8s-timeout guard + range/limit defaults are linear; splitting fragments the request lifecycle
	reader := s.history
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/history-unavailable",
			"History serving not configured", http.StatusServiceUnavailable,
			"this deployment has no HistoryReader wired — check binary configuration")
		return
	}

	base, quote, ok := parseBaseQuote(w, r)
	if !ok {
		return
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-pair",
			"Invalid pair", http.StatusBadRequest,
			err.Error())
		return
	}

	// dex-nonstandard-decimals: /v1/history reads exclusively from raw
	// trades via TradesInRangeAfter below (no CAGG involved — that's a
	// different reader method, HistoryPoints, used by /v1/chart), so it
	// no longer needs the decline guard. The per-row Price field is
	// normalized after decimals are resolved further down.

	from, to, ok := parseFromTo(w, r)
	if !ok {
		return
	}

	limit := 1000
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 10000 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-limit",
				"Invalid limit", http.StatusBadRequest,
				"limit must be an integer in [1, 10000]")
			return
		}
		limit = parsed
	}

	// Optional cursor (opaque to clients; base64 of
	// "<ts>:<ledger>:<source>:<tx_hash>:<op_index>"). Shadows `from`
	// when present — otherwise paginating callers would re-request
	// duplicate rows on each page.
	var (
		afterTs      time.Time
		afterLedger  uint32
		afterSource  string
		afterTxHash  string
		afterOpIndex uint32
	)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeHistoryCursor(raw)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-cursor",
				"Invalid cursor", http.StatusBadRequest, err.Error())
			return
		}
		afterTs = c.ts
		afterLedger = c.ledger
		afterSource = c.source
		afterTxHash = c.txHash
		afterOpIndex = c.opIndex
	}

	// 8s ceiling on the trades hypertable range query. Same
	// pattern as #1082 / #1099 / #1100 / #1101 / #1102. Long
	// `from` windows (no `from` set, or month-spanning) can take
	// 5–10s on a cold cache scanning per-trade rows. One ceiling spans
	// every alias scan below — the fan-in must not multiply the
	// endpoint's worst-case hold.
	hCtx, hCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer hCancel()
	trades, next, err := s.tradesInRangeAfterWithAliases(hCtx, reader, pair,
		from, to, afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(hCtx, err) {
			s.logger.Warn("TradesInRangeAfter deadline exceeded",
				"base", base.String(), "quote", quote.String(),
				"from", from, "to", to, "limit", limit)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/history-timeout",
				"History query timed out", http.StatusServiceUnavailable,
				"the underlying trades-hypertable scan didn't return in 8s. Try narrowing the from/to window or reducing the limit.")
			return
		}
		s.logger.Error("TradesInRangeAfter failed",
			"err", err,
			"base", base.String(), "quote", quote.String(),
			"from", from, "to", to)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// Resolve the per-ASSET scale once (base+quote are fixed request
	// params), then let each row override it with its SOURCE's scale.
	//
	// The per-asset value alone is wrong for every off-chain row. The
	// scale an amount was stamped at is a property of the CONNECTOR, not
	// of the asset: the CEX parsers stamp 1e8 and the FX pollers 1e6,
	// while this resolver returns 7 for anything non-Soroban. A page can
	// mix both — /v1/history?base=crypto:XLM&quote=fiat:USD returns sdex
	// rows at 1e7 beside coinbase rows at 1e8 — so "constant across the
	// page" was never true, and a consumer following this field's own
	// documented conversion (amount / 10^decimals) overstated every CEX
	// trade by exactly 10x. Verified live 2026-08-04: coinbase rows
	// served base_decimals 7 against a parser that stamps 8. The `price`
	// field is scale-invariant, so nothing in the response contradicted
	// it (cold audit 2026-08-04).
	baseDec := s.resolveAssetDecimals(hCtx, base)
	quoteDec := s.resolveAssetDecimals(hCtx, quote)
	rows := make([]TradeRow, len(trades))
	for i, t := range trades {
		rows[i] = tradeRowFrom(t, 10)
		rows[i].BaseDecimals = baseDec
		rows[i].QuoteDecimals = quoteDec
		if dec, ok := externalSourceAmountDecimals(t.Source); ok {
			// Off-chain sources normalise BOTH sides to one scale.
			rows[i].BaseDecimals = dec
			rows[i].QuoteDecimals = dec
		}
	}
	// dex-nonstandard-decimals forward normalization of the Price field
	// (M2). tradeRowFrom's Price is the raw quote_amount/base_amount ratio;
	// normalizeTradeRowPrices corrects it against the `nonstandard_decimals_assets`
	// guard table (s.nonstandardDecimals) — the SAME source /v1/vwap and the
	// main /v1/price use, NOT the broader live-contract baseDec/quoteDec stamped
	// above for the BaseDecimals/QuoteDecimals metadata (those two resolvers can
	// legitimately disagree, and mixing them would make Price disagree with what
	// /v1/vwap serves for the identical pair). /v1/observations shares this exact
	// helper so the two raw-trade surfaces stay in lockstep. Byte-identical no-op
	// at 7dp.
	s.normalizeTradeRowPrices(rows, trades, base, quote)

	// When rows remain beyond this page, emit a next-cursor pointing at
	// the last row we returned ([historyNextCursor]). Clients just
	// re-issue the same request with ?cursor=<next> to drain subsequent
	// pages. When the window is exhausted — no cursor, no next. The read
	// reports that itself rather than it being inferred from
	// `len(trades) == limit`: the two-direction merge can cut a page
	// short with rows still behind it, and a length test would stop the
	// client there.
	env := Envelope{Data: rows, Flags: Flags{}}
	// Probed over both legs' alias families and both stored directions —
	// see historyCoverageSet: that is exactly what the page read above
	// spans, so the floor describes rows this page can return.
	env.CoverageFrom, env.Flags.OutsideCoverage = s.coverageAnnotationIfEmpty(
		r.Context(), historyCoverageSet(pair), to, historyPageIsAmbiguous(len(trades), afterTs))
	env.Pagination = historyNextCursor(next)
	writeEnvelope(w, env)
}

// historyNextCursor renders the page's next-cursor, or nil when the
// window is drained. Where the resume point comes from — and why it is
// not always the last row's own key — is [mergeTradeDirections]'s
// [pageCursor]; this only puts it on the wire.
func historyNextCursor(next *historyCursor) *Pagination {
	if next == nil {
		return nil
	}
	return &Pagination{Next: encodeHistoryCursor(*next)}
}

// historyCursor is the decoded cursor payload for /v1/history.
// Full PK as the key avoids mid-ledger page-break data loss.
type historyCursor struct {
	ts      time.Time
	ledger  uint32
	source  string
	txHash  string
	opIndex uint32
}

// pastTieGroupCursorSource is the `source` field of a cursor that
// resumes PAST a whole tie group rather than at one row of it: a marker,
// not a source name.
//
// It exists because the two forms need different binds and the decoder
// refuses to take the second one from a client. A past-group cursor
// binds `source` EMPTY, so the database's tuple comparison decides on
// the stepped op_index and never reaches the source column at all —
// which is the whole point (see [tieGroupCursor]). But an empty source
// arriving in a cursor a CLIENT wrote is a different thing entirely: it
// would pair with an unstepped op_index and weaken the full-PK
// comparison to a partial one, which is what [decodeHistoryCursor]'s
// guard has always rejected and still does. The marker keeps that guard
// intact and lets this endpoint's own cursors say which form they are.
//
// `*` cannot collide with a real source: source names are [a-z0-9_-]
// (see below), and TestHistoryCursor_PastGroupMarkerIsNotASourceName
// pins that against the live registry.
const pastTieGroupCursorSource = "*"

// encodeHistoryCursor / decodeHistoryCursor are the opaque
// over-the-wire form of a historyCursor. Base64 keeps the cursor
// URL-safe without needing client-side URL encoding.
//
// Format inside the base64:
// "<unix_nanos>:<ledger>:<source>:<tx_hash>:<op_index>"
// Timestamp is nanosecond-precision (future-proof against sub-
// second ledger close times). Source names are [a-z0-9_-] so no
// field-separator collision, and `*` is therefore free to mark the
// past-group form ([pastTieGroupCursorSource]).
func encodeHistoryCursor(c historyCursor) string {
	raw := fmt.Sprintf("%d:%d:%s:%s:%d",
		c.ts.UnixNano(), c.ledger, c.source, c.txHash, c.opIndex)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeHistoryCursor(s string) (historyCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return historyCursor{}, fmt.Errorf("cursor base64: %w", err)
	}
	parts := strings.SplitN(string(raw), ":", 5)
	if len(parts) != 5 {
		return historyCursor{}, fmt.Errorf("cursor must be <ts_ns>:<ledger>:<source>:<tx_hash>:<op_index>")
	}
	tsNano, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return historyCursor{}, fmt.Errorf("cursor ts: %w", err)
	}
	ledger, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return historyCursor{}, fmt.Errorf("cursor ledger: %w", err)
	}
	source := parts[2]
	switch source {
	case pastTieGroupCursorSource:
		// This endpoint's own past-group cursor. Bind no source at all:
		// its op_index is already stepped past the group, so the
		// comparison is decided before the source column is reached.
		source = ""
	case "":
		// An empty source would weaken the full-PK cursor comparison
		// into a partial one, reintroducing the same-ledger page-skip
		// bug the full-PK cursor was designed to fix. Reject rather
		// than silently serve wrong-looking pages. The past-group form
		// above is the ONLY way to an empty bind, and it pairs the
		// empty source with a STEPPED op_index, so its comparison is
		// total on the first four components.
		return historyCursor{}, fmt.Errorf("cursor source must not be empty")
	}
	txHash := parts[3]
	if !isLowerHex64(txHash) {
		return historyCursor{}, fmt.Errorf("cursor tx_hash must be 64 lowercase hex chars")
	}
	opIndex, err := strconv.ParseUint(parts[4], 10, 32)
	if err != nil {
		return historyCursor{}, fmt.Errorf("cursor op_index: %w", err)
	}
	return historyCursor{
		ts:      time.Unix(0, tsNano).UTC(),
		ledger:  uint32(ledger),
		source:  source,
		txHash:  txHash,
		opIndex: uint32(opIndex),
	}, nil
}

// isLowerHex64 returns true iff s is exactly 64 characters of
// lowercase hex. Same invariant canonical.validTxHash enforces on
// the ingest side; mirrored here (without importing canonical) so
// decodeHistoryCursor doesn't create a cycle.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// parseBaseQuote extracts + validates base/quote from the request.
// Returns (base, quote, true) on success; writes a problem response
// and returns ok=false on failure.
//
// `asset=` is accepted as an alias for `base=` (F-0061 closure) so
// clients copying URLs from /v1/price (which uses asset/quote) don't
// hit a 400 on their first try. Passing BOTH `base` and `asset` is
// a 400 — they're conflicting controls for the same value and the
// silent precedence pick was confusing.
func parseBaseQuote(w http.ResponseWriter, r *http.Request) (canonical.Asset, canonical.Asset, bool) {
	rawBase := r.URL.Query().Get("base")
	rawAsset := r.URL.Query().Get("asset")
	if rawBase != "" && rawAsset != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-parameter",
			"`base` and `asset` are mutually exclusive", http.StatusBadRequest,
			"both query parameters refer to the same value — pick one (this endpoint's canonical form is `base=`; `asset=` is accepted as an alias for /v1/price compatibility)")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	if rawBase == "" {
		rawBase = rawAsset
	}
	if rawBase == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-base",
			"Missing base parameter", http.StatusBadRequest,
			"base query parameter is required (or `asset=` as an alias for /v1/price compatibility)")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	base, err := canonical.ParseAsset(rawBase)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid base identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, canonical.Asset{}, false
	}

	rawQuote := r.URL.Query().Get("quote")
	if rawQuote == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-quote",
			"Missing quote parameter", http.StatusBadRequest,
			"quote query parameter is required")
		return canonical.Asset{}, canonical.Asset{}, false
	}
	quote, err := canonical.ParseAsset(rawQuote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-quote",
			"Invalid quote identifier", http.StatusBadRequest,
			err.Error())
		return canonical.Asset{}, canonical.Asset{}, false
	}
	return base, quote, true
}

// ─── /v1/history/since-inception ────────────────────────────────

// HistorySeries is the wire shape for /v1/history/since-inception.
// Mirrors the OpenAPI HistoryEnvelope.data shape exactly.
type HistorySeries struct {
	AssetID     string             `json:"asset_id"`
	Quote       string             `json:"quote"`
	PriceType   string             `json:"price_type"`  // "vwap" today; TWAP planned
	Granularity string             `json:"granularity"` // "1m" / "15m" / "1h" / etc.
	Points      []HistoryPointWire `json:"points"`
}

// HistoryPointWire is the JSON-tagged shape that marshals as the
// OpenAPI HistoryPoint ({t, p, v_usd?}). Distinct from the
// reader-side [HistoryPoint] which carries rich types — keeps the
// internal type usable by tests + adapters without leaking wire-
// shape assumptions.
type HistoryPointWire struct {
	T    time.Time `json:"t"`
	P    string    `json:"p"`
	VUSD *string   `json:"v_usd,omitempty"`
}

const (
	defaultHistoryGranularity = "1d"

	// historyMaxPoints is the safety cap on a single response. The
	// 1m CAGG can grow to ~32 M rows over 5 years (one per minute);
	// the 1d CAGG is ~1800 rows over 5 years. Cap at 50k so a
	// granularity=1m request doesn't try to ship a 32M-row JSON
	// payload. Operators wanting the full series in 1m grain should
	// paginate (planned cursor surface).
	historyMaxPoints = 50_000
)

// handleHistorySinceInception serves GET /v1/history/since-inception?
// asset=<id>&quote=<id>&granularity=<g>. Returns CLOSED buckets
// from the granularity's CAGG, oldest to newest, capped at
// historyMaxPoints.
//
// 503 when no HistoryReader is wired. 400 on bad asset/quote/
// granularity. 200 with empty points[] when the pair has no closed
// buckets yet — distinct from 404 since the asset itself may be
// known but just hasn't accrued bucketed history.
func (s *Server) handleHistorySinceInception(w http.ResponseWriter, r *http.Request) { //nolint:funlen // option parsing + 8s-timeout guard + grain-default + clamp logic are linear; splitting fragments the request lifecycle
	if s.history == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/history-unavailable",
			"History serving not configured", http.StatusServiceUnavailable,
			"this deployment has no HistoryReader wired — check binary configuration")
		return
	}

	rawAsset := r.URL.Query().Get("asset")
	if rawAsset == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-asset",
			"Missing asset parameter", http.StatusBadRequest,
			"asset query parameter is required")
		return
	}
	asset, err := canonical.ParseAsset(rawAsset)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset identifier", http.StatusBadRequest,
			err.Error())
		return
	}

	quote := defaultPriceQuote
	if rawQuote := r.URL.Query().Get("quote"); rawQuote != "" {
		q, err := canonical.ParseAsset(rawQuote)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-quote",
				"Invalid quote identifier", http.StatusBadRequest,
				err.Error())
			return
		}
		quote = q
	}

	if asset.Equal(quote) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/identity-pair",
			"Asset is the quote", http.StatusBadRequest,
			"asset and quote must differ")
		return
	}

	gran := defaultHistoryGranularity
	if raw := r.URL.Query().Get("granularity"); raw != "" {
		gran = raw
	}

	pair, err := canonical.NewPair(asset, quote)
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-pair",
			"Invalid pair", http.StatusBadRequest,
			err.Error())
		return
	}

	hCtx, hCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer hCancel()
	points, err := s.historyPointsWithAliases(hCtx, pair, gran)
	if errors.Is(err, ErrUnknownGranularity) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-granularity",
			"Invalid granularity", http.StatusBadRequest,
			// Enumeration comes from timescale.AllHistoryGranularities,
			// the same slice Validate ranges over — a hand-written copy
			// here would keep advertising the old set after a rung is
			// added or removed.
			fmt.Sprintf("granularity must be one of: %s (got %q)", timescale.HistoryGranularityList(), gran))
		return
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(hCtx, err) {
			s.logger.Warn("HistoryPoints deadline exceeded",
				"asset", asset.String(), "quote", quote.String(), "granularity", gran)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/history-timeout",
				"History query timed out", http.StatusServiceUnavailable,
				"the underlying CAGG didn't return in 8s; cache may still be warming.")
			return
		}
		s.logger.Error("HistoryPoints failed",
			"err", err, "asset", asset.String(), "quote", quote.String(), "granularity", gran)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// F-1225 (codex audit-2026-05-12): stablecoin → fiat:USD fallback.
	// The literal X/fiat:USD pair never has rows in the CAGGs on
	// Stellar mainnet because no on-chain trades quote in fiat:USD —
	// every USD-flavoured trade quotes in classic USDC (USDC-GA5Z…)
	// or one of the other operator-declared pegs. The chart + price
	// handlers already implement this fallback; without it, since-
	// inception XLM/USD returns empty while chart/price/VWAP all
	// surface data. Mirrors `chartStablecoinFallback` shape.
	triangulated := false
	if len(points) == 0 {
		if fp, ok := s.historySinceInceptionStablecoinFallback(hCtx, pair, gran); ok {
			points = fp
			// The series was served via the X/<peg> proxy under the
			// peg≈$1 assumption — that's a triangulated value, exactly
			// as the chart/price siblings flag it (G2-13). Pre-fix this
			// path wrote Flags{} and silently hid the proxy.
			triangulated = true
		}
	}

	wire := make([]HistoryPointWire, len(points))
	for i, p := range points {
		wire[i] = HistoryPointWire{T: p.Bucket, P: p.VWAP, VUSD: p.VolumeUSD}
	}

	writeJSON(w, HistorySeries{
		AssetID:     asset.String(),
		Quote:       quote.String(),
		PriceType:   "vwap",
		Granularity: gran,
		Points:      wire,
	}, Flags{Triangulated: triangulated})
}

// tradesInRangeAfterWithAliases reads one page of raw trades trying each
// XLM dual-form alias pair and returns the FIRST alias form that holds
// rows — the raw-trade twin of [Server.historyPointsWithAliases], and
// the first-hit gate [Server.tradesInRangeWithStablecoinFallback]
// already applies to a non-fiat quote on the /v1/vwap side.
//
// A literal-keyed read is blind to every venue publishing XLM under
// the other id: `?base=native&quote=fiat:USD` served an empty page while
// the identical window under `?base=crypto:XLM` returned a full one, and
// /v1/vwap — which loops the aliases — reported a live trade population for
// the pair the same hour. `native` is the documented spelling of XLM on
// this parameter, so the blind form is the one the API description sends a
// reader to first.
//
// Each alias form is read in BOTH stored directions
// ([Server.tradesInRangeAfterBothDirections]). A market has no stored
// direction of its own — the SDEX decoder records XLM/USDC and USDC/XLM
// as separate rows — and [HistoryReader.TradesInRangeAfter] keys on
// (base_asset, quote_asset) literally, so reading one direction answered
// `?base=AQUA&quote=USDC` with an empty page for every market recorded
// the other way round, while /v1/ohlc and /v1/chart served the same
// window from the same rows through a store read that folds the two
// directions in SQL ([Store.OHLCSeries]).
//
// First-hit ACROSS alias forms is unchanged: the endpoint's ordering
// contract is per-pair — rows come back ordered (ts, ledger, tx_hash,
// op_index, source) and `cursor` resumes on that tuple — so serving one
// alias form per page keeps the cursor monotonic over exactly the
// population it was minted from. Cross-form trade FUSION is the same
// separate design decision [Server.historyPointsWithAliases] defers on
// the bucket side; direction is not a form, it is the same market.
//
// The returned cursor is the read's own answer about where the next page
// resumes, nil when the window is drained. It is deliberately NOT
// derived from `len(page) == limit`: the merge can cut a page short
// while rows remain (see the tie-group rule below), a page can run OVER
// `limit`, and the resume point is not always the last row's own key
// ([pageCursor]).
//
// Non-XLM pairs have exactly one spelling, so they cost one page read
// per direction; the literal form is tried first, so a populated pair
// answers on its first form. The caller's deadline covers ALL scans
// (same rule as [Server.computeObservations]). The first error
// propagates unchanged.
func (s *Server) tradesInRangeAfterWithAliases(
	ctx context.Context,
	reader HistoryReader,
	pair canonical.Pair,
	from, to, afterTs time.Time,
	afterLedger uint32,
	afterTxHash, afterSource string,
	afterOpIndex uint32,
	limit int,
) ([]canonical.Trade, *historyCursor, error) {
	for _, b := range assetAliases(pair.Base) {
		for _, q := range assetAliases(pair.Quote) {
			ap, perr := canonical.NewPair(b, q)
			if perr != nil {
				continue // degenerate alias combination (identity pair)
			}
			page, next, err := s.tradesInRangeAfterBothDirections(ctx, reader, ap,
				from, to, afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, limit)
			if err != nil || len(page) > 0 {
				return page, next, err
			}
		}
	}
	return nil, nil, nil
}

// tieGroupReadMax is the size of the ONE re-read that completes a tie
// group, and it is [Store.TradesInRangeAfter]'s own clamp.
//
// A tie group is the set of rows sharing (ts, ledger, tx_hash,
// op_index). The trades primary key (source, ledger, tx_hash, op_index,
// ts) makes them differ only in `source`, so a group is one operation
// attributed to several connectors and cannot hold more rows than there
// are distinct sources — 28 in the live registry. Reading straight to
// the store's maximum therefore ends the question in one go: a
// direction that came back still inside the group after asking for
// 10,000 rows would need 10,000 sources on one operation.
//
// It replaces a 1 -> 34 -> 100 -> 232 ladder with an attempt budget,
// whose exhaustion branch cut a page THROUGH the group and minted a
// last-row cursor — the round-one loss shape, in the branch documented
// as the safe one, reachable at a 233-row group. One wider read in a
// case that already needs a 29th source is the better trade than an
// unreachable lossy path.
const tieGroupReadMax = 10000

// tradesInRangeAfterBothDirections reads `pair` and its flip, re-expresses
// every row in `pair`'s orientation, and merges the two streams into one
// page carrying the keyset order the store returned each of them in.
//
// CURSOR. The cursor tuple (ts, ledger, tx_hash, op_index, source) names
// no asset, so the same `after*` values bound both directions and the
// database applies them with its own comparison. The two streams are
// disjoint: the trades primary key holds no asset column, so one stored
// row satisfies exactly one direction's (base_asset, quote_asset)
// predicate.
//
// ORDER. Both streams arrive ordered (ts, ledger, tx_hash, op_index,
// source) ASC. The merge compares (ts, ledger, tx_hash, op_index) only,
// and keeps each stream's own order for rows that tie on all four. That
// is the whole point: ts, ledger and op_index are numeric, and tx_hash
// is fixed-width lowercase hex, so Go's comparison of those four agrees
// with the database's for every value the columns can hold. `source` is
// the one component whose Go byte order can disagree with the database's
// collation — source names carry `-` and `_`, which a non-C collation
// weighs differently from their code points — so the merge never
// compares it, and never cuts a page THROUGH a tie group either: a page
// ending inside one would mint a cursor the database may order after a
// row that was dropped, and that row would never be served. The cut
// retreats to the group's LOWER edge, which is always union-correct.
//
// THE OVER-LIMIT PAGE, and the invariant that makes it safe. A group
// that both opens the page and still holds rows at index `limit` cannot
// take the lower edge — that edge is index 0, and a page of no rows
// carrying a cursor stalls the client. Such a page runs to the group's
// UPPER edge instead, over `limit`. It may do so ONLY IF THE GROUP IS
// COMPLETE IN BOTH DIRECTIONS ([tieGroupFetched]): a direction whose
// read stopped ON the group holds further rows of it that this page
// cannot see, and the cursor — minted from the other direction's row —
// would exclude them under the database's own `source` comparison. They
// would never be served. So the group is COMPLETED FIRST, by re-reading
// the truncated direction with a raised limit; a group is bounded by the
// number of sources that can record one operation, so the raise
// terminates. If it somehow does not, the page cuts at `limit` rather
// than serving past rows it has not read.
//
// EXACTLY ONCE, and this is where that is won. A cursor naming the last
// served ROW re-serves the rows of its group that the database orders
// above it — the merge puts the requested orientation's rows first on a
// tie and the database orders them by `source`, so the two disagree by
// construction. A page that ends on a COMPLETE group therefore resumes
// past the whole group by key instead ([tieGroupCursor]), which is
// possible only because completeness is established here rather than
// assumed. The over-limit page has no other case to fall to: a group it
// would serve past is completed first, at the store's maximum read, and
// the branch that used to cut through an incomplete one is gone
// ([tieGroupReadMax]).
//
// PAGE. Each direction is asked for `limit` rows, so the first `limit`
// of the merge are the first `limit` of the union. A page is short only
// when both directions came back short, i.e. both are drained — except
// at a tie-group cut, which is why the drained/not-drained answer rides
// on the returned cursor rather than on the page length. Every page
// advances: the resume point is at or past the last row served, and
// strictly past the point it resumed from.
//
// COST, and it is not free. This is TWO store reads per alias form where
// there was one, run in sequence, under the SAME 8s ceiling the caller
// sets over every scan — so that budget now covers twice the reads. The
// two halves of that pull in opposite directions and it is worth being
// exact about which:
//
//   - The wide case is cheap. Only a pair with NO rows fans out over
//     every alias form (first-hit stops at the first form that answers),
//     and proving a form empty is the bounded index probe
//     [HistoryReader.LatestTradePerSource]'s note measures at tens of
//     milliseconds on this index family. A three-form empty pair costs
//     six such probes.
//   - The expensive case is narrow but real. A POPULATED pair answers on
//     its first form and so costs two reads, not six — but a long cold
//     window whose single-direction read already ran 4-8s now runs
//     roughly twice that and reaches the ceiling. Such a request is
//     answered with the endpoint's documented 503 problem, which is
//     retryable and honest, rather than with half a market; narrowing
//     `from`/`to` or lowering `limit` is the caller-side answer, and
//     both were already the guidance for this ceiling.
//
// The two reads are deliberately NOT run concurrently. It would halve
// the wall clock, but it doubles the in-flight database work per request
// on a shared pool, and it makes the read ORDER unobservable — the
// literal alias form leading is a property this endpoint pins.
func (s *Server) tradesInRangeAfterBothDirections(
	ctx context.Context,
	reader HistoryReader,
	pair canonical.Pair,
	from, to, afterTs time.Time,
	afterLedger uint32,
	afterTxHash, afterSource string,
	afterOpIndex uint32,
	limit int,
) ([]canonical.Trade, *historyCursor, error) {
	read := func(p canonical.Pair, n int) ([]canonical.Trade, error) {
		return reader.TradesInRangeAfter(ctx, p, from, to,
			afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, n)
	}
	// Requested orientation first — the order this endpoint pins.
	stored, err := read(pair, limit)
	if err != nil {
		return nil, nil, err
	}
	flipped, err := read(pair.Flip(), limit)
	if err != nil {
		return nil, nil, err
	}
	pages := directionPages{
		stored: stored, flipped: flipped,
		storedLimit: limit, flippedLimit: limit,
	}
	if pages, err = s.completeOverLimitTieGroup(pages, read, pair, limit); err != nil {
		return nil, nil, err
	}
	page, next := mergeTradeDirections(pages, pair, limit)
	return page, next, nil
}

// directionPages is one page read of each stored orientation, with the
// limits the two reads were GIVEN. Those limits, not `limit`, are what
// says whether a stream is drained: completing a tie group can raise one
// of them.
type directionPages struct {
	stored, flipped           []canonical.Trade
	storedLimit, flippedLimit int
}

// completeOverLimitTieGroup re-reads whichever direction stopped ON the
// tie group that a `limit`-row page would have to be served past, so
// that the merge may serve the group whole. Returns `pages` untouched
// when no group straddles the page edge, which is every ordinary
// request: this path needs more rows sharing one
// (ts, ledger, tx_hash, op_index) than the caller asked for in total.
//
// One re-read, straight to [tieGroupReadMax], and then the group is
// whole — see that constant for why there is no second round and no
// budget to exhaust. The remaining check exists only to make the
// impossible state LOUD: it does not branch, because a branch here
// cannot both serve the group and keep the cursor honest, and the state
// it reports needs 10,000 sources on a single operation.
//
// See [Server.tradesInRangeAfterBothDirections] for why a group served
// incomplete loses rows.
func (s *Server) completeOverLimitTieGroup(
	pages directionPages,
	read func(canonical.Pair, int) ([]canonical.Trade, error),
	pair canonical.Pair,
	limit int,
) (directionPages, error) {
	group, straddles := overLimitTieGroup(pages.stored, pages.flipped, limit)
	if !straddles {
		return pages, nil
	}
	var err error
	if !tieGroupFetched(pages.stored, group, pages.storedLimit) {
		pages.storedLimit = tieGroupReadMax
		if pages.stored, err = read(pair, pages.storedLimit); err != nil {
			return pages, err
		}
	}
	if !tieGroupFetched(pages.flipped, group, pages.flippedLimit) {
		pages.flippedLimit = tieGroupReadMax
		if pages.flipped, err = read(pair.Flip(), pages.flippedLimit); err != nil {
			return pages, err
		}
	}
	if !tieGroupFetched(pages.stored, group, pages.storedLimit) ||
		!tieGroupFetched(pages.flipped, group, pages.flippedLimit) {
		// Not survivable as a silent state: the page below WILL serve
		// the group and mint a cursor past it, so a row this read never
		// saw would be skipped. Error, not Warn — it means the trades
		// table holds more rows on one operation than there are sources,
		// which is a data-integrity failure, not a busy market.
		s.logger.Error("history page: tie group larger than the store's maximum read",
			"base", pair.Base.String(), "quote", pair.Quote.String(),
			"ts", group.Timestamp, "ledger", group.Ledger,
			"tx_hash", group.TxHash, "op_index", group.OpIndex,
			"read_limit", tieGroupReadMax)
	}
	return pages, nil
}

// overLimitTieGroup returns the tie group a page would have to serve
// PAST `limit` to return any rows at all — the group that opens the
// merge and still holds rows at index `limit` — and whether there is
// one.
//
// Computed from the two streams rather than from the merged slice: rows
// of one group are contiguous at the head of each stream (both arrive
// key-ordered), so counting the two heads answers it without building
// the merge. Every other cut retreats to a group's lower edge and never
// serves past `limit`, so this is the only shape whose completeness
// matters.
func overLimitTieGroup(stored, flipped []canonical.Trade, limit int) (canonical.Trade, bool) {
	var none canonical.Trade
	if limit < 1 {
		return none, false
	}
	var head canonical.Trade
	switch {
	case len(stored) == 0 && len(flipped) == 0:
		return none, false
	case len(stored) == 0:
		head = flipped[0]
	case len(flipped) == 0:
		head = stored[0]
	case tradeOrderLess(flipped[0], stored[0]):
		head = flipped[0]
	default:
		head = stored[0]
	}
	if leadingTieGroupLen(stored, head)+leadingTieGroupLen(flipped, head) <= limit {
		return none, false
	}
	return head, true
}

// leadingTieGroupLen counts the rows at the head of one key-ordered
// stream that tie with `probe`. Zero when the stream opens on a
// different group.
func leadingTieGroupLen(rows []canonical.Trade, probe canonical.Trade) int {
	n := 0
	for n < len(rows) && sameTradeOrderKey(rows[n], probe) {
		n++
	}
	return n
}

// tieGroupFetched reports whether one direction's read returned EVERY
// row it holds in `probe`'s tie group.
//
// True when the read came back SHORT of the limit it was given — the
// direction is drained, so it holds nothing unfetched at all — or when
// its LAST row falls outside the group: rows arrive in key order, so a
// last row past the group proves the group ended inside what was read.
//
// False is the state that loses rows. The read stopped ON the group, so
// the direction may hold further rows of it, differing from the fetched
// ones only in `source`. A page served past such a group mints its
// cursor from the other direction's row, and the database's own
// `(ts, ledger, tx_hash, op_index, source) >` predicate then excludes
// every unfetched row whose source it orders below that one. Proven
// against a three-source group at limit=1, where the page served two
// rows and the third was never returned on any page.
func tieGroupFetched(rows []canonical.Trade, probe canonical.Trade, readLimit int) bool {
	if len(rows) < readLimit {
		return true
	}
	return !sameTradeOrderKey(rows[len(rows)-1], probe)
}

// mergeTradeDirections merges two keyset-ordered pages of the same
// market into one page in `want`'s orientation, cuts it without
// splitting a tie group, and reports whether rows remain behind the cut.
//
// storedLimit / flippedLimit are the limits the two reads were actually
// GIVEN, which is not `limit` once a group has been completed by a
// raised re-read — they are what says whether a stream is drained. See
// [Server.tradesInRangeAfterBothDirections] for why the comparison stops
// at op_index and what the over-limit page requires.
func mergeTradeDirections(pages directionPages, want canonical.Pair, limit int) ([]canonical.Trade, *historyCursor) {
	if limit < 1 {
		limit = 1
	}
	stored, flipped := pages.stored, pages.flipped
	// A direction that filled its own read may hold more rows behind it
	// even when nothing survives the cut below, so the drained answer is
	// taken from the reads, not from the merged length.
	full := len(stored) >= pages.storedLimit || len(flipped) >= pages.flippedLimit

	merged := make([]canonical.Trade, 0, len(stored)+len(flipped))
	take := func(t canonical.Trade) {
		merged = append(merged, orientTradeTo(t, want))
	}
	i, j := 0, 0
	for i < len(stored) && j < len(flipped) {
		if tradeOrderLess(flipped[j], stored[i]) {
			take(flipped[j])
			j++
			continue
		}
		take(stored[i])
		i++
	}
	for ; i < len(stored); i++ {
		take(stored[i])
	}
	for ; j < len(flipped); j++ {
		take(flipped[j])
	}

	if limit >= len(merged) {
		return merged, pageCursor(merged, len(merged), pages, full)
	}
	cut := limit
	for cut > 0 && sameTradeOrderKey(merged[cut-1], merged[cut]) {
		cut--
	}
	if cut == 0 {
		// The group opens the page and reaches past `limit`, so the
		// lower edge is index 0 and a page of no rows carrying a cursor
		// would stall the client. Run to the group's UPPER edge instead.
		// [Server.completeOverLimitTieGroup] has already read both
		// directions to the store's maximum if it had to, so the group
		// is whole and the cursor this mints has no unread row behind
		// it. There is deliberately no other branch here: the one this
		// replaced cut through the group and lost a row.
		cut = 1
		for cut < len(merged) && sameTradeOrderKey(merged[cut-1], merged[cut]) {
			cut++
		}
	}
	return merged[:cut], pageCursor(merged, cut, pages, cut < len(merged) || full)
}

// pageCursor is where the next page resumes after `merged[:cut]`, or nil
// when the window is drained.
//
// TWO FORMS, and which one is used is the difference between
// exactly-once and at-least-once pagination on this endpoint.
//
// The default names the LAST SERVED ROW's primary key. It never skips —
// but a row tying with it on (ts, ledger, tx_hash, op_index) and served
// EARLIER on the same page comes back when the database orders that
// row's `source` above the cursor's, and is served twice. The merge puts
// the requested orientation's rows before the flipped one's on a tie,
// and the database orders the group by `source`, so the two disagree by
// construction whenever a group spans both directions.
//
// So when the page ends on a tie group that is COMPLETE in both
// directions — no fetched row of it left behind the cut, and neither
// read stopped inside it — the cursor steps past the whole group by key
// instead ([tieGroupCursor]). Every row of the group has been served, so
// nothing is skipped, and no row of it can return.
//
// Completeness is REQUIRED, not assumed: it is the same
// [tieGroupFetched] predicate the over-limit page turns on. Where it
// does not hold, the last-row cursor stands — and it is exact there
// too, for a reason worth writing down rather than trusting. A page can
// only END on a group whose fetched rows all came from ONE direction:
// the cut retreats BELOW any group it straddles, so a group at the
// page's edge is one the page holds whole, and holding a group whole
// from both directions while a direction is still inside it needs more
// rows than the page has room for. Within one direction the database's
// own ordering governs the cursor, so nothing of that group repeats and
// nothing of it is skipped. The measured sweeps agree: zero duplicates
// across every drain.
func pageCursor(merged []canonical.Trade, cut int, pages directionPages, more bool) *historyCursor {
	if !more || cut < 1 || cut > len(merged) {
		return nil
	}
	last := merged[cut-1]
	endsGroup := cut == len(merged) || !sameTradeOrderKey(last, merged[cut])
	if endsGroup &&
		tieGroupFetched(pages.stored, last, pages.storedLimit) &&
		tieGroupFetched(pages.flipped, last, pages.flippedLimit) {
		if c, ok := tieGroupCursor(last); ok {
			return &c
		}
	}
	return &historyCursor{
		ts:      last.Timestamp,
		ledger:  last.Ledger,
		source:  last.Source,
		txHash:  last.TxHash,
		opIndex: last.OpIndex,
	}
}

// tieGroupCursor is the resume point strictly PAST a tie group whose
// every row has been served: the group's (ts, ledger, tx_hash) with
// op_index stepped once, and NO source.
//
// Why it is correct. The database compares
// (ts, ledger, tx_hash, op_index, source) as a tuple, so against
// (T, L, H, op+1, EMPTY) every row of the group (op_index = op) loses on
// the fourth component and is excluded, and every row after it wins on
// one of the first four and is kept — whatever collation is installed,
// because the comparison never reaches `source`. The one row class that
// could tie into the fifth component is (T, L, H, op+1, S), and it is
// kept for every S: `source` is NOT NULL and [canonical.Trade.Validate]
// rejects an empty one, so every S sorts above the empty string.
//
// THAT PREMISE IS THE APPLICATION'S, NOT THE SCHEMA'S. `trades.source`
// is `text NOT NULL`, which admits the empty string; what excludes it
// is [canonical.Trade.Validate] on both write paths. A row inserted
// around the application with an empty source would be skipped by a
// cursor that steps past its group. Deliberately NOT closed with a
// non-empty CHECK constraint: that is a migration against the trades
// hypertable for a hazard reachable only by writing to the table
// directly, and the premise is already load-bearing elsewhere —
// [decodeHistoryCursor] refuses an empty source, so such a row could
// not be a resume point under the row cursor either. Recorded here so
// that the next person adding a write path knows what holds it up.
//
// Why it is guarded. op_index is uint32, so op+1 at [math.MaxUint32]
// wraps to 0 — a cursor pointing at the START of the transaction, which
// would re-serve it on every page and never terminate. That returns
// false and the caller keeps the last-row cursor, which on THIS path
// alone — a complete group spanning both directions, resumed by naming
// one of its rows — can repeat a row. It cannot skip one or loop, and
// it is the only place on the endpoint where a repeat is possible at
// all; everywhere else the resume point steps past the group. The
// case is doubly unreachable — op_index is stored in an `integer`
// column, so it cannot exceed MaxInt32, and the value is a FANNED
// operation index (operation index packed with the in-operation
// discriminator) that is orders of magnitude below either ceiling — but
// wrapping arithmetic on a served cursor is not a thing to leave
// implicit.
func tieGroupCursor(last canonical.Trade) (historyCursor, bool) {
	if last.OpIndex == math.MaxUint32 {
		return historyCursor{}, false
	}
	return historyCursor{
		ts:      last.Timestamp,
		ledger:  last.Ledger,
		source:  pastTieGroupCursorSource,
		txHash:  last.TxHash,
		opIndex: last.OpIndex + 1,
	}, true
}

// orientTradeTo re-expresses one stored trade in `want`'s orientation.
//
// A row stored that way already is returned untouched. A row stored the
// other way round has its two legs and its two smallest-unit amounts
// swapped — what [canonical.Orient] documents for a flipped row, and
// what [Store.OHLCSeries]'s `norm` CTE does per row on the bucket side,
// keyed on the ROW's own base_asset rather than on which read returned
// it. The swap is exact and performs no division: the wire `price` is
// rendered downstream as quote_amount/base_amount ([tradeRowFrom]), so
// it inverts as a consequence of the swap, at full precision, and a zero
// amount cannot poison a row — that render already answers "0" for a
// zero denominator ([priceRatioDecimal]).
//
// A row that is in neither orientation is left exactly as it came: this
// fold re-expresses rows, it does not relabel them.
func orientTradeTo(t canonical.Trade, want canonical.Pair) canonical.Trade {
	if !t.Pair.Equal(want.Flip()) {
		return t
	}
	t.Pair = want
	t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
	return t
}

// tradeOrderLess is the endpoint's keyset order, stopping at op_index —
// the four components Go and the database compare identically. Rows that
// tie on all four are ordered by `source` in the database and are left
// in their stream's order here; see
// [Server.tradesInRangeAfterBothDirections].
func tradeOrderLess(a, b canonical.Trade) bool {
	switch {
	case !a.Timestamp.Equal(b.Timestamp):
		return a.Timestamp.Before(b.Timestamp)
	case a.Ledger != b.Ledger:
		return a.Ledger < b.Ledger
	case a.TxHash != b.TxHash:
		return a.TxHash < b.TxHash
	default:
		return a.OpIndex < b.OpIndex
	}
}

// sameTradeOrderKey reports whether two rows tie on everything
// [tradeOrderLess] compares — the group a page must not be cut through.
func sameTradeOrderKey(a, b canonical.Trade) bool {
	return a.Timestamp.Equal(b.Timestamp) &&
		a.Ledger == b.Ledger &&
		a.TxHash == b.TxHash &&
		a.OpIndex == b.OpIndex
}

// historyPointsWithAliases reads the point series trying each XLM
// dual-form alias pair (F-1340) and returns the FIRST non-empty series —
// mirroring [Server.ohlcSeriesWithAliases] on the OHLC side. A literal
// read keyed by the requested form silently omits every venue publishing
// XLM under the other id (native vs crypto:XLM vs the SAC), so
// ?asset=native returned empty while crypto:XLM-keyed CEX history existed
// one loop iteration away. First-hit matches the series endpoint;
// cross-form point FUSION is a separate design decision. The first form's
// error (e.g. [ErrUnknownGranularity], which is form-invariant) propagates
// unchanged.
func (s *Server) historyPointsWithAliases(
	ctx context.Context, pair canonical.Pair, gran string,
) ([]HistoryPoint, error) {
	for _, a := range assetAliases(pair.Base) {
		for _, q := range assetAliases(pair.Quote) {
			ap, perr := canonical.NewPair(a, q)
			if perr != nil {
				continue // degenerate alias combination (identity pair)
			}
			points, err := s.history.HistoryPoints(ctx, ap, gran, historyMaxPoints)
			if err != nil || len(points) > 0 {
				return points, err
			}
		}
	}
	return nil, nil
}

// historySinceInceptionStablecoinFallback is the fiat fallback chain for
// a since-inception series whose literal pair (and alias spellings)
// returned no points. Mirrors [chartStablecoinFallback] but uses the
// since-inception read (no `from` lower bound): for a `fiat:USD` quote
// it walks the operator's USD-pegged allow-list, and when no peg answers
// — or the fiat is not USD — it derives the series through XLM
// ([Server.fiatSeriesThroughXLM]), which runs last so a directly
// observed market always wins over a derived one. Returns ok=false when:
//
//   - quote is not fiat,
//   - every peg combination's CAGG read returns empty / errors out AND
//     the XLM cross has no populated leg.
//
// Both sides of each proxied pair are alias-crossed, matching
// [Server.chartFiatProxyPairs]: the base through assetAliases and the
// peg through [Server.usdPegProxyQuotes]. The literal-spelling walk
// above ([Server.historyPointsWithAliases]) already crosses the base
// aliases, so a fallback keyed on the literal base and the classic peg
// alone left the one combination Soroban depth is actually stored under
// — SAC base quoted in the peg's SAC — unread, and answered a
// since-inception request for such an asset with an empty series.
// Priority order is preserved (literal base first; every classic peg
// before any SAC form), so a pair that already answered still answers
// on its first read; only pairs that previously came back empty reach
// the new combinations.
//
// F-1225 (codex audit-2026-05-12).
func (s *Server) historySinceInceptionStablecoinFallback(
	ctx context.Context, pair canonical.Pair, gran string,
) ([]HistoryPoint, bool) {
	if pair.Quote.Type != canonical.AssetFiat {
		return nil, false
	}
	read := func(rc context.Context, p canonical.Pair) ([]HistoryPoint, error) {
		return s.history.HistoryPoints(rc, p, gran, historyMaxPoints)
	}
	if pair.Quote.Code == "USD" {
		pegs := s.usdPegProxyQuotes()
		for _, base := range assetAliases(pair.Base) {
			for _, peg := range pegs {
				if sameAsset(peg, base) {
					continue
				}
				proxied, err := canonical.NewPair(base, peg)
				if err != nil {
					continue
				}
				pp, err := read(ctx, proxied)
				if err != nil || len(pp) == 0 {
					continue
				}
				return pp, true
			}
		}
	}
	return s.fiatSeriesThroughXLM(ctx, pair, read)
}

// externalSourceAmountDecimals returns the amount scale an off-chain
// connector stamps its trades at, and whether the source is a REGISTERED
// off-chain venue at all.
//
// The ok=false path is load-bearing. external.Lookup returns a
// zero-value Metadata for an unknown source, whose AmountScaleDecimals()
// defaults to 8 — correct as a CEX convention, catastrophic as a
// fallback for the on-chain sources (sdex, soroswap, aquarius, …) that
// stamp at per-asset decimals. Only a source present in the registry may
// override the per-asset resolution; everything else keeps it.
func externalSourceAmountDecimals(source string) (int, bool) {
	m, ok := external.Registry[source]
	if !ok {
		return 0, false
	}
	// Only OFF-CHAIN venues normalise to a per-source scale. On-chain
	// sources (SubclassDEX) stamp amounts at the ASSET's own decimals, so
	// the per-asset resolver — which consults a Soroban token's declared
	// decimals() — is authoritative for them and the registry's flat 7
	// would be wrong for any non-7dp token.
	if m.Subclass == external.SubclassDEX {
		return 0, false
	}
	return m.AmountScaleDecimals(), true
}
