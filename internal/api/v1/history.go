package v1

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// HistoryReader is the storage-side interface for /v1/history lookups.
type HistoryReader interface {
	// TradesInRange returns trades for pair closing in [from, to), ts ASC, for the aggregation
	// endpoints (/v1/vwap, /v1/twap, /v1/ohlc) that consume the whole window.
	//
	// When the window holds more than `limit` trades, implementations MUST keep the NEWEST `limit`
	// rows (still returned ts ASC): a truncated aggregate has to run to the window end or a busy
	// 24h VWAP reports a price from hours ago. See VWAPResult.Truncated.
	TradesInRange(ctx context.Context, pair canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error)

	// TradesInRangeAfter is TradesInRange with a full-PK cursor: rows are filtered to (ts, ledger,
	// tx_hash, op_index, source) > the after* tuple, and a zero afterTs disables it. The full PK
	// is needed because many trades can share (ts, ledger), so a page break mid-ledger would
	// otherwise drop rows.
	TradesInRangeAfter(
		ctx context.Context,
		pair canonical.Pair,
		from, to, afterTs time.Time,
		afterLedger uint32,
		afterTxHash, afterSource string,
		afterOpIndex uint32,
		limit int,
	) ([]canonical.Trade, error)

	// HistoryPoints returns every CLOSED bucket from the granularity's CAGG (prices_1m, prices_1h,
	// etc.) for the pair, chronologically, for /v1/history/since-inception. granularity is one of
	// "1m", "15m", "1h", "4h", "1d", "1w", "1mo"; others return [ErrUnknownGranularity]. limit
	// clamps the row count (0 = unbounded). Empty slice and nil error when nothing has closed yet.
	HistoryPoints(ctx context.Context, pair canonical.Pair, granularity string, limit int) ([]HistoryPoint, error)

	// HistoryPointsInRange is [HistoryPoints] with an explicit [from, to) bucket bound (zero
	// disables that bound), serving /v1/chart's rolling window per ADR-0020.
	HistoryPointsInRange(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int) ([]HistoryPoint, error)

	// TWAPPointsInRange is the time-weighted sibling of [HistoryPointsInRange], reading the
	// twap_<granularity> CAGG; the VWAP field carries the TWAP value. Only 1h and 1d have a TWAP
	// CAGG; other granularities return [ErrUnknownGranularity]. Backs /v1/chart?price_type=twap.
	TWAPPointsInRange(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int) ([]HistoryPoint, error)

	// OHLCSeries returns OHLC bars in [from, to), chronologically, for /v1/ohlc's multi-bar mode.
	// Bucket is the START of each window. interval is one of [timescale.OHLCRoutes]; unknown
	// values return [ErrUnknownGranularity]. limit clamps rows (0 = unbounded). The storage
	// side re-buckets a finer CAGG when the interval has no native one.
	OHLCSeries(ctx context.Context, pair canonical.Pair, interval string, from, to time.Time, limit int) ([]OHLCSeriesBar, error)

	// LatestTradePerSource returns the most-recent trade from each source that has ever traded
	// the market `pair` names. Empty slice and nil error when none.
	//
	// It matches either stored direction and returns the requested orientation: SDEX records
	// XLM/USDC and USDC/XLM as separate rows, so a literal bind would miss a market /v1/history
	// serves. Implementations MUST read both directions and re-express flipped rows by swapping
	// the legs and the smallest-unit amounts, so a derived price is the exact reciprocal. A source
	// that traded both ways folds to its later trade.
	//
	// sourceFilter ("" = none) restricts to one source in SQL. Backs /v1/observations with a
	// DISTINCT ON (source) scan per direction and no time bound.
	//
	// Cost: trades_pair_source_ts_idx gives per-chunk SkipScans (tens to hundreds of ms), well
	// inside the 8s ceiling, and [CachedHistoryReader] SWR-caches it.
	LatestTradePerSource(ctx context.Context, pair canonical.Pair, sourceFilter string) ([]canonical.Trade, error)
}

// HistoryPoint is one (timestamp, price, optional usd-volume) CAGG row that the handler marshals to the OpenAPI `{t, p, v_usd?}` shape.
type HistoryPoint struct {
	Bucket    time.Time
	VWAP      string   // NUMERIC text — pass-through, no float round-trip
	VolumeUSD *string  // null when the bucket's underlying trades had no usd_volume
	Sources   []string // venues behind the bucket; nil for TWAP and FX points
}

