package divergence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// CoinGeckoReference looks up prices via CoinGecko's public
// /api/v3/simple/price endpoint. Free tier has no API key but a
// modest rate limit (~30 req/min); the reference is best-effort —
// transient 429s bubble up as transport failures and the caller
// just treats this run's CoinGecko response as missing.
//
// The reference batches per-tick lookups: the first LookupPrice in
// a tick burst issues a single `/simple/price?ids=A,B,C&vs_currencies=usd,eur`
// covering EVERY configured (id, quote) pair, then caches the
// response for [batchTTL]. Subsequent LookupPrice calls within the
// TTL hit the cache and issue zero HTTP requests. With the default
// 25s TTL and the orchestrator's 30s tick, each tick produces ONE
// HTTP call regardless of how many pairs the operator has
// configured — down from one-per-pair in the original implementation.
// F-0030 follow-up: at 9 default pairs the prior shape was
// ~25,920 calls/day (9 × 2 ticks/min × 1440 min); batched is ~2,880
// (1 × 2 × 1440), well inside the demo-tier 10K daily limit.
type CoinGeckoReference struct {
	httpClient *http.Client
	baseURL    string

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

	// maxAge is the CS-089 staleness ceiling: a quote whose upstream
	// last_updated_at is older than this (relative to the
	// comparison's observedAt) is rejected as ErrPriceUnavailable
	// instead of served as a fresh reference. A FROZEN CoinGecko
	// price served as fresh can both mask a real divergence and
	// fabricate a false one — the same failure mode the Chainlink
	// (chainlink.go) and on-chain oracle (oracle.go) references gate
	// against. See defaultCoinGeckoMaxAge for the default rationale.
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

	// batchUpdatedAt maps cgID → the UPSTREAM publication time
	// CoinGecko reports for that coin (the `last_updated_at` field
	// returned when the request sets include_last_updated_at=true).
	// Absent from the map when CoinGecko omitted the field for an id
	// (older/compat endpoints); the staleness gate then no-ops for
	// that id rather than rejecting an otherwise-usable price.
	batchUpdatedAt map[string]time.Time
}

// CoinGeckoOptions configures [NewCoinGeckoReference].
type CoinGeckoOptions struct {
	// HTTPClient — nil falls back to a 10s-timeout client.
	HTTPClient *http.Client

	// BaseURL overrides the API base. Empty defaults to
	// "https://api.coingecko.com/api/v3". Tests pass an
	// httptest.Server URL.
	BaseURL string

	// IDMap maps canonical asset_id → CoinGecko slug. At minimum
	// the operator should provide entries for every base asset
	// the aggregator publishes prices for. Empty map yields
	// ErrAssetUnsupported on every lookup.
	IDMap map[string]string

	// QuoteMap maps canonical quote string → CoinGecko vs_currency.
	// Empty falls back to a small built-in default covering
	// fiat:USD/EUR/GBP/JPY + crypto:BTC/ETH.
	QuoteMap map[string]string

	// BatchTTL is the window over which a single batched
	// /simple/price response is reused across per-pair LookupPrice
	// calls. Default 25s (less than the orchestrator's 30s tick so
	// each tick triggers a fresh fetch). Operators can set it
	// shorter for higher freshness at the cost of more HTTP calls,
	// or longer for paranoid rate-limit conservation.
	BatchTTL time.Duration

	// MaxAge is the CS-089 staleness ceiling for a quote's upstream
	// last_updated_at. <= 0 falls back to [defaultCoinGeckoMaxAge].
	// A quote older than this is rejected as ErrPriceUnavailable so a
	// frozen upstream can't silently drive the divergence signal.
	MaxAge time.Duration

	// NowFn overrides time.Now for deterministic tests. Nil = time.Now.
	NowFn func() time.Time
}

// defaultCoinGeckoMaxAge is the staleness ceiling for a CoinGecko
// /simple/price quote's upstream last_updated_at (CS-089). CoinGecko
// refreshes /simple/price on the order of every 1–5 minutes for
// liquid coins; 30m means "missed roughly five refresh cycles + slack"
// — the same shape as the Reflector on-chain default
// (DefaultOracleMaxAgeReflector). It exists to catch a genuinely
// FROZEN feed (last_updated_at stops advancing for hours), not to
// second-guess normal update jitter.
//
// VALUE DECISION: 30m is a defensible engineering default, not a
// business rule. Operators tune it via
// `[divergence.coingecko].max_age_minutes`.
const defaultCoinGeckoMaxAge = 30 * time.Minute

