package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
)

// captureRefresher records every RefreshPair call. Configurable
// per-pair error injection lets tests exercise the refresh-error
// branch.
type captureRefresher struct {
	mu       sync.Mutex
	calls    []refreshCall
	errForCC map[string]error // pair-string → error to return
}

type refreshCall struct {
	Pair     canonical.Pair
	OurPrice float64
	At       time.Time
	Pinned   bool
}

func (r *captureRefresher) RefreshPair(_ context.Context, pair canonical.Pair, ourPrice float64, at time.Time) error {
	return r.record(refreshCall{Pair: pair, OurPrice: ourPrice, At: at})
}

func (r *captureRefresher) RefreshPinnedPair(_ context.Context, pair canonical.Pair, pinnedPrice float64, at time.Time) error {
	return r.record(refreshCall{Pair: pair, OurPrice: pinnedPrice, At: at, Pinned: true})
}

func (r *captureRefresher) record(c refreshCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
	if e, ok := r.errForCC[c.Pair.String()]; ok {
		return e
	}
	return nil
}

// silentLogger discards all log output for tests.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pairXLMUSD is a small helper for the tests below.
func pairXLMUSD(t *testing.T) canonical.Pair {
	t.Helper()
	xlm := canonical.NativeAsset()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}
	p, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

// TestRefreshDivergenceAll_NilRefresherIsNoOp — the producer being
// unconfigured is the pre-Phase default behaviour. No calls, no
// emitted metrics, no log noise.
func TestRefreshDivergenceAll_NilRefresherIsNoOp(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pairXLMUSD(t)},
		Windows:             []time.Duration{5 * time.Minute},
		DivergenceRefresher: nil,
		Logger:              silentLogger(),
	})
	o.refreshDivergenceAll(context.Background(), time.Now().UTC())
	// No assertion on metrics or calls — the function returned
	// without panicking, which is the whole assertion.
}

// TestRefreshDivergenceAll_PublishesWiredGauge pins the gauge that arms
// stellarindex_divergence_no_ok_outcomes: 1 whenever the pass has a
// refresher, even when every pair yields only no_vwap and no `ok` is
// ever counted; 0 when the refresher is deliberately unwired. Not
// parallel: the gauge is process-global.
func TestRefreshDivergenceAll_PublishesWiredGauge(t *testing.T) {
	rdb, _ := newTestRedis(t)
	base := Config{
		Pairs:   []canonical.Pair{pairXLMUSD(t)},
		Windows: []time.Duration{5 * time.Minute},
		Logger:  silentLogger(),
	}

	obs.DivergenceRefresherWired.Set(0)
	wired := base
	wired.DivergenceRefresher = &captureRefresher{}
	New(nil, rdb, wired).refreshDivergenceAll(context.Background(), time.Now().UTC())
	if got := testutil.ToFloat64(obs.DivergenceRefresherWired); got != 1 {
		t.Fatalf("wired refresher, only no_vwap outcomes: gauge = %v, want 1", got)
	}

	New(nil, rdb, base).refreshDivergenceAll(context.Background(), time.Now().UTC())
	if got := testutil.ToFloat64(obs.DivergenceRefresherWired); got != 0 {
		t.Fatalf("nil refresher: gauge = %v, want 0", got)
	}
}

// TestRefreshDivergenceAll_NoCachedVWAP — pair has no VWAP entry
// (cold start, frozen, all-empty windows). Refresher is NOT called;
// outcome is a no-op (no-vwap counter would tick if a metric
// registry was wired, but the predicate is the call count).
func TestRefreshDivergenceAll_NoCachedVWAP(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	capR := &captureRefresher{}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pairXLMUSD(t)},
		Windows:             []time.Duration{5 * time.Minute},
		DivergenceRefresher: capR,
		Logger:              silentLogger(),
	})
	o.refreshDivergenceAll(context.Background(), time.Now().UTC())
	if len(capR.calls) != 0 {
		t.Fatalf("got %d RefreshPair calls, want 0 (no VWAP cached)", len(capR.calls))
	}
}

