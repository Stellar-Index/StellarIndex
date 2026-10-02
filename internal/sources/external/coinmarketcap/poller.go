// Package coinmarketcap polls CoinMarketCap's Pro /v2 quotes endpoint
// for cross-check reference prices. `ClassAggregator` — divergence
// signal only, excluded from VWAP.
//
// Tier notes:
//   - Hobbyist / Basic: 10k credits/month, 30 calls/min. Usable for
//     low-cadence divergence checks.
//   - Startup: 120k/month, 30/min. Fine for 1-min cadence.
//   - Standard ($79/mo): 500k/month, 60/min, **redistribution allowed**.
//     This is the minimum for production (earlier tiers prohibit
//     redistributing the data).
//
// Wire shape (verified 2026-04-24):
//
//	GET https://pro-api.coinmarketcap.com/v2/cryptocurrency/quotes/latest?symbol=XLM,BTC,ETH&convert=USD
//	Header: X-CMC_PRO_API_KEY: KEY
//
//	{
//	  "data": {
//	    "XLM": [{ "quote": { "USD": { "price": 0.17582, "last_updated": "..." }}}],
//	    "BTC": [{ "quote": { "USD": { "price": 50000.0,  "last_updated": "..." }}}]
//	  },
//	  "status": { "error_code": 0, "error_message": null, ... }
//	}
//
// Note: CMC wraps each symbol's payload in an array because multiple
// coins can share a ticker (e.g. two distinct projects both ticker
// "ETH2"). `id=` mode is unambiguous (the numeric CMC id pins one
// project) and its single response entry is verified against the
// requested id before use. `symbol=` mode carries no discriminator we
// can check — CMC's ranking is undocumented and unstable — so an
// entry with more than one coin is refused rather than guessed at.
package coinmarketcap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

const (
	SourceName                = "coinmarketcap"
	DefaultEndpoint           = "https://pro-api.coinmarketcap.com"
	QuotesLatestPath          = "/v2/cryptocurrency/quotes/latest"
	DefaultPollInterval       = 60 * time.Second
	DefaultDecimals     uint8 = 8

	// APIKeyHeader is CMC's auth convention — a named header rather
	// than query param or Authorization bearer.
	APIKeyHeader = "X-CMC_PRO_API_KEY"
)

var _ = external.ClassAggregator

var (
	ErrAPIKeyRequired    = errors.New("coinmarketcap: API key required (see config.External.CoinMarketCap.APIKey or env COINMARKETCAP_API_KEY)")
	ErrAPIRejected       = errors.New("coinmarketcap: API rejected request")
	ErrMalformedResponse = errors.New("coinmarketcap: malformed response")
)

// Poller implements external.Poller.
type Poller struct {
	APIKey   string
	Endpoint string
	Interval time.Duration

	// CMCIDs maps upper-case ticker → CMC numeric id (as a
	// string, e.g. "512" for XLM). F-1237 (codex audit-2026-05-12):
	// querying CMC by `symbol=` is ambiguous — multiple coins can
	// share the same ticker (LUNA, LUNC, etc.) and CMC picks the
	// highest-ranked match, which can drift between tickers and
	// over time. Every ticker present here is queried by
	// `id=<numeric>`; tickers absent from the map fall back to
	// `symbol=`. The two selectors are mutually exclusive PER
	// TICKER but the poller issues at most one `id=` request and
	// one `symbol=` request per poll — never mixed on the
	// same request, so CMC is never asked to disambiguate a
	// selector it wasn't given.
	//
	// Wired from `currency.Catalogue.CoinMarketCapIDs()` in the
	// indexer/aggregator binaries. Leave nil to preserve the
	// legacy symbol-only path.
	CMCIDs map[string]string

	// AmbiguousSymbolSkips counts response entries dropped because
	// symbol-mode returned more than one coin for a ticker CMC could
	// not resolve to a numeric id. Atomic: read from outside the
	// poll goroutine (metrics/tests) without a lock.
	AmbiguousSymbolSkips uint64

	// IDMismatchSkips counts id-mode response entries dropped
	// because the coin's own `id` field did not match the numeric
	// id requested under that response key — a payload integrity
	// fault, not a normal skip.
	IDMismatchSkips uint64

	// Logger receives a Warn per refused response entry. nil → slog.Default.
	Logger *slog.Logger
}