// ErrUnknownGranularity is returned for a granularity outside the canonical set; the handler maps it to 400.
var ErrUnknownGranularity = fmt.Errorf("unknown granularity")

// TradeRow is the /v1/history entry shape; amounts are decimal strings (ADR-0003) and Price is computed at response time, null when a leg is zero.
type TradeRow struct {
	Source      string   `json:"source"`
	Ledger      uint32   `json:"ledger"`
	TxHash      string   `json:"tx_hash"`
	OpIndex     uint32   `json:"op_index"`
	Timestamp   WireTime `json:"ts"`
	BaseAsset   string   `json:"base_asset"`
	QuoteAsset  string   `json:"quote_asset"`
	BaseAmount  string   `json:"base_amount"`
	QuoteAmount string   `json:"quote_amount"`
	Price       *string  `json:"price"` // quote/base as decimal; nil when a leg is zero
	// BaseDecimals / QuoteDecimals are the smallest-unit scale of each side's amount. The scale
	// belongs to the row's source, not the asset: on-chain rows carry the asset's own scale, CEX
	// rows carry 8 regardless of the pair. Never substitute a constant: the error is
	// undetectable in the payload because `price` is scale-invariant. Omitted (0) on
	// /v1/observations.
	BaseDecimals  int `json:"base_decimals,omitempty"`
	QuoteDecimals int `json:"quote_decimals,omitempty"`
	// RoutedVia is the router whose same-tx invocation drove this trade (routers.name). Omitted
	// for direct trades and for recent routed trades the 1-minute attribution sweeper has not tagged.
	RoutedVia string `json:"routed_via,omitempty"`
}

// SourceHistoryReader is the optional single-source [HistoryReader.TradesInRangeAfter]; without it `?source=` cannot be served.
type SourceHistoryReader interface {
	TradesInRangeAfterFromSource(
		ctx context.Context,
		pair canonical.Pair,
		source string,
		from, to, afterTs time.Time,
		afterLedger uint32,
		afterTxHash, afterSource string,
		afterOpIndex uint32,
		limit int,
	) ([]canonical.Trade, error)
}

// tradeRowFrom converts canonical.Trade → wire shape; Price uses `decimals` fractional digits (default 10).
func tradeRowFrom(t canonical.Trade, decimals int) TradeRow {
	if decimals <= 0 {
		decimals = 10
	}
	row := TradeRow{
		Source:      t.Source,
		Ledger:      t.Ledger,
		TxHash:      t.TxHash,
		OpIndex:     t.OpIndex,
		Timestamp:   WireTime(t.Timestamp),
		BaseAsset:   t.Pair.Base.String(),
		QuoteAsset:  t.Pair.Quote.String(),
		BaseAmount:  t.BaseAmount.String(),
		QuoteAmount: t.QuoteAmount.String(),
		RoutedVia:   t.RoutedVia,
	}
	if p, ok := priceRatioDecimal(t, decimals); ok {
		row.Price = &p
	}
	return row
}

// normalizeTradeRowPrices rewrites each row's Price from the raw quote/base ratio to the
// nonstandard-decimals-corrected price. rows and trades are parallel slices; the legs are resolved
// once against the nonstandard_decimals_assets guard table (as /v1/vwap and /v1/price do). A no-op
// when both legs share a scale. Amounts stay raw smallest-unit integers; only Price is corrected.
func (s *Server) normalizeTradeRowPrices(rows []TradeRow, trades []canonical.Trade, base, quote canonical.Asset) {
	baseDec := aggregate.ResolveDecimals(s.NonstandardDecimals, base)
	quoteDec := aggregate.ResolveDecimals(s.NonstandardDecimals, quote)
	if baseDec == quoteDec {
		return
	}
	for i := range rows {
		b, q := trades[i].BaseAmount.BigInt(), trades[i].QuoteAmount.BigInt()
		if b.Sign() <= 0 || q.Sign() <= 0 {
			continue
		}
		p := ratToDecimal(aggregate.AdjustPrice(new(big.Rat).SetFrac(q, b), baseDec, quoteDec), 10)
		rows[i].Price = &p
	}
}

// ─── Handler ──────────────────────────────────────────────────────

// historyTradeRows renders trades stamped with each row's amount scale:
// the per-asset decimals on-chain, the connector's single scale off-chain.
func historyTradeRows(trades []canonical.Trade, baseDec, quoteDec int) []TradeRow {
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
	return rows
}

