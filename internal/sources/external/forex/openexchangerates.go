package forex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/httpx"
)

// OpenExchangeRatesEndpoint is the Open Exchange Rates API root.
const OpenExchangeRatesEndpoint = "https://openexchangerates.org/api"

// oxrMaxBody caps the latest.json read; a full board is ~4 KB.
const oxrMaxBody = 1 << 20

// ErrOXRBoard marks an Open Exchange Rates response the provider refuses
// whole: the worker must see no rates rather than a partial board.
var ErrOXRBoard = errors.New("forex: openexchangerates board refused")

// OpenExchangeRatesProvider adapts Open Exchange Rates' latest.json into
// the worker's USD-base contract. Its board is republished hourly, where
// massive's grouped aggregate is a daily bar.
//
// The Free plan allows 1,000 requests a month and refuses the `base` and
// `symbols` parameters, so the request carries no query string at all. The
// app id travels only in the Authorization header, which keeps it out of
// every URL, log line and error the provider produces.
type OpenExchangeRatesProvider struct {
	AppID string
	// Endpoint overrides [OpenExchangeRatesEndpoint]; tests point it at
	// an httptest server.
	Endpoint string
}

// Name implements [RateProvider].
func (p OpenExchangeRatesProvider) Name() string { return "openexchangerates" }

// LatestUSDRates implements [RateProvider]. Keys are the UPPER-case codes
// the board publishes; values are units of the code per 1 USD, already the
// worker's contract, so no rebase is needed.
func (p OpenExchangeRatesProvider) LatestUSDRates(ctx context.Context) (map[string]float64, time.Time, error) {
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = OpenExchangeRatesEndpoint
	}
	url := strings.TrimRight(endpoint, "/") + "/latest.json"
	if p.AppID == "" {
		return nil, time.Time{}, fmt.Errorf("forex: openexchangerates %s: app id is empty", url)
	}
	body, err := p.get(ctx, url)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("forex: openexchangerates %s: %w", url, err)
	}
	rates, publishedAt, err := parseOXRBoard(body)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("forex: openexchangerates %s: %w", url, err)
	}
	return rates, publishedAt, nil
}

func (p OpenExchangeRatesProvider) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Token "+p.AppID)

	resp, err := httpx.NewKeyedClient("openexchangerates", fetchTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, oxrMaxBody))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The error body's `message` is a short code (invalid_app_id,
		// not_allowed, access_restricted) that tells a bad key from a spent quota.
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		return nil, fmt.Errorf("status %d %s", resp.StatusCode, strings.ReplaceAll(e.Message, p.AppID, "<redacted>"))
	}
	return body, nil
}

// parseOXRBoard validates a latest.json body. Any defect refuses the whole
// board: one bad rate would otherwise reach the snapshot beside good ones.
func parseOXRBoard(body []byte) (map[string]float64, time.Time, error) {
	var board struct {
		Timestamp int64  `json:"timestamp"`
		Base      string `json:"base"`
		//floatmoney:ok known debt (#600) — same float chain as frankfurter DayRates.Rates: RateProvider.LatestUSDRates hands this map to worker.go RateUSD unconverted
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &board); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: decode: %w", ErrOXRBoard, err)
	}
	if board.Base != "USD" {
		return nil, time.Time{}, fmt.Errorf("%w: base %q, want USD", ErrOXRBoard, board.Base)
	}
	if board.Timestamp <= 0 {
		return nil, time.Time{}, fmt.Errorf("%w: no publication timestamp", ErrOXRBoard)
	}
	if len(board.Rates) == 0 {
		return nil, time.Time{}, fmt.Errorf("%w: no rates", ErrOXRBoard)
	}
	for code, r := range board.Rates {
		if code == "" || !isFiniteFloat(r) || r <= 0 {
			return nil, time.Time{}, fmt.Errorf("%w: rate %q = %v", ErrOXRBoard, code, r)
		}
	}
	return board.Rates, time.Unix(board.Timestamp, 0).UTC(), nil
}
