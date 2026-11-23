package coingecko

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// MarketChartRangePath is the per-coin historical series endpoint; %s is the CoinGecko slug.
//
//	GET /api/v3/coins/stellar/market_chart/range?vs_currency=usd&from=<unix>&to=<unix>&precision=full
//	{"prices": [[<unix ms>, <price>], ...], "market_caps": [...], "total_volumes": [...]}
const MarketChartRangePath = "/api/v3/coins/%s/market_chart/range"

// MaxHistoryWindow is the longest window one call keeps at hourly points; past 90 days the
// vendor's automatic granularity drops to daily closes.
const MaxHistoryWindow = 90 * 24 * time.Hour

// historySeedPrefix has '_' where every live seed (syntheticTxHash) has '-', so a backfilled row's
// tx_hash differs from every live one in its 10th byte and can never upsert over a live row.
const historySeedPrefix = "COINGECKO_HIST-"

// HistoryTxHashPrefix is the hex tx_hash prefix every [Poller.History] row carries.
var HistoryTxHashPrefix = hex.EncodeToString([]byte(historySeedPrefix))

// historyCallGap paces history calls one per series: the Demo tier allows 30 calls a minute.
var historyCallGap = 2 * time.Second

// historyMaxAttempts bounds 429 retries of one series before the run fails.
const historyMaxAttempts = 5

// maxPriceExponent bounds a JSON number's exponent before big.Rat expands it.
const maxPriceExponent = 40

// History returns the vendor's retrospective series for every pair PollOnce serves, as
// OracleUpdates stamped at the vendor's point time and kept only inside [from, to). These are not
// live observations: each carries a [HistoryTxHashPrefix] tx_hash. A 429 arms the same cooldown
// PollOnce honours and the series is retried after it; any other failure fails the whole run.
func (p *Poller) History(ctx context.Context, pairs []canonical.Pair, from, to time.Time) ([]canonical.OracleUpdate, error) {
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return nil, fmt.Errorf("coingecko history: from %s must be before to %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if to.Sub(from) > MaxHistoryWindow {
		return nil, fmt.Errorf("coingecko history: window %s exceeds %s, past which the vendor serves daily points", to.Sub(from), MaxHistoryWindow)
	}
	targets := p.targets(pairs)
	if len(targets) == 0 {
		return nil, external.ErrNoApplicablePairs
	}
	var out []canonical.OracleUpdate
	for i, t := range targets {
		if i > 0 {
			if err := sleepCtx(ctx, historyCallGap); err != nil {
				return nil, err
			}
		}
		rows, err := p.historySeries(ctx, t, from, to)
		if err != nil {
			return nil, fmt.Errorf("coingecko history %s/%s: %w", t.ticker, t.currency, err)
		}
		out = append(out, rows...)
	}
	return out, nil
}

func (p *Poller) historySeries(ctx context.Context, t target, from, to time.Time) ([]canonical.OracleUpdate, error) {
	for attempt := 1; ; attempt++ {
		if err := sleepCtx(ctx, p.cooldownRemaining()); err != nil {
			return nil, err
		}
		status, retryAfter, body, err := p.fetchHistory(ctx, t, from, to)
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusTooManyRequests:
			p.applyBackoff(backoffFromRetryAfter(retryAfter))
			if attempt >= historyMaxAttempts {
				return nil, fmt.Errorf("http 429 after %d attempts: %s", attempt, truncate(string(body)))
			}
		case status != http.StatusOK:
			// 403 included: a plan or key refusal for history does not clear on a cooldown.
			return nil, fmt.Errorf("http %d: %s", status, truncate(string(body)))
		default:
			p.resetBackoff()
			return parseHistory(body, t, from, to)
		}
	}
}

func (p *Poller) fetchHistory(ctx context.Context, t target, from, to time.Time) (status int, retryAfter string, body []byte, err error) {
	toUnix := to.Unix()
	if to.Nanosecond() > 0 {
		toUnix++
	}
	q := url.Values{}
	q.Set("vs_currency", strings.ToLower(t.currency))
	q.Set("from", strconv.FormatInt(from.Unix(), 10))
	q.Set("to", strconv.FormatInt(toUnix, 10))
	q.Set("precision", "full")
	endpoint := ResolveEndpoint(p.Endpoint, p.APIKey)
	path := fmt.Sprintf(MarketChartRangePath, url.PathEscape(t.id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path+"?"+q.Encode(), nil)
	if err != nil {
		return 0, "", nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	SetAuthHeader(req.Header, p.APIKey, p.DemoAPIKey)
	resp, err := httpx.NewKeyedClient("coingecko", 30*time.Second).Do(req)
	if err != nil {
		return 0, "", nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return 0, "", nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), body, nil
}

// parseHistory decodes prices with UseNumber so no price passes through float64.
func parseHistory(body []byte, t target, from, to time.Time) ([]canonical.OracleUpdate, error) {
	var r struct {
		Prices [][]json.Number `json:"prices"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}
	if r.Prices == nil {
		return nil, fmt.Errorf("%w: no prices array", ErrMalformedResponse)
	}
	out := make([]canonical.OracleUpdate, 0, len(r.Prices))
	for _, pt := range r.Prices {
		if len(pt) != 2 {
			return nil, fmt.Errorf("%w: price point has %d fields, want 2", ErrMalformedResponse, len(pt))
		}
		ms, err := pt[0].Int64()
		if err != nil {
			return nil, fmt.Errorf("%w: point time %q: %w", ErrMalformedResponse, pt[0], err)
		}
		at := time.UnixMilli(ms).UTC()
		if at.Before(from) || !at.Before(to) || pt[1] == "" {
			continue
		}
		scaled, err := decimalToScaled(pt[1].String(), int(DefaultDecimals))
		if err != nil {
			return nil, fmt.Errorf("%w: price %q: %w", ErrMalformedResponse, pt[1], err)
		}
		if scaled.Sign() <= 0 {
			continue // below 1e-8, as PollOnce skips it
		}
		out = append(out, canonical.OracleUpdate{
			Source:    SourceName,
			TxHash:    historyTxHash(t.ticker, t.currency, at),
			Timestamp: at,
			Asset:     t.base,
			Quote:     t.quote,
			Price:     canonical.NewAmount(scaled),
			Decimals:  DefaultDecimals,
		})
	}
	return out, nil
}

// decimalToScaled rounds a non-negative JSON number to decimals places, half-up, exactly.
func decimalToScaled(s string, decimals int) (*big.Int, error) {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(s[i+1:])
		if err != nil || exp > maxPriceExponent || exp < -maxPriceExponent {
			return nil, fmt.Errorf("exponent out of range")
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("not a non-negative decimal")
	}
	num := new(big.Int).Mul(r.Num(), scale.Pow10(decimals))
	num.Lsh(num, 1).Add(num, r.Denom())
	return num.Quo(num, new(big.Int).Lsh(r.Denom(), 1)), nil
}

func historyTxHash(ticker, currency string, at time.Time) string {
	return scale.SyntheticTxHash(fmt.Sprintf("%s%s-%s-%d", historySeedPrefix, strings.ToUpper(ticker), strings.ToUpper(currency), at.UnixMilli()))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}