// TestRefreshDivergenceAll_HappyPath — a cached VWAP for the
// shortest window triggers exactly one RefreshPair call per pair
// with the right price + timestamp.
func TestRefreshDivergenceAll_HappyPath(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	windows := []time.Duration{5 * time.Minute, 1 * time.Hour}

	// Pre-populate the shortest window's cache entry.
	key := cachekeys.VWAP(pair.Base, pair.Quote, windows[0])
	if err := rdb.Set(context.Background(), key.String(), "0.42", time.Minute).Err(); err != nil {
		t.Fatalf("seed redis: %v", err)
	}

	capR := &captureRefresher{}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pair},
		Windows:             windows,
		DivergenceRefresher: capR,
		Logger:              silentLogger(),
	})

	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	o.refreshDivergenceAll(context.Background(), now)

	if len(capR.calls) != 1 {
		t.Fatalf("RefreshPair calls: got %d, want 1", len(capR.calls))
	}
	got := capR.calls[0]
	if !got.Pair.Equal(pair) {
		t.Errorf("pair = %v, want %v", got.Pair, pair)
	}
	if got.OurPrice != 0.42 {
		t.Errorf("ourPrice = %v, want 0.42", got.OurPrice)
	}
	if !got.At.Equal(now) {
		t.Errorf("at = %v, want %v", got.At, now)
	}
}

// TestRefreshDivergenceAll_ReversedWindowsUseShortest: an operator who
// lists windows longest-first must still get the 5m VWAP as the divergence
// input, not the lagged 24h one that happens to be listed first.
func TestRefreshDivergenceAll_ReversedWindowsUseShortest(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	ctx := context.Background()
	for w, price := range map[time.Duration]string{
		5 * time.Minute: "0.42",
		time.Hour:       "0.40",
		24 * time.Hour:  "0.30",
	} {
		if err := rdb.Set(ctx, cachekeys.VWAP(pair.Base, pair.Quote, w).String(), price, time.Minute).Err(); err != nil {
			t.Fatalf("seed redis: %v", err)
		}
	}

	capR := &captureRefresher{}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pair},
		Windows:             []time.Duration{24 * time.Hour, time.Hour, 5 * time.Minute},
		DivergenceRefresher: capR,
		Logger:              silentLogger(),
	})
	o.refreshDivergenceAll(ctx, time.Now().UTC())

	if len(capR.calls) != 1 {
		t.Fatalf("RefreshPair calls: got %d, want 1", len(capR.calls))
	}
	if got := capR.calls[0].OurPrice; got != 0.42 {
		t.Errorf("ourPrice = %v, want 0.42 (the 5m VWAP); windows listed longest-first "+
			"must not make the 24h VWAP the divergence input", got)
	}
}

// TestRefreshDivergenceAll_ParseErrorSkipsCall — a malformed VWAP
// in the cache (writer regression) doesn't propagate to the
// refresher. The pair is skipped silently (parse_error counter
// would tick).
func TestRefreshDivergenceAll_ParseErrorSkipsCall(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	windows := []time.Duration{5 * time.Minute}

	key := cachekeys.VWAP(pair.Base, pair.Quote, windows[0])
	if err := rdb.Set(context.Background(), key.String(), "definitely-not-a-float", time.Minute).Err(); err != nil {
		t.Fatalf("seed redis: %v", err)
	}

	capR := &captureRefresher{}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pair},
		Windows:             windows,
		DivergenceRefresher: capR,
		Logger:              silentLogger(),
	})
	o.refreshDivergenceAll(context.Background(), time.Now().UTC())
	if len(capR.calls) != 0 {
		t.Fatalf("RefreshPair calls: got %d, want 0 (malformed VWAP)", len(capR.calls))
	}
}

// TestRefreshDivergenceAll_RefresherErrorDoesNotAbortOtherPairs —
// per-pair errors are logged + counted; subsequent pairs still
// process. Mirrors the per-asset isolation pattern used by the
// supply cross-check refresher.
func TestRefreshDivergenceAll_RefresherErrorDoesNotAbortOtherPairs(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)

	xlm := canonical.NativeAsset()
	usd, _ := canonical.NewFiatAsset("USD")
	eur, _ := canonical.NewFiatAsset("EUR")
	pXLMUSD, _ := canonical.NewPair(xlm, usd)
	pXLMEUR, _ := canonical.NewPair(xlm, eur)

	windows := []time.Duration{5 * time.Minute}
	for _, p := range []canonical.Pair{pXLMUSD, pXLMEUR} {
		key := cachekeys.VWAP(p.Base, p.Quote, windows[0])
		if err := rdb.Set(context.Background(), key.String(), "1.00", time.Minute).Err(); err != nil {
			t.Fatalf("seed redis %s: %v", key, err)
		}
	}

	capR := &captureRefresher{
		errForCC: map[string]error{
			pXLMUSD.String(): errors.New("synthetic upstream failure"),
		},
	}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pXLMUSD, pXLMEUR},
		Windows:             windows,
		DivergenceRefresher: capR,
		Logger:              silentLogger(),
	})

	o.refreshDivergenceAll(context.Background(), time.Now().UTC())
	if len(capR.calls) != 2 {
		t.Fatalf("RefreshPair calls: got %d, want 2 (one error, one ok)", len(capR.calls))
	}
}

