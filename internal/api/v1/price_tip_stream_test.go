package v1_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// readTipStreamEvent reads one full SSE frame (id/event/data block)
// from body and returns the data payload. Skips comment frames
// (`:keepalive`, `:connected`). Returns "" on EOF.
func readTipStreamEvent(t *testing.T, br *bufio.Reader, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var data string
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			return data
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if line == "\n" {
			if data != "" {
				return data
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
		}
		// id:/event: lines are skipped — tests assert against payload.
	}
	return data
}

// testStreamSecond stands in for one second of a stream's window_seconds / interval_seconds
// cadence, so a window_seconds=1 tick lands in 100ms. Tick-timing bounds are either against
// this or against an unscaled budget (the 8s tick budget), so the edges probed are unchanged.
const testStreamSecond = 100 * time.Millisecond

// startTipStreamServer wires a v1.Server with the given Prices +
// History readers behind an httptest.Server and returns its URL.
func startTipStreamServer(t *testing.T, prices v1.PriceReader, history v1.HistoryReader) string {
	t.Helper()
	srv := v1.New(v1.Options{Prices: prices, History: history})
	srv.SetStreamTimingForTest(testStreamSecond, 0)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// TestPriceTipStream_NoReader_Returns503 — same prelude as the
// request endpoint: no PriceReader → 503 BEFORE the response
// switches into SSE mode.
func TestPriceTipStream_NoReader_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/price/tip/stream?asset=native&quote=fiat:USD")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestPriceTipStream_RejectsGranularity — URL discipline: ?granularity=
// is closed-bucket-only; on the tip stream URL it's a 400.
func TestPriceTipStream_RejectsGranularity(t *testing.T) {
	url := startTipStreamServer(t, &stubPriceReader{}, nil)
	resp, err := http.Get(url + "/v1/price/tip/stream?asset=native&quote=fiat:USD&granularity=1m")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestPriceTipStream_PreFlight404 — when the first compute returns ErrPriceNotFound, the stream
// returns 404 before switching to SSE. SSE cannot signal "not found" mid-stream, so emptiness
// must be detected pre-flight.
func TestPriceTipStream_PreFlight404(t *testing.T) {
	url := startTipStreamServer(t, &stubPriceReader{err: v1.ErrPriceNotFound}, nil)
	resp, err := http.Get(url + "/v1/price/tip/stream?asset=native&quote=fiat:USD")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestPriceTipStream_InitialEventEmittedSynchronously: the first event lands as the connection opens, not a
// window_seconds later, so a 60s window does not leave clients on heartbeats for a minute.
func TestPriceTipStream_InitialEventEmittedSynchronously(t *testing.T) {
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {
				AssetID:   "native",
				Quote:     "fiat:USD",
				Price:     "0.42",
				PriceType: "last_trade",
			},
		},
	}
	url := startTipStreamServer(t, prices, nil)

	// Use the maximum window so the first tick wouldn't fire for 60s
	// — if the test sees an event quickly, it MUST be the
	// pre-tick initial emission.
	resp, err := http.Get(url + "/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=60")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	data := readTipStreamEvent(t, br, 2*time.Second)
	if data == "" {
		t.Fatal("no event received within 2s — initial emit failed")
	}
	if !strings.Contains(data, `"price":"0.42"`) {
		t.Errorf("payload missing price: %s", data)
	}
	if !strings.Contains(data, `"price_type":"last_trade"`) {
		t.Errorf("payload missing price_type: %s", data)
	}
	// Per ADR-0018 stale stays false on tip even on the fallback.
	if !strings.Contains(data, `"stale":false`) {
		t.Errorf("stale flag wrong: %s", data)
	}
}

// TestPriceTipStream_WindowVWAPBranch — when history has fresh
// trades, the initial event uses the rolling-window VWAP (not the
// fallback). Confirms the per-tick path goes through computeTip.
func TestPriceTipStream_WindowVWAPBranch(t *testing.T) {
	now := time.Now().UTC()
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	hist := &stubHistoryReader{
		trades: []canonical.Trade{
			{
				Source: "soroswap", Ledger: 1,
				TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
				Timestamp:   now.Add(-2 * time.Second),
				Pair:        pair,
				BaseAmount:  canonical.NewAmount(big.NewInt(1)),
				QuoteAmount: canonical.NewAmount(big.NewInt(7)),
			},
		},
	}
	url := startTipStreamServer(t, &stubPriceReader{}, hist)

	resp, err := http.Get(url + "/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=60")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	data := readTipStreamEvent(t, br, 2*time.Second)
	if !strings.Contains(data, `"price_type":"vwap"`) {
		t.Errorf("expected vwap branch on initial event: %s", data)
	}
	if !strings.Contains(data, `"window_seconds":60`) {
		t.Errorf("expected window_seconds echoed: %s", data)
	}
}

// TestPriceTipStream_TickEmitsRepeatedly — at window_seconds=1 a
// short-lived stream sees multiple consecutive events. Validates
// the producer's ticker fires (at [testStreamSecond] per window second).
func TestPriceTipStream_TickEmitsRepeatedly(t *testing.T) {
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.5", PriceType: "last_trade"},
		},
	}
	url := startTipStreamServer(t, prices, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		url+"/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	const want = 2
	got := 0
	for got < want {
		ev := readTipStreamEvent(t, br, 2500*time.Millisecond)
		if ev == "" {
			break
		}
		got++
	}
	if got < want {
		t.Errorf("got %d events, want >= %d (window_seconds=1 should produce repeat ticks)", got, want)
	}
}

