package freeze_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

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
	calls int
}

func (l *fakeOpenLister) ListOpen(_ context.Context) ([]freeze.OpenFreezePair, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
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
	by       []string
	failWith error
}

func (r *fakeRecoverer) MarkRecovered(_ context.Context, asset, quote canonical.Asset, releasedBy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, freeze.OpenFreezePair{Asset: asset, Quote: quote})
	r.by = append(r.by, releasedBy)
	return r.failWith
}

// TestRecovery_ClosesRowsWhenRedisMarkerGone: an open row with no Redis marker (TTL elapsed) fires MarkRecovered.
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
	if closer.by[0] != freeze.ReleasedBySystemRecovery {
		t.Errorf("released_by = %q, want %q", closer.by[0], freeze.ReleasedBySystemRecovery)
	}
}

// TestRecovery_LeavesStillFiringRowsAlone: a present marker means MarkRecovered MUST NOT fire.
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

// TestRecovery_ListErrorIsNonFatal: a lister failure is counted under outcome="error", closes nothing, and retries next tick.
func TestRecovery_ListErrorIsNonFatal(t *testing.T) {
	_, rdb := newRedis(t)
	lister := &fakeOpenLister{err: errors.New("boom")}
	closer := &fakeRecoverer{}

	r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
		Interval: 5 * time.Millisecond,
	})
	sweepsBefore := testutil.ToFloat64(obs.AnomalyFreezeRecoverySweepsTotal.WithLabelValues("error"))
	durBefore := obstest.HistogramSampleCount(t, obs.AnomalyFreezeRecoverySweepDurationSeconds, "outcome", "error")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v; want the context's DeadlineExceeded (a list error must not end the loop)", err)
	}

	lister.mu.Lock()
	listCalls := lister.calls
	lister.mu.Unlock()
	if listCalls < 2 {
		t.Errorf("ListOpen called %d times; want >= 2 (the tick after a failure must retry)", listCalls)
	}
	if got := testutil.ToFloat64(obs.AnomalyFreezeRecoverySweepsTotal.WithLabelValues("error")) - sweepsBefore; int(got) != listCalls {
		t.Errorf("sweeps_total{outcome=error} advanced by %v; want %d (one per failed list)", got, listCalls)
	}
	if got := obstest.HistogramSampleCount(t, obs.AnomalyFreezeRecoverySweepDurationSeconds, "outcome", "error") - durBefore; int(got) != listCalls {
		t.Errorf("sweep duration{outcome=error} gained %d samples; want %d", got, listCalls)
	}
	closer.mu.Lock()
	defer closer.mu.Unlock()
	if len(closer.calls) != 0 {
		t.Errorf("MarkRecovered called %d times after a list failure; want 0", len(closer.calls))
	}
}

// TestRecovery_SweepDurationMetricRecorded: an empty sweep still records a sample on the recovery sweep duration histogram with outcome="ok".
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

func (c *ladderCloser) MarkRecovered(_ context.Context, _, _ canonical.Asset, _ string) error {
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

// TestRecovery_DoesNotCloseARowWhoseDurableHoldIsLive: a missing marker is ambiguous (ended vs Redis flush); closing the row deletes the
// rehydrate's evidence, and the sweep wins on restart. With no marker and a live escalated ladder, the sweep runs FIRST and the ladder must survive.
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

// TestRecovery_ClosesRowsWhoseDurableHoldHasLapsed: past hold plus grace nobody will rehydrate, so the row must close or /v1/anomalies shows it firing forever.
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

// TestRecovery_LadderReadFailureClosesOnThePre0119Rule: a ladder read failure must not strand rows; a wrong close is bounded, never closing is not.
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

// TestLadderStillLive_BoundaryIsShared pins the predicate all marker-miss consumers read; divergence re-opens the finding.
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

// TestRecovery_LadderGraceBoundaryOnInjectedClock pins the hold+grace boundary on the injected clock, not the wall clock.
func TestRecovery_LadderGraceBoundaryOnInjectedClock(t *testing.T) {
	holdUntil := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	grace := 5 * time.Minute
	cases := []struct {
		name      string
		now       time.Time
		wantClose bool
	}{
		{"inside grace", holdUntil.Add(grace - time.Second), false},
		{"on the boundary", holdUntil.Add(grace), false},
		{"past grace", holdUntil.Add(grace + time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rdb := newRedis(t)
			asset, quote := nativeUSD(t)
			st := escalatedLadder(holdUntil)
			st.HoldUntil = holdUntil
			ladder := &ladderStub{state: st}
			lister := &fakeOpenLister{pairs: []freeze.OpenFreezePair{{Asset: asset, Quote: quote}}}
			closer := &ladderCloser{ladder: ladder}

			now := tc.now
			r := freeze.NewRecovery(rdb, lister, closer, freeze.RecoveryOptions{
				Interval:    10 * time.Millisecond,
				Ladder:      ladder,
				LadderGrace: grace,
				Clock:       func() time.Time { return now },
			})
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_ = r.Run(ctx)

			if closed := closer.callCount() > 0; closed != tc.wantClose {
				t.Errorf("at hold+%v: row closed = %v, want %v", tc.now.Sub(holdUntil), closed, tc.wantClose)
			}
		})
	}
}
