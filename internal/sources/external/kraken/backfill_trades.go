// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package kraken

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/sources/external/scale"
)

// tradesPath is Kraken's raw-fills endpoint. Unlike /OHLC (which
// serves only the most recent 720 intervals — the reason the 2018-era
// XLM/USD backfill returned zero candles, board #44), /Trades serves
// the FULL history of a pair, paginated by a nanosecond `since`
// cursor with up to 1000 fills per page.
const tradesPath = "/0/public/Trades"

// tradesPageLimit is Kraken's documented per-page maximum.
const tradesPageLimit = 1000

// tradesRateLimit paces the pagination loop. Kraken's public tier
// allows ~1 req/s sustained; a deep backfill (2018→2021 XLM/USD is
// thousands of pages) must stay a good citizen or the venue serves
// HTTP 429s and the whole run dies.
const tradesRateLimit = 1100 * time.Millisecond

// BackfillTrades walks Kraken's /Trades pagination for [from, to),
// returning EXACT venue fills (not synthesised candles — price,
// volume, and timestamp are per-trade). This is the deep-history
// path: use Backfill (OHLC) for recent windows where candles are
// cheaper, and this for anything past the OHLC horizon.
//
// The venue timestamp is fractional seconds; the cursor is
// nanoseconds. Fills are converted with the same fixed 10^8 external
// scale as the streaming path (see externalAmountDecimals — the
// CLAUDE.md scaling trap).
func (s *Streamer) BackfillTrades(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]canonical.Trade, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("kraken.BackfillTrades: from %v must be before to %v", from, to)
	}
	inverse := make(map[string]string, len(s.PairMap))
	for sym, p := range s.PairMap {
		inverse[p.String()] = sym
	}
	symbol, ok := inverse[pair.String()]
	if !ok {
		return nil, fmt.Errorf("kraken.BackfillTrades: pair %s not in configured PairMap", pair.String())
	}

	endpoint := s.restBase() + tradesPath
	cursor := strconv.FormatInt(from.UnixNano(), 10)
	endNano := to.UnixNano()
	var out []canonical.Trade

	ticker := time.NewTicker(tradesRateLimit)
	defer ticker.Stop()

	for {
		q := url.Values{}
		q.Set("pair", symbol)
		q.Set("since", cursor)
		q.Set("count", strconv.Itoa(tradesPageLimit))

		fills, last, err := fetchKrakenTrades(ctx, endpoint, q)
		if err != nil {
			return nil, fmt.Errorf("kraken.BackfillTrades: %w", err)
		}
		if len(fills) == 0 {
			break
		}
		page, done := fillsToTrades(fills, symbol, pair, endNano)
		out = append(out, page...)
		if done || last == "" || last == cursor {
			break
		}
		cursor = last
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ticker.C:
		}
	}
	return out, nil
}

// krakenFill is one raw fill: [price, volume, time, buy/sell,
// market/limit, misc, trade_id].
type krakenFill struct {
	price  string
	volume string
	ts     float64
	id     int64
}

// fetchKrakenTrades GETs one /Trades page and returns the fills plus
// the `last` pagination cursor (nanosecond string).
func fetchKrakenTrades(ctx context.Context, endpoint string, q url.Values) ([]krakenFill, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("kraken trades: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Error  []string                   `json:"error"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", err
	}
	if len(body.Error) > 0 {
		return nil, "", fmt.Errorf("kraken trades: venue error %v", body.Error)
	}
	var last string
	var fills []krakenFill
	for key, raw := range body.Result {
		if key == "last" {
			_ = json.Unmarshal(raw, &last)
			continue
		}
		var rows [][]any
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, "", fmt.Errorf("kraken trades: pair rows: %w", err)
		}
		for _, r := range rows {
			if f, ok := decodeKrakenFill(r); ok {
				fills = append(fills, f)
			}
		}
	}
	return fills, last, nil
}

// decodeKrakenFill converts one positional row
// [price, volume, time, side, kind, misc, id] into a krakenFill.
func decodeKrakenFill(r []any) (krakenFill, bool) {
	if len(r) < 3 {
		return krakenFill{}, false
	}
	f := krakenFill{}
	if s, ok := r[0].(string); ok {
		f.price = s
	}
	if s, ok := r[1].(string); ok {
		f.volume = s
	}
	if t, ok := r[2].(float64); ok {
		f.ts = t
	}
	if len(r) >= 7 {
		if id, ok := r[6].(float64); ok {
			f.id = int64(id)
		}
	}
	return f, true
}

// fillsToTrades converts one page of fills, stopping at endNano.
// done=true when the page crossed the requested end.
func fillsToTrades(fills []krakenFill, symbol string, pair canonical.Pair, endNano int64) ([]canonical.Trade, bool) {
	var out []canonical.Trade
	for _, f := range fills {
		if int64(f.ts*float64(time.Second)) >= endNano {
			return out, true
		}
		trade, err := krakenFillToTrade(f, symbol, pair)
		if err != nil {
			continue
		}
		out = append(out, trade)
	}
	return out, false
}

// krakenFillToTrade converts one raw fill to a canonical.Trade using
// the same synthetic-identity convention as the candle path (ledger 0
// = off-chain; deterministic tx hash from symbol+timestamp+id so
// re-runs are idempotent under the trades PK).
func krakenFillToTrade(f krakenFill, symbol string, pair canonical.Pair) (canonical.Trade, error) {
	sec := int64(f.ts)
	nsec := int64((f.ts - float64(sec)) * float64(time.Second))
	ts := time.Unix(sec, nsec).UTC()
	base, err := scale.DecimalStringToScaledInt(f.volume, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("volume %q: %w", f.volume, err)
	}
	if base.Sign() == 0 {
		return canonical.Trade{}, fmt.Errorf("zero volume")
	}
	price, err := scale.DecimalStringToScaledInt(f.price, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("price %q: %w", f.price, err)
	}
	// quote = base × price / 10^8 — the candle path's exact idiom.
	quoteRaw := new(big.Int).Mul(base, price)
	quote := new(big.Int).Quo(quoteRaw, scale.Pow10(externalAmountDecimals))
	if quote.Sign() == 0 {
		return canonical.Trade{}, ErrDustTrade
	}
	return canonical.Trade{
		Source:      SourceName,
		Ledger:      0,
		TxHash:      backfillTxHash(symbol+"-fill-"+strconv.FormatInt(f.id, 10), ts.UnixNano()),
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}, nil
}