// TestRefreshDivergenceAll_DurationMetricRecorded pins the
// latency-histogram wiring: a successful
// per-pair refresh advances
// `stellarindex_divergence_refresh_duration_seconds{outcome="ok"}`.
// Guards against
// a future refactor silently dropping the timing call.
func TestRefreshDivergenceAll_DurationMetricRecorded(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	windows := []time.Duration{5 * time.Minute}

	key := cachekeys.VWAP(pair.Base, pair.Quote, windows[0])
	if err := rdb.Set(context.Background(), key.String(), "0.42", time.Minute).Err(); err != nil {
		t.Fatalf("seed redis: %v", err)
	}

	capR := &captureRefresher{}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pair},
		Windows:             windows,
		DivergenceRefresher: capR,
		Logger:              silentLogger(),
	})

	before := obstest.HistogramSampleCount(t, obs.DivergenceRefreshDurationSeconds, "outcome", "ok")
	o.refreshDivergenceAll(context.Background(), time.Now().UTC())
	after := obstest.HistogramSampleCount(t, obs.DivergenceRefreshDurationSeconds, "outcome", "ok")

	if after <= before {
		t.Errorf("divergence refresh duration histogram did not advance: before=%d after=%d", before, after)
	}
}

// TestRefreshDivergenceAll_MinIntervalSkipsConsecutiveTicks asserts
// the min-interval gate: if cfg.DivergenceMinInterval is set,
// only the first call within a window of that duration actually
// invokes the refresher. Subsequent calls within the same window
// are skipped silently (no metric increment, no RefreshPair call,
// no log spam). After the window elapses, the next call is allowed.
//
// Without the gate the live r1 deployment hit the CMC monthly cap
// inside 5 days; with the default 5-minute gate the quota drops
// roughly 10× to a sustainable footprint.
func TestRefreshDivergenceAll_MinIntervalSkipsConsecutiveTicks(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	windows := []time.Duration{5 * time.Minute}

	key := cachekeys.VWAP(pair.Base, pair.Quote, windows[0])
	if err := rdb.Set(context.Background(), key.String(), "0.42", time.Minute).Err(); err != nil {
		t.Fatalf("seed redis: %v", err)
	}

	capR := &captureRefresher{}
	o := New(nil, rdb, Config{
		Pairs:                 []canonical.Pair{pair},
		Windows:               windows,
		DivergenceRefresher:   capR,
		DivergenceMinInterval: 5 * time.Minute,
		Logger:                silentLogger(),
	})

	t0 := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

	// First call at t0 — should refresh.
	o.refreshDivergenceAll(context.Background(), t0)
	if got := len(capR.calls); got != 1 {
		t.Fatalf("after 1st tick: got %d calls, want 1", got)
	}

	// 30 s later — well inside the interval. Should skip.
	o.refreshDivergenceAll(context.Background(), t0.Add(30*time.Second))
	if got := len(capR.calls); got != 1 {
		t.Fatalf("after 2nd tick (30s): got %d calls, want still 1 (gated)", got)
	}

	// 4 min 30 s after first — still inside the 5-min interval.
	o.refreshDivergenceAll(context.Background(), t0.Add(4*time.Minute+30*time.Second))
	if got := len(capR.calls); got != 1 {
		t.Fatalf("after 3rd tick (4m30s): got %d calls, want still 1 (gated)", got)
	}

	// 5 min after first — interval elapsed, should refresh again.
	o.refreshDivergenceAll(context.Background(), t0.Add(5*time.Minute))
	if got := len(capR.calls); got != 2 {
		t.Fatalf("after 4th tick (5m): got %d calls, want 2 (gate released)", got)
	}
}

// TestRefreshDivergenceAll_MinIntervalZeroPreservesLegacy asserts
// that DivergenceMinInterval=0 (the zero-value, also the legacy
// default for callers that haven't migrated) bypasses the gate
// entirely — every call invokes the refresher.
func TestRefreshDivergenceAll_MinIntervalZeroPreservesLegacy(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	windows := []time.Duration{5 * time.Minute}

	key := cachekeys.VWAP(pair.Base, pair.Quote, windows[0])
	if err := rdb.Set(context.Background(), key.String(), "0.42", time.Minute).Err(); err != nil {
		t.Fatalf("seed redis: %v", err)
	}

	capR := &captureRefresher{}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pair},
		Windows:             windows,
		DivergenceRefresher: capR,
		// DivergenceMinInterval intentionally unset (zero).
		Logger: silentLogger(),
	})

	t0 := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		o.refreshDivergenceAll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if got := len(capR.calls); got != 3 {
		t.Errorf("legacy mode (zero interval): got %d calls, want 3 (every tick)", got)
	}
}