// historySourceParam validates the optional `source` filter and that the
// reader can honour it, writing the problem response when not.
func historySourceParam(w http.ResponseWriter, r *http.Request, reader HistoryReader) (string, bool) {
	source := r.URL.Query().Get("source")
	if !rawTradeSourceFilterOK(w, r, source) {
		return "", false
	}
	if _, ok := reader.(SourceHistoryReader); source != "" && !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/history-unavailable",
			"Source filter not available", http.StatusServiceUnavailable,
			"this deployment's HistoryReader cannot filter by source")
		return "", false
	}
	return source, true
}

// handleHistory serves GET /v1/history?base=<id>&quote=<id>&from=<rfc3339>&to=<rfc3339>&limit=<int>[&source=<name>].
//
// Defaults:
//   - from: to - 1h (1-hour window rolling back from `to`)
//   - to:   now
//   - limit: 1000 (server clamps to ≤ 10000)
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) { //nolint:funlen // option parsing + 8s-timeout guard + range/limit defaults are linear; splitting fragments the request lifecycle
	reader := s.History
	if reader == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/history-unavailable",
			"History serving not configured", http.StatusServiceUnavailable,
			"this deployment has no HistoryReader wired — check binary configuration")
		return
	}
	asCSV := explorerpkg.NegotiateCSV(w, r)

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

	// Reads raw trades only (no CAGG), so no nonstandard-decimals decline guard is needed; Price
	// is normalized below once decimals resolve.

	source, ok := historySourceParam(w, r, reader)
	if !ok {
		return
	}

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

	// The opaque cursor shadows `from`; otherwise paginating callers would re-request
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

	// 8s ceiling on the trades range query: long windows can take 5-10s cold. One ceiling spans
	// every alias scan below so the fan-in cannot multiply the endpoint's worst-case hold.
	hCtx, hCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer hCancel()
	trades, next, err := s.tradesInRangeAfterWithAliases(hCtx, reader, pair, source,
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

	// Resolve the per-asset scale once; each row overrides it with its source's scale. The
	// per-asset value is wrong for off-chain rows (CEX 1e8, FX 1e6) and one page can mix them;
	// the scale-invariant `price` would not reveal a 10x overstatement.
	baseDec, baseOK := s.resolveAssetDecimals(hCtx, base)
	quoteDec, quoteOK := s.resolveAssetDecimals(hCtx, quote)
	if !baseOK || !quoteOK {
		writeDecimalsUnavailable(w, r, "https://api.stellarindex.io/errors/history-unavailable")
		return
	}
	rows := historyTradeRows(trades, baseDec, quoteDec)
	// Price is corrected against the nonstandard_decimals_assets guard table, not the
	// baseDec/quoteDec above; the resolvers can disagree, and mixing them would make Price
	// differ from /v1/vwap for the same pair.
	s.normalizeTradeRowPrices(rows, trades, base, quote)

	// Pagination: next comes from the read itself, not from len(trades) == limit, because the
	// two-direction merge can cut a page short with rows still behind it.
	env := Envelope{Data: rows, Flags: Flags{}}
	// The coverage probe spans both legs' alias families and both stored directions, exactly
	// what the page read spans (see historyCoverageSet), so the floor describes returnable rows.
	coverageFrom, outsideCoverage := s.coverageAnnotationIfEmpty(
		r.Context(), historyCoverageSet(pair), to, historyPageIsAmbiguous(len(trades), afterTs))
	env.CoverageFrom, env.Flags.OutsideCoverage = wireTimePtr(coverageFrom), outsideCoverage
	env.Pagination = historyNextCursor(next)
	if asCSV {
		s.writeHistoryCSV(w, r, rows, env)
		return
	}
	writeEnvelope(w, env)
}

// historyNextCursor renders the next-cursor, or nil when the window is drained. The resume point
// (not always the last row's key) comes from [streamPageCursor].
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

// pastTieGroupCursorSource is the `source` of a cursor that resumes past a whole tie group: a
// marker, not a source name. It binds source empty so the stepped op_index decides the tuple
// comparison (see [tieGroupCursor]); a client-written empty source would weaken the full-PK
// comparison, which [decodeHistoryCursor] rejects. `*` cannot collide with a real source name
// ([a-z0-9_-]); TestHistoryCursor_PastGroupMarkerIsNotASourceName pins that.
const pastTieGroupCursorSource = "*"

