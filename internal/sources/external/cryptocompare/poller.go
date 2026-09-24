// Package cryptocompare polls CryptoCompare's /data/pricemultifull for
// cross-check reference prices. `ClassAggregator` — divergence
// signal only, excluded from VWAP.
//
// Wire shape (fields we read; the full object carries ~40 more):
//
//	GET https://min-api.cryptocompare.com/data/pricemultifull?fsyms=XLM,BTC&tsyms=USD,EUR
//	Header: Authorization: Apikey <KEY>
//
//	{
//	  "RAW": {
//	    "XLM": {"USD": {"PRICE": 0.17582, "LASTUPDATE": 1710000000}, "EUR": {...}},
//	    "BTC": {"USD": {"PRICE": 50000.0, "LASTUPDATE": 1710000000}, "EUR": {...}}
//	  },
//	  "DISPLAY": {...}
//	}
//
// /data/pricemultifull rather than /data/pricemulti because only the
// former carries LASTUPDATE: each row is stamped with the upstream
// publication time, never our poll time, since the aggregator price
// tier, price_as_of and the oracle-stale alert all read
// OracleUpdate.Timestamp as "when this price was true". A quote with
// no LASTUPDATE is dropped. Free tier works (~100k calls/month); paid ~$80/mo removes the
// redistribution restriction.
package cryptocompare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

const (
	SourceName                = "cryptocompare"
	DefaultEndpoint           = "https://min-api.cryptocompare.com"
	PriceMultiFullPath        = "/data/pricemultifull"
	DefaultPollInterval       = 60 * time.Second
	DefaultDecimals     uint8 = 8
)

var _ = external.ClassAggregator

var (
	ErrAPIKeyRequired    = errors.New("cryptocompare: API key required (see config.External.CryptoCompare.APIKey or env CRYPTOCOMPARE_API_KEY)")
	ErrAPIRejected       = errors.New("cryptocompare: API rejected request")
	ErrMalformedResponse = errors.New("cryptocompare: malformed response")
)

// Poller implements external.Poller.
type Poller struct {
	APIKey   string
	Endpoint string
	Interval time.Duration
}

// NewPoller constructs a Poller with validated key. CryptoCompare's
// free tier works without a key in theory, but rate limits are so
// aggressive that we require one in practice (and all serious use
// needs a key anyway).
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

func (p *Poller) Name() string          { return SourceName }
func (p *Poller) Class() external.Class { return external.ClassAggregator }
func (p *Poller) PollInterval() time.Duration {
	if p.Interval <= 0 {
		return DefaultPollInterval
	}
	return p.Interval
}

// priceMultiFullResponse is the RAW asset→currency→quote shape.
// On error CryptoCompare returns a DIFFERENT top-level shape
// ({"Response":"Error","Message":"..."}) — we probe for that before
// attempting the price-map decode.
type priceMultiFullResponse struct {
	Raw map[string]map[string]rawQuote `json:"RAW"`
}

// rawQuote is one RAW entry; LastUpdate is unix seconds.
type rawQuote struct {
	Price      float64 `json:"PRICE"`
	LastUpdate int64   `json:"LASTUPDATE"`
}

type errorResponse struct {
	Response   string `json:"Response"`
	Message    string `json:"Message"`
	HasWarning bool   `json:"HasWarning"`
	Type       int    `json:"Type"`
}

// PollOnce implements external.Poller.
func (p *Poller) PollOnce(ctx context.Context, pairs []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) { //nolint:gocognit,gocyclo,funlen // dispatch-heavy; splitting would reduce linearity
	symbolSet := map[string]struct{}{}
	cryptoAssets := map[string]canonical.Asset{}
	currencySet := map[string]struct{}{}
	fiatAssets := map[string]canonical.Asset{}
	// wantedCombos filters the N×M response matrix down to just
	// the (crypto, fiat) pairs the operator actually configured.
	// Without this, emitting the full cross-product fills
	// oracle_updates with rows that have no corresponding
	// aggregated output to check against.
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
		return nil, nil, nil
	}

	symbols := make([]string, 0, len(symbolSet))
	for s := range symbolSet {
		symbols = append(symbols, s)
	}
	currencies := make([]string, 0, len(currencySet))
	for c := range currencySet {
		currencies = append(currencies, c)
	}

	q := url.Values{}
	q.Set("fsyms", strings.Join(symbols, ","))
	q.Set("tsyms", strings.Join(currencies, ","))

	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+PriceMultiFullPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Apikey "+p.APIKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}

	// Probe for error shape before decoding the price map.
	// CryptoCompare returns 200 with {"Response":"Error","Message":...}
	// on auth failures and unknown symbols.
	var maybeErr errorResponse
	if err := json.Unmarshal(body, &maybeErr); err == nil && maybeErr.Response == "Error" {
		return nil, nil, fmt.Errorf("%w: %s", ErrAPIRejected, maybeErr.Message)
	}

	var prices priceMultiFullResponse
	if err := json.Unmarshal(body, &prices); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}

	updates := make([]canonical.OracleUpdate, 0, len(prices.Raw))
	undated := 0

	for ticker, cs := range prices.Raw {
		ticker = strings.ToUpper(ticker)
		cryptoAsset, ok := cryptoAssets[ticker]
		if !ok {
			continue
		}
		for currency, quote := range cs {
			cUp := strings.ToUpper(currency)
			quoteAsset, ok := fiatAssets[cUp]
			if !ok {
				continue
			}
			if _, want := wantedCombos[ticker+"/"+cUp]; !want {
				// Cross-product entry not in configured pair list.
				continue
			}
			if quote.Price <= 0 {
				continue
			}
			if quote.LastUpdate <= 0 {
				undated++
				continue
			}
			scaled, err := scale.FloatToScaledInt(quote.Price, int(DefaultDecimals))
			if err != nil || scaled.Sign() <= 0 {
				continue
			}
			ts := time.Unix(quote.LastUpdate, 0).UTC()
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
		return nil, nil, fmt.Errorf("%w: %d quote(s) returned no LASTUPDATE (freshness unverifiable)",
			ErrMalformedResponse, undated)
	}
	return nil, updates, nil
}

func syntheticTxHash(ticker, currency string, ts int64) string {
	s := fmt.Sprintf("CC-%s-%s-%020d", strings.ToUpper(ticker), strings.ToUpper(currency), ts)
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