// Refusal reasons, the `reason` label of
// stellarindex_external_poller_refused_entries_total.
const (
	refusedAmbiguousSymbol = "ambiguous_symbol"
	refusedIDMismatch      = "id_mismatch"
)

// refuse records a dropped response entry on its atomic counter, the
// exported metric and a Warn naming the entry, so a refused ticker is
// never a silent loss of a reference price.
func (p *Poller) refuse(counter *uint64, reason string, attrs ...any) {
	atomic.AddUint64(counter, 1)
	obs.ExternalPollerRefusedEntriesTotal.WithLabelValues(SourceName, reason).Inc()
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("coinmarketcap: response entry refused",
		append([]any{"source", SourceName, "reason", reason}, attrs...)...)
}

// NewPoller constructs a Poller with validated API key.
func NewPoller(apiKey string) (*Poller, error) {
	if apiKey == "" {
		return nil, ErrAPIKeyRequired
	}
	return &Poller{
		APIKey:   apiKey,
		Endpoint: DefaultEndpoint,
		Interval: DefaultPollInterval,
	}, nil
}

func (p *Poller) Name() string { return SourceName }

func (p *Poller) Class() external.Class { return external.ClassAggregator }

func (p *Poller) PollInterval() time.Duration {
	if p.Interval <= 0 {
		return DefaultPollInterval
	}
	return p.Interval
}

// quotesResponse matches CMC's /v2 quotes/latest shape.
type quotesResponse struct {
	Data   map[string][]cmcCoin `json:"data"`
	Status cmcStatus            `json:"status"`
}

type cmcCoin struct {
	ID     int                 `json:"id"`
	Symbol string              `json:"symbol"`
	Quote  map[string]cmcQuote `json:"quote"`
}

type cmcQuote struct {
	//floatmoney:ok known debt (#600) — CoinMarketCap wire decode (JSON numbers are float64 upstream); converts onward at the aggregator boundary
	Price       float64 `json:"price"`
	LastUpdated string  `json:"last_updated"`
}

type cmcStatus struct {
	ErrorCode    int    `json:"error_code"`
	ErrorMessage string `json:"error_message"`
	Timestamp    string `json:"timestamp"`
}

// requestPlan is the pairs → CMC-request translation: which tickers go
// by `id=`, which fall back to `symbol=`, and the lookup tables the
// two decoders need to resolve a response entry back to a canonical
// asset. Split out of PollOnce so the id/symbol paths — which must
// never share one HTTP request — can be built once and
// consumed independently.
type requestPlan struct {
	cryptoAssets map[string]canonical.Asset
	fiatAssets   map[string]canonical.Asset
	wantedCombos map[string]struct{}
	ids          []string
	symbols      []string
	idToTicker   map[string]string
	currencies   []string
}

