package kraken

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

// RESTEndpoint is Kraken's public REST base. Public endpoints need
// no auth; private (trading) endpoints do, but we never use them.
const RESTEndpoint = "https://api.kraken.com"

// ohlcPath is the historical candle endpoint.
// Docs: https://docs.kraken.com/api/docs/rest-api/get-ohlc-data
const ohlcPath = "/0/public/OHLC"

// krakenMaxResponse is the hard cap on candles returned per call —
// documented at 720 and unaffected by query params. Implies ~30
// days at 1h granularity or ~30 weeks at 1d. The caller can
// paginate via `since` but the venue's total historical depth is
// effectively capped at this window for recent data (older data is
// simply not served via this endpoint).
const krakenMaxResponse = 720

// krakenRESTTimeout bounds EVERY REST call this package makes
// (/OHLC and /Trades). Both pagination loops check ctx only BETWEEN
// pages, and `stellarindex-ops backfill` hands them the process root
// context — which carries no deadline. So a venue that accepts the
// connection and then never writes a response is bounded by this and
// nothing else; without it the backfill wedges until an operator
// notices (#371 F5).
//
// A var rather than a const purely so the backfill tests can drive the
// real production path against a black-holing server without waiting
// 30 s. Nothing in production reassigns it.
var krakenRESTTimeout = 30 * time.Second

// ErrDepthExceeded marks a Backfill call whose requested `from` is older
// than Kraken will actually serve. Kraken's /OHLC ignores `since` once it
// is older than the venue's ~720-candle horizon and returns its most
// recent window instead, with err=nil — so this is detected from the
// gap between the requested start and the earliest candle actually
// returned, not from an HTTP-level failure. Any trades fetched before
// detection are still returned alongside this error (see partialFetchResume
// in internal/ops/ingest/backfill_external.go).
var ErrDepthExceeded = errors.New("kraken: requested range starts before the venue's OHLC serving horizon")

// Backfill implements external.Backfiller. Returns historical
// trades synthesised from Kraken OHLC candles — one canonical.Trade
// per bucket, with Kraken's own VWAP field providing the effective
// price (multiplied by base volume to compute quote volume).
//
// IMPORTANT depth caveat: Kraken caps at 720 intervals. At 1h that's
// ~30 days back; at 1d it's ~2 years. A `from` older than that horizon
// returns ErrDepthExceeded (see below) rather than silently truncating;
// it's the venue's limit, not a bug in our code. Documented in
// docs/discovery/oracles/band.md and external.Registry
// (BackfillAvailable=true with 30-day caveat).
func (s *Streamer) Backfill(ctx context.Context, pair canonical.Pair, from, to time.Time, granularity time.Duration) ([]canonical.Trade, error) { //nolint:gocognit // dispatch-heavy; splitting would reduce linearity
	if !from.Before(to) {
		return nil, fmt.Errorf("kraken.Backfill: from %v must be before to %v", from, to)
	}
	interval, err := granularityToMinutes(granularity)
	if err != nil {
		return nil, err
	}
	inverse := make(map[string]string, len(s.PairMap))
	for sym, p := range s.PairMap {
		inverse[p.String()] = sym
	}
	symbol, ok := inverse[pair.String()]
	if !ok {
		return nil, fmt.Errorf("kraken.Backfill: pair %s not in configured PairMap", pair.String())
	}
	// Refuse up front: the per-candle skip below would otherwise turn
	// an unrepresentable symbol into a silently empty backfill.
	if _, err := candleTxHash(symbol, 0); err != nil {
		return nil, fmt.Errorf("kraken.Backfill: %w", err)
	}

	endpoint := s.restBase() + ohlcPath
	sinceSec := from.Unix()
	endSec := to.Unix()
	requestedSinceSec := sinceSec
	// Each candle's time is the OPEN time. We stamp the synthesised
	// Trade with the close time — open + interval.
	intervalSec := int64(granularity / time.Second)
	var out []canonical.Trade
	firstPage := true

	for sinceSec < endSec {
		q := url.Values{}
		q.Set("pair", symbol)
		q.Set("interval", strconv.Itoa(interval))
		if sinceSec > 0 {
			q.Set("since", strconv.FormatInt(sinceSec, 10))
		}

		candles, lastTs, err := fetchKrakenOHLC(ctx, endpoint, q)
		if err != nil {
			return nil, fmt.Errorf("kraken.Backfill: %w", err)
		}
		if len(candles) == 0 {
			break
		}

		// Kraken silently ignores `since` once it is older than the
		// venue's serving horizon and returns its most recent window
		// instead — with err=nil. Only the FIRST page's cursor is the
		// caller's actual requested start; detect the gap there.
		var depthErr error
		if firstPage {
			if earliestTs, ok := candles[0].openTimeSec(); ok && earliestTs > requestedSinceSec+intervalSec {
				depthErr = fmt.Errorf("kraken.Backfill: %w: requested from %s but the venue's earliest available candle is %s",
					ErrDepthExceeded,
					time.Unix(requestedSinceSec, 0).UTC().Format(time.RFC3339),
					time.Unix(earliestTs, 0).UTC().Format(time.RFC3339))
			}
			firstPage = false
		}

		for _, c := range candles {
			openTs, ok := c.openTimeSec()
			if !ok {
				continue
			}
			if openTs >= endSec {
				break
			}
			closeTs := openTs + intervalSec - 1
			trade, err := krakenCandleToTrade(c, symbol, pair, closeTs)
			if err != nil {
				continue
			}
			out = append(out, trade)
		}
		if depthErr != nil {
			return out, depthErr
		}

		// Advance via the `last` cursor Kraken returns, one interval
		// past the last delivered candle. Guard against no-progress
		// in the rare case `last` doesn't move.
		next := lastTs + 1
		if next <= sinceSec {
			break
		}
		sinceSec = next
		if len(candles) < krakenMaxResponse {
			break
		}
	}
	return out, nil
}