// coinGeckoLastUpdatedKey is the sentinel key CoinGecko folds into
// each id's object when the request sets include_last_updated_at=true
// (`{"stellar":{"usd":0.12,"last_updated_at":1710000000}}`). It is NOT
// a vs_currency, so [fetchBatch] lifts it out of the price map into
// the per-id timestamp map before the quote lookup runs.
const coinGeckoLastUpdatedKey = "last_updated_at"

// NewCoinGeckoReference constructs a CoinGecko-backed reference.
//
// When opts.IDMap is empty, the reference falls back to a built-in
// default that covers the canonical asset_id forms the aggregator
// computes by default (XLM in both `crypto:XLM` and `native` forms,
// BTC, ETH, LINK, plus the major USD stablecoins). Without this
// fallback every divergence-cross-check call returns
// `ErrAssetUnsupported` and `divergence_observations` stays empty
// for any operator who hasn't manually populated `[divergence.coingecko].id_map`
// — which the type-level docs already promised wouldn't happen.
// Operator-supplied entries merge OVER the defaults (operator wins),
// so an operator can still narrow the set or override a slug.
func NewCoinGeckoReference(opts CoinGeckoOptions) *CoinGeckoReference {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = "https://api.coingecko.com/api/v3"
	}
	baseURL = strings.TrimRight(baseURL, "/")

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
		httpClient: httpClient,
		baseURL:    baseURL,
		idMap:      idMap,
		quoteMap:   quoteMap,
		batchTTL:   batchTTL,
		maxAge:     maxAge,
		nowFn:      nowFn,
	}
}