// TestShortestWindowIsTheDivergenceRefreshWindow — the divergence service
// stamps ShortestWindow(configured) on every verdict, so it must be exactly
// the window refreshDivergenceAll compares, Windows[0] of the orchestrator
// New builds from the same configuration.
func TestShortestWindowIsTheDivergenceRefreshWindow(t *testing.T) {
	for _, ws := range [][]time.Duration{
		nil,
		{},
		{0, -time.Minute},
		{time.Hour, 15 * time.Minute, 24 * time.Hour},
		{time.Hour, time.Hour},
	} {
		o := New(nil, nil, Config{Windows: ws})
		if got, want := ShortestWindow(ws), o.cfg.Windows[0]; got != want {
			t.Errorf("ShortestWindow(%v) = %v, want the refresh window %v", ws, got, want)
		}
	}
	if got := ShortestWindow([]time.Duration{time.Hour, 15 * time.Minute}); got != 15*time.Minute {
		t.Errorf("ShortestWindow = %v, want 15m", got)
	}
}

// TestRefreshDivergenceAll_FrozenPairIsNotJudgedOnItsPinnedPrice: a pair
// frozen on a genuine repricing keeps serving its last-known-good while
// the market settles 8% higher. Across two divergence passes past the
// warning persistence — the first in the freeze's own tick, the second
// later in its hold — the pinned value must not raise warning_fired or the
// divergence.firing hook, and must not reach the confidence score as a
// cross-oracle disagreement. The reference median must still reach the
// release path.
func TestRefreshDivergenceAll_FrozenPairIsNotJudgedOnItsPinnedPrice(t *testing.T) {
	t.Parallel()
	o, rdb, pair, hooks := pinnedDivergenceRig(t)
	window := 5 * time.Minute
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	// Tick 1: the freeze fires this tick.
	o.markFrozenThisTick(pair, window)
	o.refreshDivergenceAll(context.Background(), t0)

	// Tick 2, ten minutes on: a new tick's set is empty, the hold is live.
	t1 := t0.Add(10 * time.Minute)
	o.frozenThisTick = make(map[string]struct{})
	o.freezeStates[pair.String()+":"+window.String()] = freeze.State{FiredAt: t0, HoldUntil: t0.Add(30 * time.Minute)}
	o.clock = func() time.Time { return t1 }
	o.refreshDivergenceAll(context.Background(), t1)

	if n := hooks.Load(); n != 0 {
		t.Errorf("divergence.firing hook fired %d time(s) for a pair serving its pinned LKG", n)
	}
	if got := readDivergenceEntry(t, rdb, pair); got.WarningFired {
		t.Errorf("warning_fired = true on a frozen pair: the pinned %v was judged against the %v market",
			pinnedLKG, marketPrice)
	}
	xo := o.lookupCrossOracle(context.Background(), pair)
	if xo.divergencePct != -1 || xo.agreementCount != -1 {
		t.Errorf("confidence cross-oracle input = {pct %v, agree %d}, want unchecked {-1, -1}: "+
			"a divergence measured against the pinned LKG would depress the score that is itself a freeze leg",
			xo.divergencePct, xo.agreementCount)
	}
	if math.Abs(xo.median-marketPrice) > 1e-12 {
		t.Errorf("reference median = %v, want %v — the release path corroborates the candidate against it",
			xo.median, marketPrice)
	}
}

// TestRefreshDivergenceAll_UnfrozenDivergenceStillFires is the control:
// the same rig with no freeze does fire, so the frozen case above is
// suppressed by the freeze and not by the rig.
func TestRefreshDivergenceAll_UnfrozenDivergenceStillFires(t *testing.T) {
	t.Parallel()
	o, rdb, pair, hooks := pinnedDivergenceRig(t)
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	o.refreshDivergenceAll(context.Background(), t0)
	o.refreshDivergenceAll(context.Background(), t0.Add(10*time.Minute))

	if !readDivergenceEntry(t, rdb, pair).WarningFired {
		t.Error("warning_fired = false for an unfrozen pair 8% off the references for 10 minutes")
	}
	if n := hooks.Load(); n != 1 {
		t.Errorf("divergence.firing hook fired %d time(s), want 1", n)
	}
	if xo := o.lookupCrossOracle(context.Background(), pair); xo.agreementCount != 0 || xo.divergencePct <= 5 {
		t.Errorf("confidence cross-oracle input = {pct %v, agree %d}, want a checked disagreement",
			xo.divergencePct, xo.agreementCount)
	}
}
