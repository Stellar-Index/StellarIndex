package v1_test

import (
	"context"
	"errors"
	"log/slog"
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
)

// gatedDivergenceLooker models a verdict store that has stopped
// answering: every lookup parks until the test releases it, or until the
// caller's context gives up on it. Parking on a channel rather than
// sleeping keeps the tests below fast and free of a sleep's timing
// slack — the only real time they spend is the sub-budget the handler
// itself is being measured against.
//
// A store that ignored its context could not be modelled this way, and
// is not what the wired looker does: it reads Redis through go-redis,
// which honours the deadline (the 8.0036s that prompted this work landed
// exactly on the pre-flight budget, i.e. the read was cut by the
// context, not by the store answering).
type gatedDivergenceLooker struct {
	release chan struct{} // closed by the test to let parked calls return
	entered chan struct{} // closed on the first call

	once    sync.Once
	calls   atomic.Int32
	firing  bool
	checked bool
}

func newGatedDivergenceLooker(firing, checked bool) *gatedDivergenceLooker {
	return &gatedDivergenceLooker{
		release: make(chan struct{}),
		entered: make(chan struct{}),
		firing:  firing,
		checked: checked,
	}
}

func (g *gatedDivergenceLooker) DivergenceFiringFor(ctx context.Context, _, _ canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	g.calls.Add(1)
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return g.firing, g.checked, 0, nil
	case <-ctx.Done():
		return false, false, 0, ctx.Err()
	}
}

// stallBudget is the divergence sub-budget the stall tests run against.
// A never-released lookup costs exactly this, so it is what each stalled
// emission waits; the regression they guard against is the lookup riding
// the UNSCALED 8s tick budget instead, which every bound below still
// separates by the same margin as at the production 1s.
const stallBudget = 100 * time.Millisecond

// tipStreamServerWithLooker wires a hub-less server (so both the
// pre-flight frame and every later frame come from this process's own
// code paths) over one priced pair. A zero budget keeps the production
// [v1.TipStreamDivergenceBudget].
func tipStreamServerWithLooker(t *testing.T, div v1.DivergenceLooker, budget time.Duration) string {
	t.Helper()
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:BTC/fiat:USD": {Price: "65000", PriceType: "last_trade"},
		},
	}
	srv := v1.New(v1.Options{Prices: prices, Divergence: div})
	srv.SetStreamTimingForTest(testStreamSecond, budget)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// tipStreamURL is the stream under test. window_seconds=60 (6s at
// [testStreamSecond]) keeps the producer from ticking during the test, so
// what is measured is the pre-flight path — the one that sits between the client's request and
// the response headers.
const tipStreamURL = "/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=60"

// timedLooker answers after a fixed delay, honouring its context. It is
// how the budget EDGE is probed: a gate that reports "unchecked"
// whenever the lookup is not instant would satisfy every stall test in
// this file while throwing the verdict away on each healthy-but-loaded
// read, so the guard has to discriminate, not merely fire.
type timedLooker struct {
	delay   time.Duration
	firing  bool
	checked bool
	calls   atomic.Int32
}

func (l *timedLooker) DivergenceFiringFor(ctx context.Context, _, _ canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	l.calls.Add(1)
	t := time.NewTimer(l.delay)
	defer t.Stop()
	select {
	case <-t.C:
		return l.firing, l.checked, 0, nil
	case <-ctx.Done():
		return false, false, 0, ctx.Err()
	}
}

// hubTipStreamServer wires the PRODUCTION shape: a Hub, so the recurring
// frames come from the ONE shared runSharedTipProducer loop rather than
// the per-connection fallback. Hub-less servers never execute that loop,
// so without this helper the code path r1 actually runs is untested.
func hubTipStreamServer(t *testing.T, div v1.DivergenceLooker, sources []string) string {
	t.Helper()
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:BTC/fiat:USD": {Price: "65000", PriceType: "last_trade"},
		},
		sources: map[string][]string{"crypto:BTC/fiat:USD": sources},
	}
	srv := v1.New(v1.Options{Prices: prices, Divergence: div, Hub: streaming.NewHub(0)})
	srv.SetStreamTimingForTest(testStreamSecond, stallBudget)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// capturingHandler collects log messages so the rate limit can be
