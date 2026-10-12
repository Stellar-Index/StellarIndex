package divergence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	externalcoingecko "github.com/Stellar-Index/StellarIndex/internal/sources/external/coingecko"
)

// CoinGeckoReference looks up prices via /api/v3/simple/price with the ingest poller's Pro/Demo keys, best-effort (a 429 is ErrPriceUnavailable); one batched request per tick covers EVERY configured pair and is cached for batchTTL.
type CoinGeckoReference struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	demoAPIKey string
	logger     *slog.Logger

	// idMap maps canonical asset_id strings to CoinGecko's own
	// asset slugs (e.g. "native" → "stellar"). Operator-curated;
	// any asset not in the map yields ErrAssetUnsupported.
	idMap map[string]string

	// quoteMap maps canonical quote currency to CoinGecko's
	// supported vs_currency code (e.g. "fiat:USD" → "usd",
	// "fiat:EUR" → "eur"). Limited set; CoinGecko supports the
	// common fiats + a few major cryptos.
	quoteMap map[string]string

	// batchTTL is the window over which a single batched
	// /simple/price response is reused. Default 25s — short enough
	// to capture price moves on the next tick, long enough that
	// the per-tick fan-out across pairs reuses one HTTP call.
	batchTTL time.Duration

	// maxAge is the staleness ceiling for upstream last_updated_at: an older quote is ErrPriceUnavailable, since a FROZEN price served as fresh can mask or fabricate a divergence.
	// See defaultCoinGeckoMaxAge.
	maxAge time.Duration

	// nowFn returns the current time. Hookable for tests so the
	// TTL-window logic is deterministic. Production uses time.Now.
	nowFn func() time.Time

	// batchMu guards the per-tick batched response cache.
	batchMu sync.Mutex

	// batchAt is when batchData was fetched. Zero = no cache yet.
	batchAt time.Time

	// batchData is the parsed response: cgID → cgQuote → price.
	// Nil/empty until the first successful fetch.
	batchData map[string]map[string]float64

	// batchUpdatedAt maps cgID to CoinGecko's upstream last_updated_at; an id missing from it is REJECTED by the staleness gate, since every request asks for the field and freshness is unverifiable without it.
	batchUpdatedAt map[string]time.Time
}

// CoinGeckoOptions configures [NewCoinGeckoReference].
type CoinGeckoOptions struct {
	// HTTPClient — nil falls back to a 10s-timeout client. One without a
	// CheckRedirect is used with [httpx.KeyedSameOriginRedirect] added.
	HTTPClient *http.Client

	// BaseURL overrides the API base. Empty defaults to the public host's
	// /api/v3, or the Pro host's when APIKey is set. An explicit BaseURL
	// always wins. Tests pass an httptest.Server URL.
	BaseURL string

	// APIKey (Pro) and DemoAPIKey are sent per
	// [externalcoingecko.SetAuthHeader]; both empty = keyless.
	APIKey     string
	DemoAPIKey string

	// Logger receives the per-batch HTTP-status warning. nil → slog.Default.
	Logger *slog.Logger

	// IDMap maps canonical asset_id → CoinGecko slug. At minimum
	// the operator should provide entries for every base asset
	// the aggregator publishes prices for. Empty map yields
	// ErrAssetUnsupported on every lookup.
	IDMap map[string]string

	// QuoteMap maps canonical quote string → CoinGecko vs_currency.
	// Empty falls back to a small built-in default covering
	// fiat:USD/EUR/GBP/JPY + crypto:BTC/ETH.
	QuoteMap map[string]string

	// BatchTTL is how long one batched /simple/price response is reused across lookups; default 25s (under the 30s tick). Shorter is fresher but costs HTTP calls.
	BatchTTL time.Duration

	// MaxAge is the staleness ceiling for a quote's upstream
	// last_updated_at. <= 0 falls back to [defaultCoinGeckoMaxAge].
	// A quote older than this is rejected as ErrPriceUnavailable so a
	// frozen upstream can't silently drive the divergence signal.
	MaxAge time.Duration

	// NowFn overrides time.Now for deterministic tests. Nil = time.Now.
	NowFn func() time.Time
}

// defaultCoinGeckoMaxAge (30m, about five refresh cycles plus slack, like the Reflector default) exists to catch a FROZEN feed, not normal jitter. An engineering default, not a business rule.
const defaultCoinGeckoMaxAge = 30 * time.Minute

// coinGeckoLastUpdatedKey is the sentinel CoinGecko folds into each id's object under include_last_updated_at=true; it is not a vs_currency, so [fetchBatch] lifts it out of the price map.
const coinGeckoLastUpdatedKey = "last_updated_at"

