package coinmarketcap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

func buildPairs(t *testing.T) []canonical.Pair {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	btc, _ := canonical.NewCryptoAsset("BTC")
	usd, _ := canonical.NewFiatAsset("USD")
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	btcUSD, _ := canonical.NewPair(btc, usd)
	return []canonical.Pair{xlmUSD, btcUSD}
}

func newTestServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != QuotesLatestPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get(APIKeyHeader) == "" {
			t.Errorf("missing %s header", APIKeyHeader)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
}

func TestNewPoller_RejectsEmptyKey(t *testing.T) {
	_, err := NewPoller("")
	if !errors.Is(err, ErrAPIKeyRequired) {
		t.Errorf("expected ErrAPIKeyRequired, got %v", err)
	}
}

func TestPollOnce_HappyPath(t *testing.T) {
	srv := newTestServer(t, `{
      "status": {"error_code": 0, "error_message": null},
      "data": {
        "XLM": [{"symbol": "XLM", "quote": {"USD": {"price": 0.17582, "last_updated": "2026-04-24T00:00:00.000Z"}}}],
        "BTC": [{"symbol": "BTC", "quote": {"USD": {"price": 50000.0,  "last_updated": "2026-04-24T00:00:00.000Z"}}}]
      }
    }`, http.StatusOK)
	defer srv.Close()

	p, err := NewPoller("TEST_KEY")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Endpoint = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	trades, updates, err := p.PollOnce(ctx, buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(trades) != 0 {
		t.Errorf("aggregator must emit 0 trades, got %d", len(trades))
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (XLM/USD, BTC/USD), got %d", len(updates))
	}

	xlm, _ := canonical.NewCryptoAsset("XLM")
	var xlmU *canonical.OracleUpdate
	for i := range updates {
		if updates[i].Asset.Equal(xlm) {
			xlmU = &updates[i]
			break
		}
	}
	if xlmU == nil {
		t.Fatal("missing XLM update")
	}
	priceInt := xlmU.Price.BigInt().Int64()
	if priceInt < 17_580_000 || priceInt > 17_584_000 {
		t.Errorf("XLM price = %d want ~17582000", priceInt)
	}
	if xlmU.Source != "coinmarketcap" {
		t.Errorf("Source = %q", xlmU.Source)
	}
	// Timestamp parsed from last_updated.
	wantTs, _ := time.Parse(time.RFC3339Nano, "2026-04-24T00:00:00.000Z")
	if !xlmU.Timestamp.Equal(wantTs) {
		t.Errorf("Timestamp = %v want %v", xlmU.Timestamp, wantTs)
	}
}

// An empty 200 must come back as a non-nil empty slice: nil is the runner's
// "skipped" convention and would refresh the staleness clock.
func TestPollOnce_EmptyDataIsNotASkip(t *testing.T) {
	srv := newTestServer(t, `{"status": {"error_code": 0, "error_message": null}, "data": {}}`, http.StatusOK)
	defer srv.Close()
	p, err := NewPoller("TEST_KEY")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Endpoint = srv.URL
	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if updates == nil || len(updates) != 0 {
		t.Fatalf("updates = %#v, want non-nil empty", updates)
	}
}

func TestPollOnce_NoApplicablePairs(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	pair, _ := canonical.NewPair(xlm, usdt)
	p, err := NewPoller("TEST_KEY")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Endpoint = "http://localhost:1" // would fail if reached
	_, _, err = p.PollOnce(context.Background(), []canonical.Pair{pair})
	if !errors.Is(err, external.ErrNoApplicablePairs) {
		t.Fatalf("err = %v, want ErrNoApplicablePairs", err)
	}
}

func TestPollOnce_APIError(t *testing.T) {
	srv := newTestServer(t, `{"status":{"error_code":401,"error_message":"Invalid API key"},"data":{}}`, http.StatusOK)
	defer srv.Close()
	p, _ := NewPoller("WRONG")
	p.Endpoint = srv.URL
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if !errors.Is(err, ErrAPIRejected) {
		t.Errorf("expected ErrAPIRejected, got %v", err)
	}
}

func TestPollOnce_401Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	p, _ := NewPoller("BAD")
	p.Endpoint = srv.URL
	_, _, err := p.PollOnce(context.Background(), buildPairs(t))
	if !errors.Is(err, ErrAPIRejected) {
		t.Errorf("expected ErrAPIRejected on 401, got %v", err)
	}
}

// refusalProbe captures what production can see of a refusal: the
// exported counter's value for reason and the poller's log output.
type refusalProbe struct {
	reason string
	before float64
	log    bytes.Buffer
}

