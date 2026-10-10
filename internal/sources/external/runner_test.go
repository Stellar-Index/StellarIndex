package external

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// mockStreamer drives a hand-controlled channel for test purposes.
type mockStreamer struct {
	name    string
	trades  []canonical.Trade
	startFn func(context.Context, []canonical.Pair) (<-chan canonical.Trade, error)
}

func (m *mockStreamer) Name() string { return m.name }
func (m *mockStreamer) Class() Class { return ClassExchange }
func (m *mockStreamer) Start(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error) {
	if m.startFn != nil {
		return m.startFn(ctx, pairs)
	}
	out := make(chan canonical.Trade, len(m.trades))
	for _, t := range m.trades {
		out <- t
	}
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

// newTestPair builds a simple XLM/USDT canonical.Pair.
func newTestPair(t *testing.T) canonical.Pair {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	p, err := canonical.NewPair(xlm, usdt)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

// testTrade builds a canonical.Trade with minimum valid fields.
func testTrade(t *testing.T, source string, ledger uint32) canonical.Trade {
	t.Helper()
	return canonical.Trade{
		Source:      source,
		Ledger:      ledger,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
		OpIndex:     0,
		Timestamp:   time.Unix(1_745_000_000, 0).UTC(),
		Pair:        newTestPair(t),
		BaseAmount:  canonical.NewAmount(big.NewInt(100000000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(17500000)),
	}
}

func TestRun_NoStreamers_ReturnsUsableWait(t *testing.T) {
	ctx := context.Background()
	wait, err := Run(ctx, nil, nil, make(chan consumer.Event, 1), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if wait == nil {
		t.Fatal("Run returned nil wait func")
	}
	// Calling wait() on an empty runner is a no-op — must return
	// promptly.
	wait()
}

func TestRun_ForwardsTradesAndWrapsAsEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	trades := []canonical.Trade{
		testTrade(t, "mock-exchange-1", 1),
		testTrade(t, "mock-exchange-1", 2),
	}
	m := &mockStreamer{name: "mock-exchange-1", trades: trades}

	sink := make(chan consumer.Event, 4)
	wait, err := Run(ctx, []StreamerSpec{{Streamer: m, Pairs: []canonical.Pair{newTestPair(t)}}}, nil, sink, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Drain.
	got := make([]consumer.Event, 0, 2)
	for len(got) < 2 {
		select {
		case e := <-sink:
			got = append(got, e)
		case <-ctx.Done():
			t.Fatalf("timeout waiting for events; got %d", len(got))
		}
	}

	cancel()
	wait()

	// Each event must be a TradeEvent wrapping the original Trade.
	for i, e := range got {
		te, ok := e.(TradeEvent)
		if !ok {
			t.Errorf("got[%d] is %T, want TradeEvent", i, e)
			continue
		}
		if te.Source() != "mock-exchange-1" {
			t.Errorf("got[%d].Source() = %q want mock-exchange-1", i, te.Source())
		}
		if te.EventKind() != "external.trade" {
			t.Errorf("got[%d].EventKind() = %q", i, te.EventKind())
		}
	}
}

func TestRun_FatalStartError_SurfacesSynchronously(t *testing.T) {
	// A streamer whose Start returns an error must cause Run to
	// return a non-nil error without spawning the goroutine.
	m := &mockStreamer{
		name: "mock-failing",
		startFn: func(ctx context.Context, pairs []canonical.Pair) (<-chan canonical.Trade, error) {
			return nil, errors.New("bad config")
		},
	}
	ctx := context.Background()
	_, err := Run(ctx, []StreamerSpec{{Streamer: m, Pairs: []canonical.Pair{newTestPair(t)}}}, nil, make(chan consumer.Event, 1), nil)
	if err == nil {
		t.Fatal("expected Run to return an error; got nil")
	}
}

// mockPoller satisfies the Poller interface with scripted outputs.
type mockPoller struct {
	name     string
	interval time.Duration
	trades   []canonical.Trade
	updates  []canonical.OracleUpdate
	calls    int
}

func (m *mockPoller) Name() string                { return m.name }
func (m *mockPoller) Class() Class                { return ClassExchange }
func (m *mockPoller) PollInterval() time.Duration { return m.interval }
func (m *mockPoller) PollOnce(ctx context.Context, pairs []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) {
	m.calls++
	return m.trades, m.updates, nil
}

func testOracleUpdate(t *testing.T, source string) canonical.OracleUpdate {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	return canonical.OracleUpdate{
		Source:    source,
		Ledger:    0,
		TxHash:    "0000000000000000000000000000000000000000000000000000000000000000",
		OpIndex:   0,
		Timestamp: time.Unix(1_745_000_000, 0).UTC(),
		Asset:     xlm,
		Quote:     usd,
		Price:     canonical.NewAmount(big.NewInt(17500000)),
		Decimals:  8,
	}
}

func TestRun_PollerFiresImmediatelyAndEmitsUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p := &mockPoller{
		name:     "mock-poller",
		interval: 50 * time.Millisecond,
		updates: []canonical.OracleUpdate{
			testOracleUpdate(t, "mock-poller"),
		},
	}

	sink := make(chan consumer.Event, 8)
	wait, err := Run(ctx, nil, []PollerSpec{{Poller: p, Pairs: []canonical.Pair{newTestPair(t)}}}, sink, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// First event arrives from the immediate startup poll, not
	// after the interval elapses.
	select {
	case ev := <-sink:
		ue, ok := ev.(UpdateEvent)
		if !ok {
			t.Errorf("got %T want UpdateEvent", ev)
		}
		if ue.Source() != "mock-poller" {
			t.Errorf("source = %q want mock-poller", ue.Source())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected immediate poll; no event received")
	}

	cancel()
	wait()

	if p.calls < 1 {
		t.Errorf("PollOnce called %d times, want ≥1", p.calls)
	}
}

func TestRun_RejectsNonPositivePollInterval(t *testing.T) {
	p := &mockPoller{
		name:     "bad-poller",
		interval: 0,
	}
	_, err := Run(context.Background(), nil, []PollerSpec{{Poller: p, Pairs: []canonical.Pair{newTestPair(t)}}}, make(chan consumer.Event, 1), nil)
	if err == nil {
		t.Error("expected error for non-positive PollInterval")
	}
}

// TestRun_LatePollerConfigErrorDoesNotDeadlock is the regression test
// for a startup deadlock: if runPoller goroutines are bound to the raw
// parent ctx, teardown() (triggered when a
// LATER poller in the same Run call fails config validation) only
// cancels the derived streamerCtx. An earlier poller's goroutine
// bound to the raw ctx never observes that cancellation, so
// teardown's wg.Wait() — and thus Run() itself — hangs forever
// whenever the caller's own ctx isn't independently cancelled (the
// normal case for a startup-time config error, matched here by
// context.Background()).
func TestRun_LatePollerConfigErrorDoesNotDeadlock(t *testing.T) {
	good := &mockPoller{
		name:     "good-poller",
		interval: 10 * time.Millisecond,
	}
	bad := &mockPoller{
		name:     "bad-poller",
		interval: 0, // triggers Run's non-positive-PollInterval validation error
	}

	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = Run(context.Background(),
			nil,
			[]PollerSpec{
				{Poller: good, Pairs: []canonical.Pair{newTestPair(t)}},
				{Poller: bad, Pairs: []canonical.Pair{newTestPair(t)}},
			},
			make(chan consumer.Event, 8), nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s — teardown() deadlocked waiting for the earlier poller's goroutine, which was bound to the un-cancelled parent ctx instead of the derived streamerCtx")
	}
	if runErr == nil {
		t.Error("expected Run to return an error for the non-positive PollInterval poller")
	}
}

func TestRun_CtxCancelClosesForwarders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &mockStreamer{name: "mock-exchange-2"} // no trades — will hang until ctx done

	wait, err := Run(ctx, []StreamerSpec{{Streamer: m, Pairs: []canonical.Pair{newTestPair(t)}}}, nil, make(chan consumer.Event, 1), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Cancel and confirm forwarder exits.
	cancel()

	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run's wait() did not complete after ctx cancel")
	}
}

// TestRun_DeclaresFiniteOracleBudgetForEveryPoller pins the two edges of
// the runner's staleness declaration: a slow poll interval raises a
// fast registry cadence (a row can never be fresher than our fetch),
// and a poller the registry has no cadence for still gets a finite
// budget — never the +Inf that stellarindex_oracle_stale cannot exceed.
func TestRun_DeclaresFiniteOracleBudgetForEveryPoller(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     float64
	}{
		{name: "coingecko", interval: time.Hour, want: obs.OracleStaleBudgetMultiplier * 3600},
		{name: "ecb", interval: 6 * time.Hour, want: obs.OracleStaleBudgetMultiplier * 86400},
		{name: "test-unregistered-poller", interval: 2 * time.Minute, want: obs.OracleStaleBudgetMultiplier * 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			p := &mockPoller{name: tc.name, interval: tc.interval}
			wait, err := Run(ctx, nil, []PollerSpec{{Poller: p}}, make(chan consumer.Event, 1), nil)
			if err != nil {
				cancel()
				t.Fatalf("Run: %v", err)
			}
			cancel()
			wait()

			if got := obs.OracleStalenessBudget(tc.name, "crypto:XLM"); got != tc.want {
				t.Errorf("budget(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// A streamer dust guard of a flat 100_000 units at the external 10^8
// scale is 0.001 of WHATEVER the quote asset is. binance/pairs.yaml and
// bitstamp/pairs.go both configure XLM/BTC, where 0.001 BTC is ~$100, so
// every XLM/BTC print under roughly a thousand XLM would be silently
// dropped. The drop is size-biased, so the surviving XLM/BTC volume +
// VWAP would skew toward large trades.
//
// These tests pin the CORRECTED thresholds: the floor is $0.001 of
// notional expressed in the quote asset's own units, so the same
// $0.001 applies on XLM/BTC as on XLM/USDT.

// dustPair builds an XLM/<quote> pair for the dust tests.
func dustPair(t *testing.T, quote canonical.Asset) canonical.Pair {
	t.Helper()
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset(XLM): %v", err)
	}
	p, err := canonical.NewPair(xlm, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

func cryptoAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewCryptoAsset(code)
	if err != nil {
		t.Fatalf("NewCryptoAsset(%s): %v", code, err)
	}
	return a
}

func fiatAsset(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%s): %v", code, err)
	}
	return a
}

// TestMinStreamQuoteUnits_IsUSDDenominated asserts the CORRECTED
// floor value per quote asset, not merely that one exists.
//
//	units = $0.001 / usd_per_whole_unit × 10^8
func TestMinStreamQuoteUnits_IsUSDDenominated(t *testing.T) {
	cases := []struct {
		name  string
		quote canonical.Asset
		want  string
	}{
		// $1/unit → 0.001 units → 100_000 at 10^8. This matches
		// the flat constant; USDT/fiat behaviour must not change.
		{"usdt_stablecoin", cryptoAsset(t, "USDT"), "100000"},
		{"fiat_usd", fiatAsset(t, "USD"), "100000"},
		{"fiat_eur", fiatAsset(t, "EUR"), "100000"},
		// ~$100k/unit → $0.001 is 1e-8 BTC → 1 unit at 10^8. A flat
		// 100_000 here would be ~$100 of notional.
		{"btc", cryptoAsset(t, "BTC"), "1"},
		// ~$3k/unit → 1e11/3e9 = 33 units.
		{"eth", cryptoAsset(t, "ETH"), "33"},
		// $0.30/unit → 1e11/3e5 = 333_333 units.
		{"xlm", cryptoAsset(t, "XLM"), "333333"},
		// No USD reference → floor of 1, i.e. only a zero quote leg
		// counts as dust. SHIB is on the ADR-0014 allow-list but is
		// not a quote leg on any venue we stream.
		{"unreferenced_crypto", cryptoAsset(t, "SHIB"), "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := minStreamQuoteUnits(tc.quote).String()
			if got != tc.want {
				t.Errorf("minStreamQuoteUnits(%s) = %s, want %s",
					tc.quote, got, tc.want)
			}
		})
	}
}

// A quote asset worth more than $100k/unit rounds the $0.001 floor to 0
// units; the clamp keeps it at 1 so a zero quote leg is still dust.
func TestDustFloorUnits_ClampsToOne(t *testing.T) {
	for _, ref := range []uint64{dustFloorUSDMicros * externalQuoteScale, dustFloorUSDMicros*externalQuoteScale + 1, 1 << 62} {
		if got := dustFloorUnits(ref).String(); got != "1" {
			t.Errorf("dustFloorUnits(%d) = %s, want 1", ref, got)
		}
	}
	if got := dustFloorUnits(dustFloorUSDMicros*externalQuoteScale/2 - 1).String(); got != "2" {
		t.Errorf("dustFloorUnits(just under $50k) = %s, want 2 (floor division, no clamp)", got)
	}
}

// TestForwardTrades_DustFloorIsQuoteAssetAware drives the real
// streamer path. The BTC-quoted case is the quote-blind-floor regression: a
// 0.0005 BTC fill (~$50 of notional) is a genuine retail-size
// XLM/BTC print and MUST reach the sink; a flat 100_000-unit floor
// drops it.
func TestForwardTrades_DustFloorIsQuoteAssetAware(t *testing.T) {
	cases := []struct {
		name        string
		quote       canonical.Asset
		quoteAmount int64
		wantForward bool
	}{
		{
			// 0.0005 BTC ≈ $50. Real print; a flat floor DROPS it.
			name:        "btc_half_milli_is_real_money",
			quote:       cryptoAsset(t, "BTC"),
			quoteAmount: 50_000,
			wantForward: true,
		},
		{
			// 1 satoshi ≈ $0.001 — exactly at the corrected floor.
			name:        "btc_one_satoshi_at_floor",
			quote:       cryptoAsset(t, "BTC"),
			quoteAmount: 1,
			wantForward: true,
		},
		{
			// A zero quote leg has no price at any denomination.
			name:        "btc_zero_quote_is_dust",
			quote:       cryptoAsset(t, "BTC"),
			quoteAmount: 0,
			wantForward: false,
		},
		{
			// $0.0005 in USDT — genuine dust, still dropped.
			name:        "usdt_sub_tenth_cent_is_dust",
			quote:       cryptoAsset(t, "USDT"),
			quoteAmount: 50_000,
			wantForward: false,
		},
		{
			// $0.001 in USDT — exactly at the floor, kept.
			name:        "usdt_at_floor_is_kept",
			quote:       cryptoAsset(t, "USDT"),
			quoteAmount: 100_000,
			wantForward: true,
		},
		{
			// Fiat quote legs keep the flat threshold.
			name:        "fiat_eur_sub_tenth_cent_is_dust",
			quote:       fiatAsset(t, "EUR"),
			quoteAmount: 99_999,
			wantForward: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			trade := testTrade(t, "dust-venue", 1)
			trade.Pair = dustPair(t, tc.quote)
			trade.QuoteAmount = canonical.NewAmount(big.NewInt(tc.quoteAmount))

			in := make(chan canonical.Trade, 1)
			in <- trade
			close(in)

			sink := make(chan consumer.Event, 1)
			forwardTrades(ctx, "dust-venue", in, sink,
				slog.New(slog.NewTextHandler(io.Discard, nil)))

			select {
			case ev := <-sink:
				if !tc.wantForward {
					t.Fatalf("quote=%d %s: forwarded %T, want dropped as dust",
						tc.quoteAmount, tc.quote, ev)
				}
				te, ok := ev.(TradeEvent)
				if !ok {
					t.Fatalf("got %T want TradeEvent", ev)
				}
				if te.Trade.QuoteAmount.String() != trade.QuoteAmount.String() {
					t.Errorf("QuoteAmount = %s, want %s",
						te.Trade.QuoteAmount, trade.QuoteAmount)
				}
			default:
				if tc.wantForward {
					t.Fatalf("quote=%d %s: dropped as dust, want forwarded",
						tc.quoteAmount, tc.quote)
				}
			}
		})
	}
}

// The USD reference table must only name tickers the canonical
// allow-list accepts — otherwise an entry is silently dead and the
// quote it was meant to cover falls back to noDustFloor.
func TestCryptoQuoteUSDMicros_CodesAreOnCanonicalAllowList(t *testing.T) {
	for code := range cryptoQuoteUSDMicros {
		if !canonical.IsKnownCrypto(code) {
			t.Errorf("cryptoQuoteUSDMicros has %q, not on the ADR-0014 crypto allow-list", code)
		}
	}
}

func TestPlausibleTradeTime(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   time.Time
		want bool
	}{
		{"now", now, true},
		{"within_skew", now.Add(4 * time.Minute), true},
		{"past_skew", now.Add(6 * time.Minute), false},
		{"microseconds_read_as_millis", time.UnixMilli(now.UnixMilli() * 1000).UTC(), false},
		{"seconds_read_as_millis", time.UnixMilli(now.Unix()).UTC(), false},
		{"recent_past", now.Add(-48 * time.Hour), true},
	}
	for _, tc := range cases {
		if got := plausibleTradeTime(tc.ts, now); got != tc.want {
			t.Errorf("%s: plausibleTradeTime(%s) = %v, want %v", tc.name, tc.ts, got, tc.want)
		}
	}
}

func TestForwardTrades_DropsImplausibleTimestamp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	bad := testTrade(t, "ts-venue", 1)
	bad.Timestamp = time.Now().Add(24 * time.Hour)
	good := testTrade(t, "ts-venue", 2)

	in := make(chan canonical.Trade, 2)
	in <- bad
	in <- good
	close(in)
	sink := make(chan consumer.Event, 2)
	forwardTrades(ctx, "ts-venue", in, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if len(sink) != 1 {
		t.Fatalf("forwarded %d trades, want 1 (the plausible one)", len(sink))
	}
	if te := (<-sink).(TradeEvent); te.Trade.Ledger != 2 {
		t.Errorf("forwarded ledger %d, want 2", te.Trade.Ledger)
	}
}

func TestEmitPollResults_DropsImplausibleTimestamp(t *testing.T) {
	bad := testTrade(t, "ts-venue", 1)
	bad.Timestamp = time.Now().Add(24 * time.Hour)
	good := testTrade(t, "ts-venue", 2)

	sink := make(chan consumer.Event, 2)
	emitPollResults(context.Background(), "ts-venue", sink, []canonical.Trade{bad, good}, nil)

	if len(sink) != 1 {
		t.Fatalf("emitted %d trades, want 1 (the plausible one)", len(sink))
	}
}

// panicPoller panics on PollOnce — simulates a decode/parse fault
// surfacing inside a poller's tick.
type panicPoller struct{ interval time.Duration }

func (panicPoller) Name() string                  { return "panic-poller" }
func (panicPoller) Class() Class                  { return ClassExchange }
func (p panicPoller) PollInterval() time.Duration { return p.interval }
func (panicPoller) PollOnce(context.Context, []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) {
	panic("simulated poller fault in PollOnce")
}

// TestRun_RecoversPanickingPoller proves that a panic in a
// poller's tick (the per-connector goroutine Run fans out) is CONTAINED.
// The poller's immediate first doPoll panics; the guard recovers it, the
// goroutine unwinds, and Run's wait() returns — instead of the panic
// crashing the whole ingest process.
//
// Proven red: without the `defer worker.Recover(...)` added to the
// poller goroutine, the panic below is unrecovered in a DETACHED
// goroutine and takes the entire test binary down. With the guard,
// wait() returns.
func TestRun_RecoversPanickingPoller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sink := make(chan consumer.Event, 1)
	p := panicPoller{interval: time.Hour} // long interval — only the immediate poll fires
	wait, err := Run(ctx, nil, []PollerSpec{{Poller: p, Pairs: []canonical.Pair{newTestPair(t)}}}, sink, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := make(chan struct{})
	go func() { wait(); close(done) }()

	select {
	case <-done:
		// wait() returned — the poller goroutine recovered its panic and
		// exited instead of crashing the process.
	case <-time.After(3 * time.Second):
		t.Fatal("external.Run wait() did not return after the poller panicked — the per-connector recover is missing")
	}
}

// errPoller is a Poller that returns an error from PollOnce. Used
// to verify runPoller's transient-error path: the loop must log +
// continue rather than abort, since poll-based sources hit
// rate-limits and network blips routinely.
type errPoller struct {
	name     string
	interval time.Duration
	err      error
	calls    int
}

func (p *errPoller) Name() string                { return p.name }
func (p *errPoller) Class() Class                { return ClassExchange }
func (p *errPoller) PollInterval() time.Duration { return p.interval }
func (p *errPoller) PollOnce(ctx context.Context, pairs []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) {
	p.calls++
	return nil, nil, p.err
}

// runPoller covers two output emission branches: updates (already
// pinned by TestRun_PollerFiresImmediatelyAndEmitsUpdates) and
// trades. This test exercises the trades branch.

func TestRun_PollerEmitsTrades(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p := &mockPoller{
		name:     "trade-poller",
		interval: 50 * time.Millisecond,
		trades:   []canonical.Trade{testTrade(t, "trade-poller", 1)},
	}
	sink := make(chan consumer.Event, 8)
	wait, err := Run(ctx, nil, []PollerSpec{{Poller: p, Pairs: []canonical.Pair{newTestPair(t)}}}, sink, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case ev := <-sink:
		te, ok := ev.(TradeEvent)
		if !ok {
			t.Errorf("got %T want TradeEvent", ev)
		}
		if te.Trade.Source != "trade-poller" {
			t.Errorf("trade.Source = %q want trade-poller", te.Trade.Source)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected trade event, none received")
	}

	cancel()
	wait()
}

// runPoller's PollOnce-returns-error path must log + continue rather
// than abort the goroutine — transient errors are the norm for
// REST/HTTP pollers. Verify the function still survives long enough
// to retry on the next tick (calls ≥ 2).

func TestRun_PollerContinuesAfterError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	p := &errPoller{
		name:     "broken-poller",
		interval: 50 * time.Millisecond,
		err:      errors.New("upstream 503"),
	}
	sink := make(chan consumer.Event, 1)
	wait, err := Run(ctx, nil, []PollerSpec{{Poller: p, Pairs: []canonical.Pair{newTestPair(t)}}}, sink, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Let several intervals elapse, then cancel.
	time.Sleep(250 * time.Millisecond)
	cancel()
	wait()

	if p.calls < 2 {
		t.Errorf("calls = %d, want ≥2 (poller didn't retry after error)", p.calls)
	}
}

// Trades already buffered when ctx is cancelled must still reach the sink.
func TestForwardTrades_FlushesBufferedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	in := make(chan canonical.Trade, 3)
	for i := uint32(1); i <= 3; i++ {
		in <- testTrade(t, "flush-venue", i)
	}
	sink := make(chan consumer.Event, 3)

	forwardTrades(ctx, "flush-venue", in, sink,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := len(sink); got != 3 {
		t.Fatalf("flushed %d buffered trades after cancel, want 3", got)
	}
}
