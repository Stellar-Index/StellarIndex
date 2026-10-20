// Package coingecko polls CoinGecko's public /simple/price endpoint for cross-check reference
// prices. `ClassAggregator`: excluded from VWAP by registry policy (mixing aggregated prices with
// raw trades double-counts upstream markets), consumed only by the divergence-detection layer.
//
// Wire shape (verified):
//
//	GET https://api.coingecko.com/api/v3/simple/price?ids=stellar,bitcoin&vs_currencies=usd,eur&include_last_updated_at=true
//	{"stellar": {"usd": 0.17582, "eur": 0.16230, "last_updated_at": 1710000000}, ...}
//
// Each row is stamped with the id's upstream last_updated_at, never our poll time: the aggregator
// price tier, price_as_of and the oracle-stale alert read OracleUpdate.Timestamp as "when this
// price was true", so a poll-time stamp would present a frozen cache as fresh. CoinGecko uses slug
// IDs, so an allow-listed ticker-to-id table covers our pair set and unknown tickers skip
// per-entry.
package coingecko

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// SourceName is stamped on every canonical.OracleUpdate this package
// emits. Must match the registry key.
const SourceName = "coingecko"

// DefaultEndpoint is the public / demo-tier REST base.
const DefaultEndpoint = "https://api.coingecko.com"

// ProEndpoint is the paid-tier REST base. A Pro key (COINGECKO_API_KEY)
// authenticates only here; the poller auto-selects it when a Pro key is set
// and Endpoint wasn't explicitly overridden.
const ProEndpoint = "https://pro-api.coingecko.com"

// ResolveEndpoint defaults an empty endpoint to [DefaultEndpoint], and moves
// that default to [ProEndpoint] when a Pro key is set: the public host rejects it.
func ResolveEndpoint(endpoint, apiKey string) string {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if apiKey != "" && endpoint == DefaultEndpoint {
		return ProEndpoint
	}
	return endpoint
}

// SetAuthHeader sets the Pro key header, else the Demo key header, else nothing.
// The key never goes in the query: a transport *url.Error embeds the request URL.
func SetAuthHeader(h http.Header, apiKey, demoAPIKey string) {
	if apiKey != "" {
		h.Set("x-cg-pro-api-key", apiKey)
	} else if demoAPIKey != "" {
		h.Set("x-cg-demo-api-key", demoAPIKey)
	}
}

// SimplePricePath is the batch-price endpoint.
const SimplePricePath = "/api/v3/simple/price"

// DefaultPollInterval of 300s keeps the daily call volume (~288 batched /simple/price calls) well
// below the demo tier's 10,000-call/day ceiling, leaving headroom for the market-cap refresher and
// divergence lookups on a shared IP. At 60s this poller alone burns 1,440/day and can trip the cap
// into a sustained 429 loop.
const DefaultPollInterval = 300 * time.Second

// DefaultDecimals — 8dp matches the off-chain-source convention.
const DefaultDecimals uint8 = 8

// Compile-time assertion: aggregator class in registry.
var _ = external.ClassAggregator

// tickerToID is the default ticker -> CoinGecko slug map, used without a Poller TickerToID override; slugs are verified against
// https://api.coingecko.com/api/v3/coins/list, and `internal/currency` is the canonical catalogue.
var tickerToID = map[string]string{
	"BTC":   "bitcoin",
	"ETH":   "ethereum",
	"XLM":   "stellar",
	"USDT":  "tether",
	"USDC":  "usd-coin",
	"PYUSD": "paypal-usd",
	"SOL":   "solana",
	"ADA":   "cardano",
	"DOT":   "polkadot",
	"LINK":  "chainlink",
	"MATIC": "polygon",
	"AVAX":  "avalanche-2",
	"BNB":   "binancecoin",
}

var ErrMalformedResponse = errors.New("coingecko: malformed response")

// MinBackoff is the floor for the post-429 cooldown. Even if the
// venue's Retry-After is shorter (or absent), the next poll
// waits at least this long.
const MinBackoff = 60 * time.Second

// MaxBackoff caps the exponential growth per the package comment
// — at sustained denial we still re-attempt once an hour to
// auto-recover when an operator provisions a key or the
// per-IP cap resets.
const MaxBackoff = 1 * time.Hour

// Poller implements external.Poller.
type Poller struct {
	Endpoint string
	Interval time.Duration
	// APIKey is the CoinGecko Pro key (param x_cg_pro_api_key).
	// Empty → no Pro auth.
	APIKey string
	// DemoAPIKey is the free-tier "demo" key (param x_cg_demo_api_key); it raises per-IP
	// throttling over the no-auth tier. Set this or APIKey, not both; Pro wins when both are set.
	DemoAPIKey string

	// TickerToID overrides the package-default map. When non-empty only tickers present here are
	// queried; set it from `currency.Catalogue.CoinGeckoIDs()` to drive the poll set from the
	// verified-currency seed. Empty falls back to the package default.
	TickerToID map[string]string

	// mu guards the cooldown state; the runner serialises calls per source, so it only protects concurrent test and metrics reads.
	mu             sync.Mutex
	nextAllowedAt  time.Time     // earliest UTC time the poller is allowed to hit the venue again
	currentBackoff time.Duration // last applied backoff, doubled on consecutive 429s
}