// NewCoinGeckoReference constructs a CoinGecko-backed reference; an empty opts.IDMap falls back to a built-in default, and operator entries merge OVER the defaults (never removed by omission).
func NewCoinGeckoReference(opts CoinGeckoOptions) *CoinGeckoReference {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = externalcoingecko.ResolveEndpoint("", opts.APIKey) + "/api/v3"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	idMap := defaultCoinGeckoIDMap()
	for k, v := range opts.IDMap {
		idMap[k] = v
	}
	quoteMap := opts.QuoteMap
	if len(quoteMap) == 0 {
		quoteMap = defaultCoinGeckoQuoteMap()
	}
	batchTTL := opts.BatchTTL
	if batchTTL <= 0 {
		batchTTL = 25 * time.Second
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = defaultCoinGeckoMaxAge
	}
	nowFn := opts.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}

	return &CoinGeckoReference{
		httpClient: keyedCoinGeckoClient("coingecko", opts.HTTPClient),
		baseURL:    baseURL,
		apiKey:     opts.APIKey,
		demoAPIKey: opts.DemoAPIKey,
		logger:     logger,
		idMap:      idMap,
		quoteMap:   quoteMap,
		batchTTL:   batchTTL,
		maxAge:     maxAge,
		nowFn:      nowFn,
	}
}

// defaultCoinGeckoIDMap covers the aggregator's default asset_id forms (XLM in every alias form, BTC, ETH, major USD stablecoins per ADR-0026).
// It mirrors the poller's `tickerToID` map but keys on canonical asset_id strings (`crypto:XLM`, `native`), not bare tickers; slugs verified against /coins/list.
func defaultCoinGeckoIDMap() map[string]string {
	m := map[string]string{
		"crypto:BTC":  "bitcoin",
		"crypto:ETH":  "ethereum",
		"crypto:LINK": "chainlink",
		"crypto:SOL":  "solana",
		"crypto:ADA":  "cardano",
		"crypto:DOT":  "polkadot",
		// Major USD stablecoins — useful when the aggregator's
		// stablecoin-fiat proxy (ADR-0026) is on and we want to
		// cross-check the underlying X/USDC or X/USDT path.
		"crypto:USDC":  "usd-coin",
		"crypto:USDT":  "tether",
		"crypto:PYUSD": "paypal-usd",
	}
	// Every XLM alias form, the SAC included: they share one price.
	for _, form := range canonical.AssetAliasStrings(canonical.NativeAsset()) {
		m[form] = "stellar"
	}
	return m
}

// Name implements [Reference].
func (c *CoinGeckoReference) Name() string { return "coingecko" }

// LookupQuote implements Reference with AsOf the id's last_updated_at; a quote older than maxAge against observedAt, or missing the timestamp, reads "unavailable", never agreement or divergence.
func (c *CoinGeckoReference) LookupQuote(ctx context.Context, pair canonical.Pair, observedAt time.Time) (Quote, error) {
	cgID, ok := c.idMap[pair.Base.String()]
	if !ok {
		return Quote{}, fmt.Errorf("%w: base %q has no CoinGecko slug in idMap", ErrAssetUnsupported, pair.Base.String())
	}
	cgQuote, ok := c.quoteMap[pair.Quote.String()]
	if !ok {
		return Quote{}, fmt.Errorf("%w: quote %q has no CoinGecko vs_currency", ErrAssetUnsupported, pair.Quote.String())
	}

	data, updatedAt, err := c.ensureBatch(ctx)
	if err != nil {
		return Quote{}, err
	}

	idEntry, ok := data[cgID]
	if !ok {
		return Quote{}, fmt.Errorf("%w: coingecko id %q absent in response", ErrAssetUnsupported, cgID)
	}
	price, ok := idEntry[cgQuote]
	if !ok {
		return Quote{}, fmt.Errorf("%w: coingecko vs_currency %q absent for id %q", ErrAssetUnsupported, cgQuote, cgID)
	}
	if !isFinitePositive(price) {
		return Quote{}, fmt.Errorf("%w: coingecko returned non-positive price %g", ErrPriceUnavailable, price)
	}
	if err := c.staleness(cgID, updatedAt, observedAt); err != nil {
		return Quote{}, err
	}
	return Quote{Price: price, AsOf: updatedAt[cgID]}, nil
}

// staleness applies the gate to one id's last_updated_at (asOf defaults to wall time when observedAt is zero). A MISSING timestamp is rejected, not waved through: every request sets
// include_last_updated_at=true, so absence means the contract the gate rests on was not honoured, and failing open would silently serve a possibly-frozen price as fresh.
// Failing closed costs one reference for the tick (visible as price_unavailable in Result.Failures).
func (c *CoinGeckoReference) staleness(cgID string, updatedAt map[string]time.Time, observedAt time.Time) error {
	ts, ok := updatedAt[cgID]
	if !ok || ts.IsZero() {
		return fmt.Errorf("%w: coingecko id %q returned no last_updated_at (freshness unverifiable)",
			ErrPriceUnavailable, cgID)
	}
	asOf := observedAt
	if asOf.IsZero() {
		asOf = c.nowFn().UTC()
	}
	if age := asOf.Sub(ts); age > c.maxAge {
		return fmt.Errorf("%w: coingecko id %q last_updated_at is stale (updated %s ago, max %s)",
			ErrPriceUnavailable, cgID, age.Truncate(time.Second), c.maxAge)
	}
	return nil
}

