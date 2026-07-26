package freeze_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
)

// fakeOpenLister returns a fixed list of open pairs.
type fakeOpenLister struct {
	mu    sync.Mutex
	pairs []freeze.OpenFreezePair
	err   error
}

func (l *fakeOpenLister) ListOpen(_ context.Context) ([]freeze.OpenFreezePair, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	out := make([]freeze.OpenFreezePair, len(l.pairs))
	copy(out, l.pairs)
	return out, nil
}

// fakeRecoverer captures MarkRecovered calls.
type fakeRecoverer struct {
	mu       sync.Mutex
	calls    []freeze.OpenFreezePair
	failWith error
}

func (r *fakeRecoverer) MarkRecovered(_ context.Context, asset, quote canonical.Asset) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, freeze.OpenFreezePair{Asset: asset, Quote: quote})
	return r.failWith
}

// TestRecovery_ClosesRowsWhenRedisMarkerGone — the canonical happy
// path: postgres has an open row, Redis has no marker (TTL elapsed)
// → MarkRecovered fires.
func TestRecovery_ClosesRowsWhenRedisMarkerGone(t *testing.T) {
	_, rdb := newRedis(t)
	asset, quote := nativeUSD(t)
	lister := &fakeOpenLister{
		pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}},
	}
	closer := &fakeRecoverer{}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	closer.mu.Lock()
	defer closer.mu.Unlock()
	if len(closer.calls) == 0 {
		t.Fatalf("MarkRecovered never called; want at least 1")
	}
	got := closer.calls[0]
	if got.Asset.String() != asset.String() {
		t.Errorf("asset = %s, want %s", got.Asset.String(), asset.String())
	}
	if got.Quote.String() != quote.String() {
		t.Errorf("quote = %s, want %s", got.Quote.String(), quote.String())
	}
}

// TestRecovery_LeavesStillFiringRowsAlone — when the Redis marker is
// still present (orchestrator is refreshing it because the anomaly
// hasn't cleared), MarkRecovered MUST NOT fire.
func TestRecovery_LeavesStillFiringRowsAlone(t *testing.T) {
	_, rdb := newRedis(t)
	asset, quote := nativeUSD(t)

	// Pre-write a freeze marker (so Redis returns the value rather
	// than redis.Nil on the recovery sweep's GET).
	w, err := freeze.NewWriter(rdb, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Mark(context.Background(), asset, quote, "1.000000000000",
		anomaly.Decision{Action: anomaly.ActionFreeze}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	lister := &fakeOpenLister{
		pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}},
	}
	closer := &fakeRecoverer{}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	closer.mu.Lock()
	defer closer.mu.Unlock()
	if len(closer.calls) != 0 {
		t.Errorf("MarkRecovered called %d times; want 0 (marker still present)",
			len(closer.calls))
	}
}

// TestRecovery_ListErrorIsNonFatal — a lister failure logs + counts
// but doesn't crash the worker (next tick retries).
func TestRecovery_ListErrorIsNonFatal(t *testing.T) {
	_, rdb := newRedis(t)
	lister := &fakeOpenLister{err: errors.New("boom")}
	closer := &fakeRecoverer{}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)
	// No assertion beyond "didn't panic" — the metric increment is
	// observed via Prometheus in production. Test exists to guard
	// against future code that propagates the error.
}

// TestRecovery_SweepDurationMetricRecorded pins the wave-91
// (2026-05-13) latency-histogram wiring: a sweep with no open
// rows still records a sample on
// `stellarindex_anomaly_freeze_recovery_sweep_duration_seconds{outcome="ok"}`.
// Same shape as the wave-92/93 regression tests for the
// customer-webhook + divergence-refresh histograms — guards
// against a future refactor silently dropping the timing call.
func TestRecovery_SweepDurationMetricRecorded(t *testing.T) {
	_, rdb := newRedis(t)
	// Empty open-list → sweep takes the zero-rows fast path,
	// which still records a duration sample under outcome="ok".
	lister := &fakeOpenLister{}
	closer := &fakeRecoverer{}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 5 * time.Millisecond,
	})
	before := obstest.HistogramSampleCount(t, obs.AnomalyFreezeRecoverySweepDurationSeconds, "outcome", "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)
	after := obstest.HistogramSampleCount(t, obs.AnomalyFreezeRecoverySweepDurationSeconds, "outcome", "ok")

	if after <= before {
		t.Errorf("freeze recovery sweep duration histogram did not advance: before=%d after=%d", before, after)
	}
}

// ─── migration 0119: the sweep must not destroy the ladder ───────

// ladderStub is a freeze.LadderStore whose row can be CLOSED (recovered_at
// stamped), mirroring timescale.FreezeEventSink: LoadLadder reports absent
// once closed or once the ladder is retired (hold_until NULL).
type ladderStub struct {
	mu     sync.Mutex
	state  freeze.State
	closed bool
	err    error
}

func (l *ladderStub) SaveLadder(_ context.Context, _, _ canonical.Asset, st freeze.State) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.state = st
	return nil
}

func (l *ladderStub) LoadLadder(_ context.Context, _, _ canonical.Asset) (freeze.State, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return freeze.State{}, false, l.err
	}
	if l.closed || l.state.HoldUntil.IsZero() {
		return freeze.State{}, false, nil
	}
	return l.state, true, nil
}

func (l *ladderStub) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// markRecoveredInto is the fakeRecoverer behaviour wired to a ladderStub, so
// the test models the REAL coupling: MarkRecovered stamps recovered_at,
// which is exactly the predicate LoadLadder tests.
type ladderCloser struct {
	mu     sync.Mutex
	ladder *ladderStub
	calls  int
}