// planRequest derives the id-mode/symbol-mode split from pairs and
// p.CMCIDs. A ticker is total to exactly one mode — never both — so
// the two resulting requests are total-per-selector rather than the
// previous single request that set `id=` and `symbol=` together.
func (p *Poller) planRequest(pairs []canonical.Pair) requestPlan {
	symbolSet := map[string]struct{}{}
	cryptoAssets := map[string]canonical.Asset{}
	currencySet := map[string]struct{}{}
	fiatAssets := map[string]canonical.Asset{}
	wantedCombos := map[string]struct{}{}

	for _, pair := range pairs {
		if pair.Base.Type != canonical.AssetCrypto || pair.Quote.Type != canonical.AssetFiat {
			continue
		}
		sym := strings.ToUpper(pair.Base.Code)
		code := strings.ToUpper(pair.Quote.Code)
		symbolSet[sym] = struct{}{}
		cryptoAssets[sym] = pair.Base
		currencySet[code] = struct{}{}
		fiatAssets[code] = pair.Quote
		wantedCombos[sym+"/"+code] = struct{}{}
	}
	if len(symbolSet) == 0 || len(currencySet) == 0 {
		return requestPlan{}
	}

	var ids, symbols []string
	idToTicker := map[string]string{}
	for s := range symbolSet {
		if p.CMCIDs != nil {
			if id, ok := p.CMCIDs[s]; ok && id != "" {
				ids = append(ids, id)
				idToTicker[id] = s
				continue
			}
		}
		symbols = append(symbols, s)
	}
	currencies := make([]string, 0, len(currencySet))
	for c := range currencySet {
		currencies = append(currencies, c)
	}

	return requestPlan{
		cryptoAssets: cryptoAssets,
		fiatAssets:   fiatAssets,
		wantedCombos: wantedCombos,
		ids:          ids,
		symbols:      symbols,
		idToTicker:   idToTicker,
		currencies:   currencies,
	}
}

// fetchQuotes issues one /v2/quotes/latest request under a single
// selector (`id` or `symbol` — never both) and decodes the
// envelope, including CMC's in-body status.error_code.
func (p *Poller) fetchQuotes(ctx context.Context, selector, selectorValue string, currencies []string) (quotesResponse, error) {
	q := url.Values{}
	q.Set(selector, selectorValue)
	q.Set("convert", strings.Join(currencies, ","))

	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+QuotesLatestPath+"?"+q.Encode(), nil)
	if err != nil {
		return quotesResponse{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(APIKeyHeader, p.APIKey)
	req.Header.Set("Accept", "application/json")

	client := httpx.NewKeyedClient("coinmarketcap", 30*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return quotesResponse{}, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return quotesResponse{}, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return quotesResponse{}, fmt.Errorf("%w: 401 unauthorized — check CMC API key", ErrAPIRejected)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return quotesResponse{}, fmt.Errorf("%w: 429 rate limited — check tier", ErrAPIRejected)
	}

	var r quotesResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return quotesResponse{}, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}
	if r.Status.ErrorCode != 0 {
		return quotesResponse{}, fmt.Errorf("%w: code=%d %s",
			ErrAPIRejected, r.Status.ErrorCode, r.Status.ErrorMessage)
	}
	return r, nil
}

// decodeIDMode decodes a response fetched with `id=`. CMC keys the
// response map by the numeric id string, not the ticker, so the ticker
// is resolved via plan.idToTicker; the coin's own `id` must then agree
// with that key before the entry is trusted.
func (p *Poller) decodeIDMode(r quotesResponse, plan requestPlan) ([]canonical.OracleUpdate, int) {
	var updates []canonical.OracleUpdate
	undated := 0
	for key, coins := range r.Data {
		ticker, ok := plan.idToTicker[key]
		if !ok || len(coins) == 0 {
			continue
		}
		if len(coins) != 1 || strconv.Itoa(coins[0].ID) != key {
			p.refuse(&p.IDMismatchSkips, refusedIDMismatch,
				"ticker", ticker, "requested_id", key, "returned_id", coins[0].ID, "coins", len(coins))
			continue
		}
		u, n := decodeQuotes(ticker, coins[0], plan)
		updates = append(updates, u...)
		undated += n
	}
	return updates, undated
}

// decodeSymbolMode decodes a response fetched with `symbol=`. More than
// one coin under a ticker means CMC could not disambiguate it and the
// payload carries nothing we can verify the choice against, so the
// ticker is refused rather than guessed at.
func (p *Poller) decodeSymbolMode(r quotesResponse, plan requestPlan) ([]canonical.OracleUpdate, int) {
	var updates []canonical.OracleUpdate
	undated := 0
	for sym, coins := range r.Data {
		if len(coins) == 0 {
			continue
		}
		ticker := strings.ToUpper(sym)
		if _, ok := plan.cryptoAssets[ticker]; !ok {
			continue
		}
		if len(coins) > 1 {
			p.refuse(&p.AmbiguousSymbolSkips, refusedAmbiguousSymbol,
				"ticker", ticker, "coins", len(coins))
			continue
		}
		u, n := decodeQuotes(ticker, coins[0], plan)
		updates = append(updates, u...)
		undated += n
	}
	return updates, undated
}

