// Package frankfurter wraps the ECB-backed Frankfurter REST API
// (https://frankfurter.dev). Used by scripts/ops/fx-history-backfill
// to populate fx_quotes with 25+ years of daily fiat rates without
// requiring a paid Massive API key.
//
// Frankfurter publishes ECB reference rates back to 1999-01-04 for
// ~32 currencies, daily granularity only. The range endpoint returns one
// JSON document for every date in [from, to], so a 25-year backfill is one
// HTTP request. No API key.
//
// See the package doc on forex for the reconciliation against external/ecb
// (a Connector-framework poller wired into the indexer). frankfurter doesn't
// implement external.Connector.
package frankfurter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Base is the Frankfurter API root.
const Base = "https://api.frankfurter.dev/v1"

// fetchTimeout caps a single upstream call. The range endpoint
// occasionally responds in 3-5s for multi-decade requests; 30s is
// generous and well under the operator's patience threshold.
const fetchTimeout = 30 * time.Second

// Client is a thin HTTP wrapper. No auth required.
type Client struct {
	http *http.Client
	base string
}

// NewClient returns a [Client] pointed at [Base].
func NewClient() *Client {
	return &Client{
		http: &http.Client{Timeout: fetchTimeout},
		base: Base,
	}
}

// WithBase returns a copy of c with the base URL overridden — for
// tests pointing at an httptest.Server.
func (c *Client) WithBase(base string) *Client {
	cp := *c
	cp.base = base
	return &cp
}

// DayRates is one day's snapshot of USD-base rates: ticker (upper-case
// ISO-4217) → rate (1 USD = N target currency).
type DayRates struct {
	Date time.Time
	//floatmoney:ok known debt — same float-chain class as timescale.FXQuote.RateUSD (fx_quotes.go): fetchAndPersist (scripts/ops/fx-history-backfill/main.go) ranges this map straight into FXQuote.RateUSD without converting
	Rates map[string]float64
}

// RangeUSDRates fetches every daily ECB rate for every Frankfurter-
// supported currency in [from, to] in a single HTTP request. Rates
// are USD-base — Frankfurter natively quotes EUR-base, so we ask
// for `from=USD` which the API converts for us (cross-rate through
// EUR). Days where ECB didn't publish (weekends, holidays) are
// simply absent from the response.
//
// Returned slice is sorted ascending by date.
func (c *Client) RangeUSDRates(ctx context.Context, from, to time.Time) ([]DayRates, error) {
	fromStr := from.UTC().Format("2006-01-02")
	toStr := to.UTC().Format("2006-01-02")
	url := fmt.Sprintf("%s/%s..%s?base=USD", c.base, fromStr, toStr)
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("frankfurter %s..%s: %w", fromStr, toStr, err)
	}
	var raw struct {
		Base  string `json:"base"`
		Start string `json:"start_date"`
		End   string `json:"end_date"`
		//floatmoney:ok known debt — raw Frankfurter JSON decode boundary (date -> ticker -> rate document); reshaped into DayRates.Rates a few lines below, same chain as that field's marker, not stored as-is
		Rates map[string]map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	// Rates are stored as 1 USD = N; a reply in another base would be
	// silently mislabelled, so reject it rather than trust the query string.
	if !strings.EqualFold(raw.Base, "USD") {
		return nil, fmt.Errorf("frankfurter %s..%s: response base %q, want USD", fromStr, toStr, raw.Base)
	}
	out := make([]DayRates, 0, len(raw.Rates))
	for date, rates := range raw.Rates {
		d, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		clean := make(map[string]float64, len(rates))
		for code, rate := range rates {
			if rate > 0 {
				clean[strings.ToUpper(code)] = rate
			}
		}
		out = append(out, DayRates{Date: d.UTC(), Rates: clean})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	const maxBody = 64 << 20 // 64 MiB — multi-decade range responses run ~10-30 MB
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			if len(buf)+n > maxBody {
				return nil, fmt.Errorf("response exceeds %d bytes", maxBody)
			}
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			// Only a CLEAN io.EOF ends the body normally. A mid-stream
			// truncation surfaces as io.ErrUnexpectedEOF ("unexpected EOF"),
			// whose string also contains "EOF" — a substring match would
			// treat a dropped connection as a complete body and return the
			// partial buffer. errors.Is(rerr, io.EOF) is false for
			// io.ErrUnexpectedEOF, so a truncated response propagates as an
			// error instead of a silently-short JSON document.
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, rerr
		}
	}
	return buf, nil
}