func (c *ladderCloser) MarkRecovered(_ context.Context, _, _ canonical.Asset) error {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.ladder.mu.Lock()
	c.ladder.closed = true
	c.ladder.mu.Unlock()
	return nil
}

func (c *ladderCloser) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func escalatedLadder(now time.Time) freeze.State {
	return freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
		Corroborated:   true,
	}
}

// TestRecovery_DoesNotCloseARowWhoseDurableHoldIsLive is the regression for
// the third marker-miss consumer (migration 0119 follow-up).
//
// The recovery worker's trigger is "the Redis marker is gone", which since
// 0119 is AMBIGUOUS: the freeze ended (close the row — its whole job), or
// Redis lost the marker (leave it; the orchestrator rehydrates from this
// very row). Closing is not neutral in the second case, because
// `recovered_at IS NULL` is the exact predicate LoadLadder tests: the sweep
// deletes the rehydrate's evidence AND records on /v1/anomalies that the
// freeze recovered normally.
//
// The race is not theoretical and it is not close. Recovery.Run does an
// IMMEDIATE tick on start, and the orchestrator computes VWAPs before it
// evaluates any freeze — so on an aggregator restart after a flush the sweep
// wins essentially always; on a running aggregator it is a 60s sweep against
// a 30s tick.
//
// Models the restart-after-flush ordering exactly: no marker, an escalated
// durable ladder still inside its hold, sweep runs FIRST. The ladder must
// survive and still rehydrate.
func TestRecovery_DoesNotCloseARowWhoseDurableHoldIsLive(t *testing.T) {
	_, rdb := newRedis(t) // empty: the flush already happened
	asset, quote := nativeUSD(t)

	ladder := &ladderStub{state: escalatedLadder(time.Now().UTC())}
	lister := &fakeOpenLister{pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}}}
	closer := &ladderCloser{ladder: ladder}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 10 * time.Millisecond,
		Ladder:   ladder,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if n := closer.callCount(); n != 0 {
		t.Errorf("MarkRecovered called %d time(s) on a pair whose durable hold is still live — "+
			"the sweep stamped recovered_at, which is the predicate the rehydrate needs, and "+
			"/v1/anomalies now says this escalated freeze recovered normally", n)
	}
	if ladder.isClosed() {
		t.Fatal("the durable row was closed; the ladder is gone")
	}

	// The orchestrator's (later) tick must still find the escalated ladder.
	w, err := freeze.NewWriter(rdb, 0, freeze.WithLadderStore(ladder, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	st, present, err := w.LoadState(context.Background(), asset, quote)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !present {
		t.Fatal("the freeze no longer rehydrates after the sweep ran first")
	}
	if !st.Escalated || st.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("rehydrated ladder = {escalated:%v exts:%d}, want {escalated:true exts:%d}",
			st.Escalated, st.ExtensionsUsed, freeze.DefaultMaxExtensions)
	}
}

// TestRecovery_ClosesRowsWhoseDurableHoldHasLapsed — the guard must not turn
// the worker off. A hold that lapsed beyond the grace means the aggregator
// has been down longer than the freeze's own hold, so nobody is coming to
// rehydrate it; leaving it open would show a finished freeze as permanently
// firing on /v1/anomalies, which is the worker's entire reason to exist.
func TestRecovery_ClosesRowsWhoseDurableHoldHasLapsed(t *testing.T) {
	_, rdb := newRedis(t)
	asset, quote := nativeUSD(t)

	stale := escalatedLadder(time.Now().UTC())
	stale.HoldUntil = time.Now().UTC().Add(-7 * 24 * time.Hour)
	ladder := &ladderStub{state: stale}
	lister := &fakeOpenLister{pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}}}
	closer := &ladderCloser{ladder: ladder}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 10 * time.Millisecond,
		Ladder:   ladder,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if closer.callCount() == 0 {
		t.Error("a week-lapsed hold was never closed — the recovery worker is now inert")
	}
}

// TestRecovery_LadderReadFailureClosesOnThePre0119Rule — a Postgres blip
// must not strand open rows forever. Closing wrongly is bounded (the
// orchestrator re-freezes on the pair's own signal if the anomaly is live);
// never closing is unbounded.
func TestRecovery_LadderReadFailureClosesOnThePre0119Rule(t *testing.T) {
	_, rdb := newRedis(t)
	asset, quote := nativeUSD(t)

	ladder := &ladderStub{state: escalatedLadder(time.Now().UTC()), err: errors.New("pg blip")}
	lister := &fakeOpenLister{pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}}}
	closer := &ladderCloser{ladder: ladder}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 10 * time.Millisecond,
		Ladder:   ladder,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if closer.callCount() == 0 {
		t.Error("a failing ladder read stranded the open row; want the pre-0119 close")
	}
}

// TestLadderStillLive_BoundaryIsShared pins the predicate all three
// marker-miss consumers read. A divergence between the Writer's rehydrate
// bound and the sweep's close bound re-opens the whole finding, with the row
// additionally marked "recovered normally".
func TestLadderStillLive_BoundaryIsShared(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	const grace = 5 * time.Minute
	st := freeze.State{FiredAt: now.Add(-time.Hour), HoldUntil: now.Add(-time.Minute)}

	if !freeze.LadderStillLive(st, grace, now) {
		t.Error("a hold one minute past expiry with a 5m grace must still be live")
	}
	if freeze.LadderStillLive(st, grace, now.Add(5*time.Minute)) {
		t.Error("a hold past expiry+grace must not be live")
	}
	if freeze.LadderStillLive(freeze.State{}, grace, now) {
		t.Error("an inactive (zero) ladder must never read as live")
	}
}