// encodeHistoryCursor / decodeHistoryCursor are the URL-safe base64 wire form of a historyCursor:
// "<unix_nanos>:<ledger>:<source>:<tx_hash>:<op_index>". Source names never contain the separator.
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
		// Our own past-group cursor: its op_index is already stepped past the group, so the
		// comparison is decided before the source column and no source is bound.
		source = ""
	case "":
		// An empty source would weaken the full-PK comparison and reintroduce the same-ledger
		// page-skip.
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

// isLowerHex64 reports whether s is exactly 64 lowercase hex characters, mirroring
// canonical.validTxHash without importing it (which would create a cycle).
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

// parseBaseQuote extracts and validates base/quote, writing a problem response and returning
// ok=false on failure. `asset=` is an alias for `base=` so URLs copied from /v1/price work;
// passing both is a 400 rather than a silent precedence pick.
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

// HistorySeries is the /v1/history/since-inception shape, mirroring the OpenAPI HistoryEnvelope.data.
type HistorySeries struct {
	AssetID       string             `json:"asset_id"`
	Quote         string             `json:"quote"`
	PriceType     string             `json:"price_type"`  // "vwap" today; TWAP planned
	Granularity   string             `json:"granularity"` // "1m" / "15m" / "1h" / etc.
	Points        []HistoryPointWire `json:"points"`
	Discontinuous bool               `json:"discontinuous"`           // true when points skip at least one whole bucket
	GapStartsAt   *WireTime          `json:"gap_starts_at,omitempty"` // last bucket before the WIDEST interior gap
	GapEndsAt     *WireTime          `json:"gap_ends_at,omitempty"`   // first bucket after it

	// RowCapTruncated is true when the read hit historyMaxPoints, so Points holds the OLDEST
	// slice of history and stops short of the present (the opposite of ChartSeries' Truncated).
	// DataEndsAt is the last served bucket, letting a client tell "the series ends here" from
	// "the data ends here".
	RowCapTruncated bool      `json:"row_cap_truncated"`
	DataEndsAt      *WireTime `json:"data_ends_at,omitempty"` // last returned bucket; only set when RowCapTruncated
}

// markDiscontinuity stamps the interior-gap signal via [ChartSeries.markDiscontinuity] so the
// surfaces cannot disagree about what a hole is. This surface shares the chart's read chain and
// can carry the same hole, so it must flag it rather than assert continuity it does not have.
func (h *HistorySeries) markDiscontinuity() {
	c := ChartSeries{Granularity: h.Granularity, Points: h.Points}
	c.markDiscontinuity()
	h.Discontinuous, h.GapStartsAt, h.GapEndsAt = c.Discontinuous, c.GapStartsAt, c.GapEndsAt
}

// HistoryPointWire marshals as the OpenAPI HistoryPoint, kept distinct from [HistoryPoint] so wire-shape assumptions do not leak into adapters.
type HistoryPointWire struct {
	T    WireTime `json:"t"`
	P    string   `json:"p"`
	VUSD *string  `json:"v_usd,omitempty"`

	// Sources is set only when a venue outside the VWAP (a derived series such as
	// poloniex_via_btc) contributed, so the point is never read as a fill-derived VWAP.
	Sources []string `json:"sources,omitempty"`
}

// historyWirePoint renders a reader point, with provenance only when a non-VWAP venue contributed.
func historyWirePoint(p HistoryPoint) HistoryPointWire {
	return HistoryPointWire{T: WireTime(p.Bucket), P: p.VWAP, VUSD: p.VolumeUSD, Sources: nonVWAPSources(p.Sources)}
}

