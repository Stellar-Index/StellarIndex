// Package poloniex derives XLM/USD daily bars for the pre-Kraken span
// from Poloniex's BTC-quoted XLM market crossed with a USD-quoted BTC
// leg. It is history only: no streamer, no live path.
//
// The bars are a different kind of observation from a fill-derived VWAP
// (one daily close times one daily close, through a BTC pivot), so they
// are stamped with their own [SourceName], registered with
// IncludeInVWAP=false, and surface on /v1/ohlc through each bar's
// `sources`.
package poloniex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// SourceName is stamped on every trade this package emits. It names the
// derivation, not the venue: a bar under it is never a Poloniex fill.
const SourceName = "poloniex_via_btc"

// RESTEndpoint is Poloniex's public REST base; candles need no auth.
const RESTEndpoint = "https://api.poloniex.com"

// The span no fill-derived XLM/USD venue covers. A derived bar outside it
// would share a prices_* bucket with real fills, whose aggregates count
// every source, so the walk is refused beyond it.
var (
	SpanStart = time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)
	SpanEnd   = time.Date(2017, 1, 17, 0, 0, 0, 0, time.UTC)
)

// CheckRange rejects a window that is not inside [SpanStart, SpanEnd].
func CheckRange(from, to time.Time) error {
	if from.Before(SpanStart) || to.After(SpanEnd) {
		return fmt.Errorf("%s only backfills [%s, %s): -from/-to must lie inside it",
			SourceName, SpanStart.Format(time.RFC3339), SpanEnd.Format(time.RFC3339))
	}
	return nil
}

const (
	market      = "XLM_BTC"
	amountScale = 8
	day         = 24 * time.Hour
	pageLimit   = 500
	// Poloniex candle array layout: [low high open close amount quantity
	// buyTakerAmount buyTakerQuantity tradeCount ts weightedAverage
	// interval startTime closeTime].
	colClose, colQuantity, colStart, candleCols = 3, 5, 12, 13
)

// Backfiller implements external.Backfiller for XLM/USD at 1d only.
type Backfiller struct {
	// Endpoint overrides RESTEndpoint (tests).
	Endpoint string
	// BTCLeg supplies daily BTC/USD trades; BTCPair is the pair it is
	// asked for. Each trade's price (quote/base) is that day's close.
	BTCLeg  external.Backfiller
	BTCPair canonical.Pair
}

// Name implements external.Connector.
func (*Backfiller) Name() string { return SourceName }

// Class implements external.Connector.
func (*Backfiller) Class() external.Class { return external.ClassExchange }

// Backfill returns one trade per UTC day in [from, to) on which both legs
// have volume. A day missing either leg is omitted, never interpolated.
func (b *Backfiller) Backfill(ctx context.Context, pair canonical.Pair, from, to time.Time, granularity time.Duration) ([]canonical.Trade, error) {
	if granularity != day {
		return nil, fmt.Errorf("poloniex.Backfill: only 1d is supported, got %v", granularity)
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("poloniex.Backfill: from %v must be before to %v", from, to)
	}
	if pair.String() != "crypto:XLM/fiat:USD" {
		return nil, fmt.Errorf("poloniex.Backfill: pair %s unsupported (only crypto:XLM/fiat:USD)", pair.String())
	}
	from = from.UTC().Truncate(day)
	candles, err := fetchCandles(ctx, b.endpoint(), from, to)
	if err != nil {
		return nil, err
	}
	btcTrades, err := b.BTCLeg.Backfill(ctx, b.BTCPair, from, to, day)
	if err != nil {
		return nil, fmt.Errorf("poloniex.Backfill: BTC/USD leg: %w", err)
	}
	return derive(candles, btcTrades, pair, to, time.Now())
}

func (b *Backfiller) endpoint() string {
	if b.Endpoint == "" {
		return RESTEndpoint
	}
	return b.Endpoint
}

type candle struct {
	start    time.Time
	close    *big.Rat // BTC per XLM
	quantity *big.Int // XLM at amountScale decimals
}

