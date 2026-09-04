package v1_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (g *gatedDivergenceLooker) DivergenceFiringFor(ctx context.Context, _ canonical.Asset) (firing, checked bool, err error) {
	g.calls.Add(1)
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return g.firing, g.checked, nil
	case <-ctx.Done():
		return false, false, ctx.Err()
	}
}

// tipStreamServerWithLooker wires a hub-less server (so both the
// pre-flight frame and every later frame come from this process's own
// code paths) over one priced pair.
func tipStreamServerWithLooker(t *testing.T, div v1.DivergenceLooker) string {
	t.Helper()
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"crypto:BTC/fiat:USD": {Price: "65000", PriceType: "last_trade"},
		},
	}
	srv := v1.New(v1.Options{Prices: prices, Divergence: div})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// tipStreamURL is the stream under test. window_seconds=60 keeps the
// producer from ticking during the test, so what is measured is the
// pre-flight path — the one that sits between the client's request and
// the response headers.
const tipStreamURL = "/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=60"

// TestPriceTipStream_StalledDivergenceLookupDoesNotDelayHeaders — the
// divergence verdict is an overlay; the cadence is the product. A
// verdict store that has stopped answering must cost the FLAG, not the
// stream: the response headers and the first event still go out inside
// the lookup's own sub-budget, carrying the verdict unchecked.
//
// Before this, the lookup inherited the pre-flight budget, so a stalled
// store held the headers for the full 8s (measured 8.0036s) — an
// optional flag setting time-to-first-byte for the whole connection.
func TestPriceTipStream_StalledDivergenceLookupDoesNotDelayHeaders(t *testing.T) {
	div := newGatedDivergenceLooker(false, true) // never released
	url := tipStreamServerWithLooker(t, div)

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
	// The bound is the auxiliary sub-budget (1s), not the tick budget
	// (8s). 4s leaves the sub-budget three times its own width of slack
	// on a loaded machine while staying far below the tick budget a
	// regression would fall back to.
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

func (l *timedLooker) DivergenceFiringFor(ctx context.Context, _ canonical.Asset) (firing, checked bool, err error) {
	l.calls.Add(1)
	t := time.NewTimer(l.delay)
	defer t.Stop()
	select {
	case <-t.C:
		return l.firing, l.checked, nil
	case <-ctx.Done():
		return false, false, ctx.Err()
	}
}

// TestPriceTipStream_DivergenceBudgetDiscriminatesAtItsEdge brackets
// [tipStreamDivergenceBudget] from both sides on a single-spelling base
// (one store read, so the delay IS the walk). Inside the budget the
// verdict survives to the wire; outside it the event still goes out, on
// time, with the verdict unchecked.
//
// Both halves matter. Without the 900ms case, "always report unchecked"
// passes. Without the 1200ms case, "never bound anything" passes.
func TestPriceTipStream_DivergenceBudgetDiscriminatesAtItsEdge(t *testing.T) {
	for _, tc := range []struct {
		name        string
		delay       time.Duration
		wantChecked bool
		wantWarning bool
	}{
		{"inside the budget keeps the verdict", 900 * time.Millisecond, true, true},
		{"outside the budget drops the verdict", 1200 * time.Millisecond, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &timedLooker{delay: tc.delay, firing: true, checked: true}
			url := tipStreamServerWithLooker(t, l)

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
						tc.delay, 1*time.Second, want, data)
				}
			}
			if headers > 4*time.Second {
				t.Errorf("headers took %v, want < 4s", headers)
			}
		})
	}
}

// TestPriceTipStream_AsOfIsStampedAfterTheDivergenceLookup — `as_of`
// describes the event that is being emitted, so it is taken after the
// last step that can hold the emission back. Stamped before the
// divergence lookup it described a moment that had already passed by the
// time the frame reached the wire, by however long the lookup took.
func TestPriceTipStream_AsOfIsStampedAfterTheDivergenceLookup(t *testing.T) {
	div := newGatedDivergenceLooker(false, true)
	url := tipStreamServerWithLooker(t, div)

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

// TestPriceTipStream_StalledDivergenceLookupDoesNotDelayLaterEmissions —
// the other half of the same defect. Headers are the first-byte cost;
// the recurring cost is that every tick's emission waited on the same
// stalled lookup, so a one-second window emitted roughly every nine
// seconds. A tick's auxiliary read must not stretch the period it
// belongs to beyond recognition.
func TestPriceTipStream_StalledDivergenceLookupDoesNotDelayLaterEmissions(t *testing.T) {
	div := newGatedDivergenceLooker(false, true) // never released
	url := tipStreamServerWithLooker(t, div)

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

	// One-second window plus at most one sub-budget of stall. 4.5s is
	// comfortably above that and comfortably below the ~9s a lookup on
	// the tick budget produces.
	if gap > 4500*time.Millisecond {
		t.Errorf("gap to the next emission was %v on a 1s window with a stalled divergence lookup, want < 4.5s — "+
			"the auxiliary read is stretching the tick period", gap)
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
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// TestPriceTipStream_HubSharedProducerStallDoesNotDelayEmissions — the
// same guarantee on the shared producer. runSharedTipProducer builds its
// events through the same helper, but it is a separate loop with its own
// budget, and it is the one that serves every viewer in production: a
// regression reintroduced there would be invisible to every hub-less
// test in this package.
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
		t.Errorf("the shared producer's gap was %v on a 1s window, want < 4.5s — runSharedTipProducer's lookup is riding the tick budget", gap)
	}
	for _, want := range []string{`"divergence_checked":false`, `"divergence_warning":false`} {
		if !strings.Contains(second, want) {
			t.Errorf("shared-producer event missing %q: %s", want, second)
		}
	}
}

// TestPriceTipStream_StallDoesNotClobberSingleSource — the stall path
// resets the two divergence fields explicitly. That reset must touch
// ONLY those two: single_source is derived from the sources slice and
// has nothing to do with the verdict store, so a stalled lookup must not
// silently unset a flag the emission is still entitled to.
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

// TestPriceTipStream_SustainedStallDoesNotFloodTheLog — end-to-end over
// the wire, both halves of the flood at once: the stream's own warning
// is rate-limited to one line per interval, and the shared lookup no
// longer prints a line per tick for deadline expiries it should swallow.
// A sustained stall must leave the log readable, since that is the
// moment an operator is reading it.
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
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The per-frame wait is generous on purpose: an unbudgeted lookup
	// stretches the 1s window to ~9s, and this test must reach its log
	// assertions on that shape rather than bailing on a missing frame —
	// what is being pinned is the LOG, not the cadence (which its own
	// tests cover). On a patched build the frames arrive in ~1s each and
	// the generosity costs nothing.
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

func (l *storeDownLooker) DivergenceFiringFor(context.Context, canonical.Asset) (firing, checked bool, err error) {
	l.calls.Add(1)
	return false, false, errDivergenceStoreDown
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
				t.Skipf("precondition unmet: the handler never reached the divergence lookup on %s", path)
			}
			if logs.count("divergence lookup failed") == 0 {
				t.Errorf("a genuine store failure (%v) reached the lookup with an expired request context and was logged nowhere; "+
					"%s has no stall reporter to replace it", errDivergenceStoreDown, path)
			}
		})
	}
}