// ensureBatch returns the cached batch within the TTL, else fetches once, parses and caches; concurrent calls coalesce on batchMu so a second caller reuses the first's response.
func (c *CoinGeckoReference) ensureBatch(ctx context.Context) (map[string]map[string]float64, map[string]time.Time, error) {
	c.batchMu.Lock()
	defer c.batchMu.Unlock()

	now := c.nowFn()
	if c.batchData != nil && now.Sub(c.batchAt) < c.batchTTL {
		return c.batchData, c.batchUpdatedAt, nil
	}

	ids := sortedValues(c.idMap)
	quotes := sortedValues(c.quoteMap)
	if len(ids) == 0 || len(quotes) == 0 {
		return nil, nil, fmt.Errorf("%w: coingecko id/quote map empty", ErrAssetUnsupported)
	}

	data, updatedAt, err := c.fetchBatch(ctx, ids, quotes)
	if err != nil {
		return nil, nil, err
	}
	c.batchData = data
	c.batchUpdatedAt = updatedAt
	c.batchAt = now
	return data, updatedAt, nil
}

// fetchBatch issues one GET against /simple/price for every (id, quote) pair and lifts each id's `last_updated_at` out of the price map into a parallel cgID to time map for the staleness gate.
func (c *CoinGeckoReference) fetchBatch(ctx context.Context, ids, quotes []string) (map[string]map[string]float64, map[string]time.Time, error) {
	v := url.Values{}
	v.Set("ids", strings.Join(ids, ","))
	v.Set("vs_currencies", strings.Join(quotes, ","))
	// Ask CoinGecko to stamp each coin's upstream publication
	// time so the staleness gate can reject a frozen feed.
	v.Set("include_last_updated_at", "true")
	endpoint := c.baseURL + "/simple/price?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("coingecko: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "stellarindex-divergence/0.1")
	externalcoingecko.SetAuthHeader(req.Header, c.apiKey, c.demoAPIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("coingecko: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, nil, fmt.Errorf("%w: coingecko rate-limited (HTTP 429)", ErrPriceUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		// The Result outcome only says "error"; the status is what tells an
		// auth failure from an outage. Path only: the query is not needed.
		c.logger.Warn("divergence: coingecko batch fetch failed",
			"status", resp.StatusCode, "path", req.URL.Path)
		return nil, nil, fmt.Errorf("coingecko: HTTP %d", resp.StatusCode)
	}

	// Cap response size — /simple/price for ~12 ids × ~12 quotes
	// is well under 16 KiB. Bound at 256 KiB to give headroom for
	// future quote-set growth while still rejecting runaway bodies.
	const maxBody = 256 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, fmt.Errorf("coingecko: read body: %w", err)
	}

	// Response shape (include_last_updated_at=true):
	//   {"<id>": {"<vs_currency>": <price>, ..., "last_updated_at": <unix>}}
	var parsed map[string]map[string]float64
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("coingecko: decode: %w", err)
	}
	if len(parsed) == 0 {
		return nil, nil, errors.New("coingecko: empty response body")
	}

	// Lift last_updated_at out of the price object so it is not mistaken for a vs_currency; a missing/zero/negative value leaves the id out of updatedAt, so the gate REJECTS it (see staleness).
	updatedAt := make(map[string]time.Time, len(parsed))
	for id, entry := range parsed {
		if raw, ok := entry[coinGeckoLastUpdatedKey]; ok {
			delete(entry, coinGeckoLastUpdatedKey)
			if sec := int64(raw); sec > 0 {
				updatedAt[id] = time.Unix(sec, 0).UTC()
			}
		}
	}
	return parsed, updatedAt, nil
}

// sortedValues returns the unique values of m in sorted order. Sort
// is for deterministic URL composition (helps with caching layers
// and test assertions); de-dup handles the `crypto:XLM` + `native`
// both-map-to-`stellar` case so we don't pay for the same id twice.
func sortedValues(m map[string]string) []string {
	seen := make(map[string]struct{}, len(m))
	out := make([]string, 0, len(m))
	for _, v := range m {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// defaultCoinGeckoQuoteMap covers the fiat/crypto pairs we
// commonly serve. Operator can override via [CoinGeckoOptions.QuoteMap].
func defaultCoinGeckoQuoteMap() map[string]string {
	return map[string]string{
		"fiat:USD":   "usd",
		"fiat:EUR":   "eur",
		"fiat:GBP":   "gbp",
		"fiat:JPY":   "jpy",
		"fiat:CHF":   "chf",
		"fiat:AUD":   "aud",
		"fiat:CAD":   "cad",
		"fiat:CNY":   "cny",
		"fiat:KRW":   "krw",
		"fiat:INR":   "inr",
		"crypto:BTC": "btc",
		"crypto:ETH": "eth",
	}
}

// Compile-time check.
var _ Reference = (*CoinGeckoReference)(nil)
