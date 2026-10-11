package exchangeratesapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// Poller implements external.Poller for exchangeratesapi.io. One
// Poller per process; thread-safe — PollOnce is the only method
// the framework calls and it's side-effect-free besides the HTTP
// GET.
type Poller struct {
	// APIKey is the access_key passed as a query parameter.
	// Required; Poller construction returns ErrAPIKeyRequired
	// when empty.
	APIKey string

	// Base is the base currency the venue quotes rates against
	// (e.g. "USD" → rates = {EUR: 0.92, GBP: 0.78, ...}). Free
	// tier forces this to EUR; paid tiers accept any code. Empty
	// string defaults to USD.
	Base string

	// Endpoint overrides the REST base URL. Defaults to
	// [DefaultEndpoint]; tests point at httptest servers.
	Endpoint string

	// Interval is the poll cadence. Defaults to
	// [DefaultPollInterval] (60s, matches Professional tier
	// rate-limit).
	Interval time.Duration

	// Symbols is the explicit target currency list; empty means
	// "every symbol derivable from the configured pair list."
	// Setting here overrides pair-list-derived symbols — useful
	// when operator wants to pre-warm unused pairs.
	Symbols []string
}

// NewPoller constructs a Poller with validated config. Surfaces
// ErrAPIKeyRequired when APIKey is empty so operator sees the
// failure at startup.
func NewPoller(apiKey string) (*Poller, error) {
	if apiKey == "" {
		return nil, ErrAPIKeyRequired
	}
	return &Poller{
		APIKey:   apiKey,
		Base:     DefaultBase,
		Endpoint: DefaultEndpoint,
		Interval: DefaultPollInterval,
	}, nil
}

// Name implements external.Connector.
func (p *Poller) Name() string { return SourceName }

// Class implements external.Connector.
func (p *Poller) Class() external.Class { return external.ClassExchange }

// PollInterval implements external.Poller.
func (p *Poller) PollInterval() time.Duration {
	if p.Interval <= 0 {
		return DefaultPollInterval
	}
	return p.Interval
}

// latestResponse is the JSON shape the /latest endpoint returns.
// Floats kept as json.Number so we scale to integer form without
// the float64 precision round-trip.
type latestResponse struct {
	Success   bool                   `json:"success"`
	Timestamp int64                  `json:"timestamp"`
	Base      string                 `json:"base"`
	Date      string                 `json:"date"`
	Rates     map[string]json.Number `json:"rates"`
	Error     *apiError              `json:"error,omitempty"`
}

type apiError struct {
	Code int    `json:"code"`
	Type string `json:"type"`
	Info string `json:"info"`
}