// deadlineCapturingTipHistoryReader records whether each TradesInRange call after the first ran
// under a bounded context. The first is the stream's synchronous prelude read, which
// RequestTimeout leaves undeadlined on `/stream` paths because the connection is long-lived.
type deadlineCapturingTipHistoryReader struct {
	stubHistoryReader
	mu              sync.Mutex
	calls           int
	tickHadDeadline chan bool
	trade           canonical.Trade
}

func (r *deadlineCapturingTipHistoryReader) TradesInRange(ctx context.Context, _ canonical.Pair, _, _ time.Time, _ int) ([]canonical.Trade, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()

	if call > 1 {
		_, hasDeadline := ctx.Deadline()
		select {
		case r.tickHadDeadline <- hasDeadline:
		default:
		}
	}
	return []canonical.Trade{r.trade}, nil
}

// TestPriceTipStream_TickIsBoundedByATimeout pins that the producer's per-tick computeTip runs under its OWN deadline,
// not the per-connection context (undeadlined on `/stream`), or a slow TradesInRange holds the producer and its DB
// connection for the client's lifetime. Uses crypto:BTC/fiat:USD (no alias pair) so prelude (call 1) and tick (2+) differ.
func TestPriceTipStream_TickIsBoundedByATimeout(t *testing.T) {
	base, err := canonical.ParseAsset("crypto:BTC")
	if err != nil {
		t.Fatalf("ParseAsset(crypto:BTC): %v", err)
	}
	quote, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset(fiat:USD): %v", err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	trade := canonical.Trade{
		Source: "soroswap", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000002",
		Timestamp:   time.Now().UTC().Add(-1 * time.Second),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1)),
		QuoteAmount: canonical.NewAmount(big.NewInt(7)),
	}
	hist := &deadlineCapturingTipHistoryReader{trade: trade, tickHadDeadline: make(chan bool, 1)}
	url := startTipStreamServer(t, &stubPriceReader{}, hist)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		url+"/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=1", nil)
	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		t.Fatalf("GET: %v", doErr)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	// Drain the synchronous initial event (prelude read, call #1 —
	// deliberately not asserted on: RequestTimeout excludes /stream
	// paths entirely, so the prelude is out of scope for this fix).
	_ = readTipStreamEvent(t, br, 2*time.Second)
	// Drain the first TICK event (window_seconds=1) so the producer
	// goroutine has actually made its second TradesInRange call.
	_ = readTipStreamEvent(t, br, 2500*time.Millisecond)

	select {
	case hadDeadline := <-hist.tickHadDeadline:
		if !hadDeadline {
			t.Error("per-tick computeTip call ran with an undeadlined context — " +
				"a slow/hung read can block the producer indefinitely (REL-01)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a per-tick TradesInRange call")
	}
}

// TestPriceTipStream_ClientDisconnectStopsProducer: when the client cancels, the producer exits via ctx.Done. Probed
// indirectly by a second connection with the same params; a panic or stuck send would break it. Checks cleanup does not deadlock.
func TestPriceTipStream_ClientDisconnectStopsProducer(t *testing.T) {
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "1.0", PriceType: "last_trade"},
		},
	}
	url := startTipStreamServer(t, prices, nil)

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			url+"/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			continue
		}
		_ = resp.Body.Close()
		cancel()
	}
	// If we reached here without the test deadlocking, all three
	// producers shut down cleanly.
}

