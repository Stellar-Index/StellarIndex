package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// RESTEndpoint is the Binance Spot REST API base for klines + other public market-data calls. Paid clients can use regional
// mirrors (api1/api2/api3/api4.binance.com) for lower latency; the default hostname auto-balances across them.
const RESTEndpoint = "https://api.binance.com"

// klinesPath is the historical candlestick endpoint. Docs:
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api#klinecandlestick-data
const klinesPath = "/api/v3/klines"

// klineMaxLimit is the per-request candle cap; above it Binance clamps silently, so we request `limit=1000` and paginate.
const klineMaxLimit = 1000

// Backfill implements external.Backfiller: one canonical.Trade per kline candle, stamped with the
// close time and the candle's base and quote volume. Unsupported granularities error before any
// HTTP call.
//
// Pages of up to 1000 candles are fetched serially to respect the venue's rate-limit weight.
// Trades are NOT deduplicated here: TxHash is deterministic from (symbol, close_time_ms), so the
// caller's idempotent insert absorbs repeated runs.
func (s *Streamer) Backfill(ctx context.Context, pair canonical.Pair, from, to time.Time, granularity time.Duration) ([]canonical.Trade, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("binance.Backfill: from %v must be before to %v", from, to)
	}
	interval, err := granularityToInterval(granularity)
	if err != nil {
		return nil, err
	}
	symbol, err := s.resolveBackfillSymbol(pair, granularity)
	if err != nil {
		return nil, err
	}

	endpoint := s.restBase() + klinesPath
	startMs := from.UnixMilli()
	endMs := to.UnixMilli()
	now := time.Now()
	var out []canonical.Trade

	for startMs < endMs {
		q := url.Values{}
		q.Set("symbol", symbol)
		q.Set("interval", interval)
		q.Set("startTime", strconv.FormatInt(startMs, 10))
		// endTime is inclusive of a candle's open time; -to is exclusive.
		q.Set("endTime", strconv.FormatInt(endMs-1, 10))
		q.Set("limit", strconv.Itoa(klineMaxLimit))

		candles, err := fetchKlines(ctx, endpoint, q)
		if err != nil {
			return nil, fmt.Errorf("binance.Backfill: %w", err)
		}
		if len(candles) == 0 {
			break
		}

		trades, err := klinesToTrades(candles, symbol, pair, granularity, to, now)
		if err != nil {
			return out, fmt.Errorf("binance.Backfill: %w", err)
		}
		out = append(out, trades...)

		next, done, err := advanceBackfillCursor(candles, startMs, granularity)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		startMs = next
		// If the venue returned fewer than limit candles, we're done
		// for this range — avoid a trailing no-op request.
		if len(candles) < klineMaxLimit {
			break
		}
	}
	return out, nil
}

// resolveBackfillSymbol maps pair to its configured Binance symbol (inverse of PairMap) and refuses up front if the symbol cannot be
// represented in a backfill tx hash; klineToTrade's per-candle skip would otherwise yield a silently empty backfill.
func (s *Streamer) resolveBackfillSymbol(pair canonical.Pair, granularity time.Duration) (string, error) {
	inverse := make(map[string]string, len(s.PairMap))
	for sym, p := range s.PairMap {
		inverse[p.String()] = sym
	}
	symbol, ok := inverse[pair.String()]
	if !ok {
		return "", fmt.Errorf("binance.Backfill: pair %s not in configured PairMap", pair.String())
	}
	if _, err := backfillTxHash(symbol, 0, granularity); err != nil {
		return "", fmt.Errorf("binance.Backfill: %w", err)
	}
	return symbol, nil
}

// klinesToTrades converts one page of candles. Zero-volume candles are skipped; any other unconvertible candle fails the page so a
// malformed row never shrinks the range silently. A candle not closed by min(to, now) is dropped; see scale.CandleClosed.
func klinesToTrades(candles []kline, symbol string, pair canonical.Pair, granularity time.Duration, to, now time.Time) ([]canonical.Trade, error) {
	out := make([]canonical.Trade, 0, len(candles))
	for _, c := range candles {
		openMs, ok := c.openTimeMs()
		if !ok {
			return nil, fmt.Errorf("kline %v: missing open time", c)
		}
		if !scale.CandleClosed(time.UnixMilli(openMs).Add(granularity), to, now) {
			continue
		}
		trade, err := klineToTrade(c, symbol, pair, granularity)
		if errors.Is(err, errZeroVolume) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("kline %v: %w", c, err)
		}
		out = append(out, trade)
	}
	return out, nil
}

// errZeroVolume marks an empty candle, an expected skip.
var errZeroVolume = errors.New("zero volume")

// advanceBackfillCursor computes the next page's startTime, one interval past the last candle's open time, so no page repeats a candle.
// done=true means no parseable open time (stop, no error); an error means the cursor did not advance (caching proxy, venue ignoring
// startTime), which would loop forever.
func advanceBackfillCursor(candles []kline, startMs int64, granularity time.Duration) (next int64, done bool, err error) {
	lastOpen, ok := candles[len(candles)-1].openTimeMs()
	if !ok {
		return 0, true, nil
	}
	next = lastOpen + int64(granularity/time.Millisecond)
	if next <= startMs {
		return 0, false, fmt.Errorf("binance.Backfill: page did not advance past startTime %d (last open %d)", startMs, lastOpen)
	}
	return next, false, nil
}