func newRefusalProbe(p *Poller, reason string) *refusalProbe {
	r := &refusalProbe{reason: reason}
	r.before = testutil.ToFloat64(obs.ExternalPollerRefusedEntriesTotal.WithLabelValues(SourceName, reason))
	p.Logger = slog.New(slog.NewTextHandler(&r.log, nil))
	return r
}

// assertSurfaced fails unless the refusal moved the metric by exactly
// one and logged a Warn carrying the reason and every wanted attribute.
func (r *refusalProbe) assertSurfaced(t *testing.T, wantAttrs ...string) {
	t.Helper()
	after := testutil.ToFloat64(obs.ExternalPollerRefusedEntriesTotal.WithLabelValues(SourceName, r.reason))
	if after-r.before != 1 {
		t.Errorf("refused_entries_total{reason=%q} moved by %v, want 1", r.reason, after-r.before)
	}
	line := r.log.String()
	for _, want := range append([]string{"level=WARN", "reason=" + r.reason}, wantAttrs...) {
		if !strings.Contains(line, want) {
			t.Errorf("refusal log %q lacks %q", line, want)
		}
	}
}

// TestPollOnce_SymbolMode_MultipleCoins_Refuses: symbol mode has no id/rank/is_active discriminator, so when CMC returns several
// coins for a ticker the poller must refuse it, not take coins[0].
func TestPollOnce_SymbolMode_MultipleCoins_Refuses(t *testing.T) {
	srv := newTestServer(t, `{
      "status": {"error_code": 0, "error_message": null},
      "data": {
        "XLM": [
          {"symbol": "XLM", "quote": {"USD": {"price": 0.175,  "last_updated": "2026-04-24T00:00:00Z"}}},
          {"symbol": "XLM", "quote": {"USD": {"price": 9999.0, "last_updated": "2026-04-24T00:00:00Z"}}}
        ]
      }
    }`, http.StatusOK)
	defer srv.Close()
	p, _ := NewPoller("TEST")
	p.Endpoint = srv.URL
	probe := newRefusalProbe(p, "ambiguous_symbol")
	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("expected 0 updates (ambiguous ticker refused), got %d: %+v", len(updates), updates)
	}
	if got := atomic.LoadUint64(&p.AmbiguousSymbolSkips); got != 1 {
		t.Errorf("AmbiguousSymbolSkips = %d, want 1", got)
	}
	probe.assertSurfaced(t, "ticker=XLM", "coins=2")
}