// TestPriceTipStream_PayloadJSONIsValid — sanity-decode the data
// line as JSON to catch any payload-shape regressions.
func TestPriceTipStream_PayloadJSONIsValid(t *testing.T) {
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {Price: "0.7", PriceType: "last_trade"},
		},
	}
	url := startTipStreamServer(t, prices, nil)

	resp, err := http.Get(url + "/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=60")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	data := readTipStreamEvent(t, br, 2*time.Second)
	if data == "" {
		t.Fatal("no event")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		t.Fatalf("payload not valid JSON: %v\nraw: %s", err, data)
	}
	for _, key := range []string{"data", "as_of", "flags"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("payload missing %q: %s", key, data)
		}
	}
}

// tipStreamDivergenceCase is one cached-verdict shape and the flags a
// tip_update must carry for it. Shared by the two producer-shape tests
// below so both cover the same four shapes.
type tipStreamDivergenceCase struct {
	name        string
	verdicts    map[string]struct{ firing, checked bool }
	wantChecked bool
	wantWarning bool
}

func tipStreamDivergenceCases() []tipStreamDivergenceCase {
	return []tipStreamDivergenceCase{
		{
			name:        "clean verdict under the requested spelling",
			verdicts:    map[string]struct{ firing, checked bool }{"native": {firing: false, checked: true}},
			wantChecked: true,
		},
		{
			name:        "firing verdict under the requested spelling",
			verdicts:    map[string]struct{ firing, checked bool }{"native": {firing: true, checked: true}},
			wantChecked: true,
			wantWarning: true,
		},
		{
			// crypto:XLM's verdict is a check on a different
			// venue population than the one the stream was asked for.
			name:     "verdict only under a sibling spelling",
			verdicts: map[string]struct{ firing, checked bool }{"crypto:XLM": {firing: true, checked: true}},
		},
		{
			// The flag is a claim, not a default.
			name: "no verdict under any spelling",
		},
	}
}

// assertTipStreamDivergenceFlags opens the stream and holds the first TWO frames to the expected divergence flags: the
// handler builds the pre-flight frame and the producer builds later ones. It also checks lookups used the requested spelling only.
func assertTipStreamDivergenceFlags(t *testing.T, url string, div *stubAliasDivergenceLooker, tc tipStreamDivergenceCase) {
	t.Helper()
	resp, err := http.Get(url + "/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	wants := []string{
		fmt.Sprintf(`"divergence_checked":%t`, tc.wantChecked),
		fmt.Sprintf(`"divergence_warning":%t`, tc.wantWarning),
	}
	for _, frame := range []string{"pre-flight frame", "producer emission"} {
		data := readTipStreamEvent(t, br, 2500*time.Millisecond)
		if data == "" {
			t.Fatalf("%s: no event within 2.5s", frame)
		}
		for _, want := range wants {
			if !strings.Contains(data, want) {
				t.Errorf("%s missing %q: %s", frame, want, data)
			}
		}
	}

	asked := div.askedSpellings()
	if len(asked) == 0 {
		t.Fatal("the verdict was never looked up")
	}
	for i, a := range asked {
		if a != "native" {
			t.Errorf("lookup %d asked %q, want only the requested native", i, a)
		}
	}
}

// TestPriceTip_DivergenceCheckedFollowsAssetAliases (stream): every tip_update carries the verdict a GET on the same
// pair would, asked for the requested spelling only, so stream and GET agree on `divergence_checked`. Hub-less server,
// so both frames come off the per-connection producer.
func TestPriceTipStream_DivergenceCheckedFollowsAssetAliases(t *testing.T) {
	for _, tc := range tipStreamDivergenceCases() {
		t.Run(tc.name, func(t *testing.T) {
			prices := &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{
					"native/fiat:USD": {Price: "0.42", PriceType: "last_trade"},
				},
			}
			div := &stubAliasDivergenceLooker{verdicts: tc.verdicts}
			srv := v1.New(v1.Options{Prices: prices, Divergence: div})
			srv.SetStreamTimingForTest(testStreamSecond, 0)
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			assertTipStreamDivergenceFlags(t, ts.URL, div, tc)
		})
	}
}