// PollOnce implements external.Poller: it fetches the current rate board for Base against the
// symbols derived from pairs and returns one canonical.OracleUpdate per rate. The venue quotes
// `Base -> <symbol>` (USD -> EUR = 0.92 means 1 USD buys 0.92 EUR), so rates are inverted where
// the pair's base asset matches the venue base.
//
// Pairs with crypto bases are silently skipped rather than rejected: the venue quotes only
// fiat-fiat, and mixed-pair configs are normal because exchange streamers already cover crypto.
func (p *Poller) PollOnce(ctx context.Context, pairs []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) { //nolint:gocognit,funlen // dispatch-heavy; splitting would reduce linearity
	base := p.Base
	if base == "" {
		base = DefaultBase
	}
	symbols := p.resolveSymbols(base, pairs)
	if len(symbols) == 0 {
		return nil, nil, external.ErrNoApplicablePairs
	}

	q := url.Values{}
	q.Set("access_key", p.APIKey)
	q.Set("base", base)
	q.Set("symbols", strings.Join(symbols, ","))

	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	// The venue accepts the key only as the `access_key` query param, so it sits in the request
	// URL; GetBody redacts the query string (RedactURL) so a transport error cannot leak it.
	status, body, err := external.GetBody(ctx, external.GetRequest{
		URL:        endpoint + LatestPath + "?" + q.Encode(),
		Headers:    map[string]string{"Accept": "application/json"},
		LimitBytes: 2 * 1024 * 1024,
		RedactURL:  endpoint + LatestPath,
	})
	if err != nil {
		return nil, nil, err
	}

	// exchangeratesapi serves 200 OK even on auth errors — the
	// `success` field is the authoritative status. We surface 5xx /
	// non-200 as errors but let the JSON shape drive the rest.
	if status >= 500 {
		return nil, nil, fmt.Errorf("http %d: %s", status, string(body))
	}

	var lr latestResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, nil, fmt.Errorf("%w: decode: %w", ErrMalformedResponse, err)
	}
	if !lr.Success {
		errInfo := "unknown"
		if lr.Error != nil {
			errInfo = fmt.Sprintf("code=%d type=%s info=%s", lr.Error.Code, lr.Error.Type, lr.Error.Info)
		}
		return nil, nil, fmt.Errorf("%w: %s", ErrAPIRejected, errInfo)
	}
	if !strings.EqualFold(lr.Base, base) {
		// Paranoid sanity — if the API's `base` field doesn't match
		// what we asked for, stop rather than stamp mislabelled
		// rows. Happens on free tier when we ask for USD base:
		// venue silently returns EUR.
		return nil, nil, fmt.Errorf("%w: base mismatch — requested %q, got %q", ErrAPIRejected, base, lr.Base)
	}

	if lr.Timestamp == 0 {
		// Refuse rather than stamp wall-clock: time.Now() would relabel a quote of unknown age as
		// fresh, and this connector is IncludeInVWAP, so a stale or replayed board would outrank
		// genuinely older quotes in every downstream freshness gate.
		return nil, nil, fmt.Errorf("%w: missing timestamp — refusing to stamp %d rate(s) with wall-clock time", ErrAPIRejected, len(lr.Rates))
	}
	ts := time.Unix(lr.Timestamp, 0).UTC()

	updates := make([]canonical.OracleUpdate, 0, len(lr.Rates))
	baseAsset, err := canonical.NewFiatAsset(base)
	if err != nil {
		return nil, nil, fmt.Errorf("base asset %q: %w", base, err)
	}

	// Emit one OracleUpdate per target currency, inverting the venue's `<base>/<symbol>` quote so
	// the update carries the canonical "price of <asset> in <quote>": "USD base, EUR = 0.9235"
	// becomes 1 EUR = 1/0.9235 USD.
	for sym, rateNum := range lr.Rates {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			continue
		}
		symAsset, err := canonical.NewFiatAsset(sym)
		if err != nil {
			// Unknown currency code (not on ADR-0010 allow-list);
			// skip per-entry, keep emitting the rest. Matches the
			// Reflector decoder's ErrUnknownSymbol pattern.
			continue
		}
		scaled, err := scale.SciDecimalStringToScaledInt(rateNum.String(), int(DefaultDecimals))
		if err != nil || scaled.Sign() <= 0 {
			continue
		}
		// Invert: we want "price of symAsset in baseAsset units."
		// venueRate = base per 1 symbol  →  our price = 1 / venueRate.
		inverted := scale.InvertScaledToDecimals(scaled, int(DefaultDecimals), int(InvertedDecimals))
		if inverted.Sign() <= 0 {
			continue
		}

		u := canonical.OracleUpdate{
			Source:     SourceName,
			ContractID: "",
			Ledger:     0,
			TxHash:     backfillTxHash(sym, base, lr.Timestamp),
			OpIndex:    0,
			Timestamp:  ts,
			Asset:      symAsset,
			Quote:      baseAsset,
			Price:      canonical.NewAmount(inverted),
			Decimals:   InvertedDecimals,
			Observer:   "",
		}
		updates = append(updates, u)
	}
	return nil, updates, nil
}

// resolveSymbols walks the pairs list and returns the unique set of
// fiat codes to query. If p.Symbols is set explicitly, we honour
// that; otherwise we derive from pairs whose assets are on the
// fiat allow-list.
func (p *Poller) resolveSymbols(base string, pairs []canonical.Pair) []string {
	if len(p.Symbols) > 0 {
		return p.Symbols
	}
	wanted := external.FiatCodesFromPairs(pairs, base)
	out := make([]string, 0, len(wanted))
	for k := range wanted {
		out = append(out, k)
	}
	return out
}

// backfillTxHash synthesises a canonical.OracleUpdate tx_hash from
// (symbol, base, timestamp). canonical.OracleUpdate.Validate()
// requires a 64-char hex string; we build one that's stable across
// repeat polls when the venue timestamp is the same.
func backfillTxHash(symbol, base string, ts int64) string {
	return scale.SyntheticTxHash(fmt.Sprintf(
		"XRATES-%s-%s-%020d", strings.ToUpper(base), strings.ToUpper(symbol), ts))
}