// TestPollOnce_IDMode_MismatchedID_Skips: in id mode the coin's own id must match the requested numeric id; a mismatch is a
// payload fault.
func TestPollOnce_IDMode_MismatchedID_Skips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
            "status": {"error_code": 0, "error_message": null},
            "data": {
                "512": [{"id": 999, "symbol": "XLM", "quote": {"USD": {"price": 0.18, "last_updated": "2026-04-24T00:00:00Z"}}}]
            }
        }`)
	}))
	defer srv.Close()

	p, _ := NewPoller("TEST_KEY")
	p.Endpoint = srv.URL
	p.CMCIDs = map[string]string{"XLM": "512"}
	probe := newRefusalProbe(p, "id_mismatch")

	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	xlmUSD, _ := canonical.NewPair(xlm, usd)

	_, updates, err := p.PollOnce(context.Background(), []canonical.Pair{xlmUSD})
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("expected 0 updates (id mismatch), got %d: %+v", len(updates), updates)
	}
	if got := atomic.LoadUint64(&p.IDMismatchSkips); got != 1 {
		t.Errorf("IDMismatchSkips = %d, want 1", got)
	}
	probe.assertSurfaced(t, "ticker=XLM", "requested_id=512", "returned_id=999")
}

// TestPollOnce_PartialCoverage_SplitsIntoTwoRequests: when some tickers have a CMC id and
// some don't, the poller must issue one `id=` and one `symbol=` request, never a single
// request carrying both selectors (CMC accepts only one; USDT0 hits this shape).
func TestPollOnce_PartialCoverage_SplitsIntoTwoRequests(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.RawQuery, "id="):
			_, _ = fmt.Fprint(w, `{
                "status": {"error_code": 0, "error_message": null},
                "data": {"512": [{"id": 512, "symbol": "XLM", "quote": {"USD": {"price": 0.18, "last_updated": "2026-04-24T00:00:00Z"}}}]}
            }`)
		case strings.Contains(r.URL.RawQuery, "symbol="):
			_, _ = fmt.Fprint(w, `{
                "status": {"error_code": 0, "error_message": null},
                "data": {"BTC": [{"symbol": "BTC", "quote": {"USD": {"price": 50000.0, "last_updated": "2026-04-24T00:00:00Z"}}}]}
            }`)
		default:
			t.Errorf("request with neither id= nor symbol=: %q", r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p, _ := NewPoller("TEST_KEY")
	p.Endpoint = srv.URL
	p.CMCIDs = map[string]string{"XLM": "512"} // BTC has none — partial coverage.

	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("expected 2 requests (id-only + symbol-only), got %d: %v", len(queries), queries)
	}
	for _, q := range queries {
		if strings.Contains(q, "id=") && strings.Contains(q, "symbol=") {
			t.Errorf("request %q mixed id= and symbol= on one call — CMC accepts only one selector", q)
		}
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (XLM via id, BTC via symbol), got %d", len(updates))
	}
}

// TestPollOnce_IDModeUsesNumericIDs — with `CMCIDs` configured, the upstream request must
// use `id=<numeric>,...` instead of `symbol=<TICKER>,...`, and the price for a
// numeric-ID-keyed entry must map back to the canonical asset (XLM here).
func TestPollOnce_IDModeUsesNumericIDs(t *testing.T) {
	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != QuotesLatestPath {
			http.NotFound(w, r)
			return
		}
		capturedQuery = r.URL.RawQuery
		// CMC keys quotes/latest by the numeric id as a string; the poller must map the response to the canonical asset by id, not
		// ticker.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
            "status": {"error_code": 0, "error_message": null},
            "data": {
                "512": [{"id": 512, "symbol": "XLM", "quote": {"USD": {"price": 0.18, "last_updated": "2026-04-24T00:00:00Z"}}}],
                "1":   [{"id": 1, "symbol": "BTC", "quote": {"USD": {"price": 60000.0, "last_updated": "2026-04-24T00:00:00Z"}}}]
            }
        }`)
	}))
	defer srv.Close()

	p, err := NewPoller("TEST_KEY")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Endpoint = srv.URL
	p.CMCIDs = map[string]string{
		"XLM": "512",
		"BTC": "1",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, updates, err := p.PollOnce(ctx, buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	// 1) Request must have used `id=`, not `symbol=`.
	if capturedQuery == "" {
		t.Fatal("test server did not capture the request URL")
	}
	if !strings.Contains(capturedQuery, "id=") {
		t.Errorf("captured query %q missing `id=` param — poller did not use ID-mode despite CMCIDs being set", capturedQuery)
	}
	if strings.Contains(capturedQuery, "symbol=") {
		t.Errorf("captured query %q still has `symbol=` — must use ID-mode exclusively when CMCIDs is populated", capturedQuery)
	}

	// 2) The response keyed by numeric ID must round-trip back
	//    to the canonical XLM/BTC assets the pairs requested.
	xlm, _ := canonical.NewCryptoAsset("XLM")
	btc, _ := canonical.NewCryptoAsset("BTC")
	gotByAsset := map[string]int64{}
	for i := range updates {
		gotByAsset[updates[i].Asset.String()] = updates[i].Price.BigInt().Int64()
	}
	if got := gotByAsset[xlm.String()]; got < 17_990_000 || got > 18_010_000 {
		t.Errorf("XLM price (id=512) = %d, want ~18000000", got)
	}
	if got := gotByAsset[btc.String()]; got < 5_999_990_000_000 || got > 6_000_010_000_000 {
		t.Errorf("BTC price (id=1) = %d, want ~6000000000000", got)
	}
}

// TestPollOnce_UndatedQuoteFailsClosed pins that a quote whose
// last_updated is absent or unparseable is dropped, never stamped with
// our poll time, and that a response with no datable quote is an error.
func TestPollOnce_UndatedQuoteFailsClosed(t *testing.T) {
	srv := newTestServer(t, `{
      "status": {"error_code": 0, "error_message": null},
      "data": {
        "XLM": [{"symbol": "XLM", "quote": {"USD": {"price": 0.17, "last_updated": "2026-04-24T00:00:00Z"}}}],
        "BTC": [{"symbol": "BTC", "quote": {"USD": {"price": 50000.0, "last_updated": "not-a-time"}}}]
      }
    }`, http.StatusOK)
	defer srv.Close()
	p, _ := NewPoller("TEST")
	p.Endpoint = srv.URL
	_, updates, err := p.PollOnce(context.Background(), buildPairs(t))
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(updates) != 1 || updates[0].Asset.Code != "XLM" {
		t.Fatalf("expected only the dated XLM row, got %d update(s)", len(updates))
	}

	allUndated := newTestServer(t, `{
      "status": {"error_code": 0, "error_message": null},
      "data": {"XLM": [{"symbol": "XLM", "quote": {"USD": {"price": 0.17}}}]}
    }`, http.StatusOK)
	defer allUndated.Close()
	p.Endpoint = allUndated.URL
	_, updates, err = p.PollOnce(context.Background(), buildPairs(t))
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected no updates, got %d", len(updates))
	}
}