// TestPriceTipStream_HubSharedProducerFollowsAssetAliases: the Hub-wired production shape. The pre-flight frame and
// the shared producer's publishes must carry the verdict the GET would, so `divergence_checked` never disagrees.
func TestPriceTipStream_HubSharedProducerFollowsAssetAliases(t *testing.T) {
	for _, tc := range tipStreamDivergenceCases() {
		t.Run(tc.name, func(t *testing.T) {
			prices := &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{
					"native/fiat:USD": {Price: "0.42", PriceType: "last_trade"},
				},
			}
			div := &stubAliasDivergenceLooker{verdicts: tc.verdicts}
			srv := v1.New(v1.Options{Prices: prices, Hub: streaming.NewHub(0), Divergence: div})
			srv.SetStreamTimingForTest(testStreamSecond, 0)
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			assertTipStreamDivergenceFlags(t, ts.URL, div, tc)
		})
	}
}

// TestPriceTipStream_CacheUnavailable503 — stream variant.
// The pre-stream synchronous computeTip call lands on cache-
// unavailable 503 instead of generic 500.
func TestPriceTipStream_CacheUnavailable503(t *testing.T) {
	srv := v1.New(v1.Options{Prices: tipCacheUnavailablePriceReader{}})
	tsv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsv.URL+"/v1/price/tip/stream?asset=native&quote=fiat:USD")
	assertCacheUnavailable(t, resp)
}

// TestPriceTipStream_HubSharesOneProducerAcrossConnections pins against per-connection DB queries: N viewers of a pair
// share ONE compute loop. Every connection still gets events, but compute calls stay near N pre-flights + ticks, not N x ticks.
func TestPriceTipStream_HubSharesOneProducerAcrossConnections(t *testing.T) {
	prices := &countingPriceReader{
		stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				"crypto:BTC/fiat:USD": {Price: "65000", PriceType: "last_trade"},
			},
		},
	}
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Prices: prices, Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const viewers = 5
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	perConnEvents := make([]int, viewers)
	for i := 0; i < viewers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				ts.URL+"/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=1", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("viewer %d GET: %v", i, err)
				return
			}
			defer resp.Body.Close()
			br := bufio.NewReader(resp.Body)
			for perConnEvents[i] < 2 {
				if ev := readTipStreamEvent(t, br, 2500*time.Millisecond); ev == "" {
					return
				}
				perConnEvents[i]++
			}
		}(i)
	}
	wg.Wait()

	for i, n := range perConnEvents {
		if n < 2 {
			t.Errorf("viewer %d received %d events, want >= 2 (hub fan-out must reach every subscriber)", i, n)
		}
	}

	// Compute budget: viewers pre-flights + one shared tick loop, ~1 + ceil(elapsed) computes;
	// the per-connection shape would burn viewers×(1+ticks). LatestPrice may be called more
	// than once per computeTip for alias fan-in, so a no-alias pair (crypto:BTC) keeps it 1:1.
	calls := int(prices.calls.Load())
	maxWant := viewers + 6
	if calls > maxWant {
		t.Errorf("LatestPrice called %d times for %d same-pair viewers, want <= %d — producer not shared",
			calls, viewers, maxWant)
	}
}

// TestPriceTipStream_StalledDivergenceLookupDoesNotDelayHeaders: the verdict is an overlay, cadence is the product. A
// stalled store must cost the flag, not the stream: headers and the first event go out inside the lookup's own
// sub-budget with the verdict unchecked, not the pre-flight budget.
func TestPriceTipStream_StalledDivergenceLookupDoesNotDelayHeaders(t *testing.T) {
	div := newGatedDivergenceLooker(false, true) // never released
	url := tipStreamServerWithLooker(t, div, stallBudget)

	// Generous ceiling so a genuinely wedged handler fails the assertion
	// below rather than the transport.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+tipStreamURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	headers := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The bound is the auxiliary sub-budget, not the 8s tick budget. 4s leaves the production 1s sub-budget 3x slack on a
	// loaded machine while staying far below the tick budget a regression would fall back to.
	if headers > 4*time.Second {
		t.Errorf("response headers took %v with a stalled divergence lookup, want < 4s — "+
			"the auxiliary lookup is riding the tick/pre-flight budget instead of its own",
			headers)
	}

	br := bufio.NewReader(resp.Body)
	data := readTipStreamEvent(t, br, 5*time.Second)
	if data == "" {
		t.Fatal("no tip_update within 5s — a stalled auxiliary lookup must not suppress the emission")
	}
	for _, want := range []string{`"divergence_checked":false`, `"divergence_warning":false`} {
		if !strings.Contains(data, want) {
			t.Errorf("emitted event missing %q: %s", want, data)
		}
	}
	if n := div.calls.Load(); n == 0 {
		t.Error("the divergence lookup was never attempted — the flag must be tried and abandoned, not skipped")
	}
}