// restBase returns the REST URL. When Endpoint is a ws:// URL (the
// streaming default), we fall back to the production REST host.
// Tests override with an http:// value to redirect at httptest.
func (s *Streamer) restBase() string {
	if s.Endpoint == "" || strings.HasPrefix(s.Endpoint, "ws://") || strings.HasPrefix(s.Endpoint, "wss://") {
		return RESTEndpoint
	}
	return s.Endpoint
}

// krakenCandle is the positional-array form Kraken returns.
// Layout: [time, open, high, low, close, vwap, volume, count].
type krakenCandle []any

func (k krakenCandle) openTimeSec() (int64, bool) { return k.intAt(0) }
func (k krakenCandle) closeStr() (string, bool)   { return k.stringAt(4) }
func (k krakenCandle) vwapStr() (string, bool)    { return k.stringAt(5) }
func (k krakenCandle) volumeStr() (string, bool)  { return k.stringAt(6) }

func (k krakenCandle) intAt(i int) (int64, bool) {
	if i >= len(k) {
		return 0, false
	}
	switch v := k[i].(type) {
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (k krakenCandle) stringAt(i int) (string, bool) {
	if i >= len(k) {
		return "", false
	}
	s, ok := k[i].(string)
	return s, ok
}

// ohlcResponse is the shape Kraken returns for /0/public/OHLC.
// The `result` field has a dynamic key (the pair name) plus a
// `last` sentinel integer, so we decode it as RawMessage and
// fish out the candle array by iterating keys.
type ohlcResponse struct {
	Error  []string                   `json:"error"`
	Result map[string]json.RawMessage `json:"result"`
}

// fetchKrakenOHLC performs one HTTP GET and returns candles +
// the `last` timestamp cursor (for pagination).
func fetchKrakenOHLC(ctx context.Context, endpoint string, q url.Values) ([]krakenCandle, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: krakenRESTTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
	if err != nil {
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	var r ohlcResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, 0, fmt.Errorf("decode: %w", err)
	}
	if len(r.Error) > 0 {
		return nil, 0, fmt.Errorf("kraken api error: %v", r.Error)
	}

	var candles []krakenCandle
	var last int64
	for key, raw := range r.Result {
		if key == "last" {
			// `last` is a single integer (unquoted).
			_ = json.Unmarshal(raw, &last)
			continue
		}
		// Any other key is the pair's candle array.
		if err := json.Unmarshal(raw, &candles); err != nil {
			return nil, 0, fmt.Errorf("decode candles for %s: %w", key, err)
		}
	}
	return candles, last, nil
}

// krakenCandleToTrade synthesises a canonical.Trade from a Kraken
// candle. Price is the candle's VWAP (authoritative for the
// bucket); quote amount is computed as price × base volume.
func krakenCandleToTrade(c krakenCandle, symbol string, pair canonical.Pair, closeTs int64) (canonical.Trade, error) {
	volStr, ok := c.volumeStr()
	if !ok {
		return canonical.Trade{}, fmt.Errorf("missing volume")
	}
	vwapStr, ok := c.vwapStr()
	if !ok || vwapStr == "0" || vwapStr == "0.0" {
		// Fall back to close price when VWAP is unavailable (rare,
		// happens for zero-volume buckets which we'd skip anyway).
		vwapStr, ok = c.closeStr()
		if !ok {
			return canonical.Trade{}, fmt.Errorf("missing vwap + close")
		}
	}
	base, err := scale.DecimalStringToScaledInt(volStr, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("volume %q: %w", volStr, err)
	}
	if base.Sign() == 0 {
		return canonical.Trade{}, fmt.Errorf("zero volume")
	}
	price, err := scale.DecimalStringToScaledInt(vwapStr, externalAmountDecimals)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("vwap %q: %w", vwapStr, err)
	}
	// quote = base × price / 10^8
	quoteRaw := new(big.Int).Mul(base, price)
	quote := new(big.Int).Quo(quoteRaw, scale.Pow10(externalAmountDecimals))
	// Dust filter — see parse.go::buildTrade for the rationale.
	// Same underflow can happen on a candle whose `volume` * `vwap`
	// rounds to 0 at our 10^8 precision floor.
	if quote.Sign() == 0 {
		return canonical.Trade{}, ErrDustTrade
	}

	txHash, err := candleTxHash(symbol, closeTs)
	if err != nil {
		return canonical.Trade{}, err
	}

	return canonical.Trade{
		Source:      SourceName,
		Ledger:      0,
		TxHash:      txHash,
		OpIndex:     0,
		Timestamp:   time.Unix(closeTs, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(base),
		QuoteAmount: canonical.NewAmount(quote),
	}, nil
}

// candleTxHash is the identity of an OHLC-synthesised row: a candle has
// no venue trade id, so it is keyed on its close time under a "-BF-"
// infix that keeps it apart from formatTxHash's per-fill identities.
// The seed leaves 8 bytes for the symbol; a longer one would truncate
// closeTs and merge neighbouring candles on the trades PK, so it is
// refused instead.
func candleTxHash(symbol string, closeTs int64) (string, error) {
	return scale.StrictSyntheticTxHash(backfillSeed(symbol, closeTs))
}

func backfillSeed(symbol string, closeTs int64) string {
	normalised := strings.ReplaceAll(strings.ToUpper(symbol), "/", "")
	return fmt.Sprintf("%s-BF-%020d", normalised, closeTs)
}

// granularityToMinutes maps a time.Duration to Kraken's interval
// parameter (in minutes). Kraken accepts: 1, 5, 15, 30, 60, 240,
// 1440, 10080, 21600 (minutes).
func granularityToMinutes(d time.Duration) (int, error) {
	switch d {
	case 1 * time.Minute:
		return 1, nil
	case 5 * time.Minute:
		return 5, nil
	case 15 * time.Minute:
		return 15, nil
	case 30 * time.Minute:
		return 30, nil
	case 1 * time.Hour:
		return 60, nil
	case 4 * time.Hour:
		return 240, nil
	case 24 * time.Hour:
		return 1440, nil
	case 7 * 24 * time.Hour:
		return 10080, nil
	case 15 * 24 * time.Hour:
		return 21600, nil
	}
	return 0, fmt.Errorf("kraken.Backfill: unsupported granularity %v (supported: 1m/5m/15m/30m/1h/4h/1d/1w/15d)", d)
}