func TestPollInterval_Default(t *testing.T) {
	p, _ := NewPoller("TEST")
	if p.PollInterval() != 60*time.Second {
		t.Errorf("default = %v", p.PollInterval())
	}
}

// TestPollOnce_ShippedSeed_NeverMixesSelectors: the shipped catalogue has more coingecko_id
// than coinmarketcap_id entries (USDT0 has no CMC id), so wiring CMCIDs from it reproduces
// the partial-coverage shape live. A single request setting both selectors fails it.
func TestPollOnce_ShippedSeed_NeverMixesSelectors(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	cmcIDs := cat.CoinMarketCapIDs()

	var pairs []canonical.Pair
	var haveIDTicker, haveNoIDTicker bool
	usd, _ := canonical.NewFiatAsset("USD")
	for _, ticker := range cat.Tickers() {
		if !canonical.IsKnownCrypto(ticker) {
			continue
		}
		ca, err := canonical.NewCryptoAsset(ticker)
		if err != nil {
			continue
		}
		pair, err := canonical.NewPair(ca, usd)
		if err != nil {
			continue
		}
		pairs = append(pairs, pair)
		if _, ok := cmcIDs[ticker]; ok {
			haveIDTicker = true
		} else {
			haveNoIDTicker = true
		}
	}
	if !haveIDTicker || !haveNoIDTicker {
		t.Fatalf("fixture assumption broken: need both an id-mapped and a non-id-mapped known crypto ticker in the shipped seed (id=%v, no-id=%v)", haveIDTicker, haveNoIDTicker)
	}

	var sawIDRequest, sawSymbolRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.RawQuery
		hasID := strings.Contains(q, "id=")
		hasSymbol := strings.Contains(q, "symbol=")
		if hasID && hasSymbol {
			t.Errorf("request mixed id= and symbol= on one call: %q", q)
		}
		sawIDRequest = sawIDRequest || hasID
		sawSymbolRequest = sawSymbolRequest || hasSymbol
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status": {"error_code": 0, "error_message": null}, "data": {}}`)
	}))
	defer srv.Close()

	p, _ := NewPoller("TEST_KEY")
	p.Endpoint = srv.URL
	p.CMCIDs = cmcIDs

	if _, _, err := p.PollOnce(context.Background(), pairs); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if !sawIDRequest || !sawSymbolRequest {
		t.Errorf("expected both an id-mode and a symbol-mode request against the shipped seed, got id=%v symbol=%v", sawIDRequest, sawSymbolRequest)
	}
}

// The paid key travels in a custom header, which net/http re-sends on
// every redirect hop regardless of host.
func TestPollOnce_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, APIKeyHeader)
	p, err := NewPoller("probe")
	if err != nil {
		t.Fatal(err)
	}
	p.Endpoint = trap.URL
	_, _, _ = p.PollOnce(context.Background(), buildPairs(t))
	trap.AssertKeyStayedOnOrigin(t)
}

// CMC must report ClassAggregator, not ClassExchange, or the registry would let its index price double-count upstream venues in
// VWAP.

func TestPoller_NameAndClass(t *testing.T) {
	p, err := NewPoller("test-key")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if got := p.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
	if got := p.Class(); got != external.ClassAggregator {
		t.Errorf("Class() = %v, want ClassAggregator (CMC is an index, not an exchange)", got)
	}
}

// decimalStringToScaledInt mirrors the helper in exchangeratesapi. It is the back-stop
// converter for a decimal string; the edge cases are pinned so a refactor that drops
// scientific-notation rejection (silently producing an invalid integer) is caught.

func TestDecimalStringToScaledInt_edges(t *testing.T) {
	cases := []struct {
		in        string
		decimals  int
		want      string
		wantError bool
	}{
		{"0.5", 8, "50000000", false},
		{"-1.0", 8, "-100000000", false},
		{".25", 8, "25000000", false},
		{"100", 0, "100", false},
		{"1.999999999", 8, "199999999", false}, // truncates
		{"", 8, "", true},
		{"1e3", 8, "", true},
		{"1.2.3", 8, "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := scale.DecimalStringToScaledInt(c.in, c.decimals)
			if c.wantError {
				if err == nil {
					t.Errorf("expected error for %q, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != c.want {
				t.Errorf("got %s, want %s", got.String(), c.want)
			}
		})
	}
}

func TestPoller_PollInterval_defaultAndOverride(t *testing.T) {
	p, err := NewPoller("k")
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	p.Interval = 0
	if got := p.PollInterval(); got != DefaultPollInterval {
		t.Errorf("PollInterval(zero) = %v, want %v", got, DefaultPollInterval)
	}
	p.Interval = 7 * time.Second
	if got := p.PollInterval(); got != 7*time.Second {
		t.Errorf("PollInterval(7s) = %v, want 7s", got)
	}
}