// TestPriceTipStream_DivergenceBudgetDiscriminatesAtItsEdge brackets [tipStreamDivergenceBudget] on a single-spelling
// base: inside the budget the verdict reaches the wire; outside it the event goes out on time unchecked. Both halves
// matter, or "always unchecked" / "never bound" would pass. Budget is 300ms with production margins (100ms in, 200ms out).
func TestPriceTipStream_DivergenceBudgetDiscriminatesAtItsEdge(t *testing.T) {
	const budget = 300 * time.Millisecond
	for _, tc := range []struct {
		name        string
		delay       time.Duration
		wantChecked bool
		wantWarning bool
	}{
		{"inside the budget keeps the verdict", budget - 100*time.Millisecond, true, true},
		{"outside the budget drops the verdict", budget + 200*time.Millisecond, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &timedLooker{delay: tc.delay, firing: true, checked: true}
			url := tipStreamServerWithLooker(t, l, budget)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+tipStreamURL, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			start := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			headers := time.Since(start)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			data := readTipStreamEvent(t, bufio.NewReader(resp.Body), 6*time.Second)
			if data == "" {
				t.Fatal("no tip_update")
			}
			if n := l.calls.Load(); n != 1 {
				t.Fatalf("lookup calls = %d, want 1 — crypto:BTC has one spelling, so this case must not be an alias walk", n)
			}
			for _, want := range []string{
				fmt.Sprintf(`"divergence_checked":%t`, tc.wantChecked),
				fmt.Sprintf(`"divergence_warning":%t`, tc.wantWarning),
			} {
				if !strings.Contains(data, want) {
					t.Errorf("a %v lookup against a %v budget: event missing %q: %s",
						tc.delay, budget, want, data)
				}
			}
			if headers > 4*time.Second {
				t.Errorf("headers took %v, want < 4s", headers)
			}
		})
	}
}