// nonVWAPSources returns the sorted venues in srcs registered with IncludeInVWAP=false.
func nonVWAPSources(srcs []string) []string {
	var out []string
	for _, s := range srcs {
		if md, ok := external.Registry[s]; ok && !md.IncludeInVWAP {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

const (
	defaultHistoryGranularity = "1d"

	// historyMaxPoints caps a single response: the 1m CAGG reaches ~32M rows over 5 years.
	// Truncation takes the EARLIEST buckets. /v1/chart knows that from the request and coarsens its
	// grain ([chartFitGranularity]); since-inception has no window parameter, so the handler checks
	// len(points) == historyMaxPoints and stamps row_cap_truncated and data_ends_at instead.
	historyMaxPoints = 50_000

	// sinceInceptionPricingSpan is the fixed history depth the rate-limit price assumes, so a
	// request's price does not creep up by the day.
	sinceInceptionPricingSpan = 10 * 365 * 24 * time.Hour
)

// sinceInceptionCost is the rate-limit price, in tokens, of one since-inception request. Work
// tracks the point count, min(historyMaxPoints, span / bucket width), so each grain costs its
// point count's multiple of the default grain's, rounded DOWN to stay conservative. An unknown
// grain costs the base token because it is a 400 before any read.
func sinceInceptionCost(gran string) int {
	width := timescale.HistoryGranularity(gran).BucketDuration()
	if width <= 0 {
		return 1
	}
	points := func(w time.Duration) int {
		return min(historyMaxPoints, int(sinceInceptionPricingSpan/w))
	}
	base := points(timescale.HistoryGranularity(defaultHistoryGranularity).BucketDuration())
	return max(1, points(width)/base)
}

// handleHistorySinceInception serves GET /v1/history/since-inception?asset=&quote=&granularity=:
// CLOSED CAGG buckets, oldest first, capped at historyMaxPoints. A pair with no closed buckets
// yet is 200 with empty points[], not 404, since the asset may be known without bucketed history.
func (s *Server) handleHistorySinceInception(w http.ResponseWriter, r *http.Request) { //nolint:funlen // option parsing + 8s-timeout guard + grain-default + clamp logic are linear; splitting fragments the request lifecycle
	if s.History == nil {
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

	// Same directory-scam gate as /v1/chart: serving a flagged issuer's aggregated VWAP
	// trajectory here would defeat withholding its price elsewhere. Raw surfaces stay visible.
	if s.seriesWithheldForScam(w, r, pair, "history_series") {
		return
	}
	if !middleware.ChargeRateLimit(w, r, sinceInceptionCost(gran)) {
		return
	}

	hCtx, hCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer hCancel()
	// Same chain as /v1/chart ([Server.chartSeriesPoints]) so the two fallbacks cannot drift;
	// only the read closure differs. A window unbounded below is never narrowed.
	read := func(rc context.Context, p canonical.Pair, _, _ time.Time, limit int) ([]HistoryPoint, error) {
		return s.History.HistoryPoints(rc, p, gran, limit)
	}
	points, walk, err := s.chartSeriesPoints(hCtx, pair, chartWindow{gran: gran}, read)
	if errors.Is(err, ErrUnknownGranularity) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-granularity",
			"Invalid granularity", http.StatusBadRequest,
			// Enumerated from the slice Validate ranges over; a hand-written copy would go stale.
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

	wire := make([]HistoryPointWire, len(points))
	for i, p := range points {
		wire[i] = historyWirePoint(p)
	}

	series := HistorySeries{
		AssetID:     asset.String(),
		Quote:       quote.String(),
		PriceType:   "vwap",
		Granularity: gran,
		Points:      wire,
	}
	if len(points) == historyMaxPoints {
		series.RowCapTruncated = true
		last := wire[len(wire)-1].T
		series.DataEndsAt = &last
	}
	series.markDiscontinuity()
	writeJSON(w, series, Flags{Triangulated: walk.proxied, Stale: walk.degraded, Degraded: walk.degraded})
}

// tradesInRangeAfterWithAliases reads one page of raw trades unioned across every XLM alias form,
// each in both stored directions: the raw-trade twin of [Server.chartMergeAliasPairs]. A
// literal-keyed, one-direction read is blind to venues publishing XLM under another id and to
// markets recorded the other way round.
//
// Union, not first-hit: the cursor carries no form field ([historyCursor]), so falling through to
// a sibling form on the next page would silently skip its rows at or before the cursor.
// [mergeTradeStreams] merges the streams into one keyset-ordered page; ties keep stream order.
//
// The returned cursor (nil when drained) is not `len(page) == limit`: tie-group completion can
// cut a page short or run it over `limit` ([streamPageCursor]).
func (s *Server) tradesInRangeAfterWithAliases(
	ctx context.Context,
	reader HistoryReader,
	pair canonical.Pair,
	source string,
	from, to, afterTs time.Time,
	afterLedger uint32,
	afterTxHash, afterSource string,
	afterOpIndex uint32,
	limit int,
) ([]canonical.Trade, *historyCursor, error) {
	read := func(p canonical.Pair, n int) ([]canonical.Trade, error) {
		if sr, ok := reader.(SourceHistoryReader); ok && source != "" {
			return sr.TradesInRangeAfterFromSource(ctx, p, source, from, to,
				afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, n)
		}
		return reader.TradesInRangeAfter(ctx, p, from, to,
			afterTs, afterLedger, afterTxHash, afterSource, afterOpIndex, n)
	}
	streams, err := readAliasFormStreams(pair, limit, read)
	if err != nil {
		return nil, nil, err
	}
	if streams, err = s.completeOverLimitTieGroup(streams, read, limit); err != nil {
		return nil, nil, err
	}
	page, next := mergeTradeStreams(streams, pair, limit)
	return page, next, nil
}

// readAliasFormStreams reads one both-direction page of every alias form
// [tradesInRangeAfterWithAliases] must union, deduping a pair that canonicalises identically to
// one already read. The literal form in the requested orientation leads, the order pinned on a tie.
func readAliasFormStreams(
	pair canonical.Pair, limit int,
	read func(canonical.Pair, int) ([]canonical.Trade, error),
) ([]tradeStream, error) {
	var streams []tradeStream
	seen := map[canonical.Pair]bool{}
	for _, b := range assetAliases(pair.Base) {
		for _, q := range assetAliases(pair.Quote) {
			ap, perr := canonical.NewPair(b, q)
			if perr != nil {
				continue // degenerate alias combination (identity pair)
			}
			if seen[ap] {
				continue
			}
			seen[ap] = true
			stored, err := read(ap, limit)
			if err != nil {
				return nil, err
			}
			flipped, err := read(ap.Flip(), limit)
			if err != nil {
				return nil, err
			}
			streams = append(streams,
				tradeStream{pair: ap, rows: stored, limit: limit},
				tradeStream{pair: ap.Flip(), rows: flipped, limit: limit})
		}
	}
	return streams, nil
}

// tieGroupReadMax is the size of the one re-read that completes a tie group, equal to
// [Store.TradesInRangeAfter]'s own clamp.
//
// A tie group is the rows sharing (ts, ledger, tx_hash, op_index); they differ only in `source`,
// so a group cannot exceed the distinct sources (a few dozen) and one read to the store maximum
// ends the question. A retry ladder was rejected: its exhaustion branch would cut a page through
// the group and mint a last-row cursor, silently losing the group's remaining rows.
const tieGroupReadMax = 10000

// tradeStream is one raw keyset-ordered page read behind the alias/direction fan-in
// [readAliasFormStreams] builds: the pair it was read with (for a tie-group re-read) and the limit
// it was given, which [Server.completeOverLimitTieGroup] may raise above the page `limit`. That
// per-stream limit, not the page limit, says whether a stream is drained.
type tradeStream struct {
	pair  canonical.Pair
	rows  []canonical.Trade
	limit int
}

// completeOverLimitTieGroup re-reads every stream that stopped ON the tie group a `limit`-row page
// would have to serve past, so the merge may serve the group whole. Returns `streams` untouched
// when no group straddles the page edge (every ordinary request).
//
// A group that opens the page and still holds rows at index `limit` cannot take the lower edge
// (an empty page carrying a cursor would stall the client), so it runs to the upper edge, over
// `limit`. That is only safe if the group is complete across every stream ([tieGroupFetched]): a
// cursor minted past the group would exclude unfetched rows differing only in `source`. So each
// under-read stream is re-read once to [tieGroupReadMax]; the final check makes the impossible
// state loud.
func (s *Server) completeOverLimitTieGroup(
	streams []tradeStream,
	read func(canonical.Pair, int) ([]canonical.Trade, error),
	limit int,
) ([]tradeStream, error) {
	group, straddles := overLimitTieGroup(streams, limit)
	if !straddles {
		return streams, nil
	}
	complete := true
	for i := range streams {
		if tieGroupFetched(streams[i].rows, group, streams[i].limit) {
			continue
		}
		rows, err := read(streams[i].pair, tieGroupReadMax)
		if err != nil {
			return streams, err
		}
		streams[i].rows, streams[i].limit = rows, tieGroupReadMax
		if !tieGroupFetched(streams[i].rows, group, streams[i].limit) {
			complete = false
		}
	}
	if !complete {
		// The page will mint a cursor past the group, skipping any row this read never saw.
		// Error, not Warn: it is a data-integrity failure, not a busy market.
		s.logger.Error("history page: tie group larger than the store's maximum read",
			"ts", group.Timestamp, "ledger", group.Ledger,
			"tx_hash", group.TxHash, "op_index", group.OpIndex,
			"read_limit", tieGroupReadMax)
	}
	return streams, nil
}

// overLimitTieGroup returns the tie group that opens the merge and still holds rows at index
// `limit` (so a page must serve past `limit` to return anything), and whether there is one.
//
// Computed from the streams: one group's rows are contiguous at each stream's head, so summing the
// heads avoids building the merge. Every other cut retreats to a group's lower edge.
func overLimitTieGroup(streams []tradeStream, limit int) (canonical.Trade, bool) {
	var none canonical.Trade
	if limit < 1 {
		return none, false
	}
	head, ok := earliestStreamHead(streams)
	if !ok {
		return none, false
	}
	total := 0
	for _, st := range streams {
		total += leadingTieGroupLen(st.rows, head)
	}
	if total <= limit {
		return none, false
	}
	return head, true
}

// earliestStreamHead returns the smallest stream head by [tradeOrderLess], and whether any exists.
func earliestStreamHead(streams []tradeStream) (canonical.Trade, bool) {
	var head canonical.Trade
	found := false
	for _, st := range streams {
		if len(st.rows) == 0 {
			continue
		}
		if !found || tradeOrderLess(st.rows[0], head) {
			head, found = st.rows[0], true
		}
	}
	return head, found
}

// leadingTieGroupLen counts the head rows of one stream that tie with `probe`.
func leadingTieGroupLen(rows []canonical.Trade, probe canonical.Trade) int {
	n := 0
	for n < len(rows) && sameTradeOrderKey(rows[n], probe) {
		n++
	}
	return n
}

// tieGroupFetched reports whether one stream's read returned every row it holds in `probe`'s tie
// group: true when the read came back short of its limit (drained) or its last row falls outside
// the group (rows arrive in key order, so the group ended inside what was read).
//
// False is the state that loses rows: the read stopped on the group, and a cursor minted from
// another stream's row would make the `(ts, ledger, tx_hash, op_index, source) >` predicate
// exclude every unfetched row whose source orders below that one.
func tieGroupFetched(rows []canonical.Trade, probe canonical.Trade, readLimit int) bool {
	if len(rows) < readLimit {
		return true
	}
	return !sameTradeOrderKey(rows[len(rows)-1], probe)
}

// mergeTradeStreams merges every alias-form/direction stream [readAliasFormStreams] built into one
// page in `want`'s orientation, cuts it without splitting a tie group, and reports whether rows
// remain behind the cut.
//
// On a tie the earlier stream wins, so the literal form's requested orientation leads. `st.limit`,
// not the page `limit`, says a stream is drained ([completeOverLimitTieGroup] can raise it). The
// merge compares only (ts, ledger, tx_hash, op_index): Go's byte order on `source` can disagree
// with the database collation, so it never compares source and never cuts through a tie group.
func mergeTradeStreams(streams []tradeStream, want canonical.Pair, limit int) ([]canonical.Trade, *historyCursor) {
	if limit < 1 {
		limit = 1
	}
	merged, full := kWayMergeTradeRows(streams, want)

	if limit >= len(merged) {
		return merged, streamPageCursor(merged, len(merged), streams, full)
	}
	cut := tradePageCut(merged, limit)
	return merged[:cut], streamPageCursor(merged, cut, streams, cut < len(merged) || full)
}

// kWayMergeTradeRows merges the ordered streams into one slice in `want`'s orientation, keeping
// stream order on a tie. The bool reports whether any stream filled its read and may hold more.
func kWayMergeTradeRows(streams []tradeStream, want canonical.Pair) ([]canonical.Trade, bool) {
	total, full := 0, false
	for _, st := range streams {
		total += len(st.rows)
		if len(st.rows) >= st.limit {
			// A filled stream may hold more rows even when nothing survives the cut, so
			// "drained" comes from the reads, not the merged length.
			full = true
		}
	}
	merged := make([]canonical.Trade, 0, total)
	idx := make([]int, len(streams))
	for {
		best := -1
		for i, st := range streams {
			if idx[i] >= len(st.rows) {
				continue
			}
			if best == -1 || tradeOrderLess(st.rows[idx[i]], streams[best].rows[idx[best]]) {
				best = i
			}
		}
		if best == -1 {
			break
		}
		merged = append(merged, orientTradeTo(streams[best].rows[idx[best]], want))
		idx[best]++
	}
	return merged, full
}

// tradePageCut finds where in `merged` to cut at `limit` without splitting
// a tie group ([sameTradeOrderKey]).
func tradePageCut(merged []canonical.Trade, limit int) int {
	cut := limit
	for cut > 0 && sameTradeOrderKey(merged[cut-1], merged[cut]) {
		cut--
	}
	if cut == 0 {
		// The group opens the page and reaches past `limit`: an empty page carrying a cursor
		// would stall the client, so run to the upper edge. [completeOverLimitTieGroup] already
		// read the streams whole.
		cut = 1
		for cut < len(merged) && sameTradeOrderKey(merged[cut-1], merged[cut]) {
			cut++
		}
	}
	return cut
}

// streamPageCursor is where the next page resumes after `merged[:cut]`, or nil when drained.
//
// Two forms, the difference between exactly-once and at-least-once pagination. The default names
// the last served row's primary key: it never skips, but a row tying with it and served earlier
// on the page comes back when the database orders its `source` above the cursor's (the merge
// orders ties by stream, the database by `source`). So when the page ends on a tie group that is
// complete across every stream ([tieGroupFetched]), the cursor steps past the whole group
// ([tieGroupCursor]); otherwise the last-row cursor stands.
func streamPageCursor(merged []canonical.Trade, cut int, streams []tradeStream, more bool) *historyCursor {
	if !more || cut < 1 || cut > len(merged) {
		return nil
	}
	last := merged[cut-1]
	endsGroup := cut == len(merged) || !sameTradeOrderKey(last, merged[cut])
	if endsGroup && allStreamsFetchedGroup(streams, last) {
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

// allStreamsFetchedGroup reports whether every stream has read `last`'s tie group in full.
func allStreamsFetchedGroup(streams []tradeStream, last canonical.Trade) bool {
	for _, st := range streams {
		if !tieGroupFetched(st.rows, last, st.limit) {
			return false
		}
	}
	return true
}

// tieGroupCursor is the resume point strictly past a fully served tie group: the group's
// (ts, ledger, tx_hash) with op_index stepped once and no source.
//
// Correct because the database compares (ts, ledger, tx_hash, op_index, source) as a tuple:
// against (T, L, H, op+1, EMPTY) every group row loses on the fourth component and every later row
// wins on one of the first four, whatever the collation. A row (T, L, H, op+1, S) is kept for
// every S because [canonical.Trade.Validate] rejects an empty source.
//
// That premise is the application's, not the schema's: `trades.source` is `text NOT NULL`, so a
// direct write with an empty source would be skipped. Not closed with a CHECK (a hypertable
// migration for a hazard reachable only by direct writes).
//
// op_index is uint32: op+1 at [math.MaxUint32] would wrap to 0 and re-serve the transaction
// forever, so that returns false and the caller keeps the last-row cursor (may repeat, never skips).
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

// orientTradeTo re-expresses one stored trade in `want`'s orientation. A row stored the other way
// round has its legs and smallest-unit amounts swapped (as [canonical.Orient]), keyed on the row's
// own base_asset, not on which read returned it. No division happens: `price` is rendered later as
// quote_amount/base_amount ([tradeRowFrom]), so it inverts at full precision. A row in neither
// orientation is left as it came.
func orientTradeTo(t canonical.Trade, want canonical.Pair) canonical.Trade {
	if !t.Pair.Equal(want.Flip()) {
		return t
	}
	t.Pair = want
	t.BaseAmount, t.QuoteAmount = t.QuoteAmount, t.BaseAmount
	return t
}

// tradeOrderLess is the keyset order through op_index, the four components Go and the database
// compare identically. Ties are ordered by `source` in the database and kept in stream order here.
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

// externalSourceAmountDecimals returns the amount scale an off-chain connector stamps its trades
// at, and whether the source is a registered off-chain venue at all.
//
// ok=false is load-bearing: an unknown source's zero Metadata defaults to 8, right for a CEX but
// catastrophic as a fallback for on-chain sources that stamp at per-asset decimals.
func externalSourceAmountDecimals(source string) (int, bool) {
	m, ok := external.Registry[source]
	if !ok {
		return 0, false
	}
	// On-chain sources (SubclassDEX) stamp at the asset's own decimals, so the per-asset resolver
	// is authoritative; the registry's flat 7 would be wrong for any non-7dp token.
	if m.Subclass == external.SubclassDEX {
		return 0, false
	}
	return m.AmountScaleDecimals(), true
}