// asserted on the log itself rather than inferred.
type capturingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) count(sub string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// budgetBurningPrices spends the caller's whole request budget before
// the handler reaches its divergence lookup, so lookupDivergenceFlag is
// entered with an ALREADY-EXPIRED request context. That is the ordinary
// shape of a request that overran on an earlier read, not a rare one.
type budgetBurningPrices struct {
	inner *stubPriceReader
	burn  time.Duration
}

func (p *budgetBurningPrices) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	select {
	case <-time.After(p.burn):
	case <-ctx.Done():
		// Keep going regardless: the point is to reach the flag walk with
		// the budget already spent, which is what the handler does too —
		// the price is in hand and only the enrichment is left.
		time.Sleep(20 * time.Millisecond)
	}
	// ctx is threaded through unchanged; stubPriceReader ignores it, so
	// the expired deadline reaches the handler rather than being laundered.
	return p.inner.LatestPrice(ctx, a, q)
}

func (p *budgetBurningPrices) RecentClosedSnapshots(ctx context.Context, a, q canonical.Asset, n int) ([]v1.PriceSnapshot, error) {
	return p.inner.RecentClosedSnapshots(ctx, a, q, n)
}

// storeDownLooker returns a GENUINE store failure — what go-redis
// surfaces for connection refused / LOADING, or what a malformed cached
// blob produces — never a context error.
type storeDownLooker struct{ calls atomic.Int32 }

var errDivergenceStoreDown = errors.New("divergence: cache get div:crypto:BTC/fiat:USD: dial tcp: connect: connection refused")

func (l *storeDownLooker) DivergenceFiringFor(context.Context, canonical.Asset, canonical.Asset) (firing, checked bool, window time.Duration, err error) {
	l.calls.Add(1)
	return false, false, 0, errDivergenceStoreDown
}

// TestPriceDivergence_GenuineStoreErrorIsLoggedEvenPastTheBudget — the
// deadline suppression must key on the ERROR, not on the context.
//
// /v1/price, its ?window= variant, /v1/price/tip and /v1/vwap have no
// stall reporter of their own; the tip stream's rate-limited warning
// covers only the stream. So suppressing on "the context is done"
// silences a real store outage on every one of them whenever the request
// budget has already blown — an observability regression on surfaces the
// sub-budget work does not otherwise touch.
func TestPriceDivergence_GenuineStoreErrorIsLoggedEvenPastTheBudget(t *testing.T) {
	for _, path := range []string{
		"/v1/price?asset=crypto:BTC&quote=fiat:USD",
		"/v1/price/tip?asset=crypto:BTC&quote=fiat:USD",
	} {
		t.Run(path, func(t *testing.T) {
			logs := &capturingHandler{}
			div := &storeDownLooker{}
			srv := v1.New(v1.Options{
				Prices: &budgetBurningPrices{
					inner: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
						"crypto:BTC/fiat:USD": {Price: "65000", PriceType: "vwap"},
					}},
					burn: 400 * time.Millisecond,
				},
				Divergence:     div,
				Logger:         slog.New(logs),
				RequestTimeout: 150 * time.Millisecond,
			})
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			resp.Body.Close()

			if div.calls.Load() == 0 {
				t.Fatalf("the handler never reached the divergence lookup on %s; budgetBurningPrices guarantees the budget "+
					"is already spent by the time the flag walk runs, so this precondition must hold", path)
			}
			if logs.count("divergence lookup failed") == 0 {
				t.Errorf("a genuine store failure (%v) reached the lookup with an expired request context and was logged nowhere; "+
					"%s has no stall reporter to replace it", errDivergenceStoreDown, path)
			}
		})
	}
}