// derive crosses each XLM/BTC day with the BTC/USD close of the same UTC
// day. Money stays in big.Int / big.Rat; the quote leg floors at
// amountScale, the same rounding the other candle backfills use.
func derive(candles []candle, btcTrades []canonical.Trade, pair canonical.Pair, to, now time.Time) ([]canonical.Trade, error) {
	btcClose := make(map[time.Time]*big.Rat, len(btcTrades))
	for i := range btcTrades {
		t := &btcTrades[i]
		if t.BaseAmount.Sign() <= 0 || t.QuoteAmount.Sign() <= 0 {
			continue
		}
		btcClose[t.Timestamp.UTC().Truncate(day)] = new(big.Rat).SetFrac(t.QuoteAmount.BigInt(), t.BaseAmount.BigInt())
	}
	out := make([]canonical.Trade, 0, len(candles))
	for _, c := range candles {
		if c.quantity.Sign() <= 0 || c.close.Sign() <= 0 || !scale.CandleClosed(c.start.Add(day), to, now) {
			continue
		}
		usd, ok := btcClose[c.start]
		if !ok {
			continue
		}
		price := new(big.Rat).Mul(c.close, usd)
		quote := new(big.Int).Quo(new(big.Int).Mul(c.quantity, price.Num()), price.Denom())
		if quote.Sign() == 0 {
			continue
		}
		closeSec := c.start.Add(day).Unix() - 1
		txHash, err := scale.CandleTxHash(market, closeSec, day)
		if err != nil {
			return nil, fmt.Errorf("poloniex.Backfill: %w", err)
		}
		out = append(out, canonical.Trade{
			Source:      SourceName,
			TxHash:      txHash,
			Timestamp:   time.Unix(closeSec, 0).UTC(),
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(c.quantity),
			QuoteAmount: canonical.NewAmount(quote),
		})
	}
	return out, nil
}

// fetchCandles walks [from, to) newest-last, one page at a time.
func fetchCandles(ctx context.Context, base string, from, to time.Time) ([]candle, error) {
	var out []candle
	for start := from; start.Before(to); {
		rows, err := fetchPage(ctx, base, start, to)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, c := range rows {
			if c.start.Before(start) {
				return nil, fmt.Errorf("poloniex.Backfill: venue returned candle %s before requested start %s; cursor not honoured", c.start.Format(time.DateOnly), start.Format(time.DateOnly))
			}
		}
		out = append(out, rows...)
		start = rows[len(rows)-1].start.Add(day)
		if len(rows) < pageLimit {
			break
		}
	}
	return out, nil
}

func fetchPage(ctx context.Context, base string, start, to time.Time) ([]candle, error) {
	q := url.Values{}
	q.Set("interval", "DAY_1")
	q.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	q.Set("endTime", strconv.FormatInt(to.UnixMilli(), 10))
	q.Set("limit", strconv.Itoa(pageLimit))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/markets/"+market+"/candles?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("poloniex.Backfill: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("poloniex.Backfill: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("poloniex.Backfill: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("poloniex.Backfill: http %d: %s", resp.StatusCode, string(body))
	}
	return parseCandles(body)
}

// parseCandles decodes the positional candle arrays. Fields arrive as
// JSON strings or numbers depending on the column, so both are accepted.
func parseCandles(body []byte) ([]candle, error) {
	var rows [][]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("poloniex.Backfill: decode: %w", err)
	}
	out := make([]candle, 0, len(rows))
	for i, r := range rows {
		if len(r) < candleCols {
			return nil, fmt.Errorf("poloniex.Backfill: candle %d has %d columns, want >= %d", i, len(r), candleCols)
		}
		closeStr, qtyStr, startStr := rawText(r[colClose]), rawText(r[colQuantity]), rawText(r[colStart])
		cl, ok := new(big.Rat).SetString(closeStr)
		if !ok {
			return nil, fmt.Errorf("poloniex.Backfill: candle %d close %q not a decimal", i, closeStr)
		}
		qty, err := scale.DecimalStringToScaledInt(qtyStr, amountScale)
		if err != nil {
			return nil, fmt.Errorf("poloniex.Backfill: candle %d quantity %q: %w", i, qtyStr, err)
		}
		ms, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("poloniex.Backfill: candle %d startTime %q: %w", i, startStr, err)
		}
		out = append(out, candle{start: time.UnixMilli(ms).UTC().Truncate(day), close: cl, quantity: qty})
	}
	return out, nil
}

func rawText(m json.RawMessage) string { return strings.Trim(string(m), `"`) }
