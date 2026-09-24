// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package kraken

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
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
// AGENTS.md scaling trap), and keyed on the streamer's (symbol,
// trade_id) identity so a backfilled fill and its live row share one PK.
//
// On any error the fills from pages already fetched are returned with it,
// in time order, so the caller decides what a partial walk is worth.
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
	// Refuse up front: the per-fill skip would turn an unrepresentable
	// symbol into a silently empty run.
	if _, err := formatTxHash(symbol, 0); err != nil {
		return nil, fmt.Errorf("kraken.BackfillTrades: %w", err)
	}

	endpoint := s.restBase() + tradesPath
	cursor := strconv.FormatInt(from.UnixNano(), 10)
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
			return out, fmt.Errorf("kraken.BackfillTrades: %w", err)
		}
		if len(fills) == 0 {
			break
		}
		page, done := fillsToTrades(fills, symbol, pair, to)
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
	ts     time.Time
	id     int64
}

// fetchKrakenTrades GETs one /Trades page and returns the fills plus
// the `last` pagination cursor (nanosecond string).
func fetchKrakenTrades(ctx context.Context, endpoint string, q url.Values) ([]krakenFill, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	// Bounded by krakenRESTTimeout, never http.DefaultClient: the
	// latter has no Timeout, and the pagination loop above only
	// consults ctx between pages (#371 F5).
	client := &http.Client{Timeout: krakenRESTTimeout}
	resp, err := client.Do(req)
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
		// UseNumber keeps the time and trade_id digits exact; a float64
		// time is off by up to a few hundred ns, enough to cross a stored µs.
		var rows [][]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&rows); err != nil {
			return nil, "", fmt.Errorf("kraken trades: pair rows: %w", err)
		}
		for _, r := range rows {
			if len(r) < 3 {
				continue
			}
			f, err := decodeKrakenFill(r)
			if err != nil {
				return nil, "", fmt.Errorf("kraken trades: %w", err)
			}
			fills = append(fills, f)
		}
	}
	return fills, last, nil
}

// decodeKrakenFill converts one positional row
// [price, volume, time, side, kind, misc, id], decoded with UseNumber,
// into a krakenFill. A fill without a trade_id is refused: it has no
// identity that can match its live row.
func decodeKrakenFill(r []any) (krakenFill, error) {
	f := krakenFill{}
	if s, ok := r[0].(string); ok {
		f.price = s
	}
	if s, ok := r[1].(string); ok {
		f.volume = s
	}
	ts, ok := r[2].(json.Number)
	if !ok {
		return krakenFill{}, fmt.Errorf("fill time %v is not a number", r[2])
	}
	ns, err := scale.DecimalStringToScaledInt(ts.String(), 9)
	if err != nil || !ns.IsInt64() || ns.Sign() < 0 {
		return krakenFill{}, fmt.Errorf("fill time %q is not a unix-seconds decimal", ts)
	}
	f.ts = time.Unix(0, ns.Int64()).UTC() // i128:ok unix nanoseconds, IsInt64 checked above
	if len(r) < 7 {
		return krakenFill{}, fmt.Errorf("fill at %s has no trade_id", ts)
	}
	id, ok := r[6].(json.Number)
	if !ok {
		return krakenFill{}, fmt.Errorf("fill at %s: trade_id %v is not a number", ts, r[6])
	}
	if f.id, err = id.Int64(); err != nil || f.id < 0 {
		return krakenFill{}, fmt.Errorf("fill at %s: trade_id %q is not a non-negative integer", ts, id)
	}
	return f, nil
}

// fillsToTrades converts one page of fills, stopping at to.
// done=true when the page crossed the requested end.
func fillsToTrades(fills []krakenFill, symbol string, pair canonical.Pair, to time.Time) ([]canonical.Trade, bool) {
	var out []canonical.Trade
	for _, f := range fills {
		if !f.ts.Before(to) {
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

// krakenFillToTrade converts one raw fill to a canonical.Trade under
// the live streamer's identity (ledger 0 = off-chain; tx_hash from
// formatTxHash(symbol, trade_id)), so a backfilled fill and its streamed
// row collapse onto one trades PK instead of double-counting.
func krakenFillToTrade(f krakenFill, symbol string, pair canonical.Pair) (canonical.Trade, error) {
	txHash, err := formatTxHash(symbol, f.id)
	if err != nil {
		return canonical.Trade{}, err
	}
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
		TxHash:      txHash,
		OpIndex:     0,
		Timestamp:   f.ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}, nil
}