// TestPriceTipStream_AsOfIsStampedAfterTheDivergenceLookup: `as_of` describes the event being emitted, so it is taken
// after the last step that can hold the emission back, not before the lookup.
func TestPriceTipStream_AsOfIsStampedAfterTheDivergenceLookup(t *testing.T) {
	div := newGatedDivergenceLooker(false, true)
	url := tipStreamServerWithLooker(t, div, 0)

	// Hold the lookup parked for a slice of real time well inside the
	// sub-budget, then record the instant it is allowed to return. An
	// as_of taken before the lookup is necessarily older than this.
	const held = 250 * time.Millisecond
	var released atomic.Int64
	go func() {
		<-div.entered
		time.Sleep(held)
		released.Store(time.Now().UTC().UnixNano())
		close(div.release)
	}()

	resp, err := http.Get(url + tipStreamURL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	data := readTipStreamEvent(t, bufio.NewReader(resp.Body), 5*time.Second)
	if data == "" {
		t.Fatal("no tip_update within 5s")
	}

	var payload struct {
		AsOf  time.Time `json:"as_of"`
		Flags struct {
			DivergenceChecked bool `json:"divergence_checked"`
		} `json:"flags"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode payload %s: %v", data, err)
	}
	if !payload.Flags.DivergenceChecked {
		t.Fatalf("the lookup did not answer inside its budget; this test cannot speak to as_of ordering: %s", data)
	}
	releasedAt := time.Unix(0, released.Load()).UTC()
	if payload.AsOf.Before(releasedAt) {
		t.Errorf("as_of = %s, lookup returned at %s (%v earlier) — as_of must be stamped after the lookup so it describes the event actually emitted",
			payload.AsOf.Format(time.RFC3339Nano), releasedAt.Format(time.RFC3339Nano),
			releasedAt.Sub(payload.AsOf))
	}
}

// TestPriceTipStream_StalledDivergenceLookupDoesNotDelayLaterEmissions is the recurring half of the stall: a tick's
// auxiliary read must not stretch its period, e.g. a one-second window emitting far slower than once a second.
func TestPriceTipStream_StalledDivergenceLookupDoesNotDelayLaterEmissions(t *testing.T) {
	div := newGatedDivergenceLooker(false, true) // never released
	url := tipStreamServerWithLooker(t, div, stallBudget)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+
		"/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	if data := readTipStreamEvent(t, br, 6*time.Second); data == "" {
		t.Fatal("no pre-flight frame")
	}
	preflight := time.Now()
	if data := readTipStreamEvent(t, br, 15*time.Second); data == "" {
		t.Fatal("no producer emission after the pre-flight frame")
	}
	gap := time.Since(preflight)

	// One window plus at most one sub-budget of stall (200ms here). 4.5s
	// is comfortably above that and comfortably below the 8s+ a lookup
	// on the tick budget produces.
	if gap > 4500*time.Millisecond {
		t.Errorf("gap to the next emission was %v on a 1-unit window with a stalled divergence lookup, want < 4.5s — "+
			"the auxiliary read is stretching the tick period", gap)
	}
}

// TestPriceTipStream_HubSharedProducerStallDoesNotDelayEmissions: the same guarantee on runSharedTipProducer, a separate
// loop with its own budget serving every viewer in production, invisible to the hub-less tests.
func TestPriceTipStream_HubSharedProducerStallDoesNotDelayEmissions(t *testing.T) {
	div := newGatedDivergenceLooker(false, true) // never released
	url := hubTipStreamServer(t, div, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+
		"/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	headers := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if headers > 4*time.Second {
		t.Errorf("hub-wired headers took %v with a stalled lookup, want < 4s", headers)
	}

	br := bufio.NewReader(resp.Body)
	if first := readTipStreamEvent(t, br, 8*time.Second); first == "" {
		t.Fatal("no pre-flight frame from the hub-wired handler")
	}
	at := time.Now()
	second := readTipStreamEvent(t, br, 15*time.Second)
	gap := time.Since(at)
	if second == "" {
		t.Fatal("the SHARED producer emitted nothing within 15s under a stalled lookup")
	}
	if gap > 4500*time.Millisecond {
		t.Errorf("the shared producer's gap was %v on a 1-unit window, want < 4.5s — runSharedTipProducer's lookup is riding the tick budget", gap)
	}
	for _, want := range []string{`"divergence_checked":false`, `"divergence_warning":false`} {
		if !strings.Contains(second, want) {
			t.Errorf("shared-producer event missing %q: %s", want, second)
		}
	}
}

// TestPriceTipStream_StallDoesNotClobberSingleSource: the stall path resets only the two divergence fields.
// single_source derives from the sources slice, not the verdict store, so a stall must not unset it.
func TestPriceTipStream_StallDoesNotClobberSingleSource(t *testing.T) {
	div := newGatedDivergenceLooker(false, true) // never released
	url := hubTipStreamServer(t, div, []string{"sdex"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+
		"/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=60", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	data := readTipStreamEvent(t, bufio.NewReader(resp.Body), 8*time.Second)
	if data == "" {
		t.Fatal("no frame")
	}
	var p struct {
		Sources []string `json:"sources"`
		Flags   struct {
			SingleSource      bool `json:"single_source"`
			DivergenceChecked bool `json:"divergence_checked"`
		} `json:"flags"`
	}
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	if p.Flags.DivergenceChecked {
		t.Fatalf("precondition unmet: the lookup did not stall: %s", data)
	}
	if len(p.Sources) != 1 {
		t.Fatalf("precondition unmet: sources = %v, want exactly one: %s", p.Sources, data)
	}
	if !p.Flags.SingleSource {
		t.Errorf("the stall reset cleared single_source, which the verdict store does not speak to (sources=%v): %s", p.Sources, data)
	}
}

// TestPriceTipStream_SustainedStallDoesNotFloodTheLog: end-to-end, the stream's warning is rate-limited to one line per
// interval and the shared lookup swallows deadline expiries, so a sustained stall leaves the log readable.
func TestPriceTipStream_SustainedStallDoesNotFloodTheLog(t *testing.T) {
	logs := &capturingHandler{}
	div := newGatedDivergenceLooker(false, true) // never released
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:BTC/fiat:USD": {Price: "65000", PriceType: "last_trade"},
		},
	}
	srv := v1.New(v1.Options{
		Prices: prices, Divergence: div,
		Hub: streaming.NewHub(0), Logger: slog.New(logs),
	})
	srv.SetStreamTimingForTest(testStreamSecond, stallBudget)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The per-frame wait is generous on purpose: an unbudgeted lookup stretches the window to
	// 8s+, and this test must still reach its log assertions rather than bail on a missing
	// frame. It pins the LOG, not the cadence (covered elsewhere).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+
		"/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	frames := 0
	for i := 0; i < 3; i++ {
		if readTipStreamEvent(t, br, 14*time.Second) != "" {
			frames++
		}
	}
	resp.Body.Close()
	cancel()
	time.Sleep(300 * time.Millisecond)

	if frames < 3 {
		t.Fatalf("precondition unmet: %d frames of 3 — the stall must not have stopped the stream", frames)
	}
	if n := logs.count("exceeded its tip-stream budget"); n > 1 {
		t.Errorf("stall warnings = %d across %d stalled emissions inside one interval, want 1", n, frames)
	}
	if n := logs.count("divergence lookup failed"); n > 0 {
		t.Errorf("the shared lookup logged %d 'divergence lookup failed' lines for deadline expiries it should swallow", n)
	}
}

// TestPriceTipStream_WithheldMidStreamEmitsMarker: a pair withheld after the stream opened emits a named price_withheld
// event on both producers (per-connection and Hub-shared), not silence that keepalives make look like a quiet market.
func TestPriceTipStream_WithheldMidStreamEmitsMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		hub  *streaming.Hub
	}{
		{"per_connection", nil},
		{"hub_shared", streaming.NewHub(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prices := &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{
					"native/fiat:USD": {Price: "0.5", PriceType: "last_trade"},
				},
			}
			gate := &flippableSubstanceGate{}
			srv := v1.New(v1.Options{Prices: prices, Substance: gate, Hub: tc.hub})
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				ts.URL+"/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the gate allows at connect)", resp.StatusCode)
			}

			br := bufio.NewReader(resp.Body)
			if ev, _ := readSSEFrame(br, 2*time.Second); ev != "tip_update" {
				t.Fatalf("first event = %q, want tip_update", ev)
			}
			gate.refuse.Store(true)

			var ev, data string
			for i := 0; i < 3 && ev != "price_withheld"; i++ {
				ev, data = readSSEFrame(br, 2500*time.Millisecond)
			}
			if ev != "price_withheld" {
				t.Fatalf("no price_withheld event after the pair was withheld (last event %q) — "+
					"the stream went silent instead", ev)
			}
			var got struct {
				AssetID string `json:"asset_id"`
				Quote   string `json:"quote"`
				Reason  string `json:"reason"`
				AsOf    string `json:"as_of"`
			}
			if err := json.Unmarshal([]byte(data), &got); err != nil {
				t.Fatalf("price_withheld data is not JSON: %v (%s)", err, data)
			}
			if got.AssetID != "native" || got.Quote != "fiat:USD" || got.Reason != string(v1.PriceWithheldSubstance) || got.AsOf == "" {
				t.Fatalf("price_withheld payload = %+v, want native / fiat:USD, reason %q, as_of set",
					got, v1.PriceWithheldSubstance)
			}
		})
	}
}

// countingPriceReader wraps stubPriceReader and counts LatestPrice
// calls — the proxy for "how many tip computations ran".
type countingPriceReader struct {
	stubPriceReader
	calls atomic.Int32
}

func (r *countingPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.calls.Add(1)
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

// flippableSubstanceGate allows until refuse is set, then withholds —
// a pair crossing the serve floor after its stream connected.
type flippableSubstanceGate struct{ refuse atomic.Bool }

func (g *flippableSubstanceGate) Allowed(context.Context, canonical.Asset, canonical.Asset, string) bool {
	return !g.refuse.Load()
}

func (g *flippableSubstanceGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return g.Allowed(ctx, base, quote, "probe"), true, pricingguard.FloorNone
}

// readSSEFrame reads one SSE frame and returns its event type and data,
// skipping comment lines. Returns empty strings on EOF or timeout.
func readSSEFrame(br *bufio.Reader, timeout time.Duration) (event, data string) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			return event, data
		}
		switch {
		case strings.HasPrefix(line, ":"):
		case line == "\n":
			if data != "" {
				return event, data
			}
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
		}
	}
	return event, data
}
