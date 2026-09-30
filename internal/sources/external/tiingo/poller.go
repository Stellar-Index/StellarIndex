package tiingo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// Poller implements external.Poller for Tiingo's end-of-day NAV series.
// PollOnce is the only method the framework calls; it holds no state
// between calls.
type Poller struct {
	// APIKey is sent as `Authorization: Token <key>`, never in the URL,
	// so no request URL, transport error or log line can carry it.
	APIKey string

	// Endpoint overrides [DefaultEndpoint]; tests point at httptest.
	Endpoint string

	// Interval is the poll cadence; zero means [DefaultPollInterval].
	Interval time.Duration

	// Tickers are the fund tickers to poll, one request each per poll.
	Tickers []string

	// Logger receives per-ticker failures when other tickers succeeded.
	// Nil means slog.Default().
	Logger *slog.Logger

	// Now overrides the clock that sets the lookback start; nil is
	// time.Now.
	Now func() time.Time
}

// NewPoller constructs a Poller, failing at startup on a missing key or
// an empty ticker list.
func NewPoller(apiKey string, tickers []string) (*Poller, error) {
	if apiKey == "" {
		return nil, ErrAPIKeyRequired
	}
	if len(tickers) == 0 {
		return nil, ErrNoTickers
	}
	return &Poller{
		APIKey:   apiKey,
		Endpoint: DefaultEndpoint,
		Interval: DefaultPollInterval,
		Tickers:  slices.Clone(tickers),
	}, nil
}

// Name implements external.Connector.
func (p *Poller) Name() string { return SourceName }

// Class implements external.Connector.
func (p *Poller) Class() external.Class { return external.ClassOracle }

// PollInterval implements external.Poller.
func (p *Poller) PollInterval() time.Duration {
	if p.Interval <= 0 {
		return DefaultPollInterval
	}
	return p.Interval
}

// priceBar is the subset of one /prices element the poller reads. Close
// stays a json.Number so the value is scaled from its decimal text and
// never passes through float64.
type priceBar struct {
	Date  string      `json:"date"`
	Close json.Number `json:"close"`
}

// PollOnce implements external.Poller: one OracleUpdate per (ticker, NAV
// date) in the lookback window. The pairs argument is unused — the
// ticker list is fixed at construction.
//
// A failing ticker does not discard the others' rows: the error is
// returned only when every ticker failed, otherwise logged per ticker.
func (p *Poller) PollOnce(ctx context.Context, _ []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	start := now().UTC().AddDate(0, 0, -LookbackDays).Format(time.DateOnly)

	var (
		updates []canonical.OracleUpdate
		errs    []error
	)
	for _, ticker := range p.Tickers {
		rows, err := p.pollTicker(ctx, ticker, start)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		updates = append(updates, rows...)
	}
	if len(errs) > 0 && len(errs) == len(p.Tickers) {
		return nil, nil, errors.Join(errs...)
	}
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for _, err := range errs {
		logger.Warn("tiingo: ticker poll failed", "source", SourceName, "err", err)
	}
	if len(updates) == 0 {
		return nil, nil, nil
	}
	return nil, updates, nil
}

func (p *Poller) pollTicker(ctx context.Context, ticker, start string) ([]canonical.OracleUpdate, error) {
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	status, body, err := external.GetBody(ctx, external.GetRequest{
		URL: endpoint + "/tiingo/daily/" + url.PathEscape(ticker) + "/prices?startDate=" + start,
		Headers: map[string]string{
			"Accept":        "application/json",
			"Authorization": "Token " + p.APIKey,
		},
		LimitBytes: 1 << 20,
	})
	if err != nil {
		return nil, fmt.Errorf("tiingo %s: %s", ticker, p.scrub(err.Error()))
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: %s: http %d: %s", ErrAPIRejected, ticker, status, p.scrub(snippet(body)))
	}
	var bars []priceBar
	if err := json.Unmarshal(body, &bars); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrMalformedResponse, ticker, err)
	}
	return barsToUpdates(ticker, bars)
}

// barsToUpdates converts one ticker's bars. An empty array is "no NAV
// published" and yields no rows; a bar without a close is skipped for the
// same reason. A bar that cannot be dated or scaled fails the ticker,
// because it means the wire shape changed.
func barsToUpdates(ticker string, bars []priceBar) ([]canonical.OracleUpdate, error) {
	asset, err := canonical.NewOracleRawAsset(ticker)
	if err != nil {
		return nil, fmt.Errorf("tiingo %s: %w", ticker, err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return nil, err
	}
	out := make([]canonical.OracleUpdate, 0, len(bars))
	for _, b := range bars {
		if b.Close == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, b.Date)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: date %q: %w", ErrMalformedResponse, ticker, b.Date, err)
		}
		ts = ts.UTC()
		price, err := scale.DecimalStringToScaledInt(b.Close.String(), int(Decimals))
		if err != nil {
			return nil, fmt.Errorf("%w: %s: close %q: %w", ErrMalformedResponse, ticker, b.Close, err)
		}
		if price.Sign() <= 0 {
			continue
		}
		// Keyed on (ticker, NAV date) with ts = the NAV date, so every
		// re-poll of a stored bar hits the same primary key and the
		// insert's conflict arm neither duplicates it nor bumps the tally.
		txHash, err := scale.StrictSyntheticTxHash("TIINGO-" + ticker + "-" + ts.Format("20060102"))
		if err != nil {
			return nil, fmt.Errorf("tiingo %s: %w", ticker, err)
		}
		out = append(out, canonical.OracleUpdate{
			Source:    SourceName,
			TxHash:    txHash,
			Timestamp: ts,
			Asset:     asset,
			Quote:     usd,
			Price:     canonical.NewAmount(price),
			Decimals:  Decimals,
		})
	}
	return out, nil
}

// scrub removes the API key from text that may reach a log line. The key
// travels only in a header, but a vendor error body could echo it.
func (p *Poller) scrub(s string) string {
	if p.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, p.APIKey, "[redacted]")
}

func snippet(body []byte) string {
	const maxLen = 256
	if len(body) > maxLen {
		return string(body[:maxLen]) + "…"
	}
	return string(body)
}