// decodeQuotes turns one resolved coin's per-currency quotes into
// OracleUpdates filtered to the wanted (ticker, currency) combos, and
// counts the quotes dropped for an unparseable last_updated.
func decodeQuotes(ticker string, coin cmcCoin, plan requestPlan) ([]canonical.OracleUpdate, int) {
	cryptoAsset := plan.cryptoAssets[ticker]
	var updates []canonical.OracleUpdate
	undated := 0
	for currencyCode, quote := range coin.Quote {
		cUp := strings.ToUpper(currencyCode)
		quoteAsset, ok := plan.fiatAssets[cUp]
		if !ok {
			continue
		}
		if _, want := plan.wantedCombos[ticker+"/"+cUp]; !want {
			continue
		}
		if quote.Price <= 0 {
			continue
		}
		scaled, err := scale.FloatToScaledInt(quote.Price, int(DefaultDecimals))
		if err != nil || scaled.Sign() <= 0 {
			continue
		}
		// An unparseable or absent last_updated drops the quote rather
		// than stamping our poll time, which would present a quote of
		// unknown age as fresh to every freshness gate downstream.
		ts, err := time.Parse(time.RFC3339Nano, quote.LastUpdated)
		if err != nil {
			undated++
			continue
		}
		updates = append(updates, canonical.OracleUpdate{
			Source:     SourceName,
			ContractID: "",
			Ledger:     0,
			TxHash:     syntheticTxHash(ticker, currencyCode, ts.Unix()),
			OpIndex:    0,
			Timestamp:  ts.UTC(),
			Asset:      cryptoAsset,
			Quote:      quoteAsset,
			Price:      canonical.NewAmount(scaled),
			Decimals:   DefaultDecimals,
			Observer:   "",
		})
	}
	return updates, undated
}

// PollOnce implements external.Poller. Tickers with a CMC id are fetched
// in one `id=` request and the rest in a separate `symbol=` request: CMC
// accepts one selector per call, so mixing them either rejects the whole
// poll or silently drops the symbol-only tickers.
func (p *Poller) PollOnce(ctx context.Context, pairs []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) {
	plan := p.planRequest(pairs)
	if len(plan.ids) == 0 && len(plan.symbols) == 0 {
		return nil, nil, external.ErrNoApplicablePairs
	}
	var updates []canonical.OracleUpdate
	undated := 0
	if len(plan.ids) > 0 {
		r, err := p.fetchQuotes(ctx, "id", strings.Join(plan.ids, ","), plan.currencies)
		if err != nil {
			return nil, nil, err
		}
		u, n := p.decodeIDMode(r, plan)
		updates, undated = append(updates, u...), undated+n
	}
	if len(plan.symbols) > 0 {
		r, err := p.fetchQuotes(ctx, "symbol", strings.Join(plan.symbols, ","), plan.currencies)
		if err != nil {
			return nil, nil, err
		}
		u, n := p.decodeSymbolMode(r, plan)
		updates, undated = append(updates, u...), undated+n
	}
	if len(updates) == 0 && undated > 0 {
		return nil, nil, fmt.Errorf("%w: %d quote(s) returned no parseable last_updated (freshness unverifiable)",
			ErrMalformedResponse, undated)
	}
	return nil, updates, nil
}

func syntheticTxHash(ticker, currency string, ts int64) string {
	s := fmt.Sprintf("CMC-%s-%s-%020d", strings.ToUpper(ticker), strings.ToUpper(currency), ts)
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