// defaultCoinGeckoIDMap covers the canonical asset_id forms the
// aggregator computes by default (per cmd/stellarindex-aggregator/
// main.go::defaultPairs — XLM/BTC/ETH × USD/EUR/GBP, with XLM in
// both `crypto:XLM` and `native` forms). Major USD stablecoins are
// included so a deployment with stablecoin-fiat-proxy enabled
// (ADR-0026) can cross-check the underlying USDC/USDT path too.
//
// Slugs verified against https://api.coingecko.com/api/v3/coins/list.
// Mirrors the per-source coingecko poller's `tickerToID` map
// (internal/sources/external/coingecko/poller.go) — kept separate
// here because the divergence path keys on canonical asset_id
// strings (`crypto:XLM`, `native`) while the poller keys on bare
// upper-case tickers.
func defaultCoinGeckoIDMap() map[string]string {
	return map[string]string{
		"crypto:XLM":  "stellar",
		"native":      "stellar",
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
}

// Name implements [Reference].
func (c *CoinGeckoReference) Name() string { return "coingecko" }

// LookupPrice implements [Reference].
//
// CS-089 staleness gate: CoinGecko's /simple/price returns the latest
// CACHED price, so a frozen upstream keeps serving a stale number with
// no error. We request include_last_updated_at=true and reject any
// quote whose upstream last_updated_at is older than [maxAge] relative
// to observedAt (the bucket-end comparison time Compare passes
// through; zero falls back to wall time defensively) — mirroring the
// Chainlink and on-chain oracle references so a stale reference reads
// as "unavailable", never as agreement or divergence. When CoinGecko
// omits last_updated_at for the id (older/compat endpoints) the gate
// no-ops for that id.
//
// Internally this delegates to the per-tick batched fetcher: the
// first call within batchTTL issues ONE HTTP request covering every
// configured (id, quote) pair; subsequent calls within the TTL read
// from the in-memory map without touching the network.
func (c *CoinGeckoReference) LookupPrice(ctx context.Context, pair canonical.Pair, observedAt time.Time) (float64, error) {
	cgID, ok := c.idMap[pair.Base.String()]
	if !ok {
		return 0, fmt.Errorf("%w: base %q has no CoinGecko slug in idMap", ErrAssetUnsupported, pair.Base.String())
	}
	cgQuote, ok := c.quoteMap[pair.Quote.String()]
	if !ok {
		return 0, fmt.Errorf("%w: quote %q has no CoinGecko vs_currency", ErrAssetUnsupported, pair.Quote.String())
	}

	data, updatedAt, err := c.ensureBatch(ctx)
	if err != nil {
		return 0, err
	}

	idEntry, ok := data[cgID]
	if !ok {
		return 0, fmt.Errorf("%w: coingecko id %q absent in response", ErrAssetUnsupported, cgID)
	}
	price, ok := idEntry[cgQuote]
	if !ok {
		return 0, fmt.Errorf("%w: coingecko vs_currency %q absent for id %q", ErrAssetUnsupported, cgQuote, cgID)
	}
	if !isFinitePositive(price) {
		return 0, fmt.Errorf("%w: coingecko returned non-positive price %g", ErrPriceUnavailable, price)
	}
	if err := c.staleness(cgID, updatedAt, observedAt); err != nil {
		return 0, err
	}
	return price, nil
}

// staleness enforces the CS-089 gate for one id's upstream
// last_updated_at. asOf defaults to wall time when observedAt is zero
// (matching chainlink.go / oracle.go). A missing upstream timestamp
// no-ops (nil) — we only reject a price we can PROVE is stale, never
// one whose freshness the upstream simply didn't report.
func (c *CoinGeckoReference) staleness(cgID string, updatedAt map[string]time.Time, observedAt time.Time) error {
	ts, ok := updatedAt[cgID]
	if !ok || ts.IsZero() {
		return nil
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

// LookupPrices returns the CoinGecko-reported price for each pair in
// a single batched fetch. Missing assets / quotes / non-finite
// prices are simply absent from the returned map (the caller can
// detect a per-pair miss by absence). Transport-level failures
// (network error, HTTP 429, malformed JSON) surface as a non-nil
// error and an empty/partial map.
//
// Use this when you have a known pair set up-front and want to
// avoid the per-pair LookupPrice indirection. The underlying HTTP
// call is identical to what LookupPrice triggers on cache miss; the
// per-pair LookupPrice path remains available for the [Reference]
// interface contract.
func (c *CoinGeckoReference) LookupPrices(ctx context.Context, pairs []canonical.Pair) (map[canonical.Pair]float64, error) {
	out := make(map[canonical.Pair]float64, len(pairs))
	if len(pairs) == 0 {
		return out, nil
	}
	data, updatedAt, err := c.ensureBatch(ctx)
	if err != nil {
		return out, err
	}
	// No per-pair observedAt on the batch path — gate against wall
	// time (staleness passes zero, which falls back to nowFn).
	for _, p := range pairs {
		cgID, ok := c.idMap[p.Base.String()]
		if !ok {
			continue
		}
		cgQuote, ok := c.quoteMap[p.Quote.String()]
		if !ok {
			continue
		}
		idEntry, ok := data[cgID]
		if !ok {
			continue
		}
		price, ok := idEntry[cgQuote]
		if !ok {
			continue
		}
		if !isFinitePositive(price) {
			continue
		}
		if c.staleness(cgID, updatedAt, time.Time{}) != nil {
			continue
		}
		out[p] = price
	}
	return out, nil
}

// ensureBatch returns the cached batched response if it's within
// the TTL; otherwise issues a single HTTP request covering every
// configured (id, quote) tuple, parses, caches, and returns the
// fresh map. Concurrent calls coalesce on batchMu — the second
// caller observes the first caller's freshly-cached response
// instead of double-fetching.
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

// fetchBatch issues a single GET against /simple/price covering
// every (id, quote) pair, parses the response into the nested
// map[id]map[quote]price shape CoinGecko emits, and lifts each id's
// upstream `last_updated_at` (requested via include_last_updated_at)
// out of the price map into a parallel cgID → time map for the
// CS-089 staleness gate.
func (c *CoinGeckoReference) fetchBatch(ctx context.Context, ids, quotes []string) (map[string]map[string]float64, map[string]time.Time, error) {
	v := url.Values{}
	v.Set("ids", strings.Join(ids, ","))
	v.Set("vs_currencies", strings.Join(quotes, ","))
	// CS-089: ask CoinGecko to stamp each coin's upstream publication
	// time so the staleness gate can reject a frozen feed.
	v.Set("include_last_updated_at", "true")
	endpoint := c.baseURL + "/simple/price?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("coingecko: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "stellarindex-divergence/0.1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("coingecko: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, nil, fmt.Errorf("%w: coingecko rate-limited (HTTP 429)", ErrPriceUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
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

	// Lift last_updated_at out of each id's price object so it can't be
	// mistaken for a vs_currency quote, and convert to a UTC time for
	// the staleness gate. A missing/zero/negative value leaves the id
	// out of updatedAt — the gate then no-ops for that id.
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