// lookupID resolves a ticker to the CG slug, preferring the
// instance override over the package default.
func (p *Poller) lookupID(ticker string) (string, bool) {
	if len(p.TickerToID) > 0 {
		id, ok := p.TickerToID[ticker]
		return id, ok
	}
	id, ok := tickerToID[ticker]
	return id, ok
}

// NewPoller constructs a Poller with defaults. No key required.
func NewPoller() *Poller {
	return &Poller{
		Endpoint: DefaultEndpoint,
		Interval: DefaultPollInterval,
	}
}

// Name implements external.Connector.
func (p *Poller) Name() string { return SourceName }

// Class implements external.Connector.
func (p *Poller) Class() external.Class { return external.ClassAggregator }

// PollInterval implements external.Poller.
func (p *Poller) PollInterval() time.Duration {
	if p.Interval <= 0 {
		return DefaultPollInterval
	}
	return p.Interval
}

// simplePriceResponse is CoinGecko's nested map: outer key asset slug, inner key lowercase fiat code or lastUpdatedKey.
type simplePriceResponse map[string]map[string]float64

// lastUpdatedKey is the per-id field include_last_updated_at=true adds.
const lastUpdatedKey = "last_updated_at"

// liftLastUpdated removes lastUpdatedKey from every id's object (so it is never read as a vs_currency quote) and returns each id's
// upstream publication time; ids with a missing or non-positive value are absent, and PollOnce drops them.
func liftLastUpdated(prices simplePriceResponse) map[string]time.Time {
	out := make(map[string]time.Time, len(prices))
	for id, entry := range prices {
		raw, ok := entry[lastUpdatedKey]
		delete(entry, lastUpdatedKey)
		if ok && raw > 0 {
			out[id] = time.Unix(int64(raw), 0).UTC()
		}
	}
	return out
}