// restBase returns the REST endpoint; tests override it. A ws:// or wss:// Endpoint (the streaming default) falls back to the
// production REST host.
func (s *Streamer) restBase() string {
	if s.Endpoint == "" || strings.HasPrefix(s.Endpoint, "ws://") || strings.HasPrefix(s.Endpoint, "wss://") {
		return RESTEndpoint
	}
	return s.Endpoint
}

// kline is one positional Binance array read by index: [0] open ms, [1-4] OHLC strings, [5] base volume, [6] close ms, [7] quote
// volume, [8] trade count, [9] taker buy base volume, [10] taker buy quote volume, [11] unused.
type kline []any

func (k kline) openTimeMs() (int64, bool)   { return k.intAt(0) }
func (k kline) closeTimeMs() (int64, bool)  { return k.intAt(6) }
func (k kline) baseVolume() (string, bool)  { return k.stringAt(5) }
func (k kline) quoteVolume() (string, bool) { return k.stringAt(7) }

func (k kline) intAt(i int) (int64, bool) {
	if i >= len(k) {
		return 0, false
	}
	// JSON numbers unmarshal as float64 by default — Binance
	// timestamps are <2^53 so this round-trips losslessly, but we
	// use json.Number + Int64() via string fallback for safety.
	switch v := k[i].(type) {
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (k kline) stringAt(i int) (string, bool) {
	if i >= len(k) {
		return "", false
	}
	s, ok := k[i].(string)
	return s, ok
}

// fetchKlines performs one HTTP GET and returns the parsed candles.
func fetchKlines(ctx context.Context, endpoint string, q url.Values) ([]kline, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	var out []kline
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// klineToTrade synthesises a Trade from one candle: timestamp is the close time, Base/QuoteAmount come from the volume fields, not
// OHLC; tx_hash is stable (see backfillTxHash).
func klineToTrade(c kline, symbol string, pair canonical.Pair, granularity time.Duration) (canonical.Trade, error) {
	closeMs, ok := c.closeTimeMs()
	if !ok {
		return canonical.Trade{}, fmt.Errorf("kline missing close time")
	}
	baseStr, ok := c.baseVolume()
	if !ok {
		return canonical.Trade{}, fmt.Errorf("kline missing base volume")
	}
	quoteStr, ok := c.quoteVolume()
	if !ok {
		return canonical.Trade{}, fmt.Errorf("kline missing quote volume")
	}
	base, err := scale.DecimalStringToScaledInt(baseStr, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("base volume %q: %w", baseStr, err)
	}
	quote, err := scale.DecimalStringToScaledInt(quoteStr, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("quote volume %q: %w", quoteStr, err)
	}
	// Skip empty-volume candles — they add no price signal and
	// would divide-by-zero in downstream VWAP math.
	if base.Sign() == 0 || quote.Sign() == 0 {
		return canonical.Trade{}, errZeroVolume
	}

	txHash, err := backfillTxHash(symbol, closeMs, granularity)
	if err != nil {
		return canonical.Trade{}, err
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      0,
		TxHash:      txHash,
		OpIndex:     0,
		Timestamp:   time.UnixMilli(closeMs).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}, nil
}

// backfillTxHash is formatTxHash for historical candles, keyed on close time and granularity, not an aggTrade ID; see
// scale.CandleTxHash.
func backfillTxHash(symbol string, closeMs int64, granularity time.Duration) (string, error) {
	return scale.CandleTxHash(strings.ToUpper(symbol), closeMs, granularity)
}

// granularityToInterval maps a time.Duration to Binance's interval string. Only the required
// granularities plus a few common intermediate buckets are exposed; anything else is an error.
func granularityToInterval(d time.Duration) (string, error) {
	switch d {
	case 1 * time.Minute:
		return "1m", nil
	case 3 * time.Minute:
		return "3m", nil
	case 5 * time.Minute:
		return "5m", nil
	case 15 * time.Minute:
		return "15m", nil
	case 30 * time.Minute:
		return "30m", nil
	case 1 * time.Hour:
		return "1h", nil
	case 2 * time.Hour:
		return "2h", nil
	case 4 * time.Hour:
		return "4h", nil
	case 6 * time.Hour:
		return "6h", nil
	case 12 * time.Hour:
		return "12h", nil
	case 24 * time.Hour:
		return "1d", nil
	case 7 * 24 * time.Hour:
		return "1w", nil
	}
	return "", fmt.Errorf("binance.Backfill: unsupported granularity %v (supported: 1m/3m/5m/15m/30m/1h/2h/4h/6h/12h/1d/1w)", d)
}

// Guard used only to silence unused-import warnings in test-only
// paths where big.Int is needed but not referenced directly.
var _ = big.NewInt
