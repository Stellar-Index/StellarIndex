package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PriceQuery selects the asset / quote pair for [Client.Price]; Asset is required and Quote defaults to "fiat:USD" server-side when empty.
type PriceQuery struct {
	Asset string
	Quote string // optional; server defaults to fiat:USD
}

// Price fetches the current closed-bucket VWAP for asset/quote, cross-region consistent per ADR-0015.
func (c *Client) Price(ctx context.Context, q PriceQuery) (*Envelope[PriceSnapshot], error) {
	if q.Asset == "" {
		return nil, &APIError{Status: 400, Title: "asset required"}
	}
	v := url.Values{}
	v.Set("asset", q.Asset)
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	var env Envelope[PriceSnapshot]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/price", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// PriceAtQuery parameterises Client.PriceAt; TS is required and must be a past instant.
type PriceAtQuery struct {
	Asset string
	Quote string    // optional; server defaults to fiat:USD
	TS    time.Time // required; historical instant
}

// PriceAt fetches the closed 1-minute VWAP bucket at-or-before TS; ObservedAt is the BUCKET's close time, and a nearest bucket more than 24h before TS is a 404 rather than a fabricated continuity.
func (c *Client) PriceAt(ctx context.Context, q PriceAtQuery) (*Envelope[PriceSnapshot], error) {
	if q.Asset == "" {
		return nil, &APIError{Status: 400, Title: "asset required"}
	}
	if q.TS.IsZero() {
		return nil, &APIError{Status: 400, Title: "ts required"}
	}
	v := url.Values{}
	v.Set("asset", q.Asset)
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	v.Set("ts", q.TS.UTC().Format(time.RFC3339))
	var env Envelope[PriceSnapshot]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/price/at", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// PriceChangesQuery parameterises [Client.PriceChanges]; Asset is required and Quote defaults to fiat:USD server-side.
type PriceChangesQuery struct {
	Asset string
	Quote string // optional; server defaults to fiat:USD
}

// PriceChanges fetches the current closed price plus signed change over 1h/24h/7d/30d; a horizon with no data returns Available=false with nil fields (never an error), and a 404 means the pair has no current price.
func (c *Client) PriceChanges(ctx context.Context, q PriceChangesQuery) (*Envelope[PriceChanges], error) {
	if q.Asset == "" {
		return nil, &APIError{Status: 400, Title: "asset required"}
	}
	v := url.Values{}
	v.Set("asset", q.Asset)
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	var env Envelope[PriceChanges]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/price/changes", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// PriceTipQuery is the input for [Client.PriceTip]; Asset is required, Quote defaults to "fiat:USD", and WindowSeconds defaults to 5 when zero while a value outside [1, 60] is REJECTED with a 400 (the SDK forwards it verbatim).
type PriceTipQuery struct {
	Asset         string
	Quote         string // optional; server defaults to fiat:USD
	WindowSeconds int    // optional; default 5; outside [1, 60] → 400
}

// PriceTip fetches the live rolling-window price (ADR-0018), either "vwap" (WindowSeconds may exceed the request when a short empty window is retried at 30s) or "last_trade" (check ObservedAt); it has no cross-region consistency, and flags.stale is always false.
func (c *Client) PriceTip(ctx context.Context, q PriceTipQuery) (*Envelope[PriceSnapshot], error) {
	if q.Asset == "" {
		return nil, &APIError{Status: 400, Title: "asset required"}
	}
	v := url.Values{}
	v.Set("asset", q.Asset)
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	if q.WindowSeconds > 0 {
		v.Set("window_seconds", strconv.Itoa(q.WindowSeconds))
	}
	var env Envelope[PriceSnapshot]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/price/tip", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// PriceBatchQuery is the input for [Client.PriceBatch]; AssetIDs is required and non-empty (1..100 via GET, 101..1000 via POST, >1000 is a 400 and the caller must chunk), and Quote defaults to "fiat:USD".
type PriceBatchQuery struct {
	AssetIDs []string
	Quote    string // optional; server defaults to fiat:USD
}

// PriceBatch fetches the current closed-bucket VWAP for many assets in one round-trip (ADR-0015); an id absent from Data is either listed in [Envelope.Withheld] (price withheld) or has no data, and flags.stale is the OR over rows.
func (c *Client) PriceBatch(ctx context.Context, q PriceBatchQuery) (*Envelope[[]PriceSnapshot], error) {
	if len(q.AssetIDs) == 0 {
		return nil, &APIError{Status: 400, Title: "asset_ids required"}
	}
	if len(q.AssetIDs) > priceBatchPOSTMax {
		return nil, &APIError{
			Status: 400,
			Title:  "too many asset_ids",
			Detail: "the server caps POST /v1/price/batch at " + strconv.Itoa(priceBatchPOSTMax) + " ids",
		}
	}

	var env Envelope[[]PriceSnapshot]
	if len(q.AssetIDs) <= priceBatchGETMax {
		v := url.Values{}
		v.Set("asset_ids", strings.Join(q.AssetIDs, ","))
		if q.Quote != "" {
			v.Set("quote", q.Quote)
		}
		if err := c.doJSON(ctx, http.MethodGet, "/v1/price/batch", v, nil, &env); err != nil {
			return nil, err
		}
		return &env, nil
	}

	// >100 ids → POST. Body shape mirrors the server's POST handler.
	body := struct {
		AssetIDs []string `json:"asset_ids"`
		Quote    string   `json:"quote,omitempty"`
	}{
		AssetIDs: q.AssetIDs,
		Quote:    q.Quote,
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/price/batch", nil, body, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// priceBatchGETMax / priceBatchPOSTMax mirror the server's priceBatchMaxAssets / priceBatchMaxAssetsPOST in internal/api/v1/price.go.
// Duplicated on purpose: the SDK is SemVer-stable (ADR-0005), so importing server constants would couple consumers to internal/.
const (
	priceBatchGETMax  = 100
	priceBatchPOSTMax = 1000
)

// HistoryQuery selects the range for [Client.HistorySinceInception]; Asset is required.
type HistoryQuery struct {
	Asset string
	Quote string // optional
	// Granularity is 1m / 15m / 1h / 4h / 1d / 1w / 1mo; default 1d. This endpoint has no window
	// parameter, and the response is capped at 50,000 buckets taking the OLDEST, so a fine grain
	// over a long-lived pair ends early with Flags.Stale raised. 1m may additionally be bounded to
	// the last 90 days on a given deployment. For a bounded window fitted to the cap automatically,
	// use [Client.Chart].
	Granularity string
}

// HistorySinceInception fetches the full historical series for an asset/quote at the chosen granularity; long-running at fine granularities, so pass a context with an appropriate deadline.
func (c *Client) HistorySinceInception(ctx context.Context, q HistoryQuery) (*Envelope[HistorySeries], error) {
	if q.Asset == "" {
		return nil, &APIError{Status: 400, Title: "asset required"}
	}
	v := url.Values{}
	v.Set("asset", q.Asset)
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	if q.Granularity != "" {
		v.Set("granularity", q.Granularity)
	}
	var env Envelope[HistorySeries]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/history/since-inception", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// HistoryRangeQuery is the input for [Client.History]; Base and Quote are required, From/To default server-side to `now-1h .. now`, Limit defaults to 1000 and a value outside [1, 10000] is REJECTED with a 400, and Cursor paginates.
type HistoryRangeQuery struct {
	Base   string
	Quote  string
	From   time.Time // optional
	To     time.Time // optional
	Limit  int       // optional; default 1000; outside [1, 10000] → 400
	Cursor string    // optional; opaque from prior response's Pagination.Next (Pagination is nil on the terminal page)
}

// History fetches raw trades for [Base, Quote] in the [From, To) window; walk pages while `prev.Pagination != nil` with `Cursor: prev.Pagination.Next` (the terminal page omits Pagination entirely), and flags.stale does not apply.
func (c *Client) History(ctx context.Context, q HistoryRangeQuery) (*Envelope[[]TradeRow], error) {
	if q.Base == "" {
		return nil, &APIError{Status: 400, Title: "base required"}
	}
	if q.Quote == "" {
		return nil, &APIError{Status: 400, Title: "quote required"}
	}
	v := url.Values{}
	v.Set("base", q.Base)
	v.Set("quote", q.Quote)
	if !q.From.IsZero() {
		v.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		v.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	var env Envelope[[]TradeRow]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/history", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// OHLCQuery is the input for [Client.OHLC]; Base and Quote are required (no implicit USD quote), From/To are optional and a defaulted To is clamped to a closed-bucket boundary per ADR-0015.
type OHLCQuery struct {
	Base  string
	Quote string
	From  time.Time // optional
	To    time.Time // optional
	// OutlierSigma drops trades more than N standard deviations from
	// the window mean before the bar is computed. nil = server default
	// (4σ); a pointer to 0 disables filtering.
	OutlierSigma *float64
}

// OHLC fetches one open/high/low/close bar over [From, To); if the window holds more trades than the server cap, `Truncated` is true and High/Low may not be the true extremes, and an empty window is an APIError 404 (ErrNoTrades).
func (c *Client) OHLC(ctx context.Context, q OHLCQuery) (*Envelope[OHLCBar], error) {
	if q.Base == "" {
		return nil, &APIError{Status: 400, Title: "base required"}
	}
	if q.Quote == "" {
		return nil, &APIError{Status: 400, Title: "quote required"}
	}
	v := url.Values{}
	v.Set("base", q.Base)
	v.Set("quote", q.Quote)
	if !q.From.IsZero() {
		v.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		v.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	if q.OutlierSigma != nil {
		v.Set("outlier_sigma", strconv.FormatFloat(*q.OutlierSigma, 'f', -1, 64))
	}
	var env Envelope[OHLCBar]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/ohlc", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// OHLCSeriesQuery is the input for [Client.OHLCSeries]; Base, Quote and Interval are required, From, To and Limit optional.
type OHLCSeriesQuery struct {
	Base  string
	Quote string
	// Interval is the bar width: one of 1m 5m 15m 30m 1h 2h 4h 12h 1d
	// 3d 1w 2w 1mo.
	Interval string
	From     time.Time // optional
	To       time.Time // optional
	Limit    int       // 0 → server default (100); max 1000
}

// OHLCSeries fetches a multi-bar candlestick series (/v1/ohlc with `interval`), decoded as an [OHLCSeriesResponse] rather than the single [OHLCBar] of [Client.OHLC].
func (c *Client) OHLCSeries(ctx context.Context, q OHLCSeriesQuery) (*Envelope[OHLCSeriesResponse], error) {
	if q.Base == "" {
		return nil, &APIError{Status: 400, Title: "base required"}
	}
	if q.Quote == "" {
		return nil, &APIError{Status: 400, Title: "quote required"}
	}
	if q.Interval == "" {
		return nil, &APIError{Status: 400, Title: "interval required"}
	}
	v := url.Values{}
	v.Set("base", q.Base)
	v.Set("quote", q.Quote)
	v.Set("interval", q.Interval)
	if !q.From.IsZero() {
		v.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		v.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var env Envelope[OHLCSeriesResponse]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/ohlc", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// AssetsOptions paginates the asset catalogue; empty Cursor starts at the beginning, and Pagination is nil on the terminal page, so check `prev.Pagination != nil` before reading Next.
type AssetsOptions struct {
	Cursor string
	Limit  int // 0 → server default (typically 100)
}

// Assets lists the asset catalogue, paginated.
func (c *Client) Assets(ctx context.Context, opts AssetsOptions) (*Envelope[[]AssetDetail], error) {
	v := url.Values{}
	if opts.Cursor != "" {
		v.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		v.Set("limit", strconv.Itoa(opts.Limit))
	}
	var env Envelope[[]AssetDetail]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/assets", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Asset fetches the dual-shape /v1/assets/{asset_id} response (ADR-0042) for a canonical asset_id or a catalogue slug; branch on [AssetLookup.Kind] rather than assuming the shape.
func (c *Client) Asset(ctx context.Context, assetID string) (*Envelope[AssetLookup], error) {
	if assetID == "" {
		return nil, &APIError{Status: 400, Title: "asset_id required"}
	}
	var env Envelope[AssetLookup]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/assets/"+url.PathEscape(assetID), nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// AssetMetadata fetches the SEP-1 overlay (home-domain / stellar.toml resolution) for an asset.
func (c *Client) AssetMetadata(ctx context.Context, assetID string) (*Envelope[AssetMetadata], error) {
	if assetID == "" {
		return nil, &APIError{Status: 400, Title: "asset_id required"}
	}
	var env Envelope[AssetMetadata]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/assets/"+url.PathEscape(assetID)+"/metadata", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// AssetSupplyFlows fetches the daily mint / burn / clawback series for one token; amounts are base-unit decimal strings (ADR-0003).
func (c *Client) AssetSupplyFlows(ctx context.Context, assetID string) (*Envelope[AssetSupplyFlows], error) {
	if assetID == "" {
		return nil, &APIError{Status: 400, Title: "asset_id required"}
	}
	var out Envelope[AssetSupplyFlows]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/assets/"+url.PathEscape(assetID)+"/supply/flows", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SourcesOptions filters [Client.Sources]; Class is a canonical class string (empty returns the full registry), and Include opts into request-gated columns ("stats", "sparkline", "sparkline7d"), comma-joined into `include`.
type SourcesOptions struct {
	Class   string   // optional; empty = all classes
	Include []string // optional; e.g. []string{"stats"}
}

// Sources lists the read-only source registry (venues, oracles, aggregators); catalogue-shaped, no pagination.
func (c *Client) Sources(ctx context.Context, opts SourcesOptions) (*Envelope[[]Source], error) {
	v := url.Values{}
	if opts.Class != "" {
		v.Set("class", opts.Class)
	}
	if len(opts.Include) > 0 {
		v.Set("include", strings.Join(opts.Include, ","))
	}
	var env Envelope[[]Source]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/sources", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Aggregators lists the router / aggregator-vault registry with each entry's trailing-24h routed-via trade count and USD volume; catalogue-shaped, no pagination, a rolling observation rather than a closed-bucket series.
func (c *Client) Aggregators(ctx context.Context) (*Envelope[[]AggregatorRow], error) {
	var env Envelope[[]AggregatorRow]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/aggregators", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Methodology returns a machine-readable summary of the active aggregation policy (VWAP method, outlier filters, stablecoin proxy list, source classes, venues, ADR refs); it takes no options.
func (c *Client) Methodology(ctx context.Context) (*Envelope[Methodology], error) {
	var env Envelope[Methodology]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/methodology", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// MarketsOrderBy controls server-side sort + cursor scheme.
type MarketsOrderBy string

const (
	// MarketsOrderByPair sorts by `<base>|<quote>` lex order
	// ascending. Stable for paginating the full set. NOT the
	// server default — pass it explicitly.
	MarketsOrderByPair MarketsOrderBy = "pair"
	// MarketsOrderByVolume24hUSDDesc sorts by 24h USD volume
	// descending (NULLS LAST), with `<base>|<quote>` as the
	// tie-breaker. Surfaces high-activity pairs first.
	MarketsOrderByVolume24hUSDDesc MarketsOrderBy = "volume_24h_usd_desc"
)

// MarketsOptions paginates the active-pair catalogue with the same Cursor + Limit semantics as [AssetsOptions].
type MarketsOptions struct {
	Cursor string
	Limit  int // 0 → server default (typically 100); max 500
	// OrderBy controls sort + cursor scheme. Empty means the server default, which is volume-desc,
	// not alphabetic (an alphabetical default surfaces spam tokens at the top).
	//
	// Volume-desc ranks on a mutable key, so a pair whose 24h volume changes mid-walk can be seen
	// twice or missed. For a complete enumeration pass [MarketsOrderPair] explicitly: its keyset is
	// the immutable `base|quote` with a strict `>`. The SDK never sends order_by=pair itself, so it
	// agrees with an equivalent curl and the OpenAPI default.
	OrderBy MarketsOrderBy
	// Include opts into the extra columns [Market] documents as
	// request-gated: "sparkline" populates VolumeHistory24h,
	// "inception" populates FirstTradeAt (server:
	// internal/api/v1/markets.go). Comma-joined into the `include`
	// query param; empty omits both.
	Include []string
	// Source restricts the listing to markets a single registered on-chain source or CEX venue observed
	// in the recency window; a data vendor (aggregator, FX, oracle) 400s `off-chain-source-filter`.
	// Mutually exclusive with Asset: the server 400s `unknown-source` / `invalid-asset-id` rather than
	// silently ignoring one, same posture as [PoolsQuery].
	Source string
	// Asset restricts the listing to markets where the given
	// canonical asset_id appears on either side (base or quote).
	// Mutually exclusive with Source.
	Asset string
}

// Markets lists the paginated (base, quote) pairs with at least one observed trade, each with a 24h activity summary (last trade, trade count, USD volume).
func (c *Client) Markets(ctx context.Context, opts MarketsOptions) (*Envelope[[]Market], error) {
	v := url.Values{}
	if opts.Cursor != "" {
		v.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		v.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.OrderBy != "" {
		v.Set("order_by", string(opts.OrderBy))
	}
	if len(opts.Include) > 0 {
		v.Set("include", strings.Join(opts.Include, ","))
	}
	if opts.Source != "" {
		v.Set("source", opts.Source)
	}
	if opts.Asset != "" {
		v.Set("asset", opts.Asset)
	}
	var env Envelope[[]Market]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/markets", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Pair returns the activity summary for one pair (base and quote required) as a 0-or-1-element `[]Market`, empty rather than 404 when the pair has no observed trades.
func (c *Client) Pair(ctx context.Context, base, quote string) (*Envelope[[]Market], error) {
	if base == "" {
		return nil, &APIError{Status: 400, Title: "base required"}
	}
	if quote == "" {
		return nil, &APIError{Status: 400, Title: "quote required"}
	}
	v := url.Values{}
	v.Set("base", base)
	v.Set("quote", quote)
	var env Envelope[[]Market]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/pairs", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Me returns the authenticated caller's account info; requires an API key (anonymous calls receive 401).
func (c *Client) Me(ctx context.Context) (*Envelope[Account], error) {
	var env Envelope[Account]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/account/me", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Usage returns the caller's trailing-30-day usage, one row per (date, endpoint family); where the rollup is missing the server degrades to per-day rows with empty Endpoint, and without a usage backend returns an empty array.
func (c *Client) Usage(ctx context.Context) (*Envelope[[]UsageRow], error) {
	var env Envelope[[]UsageRow]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/account/usage", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// CreateKeyRequest is the body for [Client.CreateKey]; empty Scopes mints a full-access key, non-empty Scopes only narrow (unknown values 400), IdempotencyKey is sent as `Idempotency-Key` (reuse it when retrying an unseen outcome; 409 while the original runs), and Reason is sent as `X-Reason` (required, else 400, from an OPERATOR-tier caller).
type CreateKeyRequest struct {
	Label          string   `json:"label"`
	Scopes         []string `json:"scopes,omitempty"`
	IdempotencyKey string   `json:"-"`
	Reason         string   `json:"-"`
}

// CreateKey issues a new API key inheriting the caller's identifier and tier; the plaintext secret appears in the response exactly once and is unrecoverable.
func (c *Client) CreateKey(ctx context.Context, req CreateKeyRequest) (*Envelope[KeyCreated], error) {
	if req.Label == "" {
		return nil, &APIError{Status: 400, Title: "label required"}
	}
	var env Envelope[KeyCreated]
	if err := c.doJSON(ctx, http.MethodPost, "/v1/account/keys", nil, req, &env, req.headers()); err != nil {
		return nil, err
	}
	return &env, nil
}

// headers returns the request headers CreateKey sends; unset fields
// send nothing.
func (req CreateKeyRequest) headers() map[string]string {
	h := map[string]string{}
	if req.IdempotencyKey != "" {
		h["Idempotency-Key"] = req.IdempotencyKey
	}
	if req.Reason != "" {
		h["X-Reason"] = req.Reason
	}
	return h
}

// AdminCreateKeyRequest is the body for [Client.AdminCreateKey]; Identifier, Label and Reason are required (Reason is sent as the `X-Reason` header, 400 without it), Tier is "apikey" (default) or "operator", and Account must equal <slug> for Identifier "acct:<slug>" and be empty otherwise.
type AdminCreateKeyRequest struct {
	Identifier      string   `json:"identifier"`
	Account         string   `json:"account,omitempty"`
	Label           string   `json:"label"`
	Tier            string   `json:"tier,omitempty"`
	RateLimitPerMin int      `json:"rate_limit_per_min,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	Reason          string   `json:"-"`
}

// AdminCreateKey mints an API key for ANOTHER identifier; requires an operator-tier credential (else 403), is audit-logged, and returns the plaintext exactly once.
func (c *Client) AdminCreateKey(ctx context.Context, req AdminCreateKeyRequest) (*Envelope[KeyCreated], error) {
	if req.Identifier == "" {
		return nil, &APIError{Status: 400, Title: "identifier required"}
	}
	if req.Label == "" {
		return nil, &APIError{Status: 400, Title: "label required"}
	}
	if req.Reason == "" {
		return nil, &APIError{Status: 400, Title: "reason required"}
	}
	var env Envelope[KeyCreated]
	headers := map[string]string{"X-Reason": req.Reason}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/admin/keys", nil, req, &env, headers); err != nil {
		return nil, err
	}
	return &env, nil
}

// RevokeKey permanently deletes the API key keyID (the public KeyID, NOT the secret) and returns nil on 204 even when keyID is unknown, already revoked or foreign; passing the secret is a silent no-op, and revoking the request's own credential is a 409 *APIError.
func (c *Client) RevokeKey(ctx context.Context, keyID string) error {
	if keyID == "" {
		return &APIError{Status: 400, Title: "keyID required"}
	}
	// Server returns 204 No Content on success — no envelope to
	// decode. Pass nil for the response struct so doJSON skips the
	// JSON-decode step. PathEscape the keyID so a value containing
	// reserved characters can't break out of the path segment (other
	// path-param endpoints already do this — Asset / Issuer / Changes).
	return c.doJSON(ctx, http.MethodDelete, "/v1/account/keys/"+url.PathEscape(keyID), nil, nil, nil)
}

// IssuersOptions paginates the issuer directory; Limit defaults to 100 and a value outside [1, 500] is REJECTED with a 400.
type IssuersOptions struct {
	Limit int
}

// Issuers lists every G-account that has minted at least one classic asset, ranked by total observation count; `home_domain` populates as SEP-1 resolves.
func (c *Client) Issuers(ctx context.Context, opts IssuersOptions) (*Envelope[[]IssuerListEntry], error) {
	v := url.Values{}
	if opts.Limit > 0 {
		v.Set("limit", strconv.Itoa(opts.Limit))
	}
	var env Envelope[[]IssuerListEntry]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/issuers", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Issuer returns the per-issuer detail (auth flags, SEP-1 metadata, assets); a G-strkey never observed as an issuer is a 404 [APIError].
func (c *Client) Issuer(ctx context.Context, gStrkey string) (*Envelope[Issuer], error) {
	if gStrkey == "" {
		return nil, &APIError{Status: 400, Title: "g_strkey required"}
	}
	var env Envelope[Issuer]
	path := "/v1/issuers/" + url.PathEscape(gStrkey)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Cursors returns the per-source ingest cursor table (operator diagnostic) with server-computed `lag_seconds`; abandoned cursors are omitted (this method never requests them) and it pages at 500 rows.
func (c *Client) Cursors(ctx context.Context) (*Envelope[[]Cursor], error) {
	var env Envelope[[]Cursor]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/diagnostics/cursors", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Keys lists the caller's API keys by CreatedAt ascending; anonymous calls get 401 and the plaintext is NEVER returned (rotate via [Client.CreateKey] then revoke).
func (c *Client) Keys(ctx context.Context) (*Envelope[[]Account], error) {
	var env Envelope[[]Account]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/account/keys", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Status returns the customer-facing health rollup from `/v1/status`; it always returns 200 (degraded state is in [Status.Overall]), and without a Prometheus backend only the in-process surface is returned with [Envelope.Flags.Stale] true.
func (c *Client) Status(ctx context.Context) (*Envelope[Status], error) {
	var env Envelope[Status]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/status", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Healthz is the shallow liveness probe (200 while the API process is up, no dependency checks); use [Client.Readyz] to verify dependencies.
func (c *Client) Healthz(ctx context.Context) (*Envelope[Health], error) {
	var env Envelope[Health]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/healthz", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Readyz is the deep readiness probe: 200 only if every dependency check responds within a 2 s server budget, otherwise an [APIError] with Status=503 and [Envelope.Flags.Stale] true.
func (c *Client) Readyz(ctx context.Context) (*Envelope[Health], error) {
	var env Envelope[Health]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/readyz", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Version reports the build metadata (git SHA, build date, Go version) of the API binary serving the request.
func (c *Client) Version(ctx context.Context) (*Envelope[Version], error) {
	var env Envelope[Version]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/version", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// ChartQuery selects the asset / quote and the chart timeframe + granularity; Asset is required.
type ChartQuery struct {
	Asset string
	Quote string // optional; defaults to fiat:USD server-side
	// Timeframe is 1h / 24h / 1w / 1mo / 1y / all; default 24h. Any
	// other token is a 400 — the previous comment here advertised
	// 7d / 30d / 90d, which the server has never accepted.
	Timeframe string
	// Granularity is 1m / 15m / 1h / 4h / 1d / 1w / 1mo; the default tracks Timeframe. The server may
	// serve a COARSER width when the grid exceeds its 50,000-bucket response cap (1y + 1m is served at
	// 15m); read [ChartSeries.Granularity] for the width actually served. 1m may also be bounded to the
	// last 90 days on a given deployment; coarser widths are never bounded.
	Granularity string
}

// Chart returns the binned price + USD-volume series trimmed to a caller-chosen window, with a server-default granularity per timeframe (24h 15m, 1w 1h, 1mo 4h, 1y 1d, all 1d).
func (c *Client) Chart(ctx context.Context, q ChartQuery) (*Envelope[ChartSeries], error) {
	if q.Asset == "" {
		return nil, &APIError{Status: 400, Title: "asset required"}
	}
	v := url.Values{}
	v.Set("asset", q.Asset)
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	if q.Timeframe != "" {
		v.Set("timeframe", q.Timeframe)
	}
	if q.Granularity != "" {
		v.Set("granularity", q.Granularity)
	}
	var env Envelope[ChartSeries]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/chart", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// NetworkStats fetches the home-page aggregate snapshot (24h USD volume, active markets, indexed assets, latest ledger, source counts); Volume24hUSD is a *string per ADR-0003, nil when no USD-equivalent trades landed in 24h.
func (c *Client) NetworkStats(ctx context.Context) (*Envelope[NetworkStats], error) {
	var env Envelope[NetworkStats]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/network/stats", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// ObservationsQuery is the input for [Client.Observations]; Asset is required, Quote defaults to fiat:USD, Source restricts to one on-chain source or CEX venue (a data vendor 400s), and Aggregate="latest" collapses to the most recent.
type ObservationsQuery struct {
	Asset     string
	Quote     string // optional
	Source    string // optional
	Aggregate string // optional; "latest" supported
}

// Observations returns one raw row per source that recorded a trade on (asset, quote) per ADR-0018; empty array (NOT 404) when none, and flags.stale is always false.
func (c *Client) Observations(ctx context.Context, q ObservationsQuery) (*Envelope[[]TradeRow], error) {
	if q.Asset == "" {
		return nil, &APIError{Status: 400, Title: "asset required"}
	}
	v := url.Values{}
	v.Set("asset", q.Asset)
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	if q.Source != "" {
		v.Set("source", q.Source)
	}
	if q.Aggregate != "" {
		v.Set("aggregate", q.Aggregate)
	}
	var env Envelope[[]TradeRow]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/observations", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// ChangeSummaryQuery selects the entity for [Client.ChangeSummary]; both fields are required, EntityType is "coin" or "pair" (anything else is a 400), and EntityID is the raw id (e.g. "crypto:XLM/fiat:USD"), percent-encoded into one path segment.
type ChangeSummaryQuery struct {
	EntityType string
	EntityID   string
}

// ChangeSummary returns the per-entity 1h/24h/7d/30d delta rollup plus ATH/ATL and streak markers (refreshed every 5 min); an entity with no published VWAP has no row and returns 404.
func (c *Client) ChangeSummary(ctx context.Context, q ChangeSummaryQuery) (*Envelope[ChangeSummary], error) {
	if q.EntityType == "" {
		return nil, &APIError{Status: 400, Title: "entity_type required"}
	}
	if q.EntityID == "" {
		return nil, &APIError{Status: 400, Title: "entity_id required"}
	}
	var env Envelope[ChangeSummary]
	path := "/v1/changes/" + url.PathEscape(q.EntityType) + "/" + url.PathEscape(q.EntityID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// Incidents fetches the customer-facing incident posts embedded in the API binary, newest started_at first; the corpus is per-deploy, so regions on different builds may differ (diff by Slug).
func (c *Client) Incidents(ctx context.Context) (*Envelope[IncidentsList], error) {
	var env Envelope[IncidentsList]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/incidents", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// RWAAssets returns the tokenized real-world-asset set, its aggregates and membership rule (GET /v1/rwa/assets); read Valuation.Status before any money field, as only "published" carries a number and withheld values are absent, not zero.
func (c *Client) RWAAssets(ctx context.Context) (*Envelope[RWAAssetsView], error) {
	var env Envelope[RWAAssetsView]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/rwa/assets", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// RWAHistoryOptions windows and decomposes the RWA value series; Timeframe defaults to "1y" and must be 1w/1mo/1y/all (sub-daily tokens get a 400), and GroupBy defaults to "none" ("asset" and "issuer" fill Groups).
type RWAHistoryOptions struct {
	Timeframe string
	GroupBy   string
}

// RWAHistory returns the RWA set valued daily on the reference basis (GET /v1/rwa/history), NOT a market capitalisation; a day with no valuable member is ABSENT, and a partial day carries LowerBound with AssetsValued / AssetsUnvalued.
func (c *Client) RWAHistory(ctx context.Context, opts RWAHistoryOptions) (*Envelope[RWAHistoryView], error) {
	v := url.Values{}
	if opts.Timeframe != "" {
		v.Set("timeframe", opts.Timeframe)
	}
	if opts.GroupBy != "" {
		v.Set("group_by", opts.GroupBy)
	}
	var env Envelope[RWAHistoryView]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/rwa/history", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// RWAPremiumHistoryOptions configures [Client.RWAPremiumHistory]; Timeframe is one of "1w", "1mo", "1y", "all" (default "1y").
type RWAPremiumHistoryOptions struct {
	Timeframe string
}

// RWAPremiumHistory returns each comparable RWA token's daily premium or discount to published NAV (GET /v1/rwa/premium); a day missing either sampled leg is ABSENT rather than carried, and thin-market days are counted in MarketWithheldDays.
func (c *Client) RWAPremiumHistory(ctx context.Context, opts RWAPremiumHistoryOptions) (*Envelope[RWAPremiumHistoryView], error) {
	v := url.Values{}
	if opts.Timeframe != "" {
		v.Set("timeframe", opts.Timeframe)
	}
	var env Envelope[RWAPremiumHistoryView]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/rwa/premium", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

func (c *Client) SACWrappers(ctx context.Context) (*Envelope[map[string]string], error) {
	var env Envelope[map[string]string]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/sac-wrappers", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// LendingPools fetches every Blend pool observed in the trailing 7d auction stream (GET /v1/lending/pools), sorted by auction count descending; the SDK ignores unknown fields, so new server fields are non-breaking.
func (c *Client) LendingPools(ctx context.Context) (*Envelope[[]LendingPool], error) {
	var env Envelope[[]LendingPool]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/lending/pools", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// AggregateQuery is the input for [Client.VWAP] and [Client.TWAP]; Base and Quote are required, From/To default to a 1-hour window ending at the previous closed-bucket boundary (ADR-0015), and OutlierSigma is forwarded to VWAP only (zero disables the filter).
type AggregateQuery struct {
	Base         string
	Quote        string
	From         time.Time
	To           time.Time
	OutlierSigma float64
}

// VWAP fetches the volume-weighted average price over [from, to) from raw trades (GET /v1/vwap), unlike the closed-bucket [Client.Price]; Truncated=true means the window exceeded the server's 10000-trade cap, so narrow it.
func (c *Client) VWAP(ctx context.Context, q AggregateQuery) (*Envelope[VWAPResult], error) {
	if q.Base == "" {
		return nil, &APIError{Status: 400, Title: "base required"}
	}
	if q.Quote == "" {
		return nil, &APIError{Status: 400, Title: "quote required"}
	}
	v := url.Values{}
	v.Set("base", q.Base)
	v.Set("quote", q.Quote)
	if !q.From.IsZero() {
		v.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		v.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	if q.OutlierSigma > 0 {
		v.Set("outlier_sigma", strconv.FormatFloat(q.OutlierSigma, 'f', -1, 64))
	}
	var env Envelope[VWAPResult]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/vwap", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// TWAP fetches the time-weighted average price over [from, to) (GET /v1/twap); the server filters outliers at its default sigma first, and AggregateQuery.OutlierSigma is not forwarded.
func (c *Client) TWAP(ctx context.Context, q AggregateQuery) (*Envelope[TWAPResult], error) {
	if q.Base == "" {
		return nil, &APIError{Status: 400, Title: "base required"}
	}
	if q.Quote == "" {
		return nil, &APIError{Status: 400, Title: "quote required"}
	}
	v := url.Values{}
	v.Set("base", q.Base)
	v.Set("quote", q.Quote)
	if !q.From.IsZero() {
		v.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		v.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	var env Envelope[TWAPResult]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/twap", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// PoolsQuery is the input for [Client.Pools]; all fields are optional: Source narrows to one DEX, Base + Quote pin a pair, Asset matches either side (mutually exclusive with Base/Quote), Cursor + Limit paginate.
type PoolsQuery struct {
	Source string
	Base   string
	Quote  string
	Asset  string
	Cursor string
	Limit  int
	// OrderBy: "" or "volume_24h_usd_desc" (default), or "pair".
	OrderBy string
}

// Pools returns DEX/AMM liquidity pools, one row per (source, base, quote) (GET /v1/pools), unlike [Client.Markets] which collapses across sources; Pagination is nil on the terminal page.
func (c *Client) Pools(ctx context.Context, q PoolsQuery) (*Envelope[[]Pool], error) {
	v := url.Values{}
	if q.Source != "" {
		v.Set("source", q.Source)
	}
	if q.Base != "" {
		v.Set("base", q.Base)
	}
	if q.Quote != "" {
		v.Set("quote", q.Quote)
	}
	if q.Asset != "" {
		v.Set("asset", q.Asset)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.OrderBy != "" {
		v.Set("order_by", q.OrderBy)
	}
	var env Envelope[[]Pool]
	if err := c.doJSON(ctx, http.MethodGet, "/v1/pools", v, nil, &env); err != nil {
		return nil, err
	}
	return &env, nil
}