// PollOnce implements external.Poller. One batched GET covers every (id, vs_currency) combo and
// emits one OracleUpdate per (ticker, currency) hit.
//
// On HTTP 429 (or 403, which CoinGecko returns for demo-key-required paths) the poller skips calls
// until `nextAllowedAt` rather than hammer the venue (see applyBackoff); the first success resets
// it. Cooldown returns (nil, nil, nil), not an error, to avoid "poller error" log spam.
func (p *Poller) PollOnce(ctx context.Context, pairs []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) { //nolint:gocognit,gocyclo,funlen // dispatch-heavy; splitting would reduce linearity
	if cooldownLeft := p.cooldownRemaining(); cooldownLeft > 0 {
		return nil, nil, nil
	}
	idSet := map[string]struct{}{}
	tickerForID := map[string]string{}
	cryptoAssets := map[string]canonical.Asset{}
	currencySet := map[string]struct{}{}
	fiatAssets := map[string]canonical.Asset{}
	// wantedCombos filters the venue's N×M response matrix down to
	// just the (crypto, fiat) pairs the operator configured —
	// avoids flooding oracle_updates with divergence-candidates
	// for pairs that have no corresponding aggregated output.
	wantedCombos := map[string]struct{}{}

	for _, pair := range pairs {
		if pair.Base.Type != canonical.AssetCrypto || pair.Quote.Type != canonical.AssetFiat {
			continue
		}
		ticker := strings.ToUpper(pair.Base.Code)
		id, ok := p.lookupID(ticker)
		if !ok {
			continue // unknown crypto ticker; skip the whole pair
		}
		code := strings.ToUpper(pair.Quote.Code)
		idSet[id] = struct{}{}
		tickerForID[id] = ticker
		cryptoAssets[ticker] = pair.Base
		currencySet[strings.ToLower(code)] = struct{}{}
		fiatAssets[code] = pair.Quote
		wantedCombos[ticker+"/"+code] = struct{}{}
	}

	if len(idSet) == 0 || len(currencySet) == 0 {
		return nil, nil, external.ErrNoApplicablePairs
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	currencies := make([]string, 0, len(currencySet))
	for c := range currencySet {
		currencies = append(currencies, c)
	}

	q := url.Values{}
	q.Set("ids", strings.Join(ids, ","))
	q.Set("vs_currencies", strings.Join(currencies, ","))
	q.Set("include_last_updated_at", "true")

	endpoint := ResolveEndpoint(p.Endpoint, p.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+SimplePricePath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	SetAuthHeader(req.Header, p.APIKey, p.DemoAPIKey)

	client := httpx.NewKeyedClient("coingecko", 30*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	// Treat 429 (Too Many Requests) and 403 (post-2024 demo-key-required
	// path) as throttling. Apply backoff and bail without spamming the
	// venue or polluting logs every minute.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		wait := backoffFromRetryAfter(resp.Header.Get("Retry-After"))
		p.applyBackoff(wait)
		return nil, nil, fmt.Errorf("http %d (throttled — backing off %s): %s",
			resp.StatusCode, p.cooldownRemaining().Truncate(time.Second), truncate(string(body), 200))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	// Successful response — clear any prior backoff so the next 429
	// starts fresh from MinBackoff rather than wherever we'd grown to.
	p.resetBackoff()

	var prices simplePriceResponse
	if err := json.Unmarshal(body, &prices); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}

	updatedAt := liftLastUpdated(prices)
	updates := make([]canonical.OracleUpdate, 0, len(idSet)*len(currencySet))
	undated := 0

	for id, cs := range prices {
		ticker, ok := tickerForID[id]
		if !ok {
			continue
		}
		// Fail closed on a missing upstream time, as the divergence
		// reference's staleness gate does: an undatable row is not written.
		ts, dated := updatedAt[id]
		if !dated {
			undated++
			continue
		}
		cryptoAsset, ok := cryptoAssets[ticker]
		if !ok {
			continue
		}
		for currency, priceFloat := range cs {
			cUp := strings.ToUpper(currency)
			quoteAsset, ok := fiatAssets[cUp]
			if !ok {
				continue
			}
			if _, want := wantedCombos[ticker+"/"+cUp]; !want {
				continue
			}
			if priceFloat <= 0 {
				continue
			}
			scaled, err := scale.FloatToScaledInt(priceFloat, int(DefaultDecimals))
			if err != nil || scaled.Sign() <= 0 {
				continue
			}
			u := canonical.OracleUpdate{
				Source:     SourceName,
				ContractID: "",
				Ledger:     0,
				TxHash:     syntheticTxHash(ticker, currency, ts.Unix()),
				OpIndex:    0,
				Timestamp:  ts,
				Asset:      cryptoAsset,
				Quote:      quoteAsset,
				Price:      canonical.NewAmount(scaled),
				Decimals:   DefaultDecimals,
				Observer:   "",
			}
			updates = append(updates, u)
		}
	}
	if len(updates) == 0 && undated > 0 {
		return nil, nil, fmt.Errorf("%w: %d id(s) returned no %s (freshness unverifiable)",
			ErrMalformedResponse, undated, lastUpdatedKey)
	}
	return nil, updates, nil
}

// CooldownRemaining reports the throttle cooldown still pending, so the
// runner can tell a cooldown skip from a healthy "nothing new" poll.
func (p *Poller) CooldownRemaining() time.Duration { return p.cooldownRemaining() }

// cooldownRemaining returns how long the poller must wait before hitting the venue again (<= 0 means polling is allowed);
// lock-protected.
func (p *Poller) cooldownRemaining() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nextAllowedAt.IsZero() {
		return 0
	}
	return time.Until(p.nextAllowedAt)
}

// applyBackoff arms the cooldown: max(hint, currentBackoff*2, MinBackoff), clamped to MaxBackoff. The Retry-After hint is a floor, not
// a ceiling: the free tier's is consistently below MinBackoff (the runner's poll interval), so taking it at face value would 429 once a
// minute forever.
func (p *Poller) applyBackoff(hint time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	doubled := p.currentBackoff * 2
	wait := doubled
	if hint > wait {
		wait = hint
	}
	if wait < MinBackoff {
		wait = MinBackoff
	}
	if wait > MaxBackoff {
		wait = MaxBackoff
	}
	p.currentBackoff = wait
	p.nextAllowedAt = time.Now().Add(wait)
}

// resetBackoff clears the cooldown after a successful poll.
func (p *Poller) resetBackoff() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentBackoff = 0
	p.nextAllowedAt = time.Time{}
}

// backoffFromRetryAfter parses Retry-After per RFC 7231 section 7.1.3 (non-negative seconds or an HTTP-date); 0 when absent or
// unparseable.
func backoffFromRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// truncate keeps log lines bounded (CoinGecko's HTML error pages can be 50KB+): cuts `s` to at most `n` bytes plus "…", on a UTF-8 rune
// boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// syntheticTxHash derives a 64-char hex from (ticker, currency, ts).
func syntheticTxHash(ticker, currency string, ts int64) string {
	s := fmt.Sprintf("COINGECKO-%s-%s-%020d", strings.ToUpper(ticker), strings.ToUpper(currency), ts)
	var hex strings.Builder
	hex.Grow(64)
	for _, b := range []byte(s) {
		fmt.Fprintf(&hex, "%02x", b)
		if hex.Len() >= 64 {
			break
		}
	}
	for hex.Len() < 64 {
		hex.WriteByte('0')
	}
	return hex.String()[:64]
}
