// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package coingecko

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// marketChartRangePath is CoinGecko's historical series endpoint. Unlike
// /simple/price (a spot read, which is all the live poller needs), this
// returns a time series for an explicit [from, to] window, which is what a
// backfill needs.
const marketChartRangePath = "/api/v3/coins/%s/market_chart/range"

// FreeTierHistoryDays is how far back CoinGecko serves without a Pro key.
// Past it the API answers 200-shaped JSON carrying error_code 10012 rather
// than an HTTP error, so the failure has to be read out of the body.
// Verified against the live demo key on 2026-09-08.
const FreeTierHistoryDays = 365

// ErrOutsideFreeTier reports the 10012 window refusal distinctly, so a
// caller can tell "this key cannot reach that far back" from "the venue is
// broken" — the difference between a purchasing decision and an incident.
var ErrOutsideFreeTier = fmt.Errorf("coingecko: history window exceeds the free/demo tier's %d-day limit (error_code 10012) — a Pro key is required", FreeTierHistoryDays)

// BackfillRange returns one OracleUpdate per historical observation CoinGecko
// holds for pair over [from, to).
//
// It emits ORACLE UPDATES, never trades, and that is a deliberate boundary
// rather than an implementation convenience. Every row in `trades` is a venue
// fill carrying source + ledger + tx_hash + op_index, and ADR-0033's
// completeness claims are provable precisely because that provenance exists
// per row. A CoinGecko price is a volume-weighted index across venues we do
// not observe: it has none of that, and writing it beside Kraken fills would
// silently weaken a claim the project currently earns. The live poller makes
// the same choice (it is external.ClassAggregator and returns only updates);
// this keeps the backfill path honest in the same way.
//
// Granularity is CoinGecko's, not ours: the API picks 5-minutely, hourly or
// daily from the window width, and does not let a caller ask. A wide range
// therefore returns daily points. Callers wanting hourly resolution must walk
// in sub-90-day windows.
func (p *Poller) BackfillRange(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]canonical.OracleUpdate, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("coingecko: empty window [%s, %s)", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	ticker := strings.ToUpper(strings.TrimPrefix(pair.Base.String(), "crypto:"))
	id, ok := p.lookupID(ticker)
	if !ok {
		return nil, fmt.Errorf("coingecko: no coin id for base %q (add it to the slug table)", ticker)
	}
	currency := strings.ToLower(strings.TrimPrefix(pair.Quote.String(), "fiat:"))
	if currency == "" {
		return nil, fmt.Errorf("coingecko: quote %q is not a fiat currency", pair.Quote.String())
	}

	prices, err := p.fetchMarketChartRange(ctx, id, currency, from, to)
	if err != nil {
		return nil, err
	}

	// The live poller rebuilds asset maps from the pairs it was handed;
	// here the pair IS the argument, so its own legs are the answer and
	// there is nothing to look up or get out of step with.
	cryptoAsset, quoteAsset := pair.Base, pair.Quote

	out := make([]canonical.OracleUpdate, 0, len(prices))
	for _, pt := range prices {
		ms, priceFloat := pt[0], pt[1]
		if priceFloat <= 0 {
			continue
		}
		ts := time.UnixMilli(int64(ms)).UTC()
		// The API clamps to its own bucket edges, so a window can return
		// points marginally outside it. Drop those rather than writing
		// observations the caller did not ask for.
		if ts.Before(from) || !ts.Before(to) {
			continue
		}
		scaled, err := scale.FloatToScaledInt(priceFloat, int(DefaultDecimals))
		if err != nil || scaled.Sign() <= 0 {
			continue
		}
		out = append(out, canonical.OracleUpdate{
			Source:     SourceName,
			ContractID: "",
			Ledger:     0,
			// Same synthetic-hash construction as the live path, so a
			// backfilled observation and a polled one for the same
			// (ticker, currency, second) collapse to one row instead of
			// double-counting.
			TxHash:    syntheticTxHash(ticker, currency, ts.Unix()),
			OpIndex:   0,
			Timestamp: ts,
			Asset:     cryptoAsset,
			Quote:     quoteAsset,
			Price:     canonical.NewAmount(scaled),
			Decimals:  DefaultDecimals,
			Observer:  "",
		})
	}
	return out, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// timeMillisString renders t as CoinGecko's epoch-milliseconds form. Used by
// the tests to build fixture payloads in the venue's own shape.
func timeMillisString(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// fetchMarketChartRange performs the HTTP half of a range read: host and auth
// selection, the request, and the two failure shapes that must stay
// distinguishable — the tier's window refusal (a purchasing decision) and
// everything else (an outage). Split out of BackfillRange so neither the
// transport concerns nor the conversion loop has to be read through the
// other.
func (p *Poller) fetchMarketChartRange(ctx context.Context, id, currency string, from, to time.Time) ([][2]float64, error) {
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	// Same auto-switch as the live path: a Pro key only authenticates
	// against pro-api.coingecko.com, so an operator who sets
	// COINGECKO_API_KEY does not also have to know the host changes.
	if p.APIKey != "" && endpoint == DefaultEndpoint {
		endpoint = ProEndpoint
	}
	u := endpoint + fmt.Sprintf(marketChartRangePath, id) +
		"?vs_currency=" + currency +
		"&from=" + strconv.FormatInt(from.Unix(), 10) +
		"&to=" + strconv.FormatInt(to.Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Key in a HEADER, never the query string: a transport error's
	// *url.Error embeds the request URL, so a key in the query would leak
	// into logs (G10-04, same reasoning as the live path).
	if p.APIKey != "" {
		req.Header.Set("x-cg-pro-api-key", p.APIKey)
	} else if p.DemoAPIKey != "" {
		req.Header.Set("x-cg-demo-api-key", p.DemoAPIKey)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// The window refusal arrives as a JSON error object; check it before
	// the status code, because CoinGecko has served it under both 200 and
	// 401 depending on tier.
	var errEnvelope struct {
		Error struct {
			Status struct {
				ErrorCode    int    `json:"error_code"`
				ErrorMessage string `json:"error_message"`
			} `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &errEnvelope) == nil && errEnvelope.Error.Status.ErrorCode == 10012 {
		return nil, ErrOutsideFreeTier
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coingecko: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:minInt(len(body), 200)])))
	}
	var payload struct {
		Prices [][2]float64 `json:"prices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return payload.Prices, nil
}
