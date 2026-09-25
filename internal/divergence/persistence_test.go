package divergence_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// readDivergence fetches + decodes the CachedResult the worker wrote
// for a pair. Fails the test on any miss/decode error so the callers
// stay assertion-only.
func readDivergence(t *testing.T, rdb *redis.Client, pair canonical.Pair) divergence.CachedResult {
	t.Helper()
	body, err := rdb.Get(context.Background(), cachekeys.Divergence(pair).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get %s: %v", cachekeys.Divergence(pair), err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal CachedResult: %v", err)
	}
	return cached
}

// TestRefreshPair_FastMoveDoesNotFalseWarn is the W3-guards-2 red
// proof. OurPrice is a shortest-window (5m) VWAP; the references are
// instantaneous spot quotes. On a fast upward move the spot has jumped
// but our VWAP still averages in the pre-move trades, so our value
// legitimately lags the references by more than the threshold. Before
// the fix that raised flags.divergence_warning immediately even though
// nothing was wrong.
//
// A SINGLE refresh of a real 10% gap (well over the 5% threshold, with
// no reference corroborating us) must NOT fire the warning: the gap has
// not yet persisted past the debounce window, so it is indistinguishable
// from the mechanical VWAP-vs-spot lag of a fast move.
//
// This is the non-vacuous assertion: against the pre-fix worker (which
// set WarningFired = checked && (DivergencePct > threshold || nobody
// agrees)) this single 10% refresh fires true and the test fails red.
func TestRefreshPair_FastMoveDoesNotFalseWarn(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
		&stubReference{name: "reflector", price: 1.00},
	}
	// Default debounce (no WarningPersistence set) — the production
	// posture the fix ships with.
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})

	// Our 5m VWAP is 10% above the spot references — a fast move the
	// VWAP hasn't caught up to yet.
	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.10, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}
	cached := readDivergence(t, rdb, xlmUSD(t))

	// Pin that the RAW divergence is genuinely present — so the test can
	// never pass for the wrong reason (e.g. the references agreeing).
	if cached.DivergencePct < 9.0 {
		t.Fatalf("DivergencePct = %g, want ~10 — the fixture must present a real over-threshold gap", cached.DivergencePct)
	}
	if cached.AgreementCount != 0 {
		t.Fatalf("AgreementCount = %d, want 0 — no reference corroborates us at this instant", cached.AgreementCount)
	}
	// ...yet the WARNING must be held: a single-tick over-threshold gap
	// is exactly the fast-move artefact the debounce suppresses.
	if cached.WarningFired || cached.FiringSince.IsZero() {
		t.Errorf("single-refresh 10%% gap: WarningFired=%v FiringSince=%v, want the raw streak started "+
			"but the warning held — a fast-move VWAP-vs-spot lag must not raise a false divergence warning "+
			"before it has persisted past the debounce window", cached.WarningFired, cached.FiringSince)
	}
}

// TestRefreshPair_SustainedDivergenceStillWarns proves the debounce
// does not blind a genuine divergence: the same over-threshold gap,
// still present one debounce window later, DOES fire. This is the
// "genuine-divergence detection preserved" half of the fix.
func TestRefreshPair_SustainedDivergenceStillWarns(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
		&stubReference{name: "reflector", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()

	// First observation of the gap — held (debounced).
	if err := svc.RefreshPair(context.Background(), pair, 1.10, t0); err != nil {
		t.Fatalf("RefreshPair #1: %v", err)
	}
	if got := readDivergence(t, rdb, pair); got.WarningFired || got.FiringSince.IsZero() {
		t.Fatalf("first observation: WarningFired=%v FiringSince=%v; the raw streak must start and "+
			"the debounce must hold the warning for one window", got.WarningFired, got.FiringSince)
	}

	// Same gap, one debounce window later — a genuine sustained
	// divergence. The warning must now fire.
	if err := svc.RefreshPair(context.Background(), pair, 1.10,
		t0.Add(divergence.DefaultWarningPersistence+time.Minute)); err != nil {
		t.Fatalf("RefreshPair #2: %v", err)
	}
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = false on a divergence that persisted a full window: the debounce " +
			"must not blind a sustained divergence")
	}
}

// TestRefreshPair_TransientDivergenceSelfClears proves the gate keys on
// an UNINTERRUPTED streak, not on wall-clock-since-first-ever. A gap
// that appears, clears, then reappears must restart its persistence
// clock — so a pair of brief spikes straddling more than a window never
// fires. This is what stops the debounce from degrading into a plain
// "warn if we've ever been over threshold for N minutes" bound.
func TestRefreshPair_TransientDivergenceSelfClears(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
		&stubReference{name: "reflector", price: 1.00},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	win := divergence.DefaultWarningPersistence

	// Spike #1: 10% gap → held.
	if err := svc.RefreshPair(context.Background(), pair, 1.10, t0); err != nil {
		t.Fatalf("RefreshPair spike#1: %v", err)
	}
	if got := readDivergence(t, rdb, pair); got.WarningFired || got.FiringSince.IsZero() {
		t.Fatalf("spike #1: WarningFired=%v FiringSince=%v; the raw streak must start and the warning be held",
			got.WarningFired, got.FiringSince)
	}

	// Recovery: prices agree again → streak resets.
	if err := svc.RefreshPair(context.Background(), pair, 1.00, t0.Add(win/2)); err != nil {
		t.Fatalf("RefreshPair recovery: %v", err)
	}
	if readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("WarningFired = true on recovery; prices agree here")
	}

	// Spike #2, well past a window since spike #1 — but the streak
	// restarted at recovery, so this is a fresh onset and must NOT fire
	// even though (t0.Add(2*win) - t0) > window.
	if err := svc.RefreshPair(context.Background(), pair, 1.10, t0.Add(2*win)); err != nil {
		t.Fatalf("RefreshPair spike#2: %v", err)
	}
	if readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = true on a fresh spike: the persistence clock must restart after the " +
			"divergence self-cleared, not accumulate across disconnected spikes")
	}
}

// TestRefreshPair_OnWarningHookFiresOnlyAfterPersistence pins that the
// edge-triggered customer-webhook hook rides the DEBOUNCED WarningFired,
// so subscribers aren't paged on a fast-move blip. The hook must fire on
// the persisted false→true edge, not on the first raw over-threshold
// refresh.
func TestRefreshPair_OnWarningHookFiresOnlyAfterPersistence(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		&stubReference{name: "chainlink", price: 1.00},
	}
	var fired int
	svc, _, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(_ context.Context, _ canonical.Pair, _ divergence.CachedResult) {
			fired++
		},
	})
	pair := xlmUSD(t)
	t0 := time.Now()

	// First over-threshold refresh: debounced, so no hook.
	_ = svc.RefreshPair(context.Background(), pair, 1.10, t0)
	if fired != 0 {
		t.Fatalf("hook fired=%d after the first (debounced) refresh, want 0", fired)
	}

	// Persisted a full window later: WarningFired flips true → one hook.
	_ = svc.RefreshPair(context.Background(), pair, 1.10, t0.Add(divergence.DefaultWarningPersistence+time.Minute))
	if fired != 1 {
		t.Fatalf("hook fired=%d after the divergence persisted, want 1", fired)
	}
}

// TestRefreshPair_BelowQuorumTickFreezesWarning pins that a refresh which
// cannot reach the reference quorum is a no-op on warning state. A firing
// pair drops to one responding reference for a single refresh: the cached
// WarningFired must stay true (not be rewritten to an all-clear), the
// persistence streak must survive so the warning is still up the moment
// references return, and no second webhook may be emitted for the same
// ongoing episode.
func TestRefreshPair_BelowQuorumTickFreezesWarning(t *testing.T) {
	flaky := &stubReference{name: "chainlink", price: 1.00}
	refs := []divergence.Reference{
		&stubReference{name: "coingecko", price: 1.00},
		flaky,
	}
	var fired int
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(_ context.Context, _ canonical.Pair, _ divergence.CachedResult) {
			fired++
		},
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	step := divergence.DefaultWarningPersistence + time.Minute

	_ = svc.RefreshPair(ctx, pair, 1.10, t0)
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step))
	if got := readDivergence(t, rdb, pair); !got.WarningFired || fired != 1 {
		t.Fatalf("setup: WarningFired=%v hooks=%d, want a firing pair with one hook", got.WarningFired, fired)
	}

	flaky.err = divergence.ErrPriceUnavailable
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step+time.Minute))
	below := readDivergence(t, rdb, pair)
	if below.SuccessCount != 1 {
		t.Fatalf("below-quorum tick SuccessCount = %d, want 1", below.SuccessCount)
	}
	if !below.WarningFired {
		t.Errorf("below-quorum tick rewrote WarningFired to false; want the last verdict (true) carried forward")
	}

	flaky.err = nil
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(step+2*time.Minute))
	if got := readDivergence(t, rdb, pair); !got.WarningFired {
		t.Errorf("first refresh after references returned: WarningFired=false; the persistence streak was reset by the below-quorum tick")
	}
	_ = svc.RefreshPair(ctx, pair, 1.10, t0.Add(2*step+2*time.Minute))
	if fired != 1 {
		t.Errorf("hook fired %d times for one uninterrupted divergence, want 1", fired)
	}
}

// restartedService builds a second Service over the SAME Redis, standing
// in for the aggregator process that comes back after a deploy: fresh
// in-memory state, the previous process's cached results still in place.
func restartedService(t *testing.T, refs []divergence.Reference, rdb *redis.Client, hook divergence.WarningHook) *divergence.Service {
	t.Helper()
	svc, err := divergence.NewService(divergence.ServiceOptions{
		References:           refs,
		Cache:                rdb,
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired:       hook,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func depeggedRefs() []divergence.Reference {
	return []divergence.Reference{
		&stubReference{name: "coingecko", price: 0.93},
		&stubReference{name: "chainlink", price: 0.93},
		&stubReference{name: "reflector", price: 0.93},
	}
}

// TestRefreshPair_RestartKeepsPublishedWarning: a divergence that has been
// published for a while must stay published across an aggregator restart,
// and the restarted process must not re-send divergence.firing for it.
// Before the fix both maps started empty, so the first post-restart refresh
// wrote WarningFired=false — /v1/price said "cross-checked and agrees" for
// a 7% depeg — and the next matured refresh re-fired the webhook.
func TestRefreshPair_RestartKeepsPublishedWarning(t *testing.T) {
	refs := depeggedRefs()
	var firstHooks, secondHooks int
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) {
			firstHooks++
		},
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	ctx := context.Background()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(divergence.DefaultWarningPersistence+time.Minute))
	if !readDivergence(t, rdb, pair).WarningFired || firstHooks != 1 {
		t.Fatalf("precondition: want a published warning and one hook, got fired=%v hooks=%d",
			readDivergence(t, rdb, pair).WarningFired, firstHooks)
	}

	restarted := restartedService(t, refs, rdb, func(context.Context, canonical.Pair, divergence.CachedResult) {
		secondHooks++
	})
	t1 := t0.Add(41 * time.Minute)
	_ = restarted.RefreshPair(ctx, pair, 1.00, t1)
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = false on the first refresh after a restart for a pair that never " +
			"stopped diverging: the restart published a false all-clear")
	}
	_ = restarted.RefreshPair(ctx, pair, 1.00, t1.Add(divergence.DefaultWarningPersistence+time.Minute))
	if secondHooks != 0 {
		t.Errorf("restarted process fired divergence.firing %d time(s) for a divergence already "+
			"announced before the restart, want 0", secondHooks)
	}
}

// TestRefreshPair_StreakSurvivesRestartInsideDebounce: restarts closer
// together than the persistence window must not reset the debounce clock,
// or a crash-looping aggregator never publishes the warning at all.
func TestRefreshPair_StreakSurvivesRestartInsideDebounce(t *testing.T) {
	refs := depeggedRefs()
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	ctx := context.Background()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = restartedService(t, refs, rdb, nil).RefreshPair(ctx, pair, 1.00, t0.Add(3*time.Minute))
	if readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("WarningFired = true 3m into the streak; the debounce must still hold it")
	}
	_ = restartedService(t, refs, rdb, nil).RefreshPair(ctx, pair, 1.00, t0.Add(6*time.Minute))
	got := readDivergence(t, rdb, pair)
	if !got.WarningFired {
		t.Error("WarningFired = false 6m into a divergence spanning two restarts: the streak " +
			"restarted with each process and the warning can never publish")
	}
	if !got.FiringSince.Equal(t0) {
		t.Errorf("FiringSince = %v, want the streak start %v", got.FiringSince, t0)
	}
}

// TestRefreshPair_RestartFromLegacyCachedWarning covers the deploy that
// ships FiringSince: the cached result was written without it, and a
// published warning must still carry over rather than reset.
func TestRefreshPair_RestartFromLegacyCachedWarning(t *testing.T) {
	refs := depeggedRefs()
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	legacy, err := json.Marshal(map[string]any{
		"pair_id": pair.String(), "warning_fired": true, "success_count": 3,
		"computed_at": time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rdb.Set(context.Background(), cachekeys.Divergence(pair).String(), legacy, cachekeys.DivergenceTTL).Err(); err != nil {
		t.Fatalf("seed legacy result: %v", err)
	}
	_ = svc.RefreshPair(context.Background(), pair, 1.00, time.Now())
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("WarningFired = false after restarting over a pre-FiringSince cached warning")
	}
}

// TestRefreshPair_RestartDoesNotResurrectClearedWarning: the restore only
// carries a streak forward; a pair that is no longer diverging publishes
// false on its first post-restart refresh, whatever the prior entry said.
func TestRefreshPair_RestartDoesNotResurrectClearedWarning(t *testing.T) {
	refs := depeggedRefs()
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
	})
	pair := xlmUSD(t)
	t0 := time.Now()
	ctx := context.Background()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(10*time.Minute))
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("precondition: want a published warning")
	}
	_ = restartedService(t, refs, rdb, nil).RefreshPair(ctx, pair, 0.93, t0.Add(11*time.Minute))
	got := readDivergence(t, rdb, pair)
	if got.WarningFired || !got.FiringSince.IsZero() {
		t.Errorf("after recovery: WarningFired=%v FiringSince=%v, want false and zero",
			got.WarningFired, got.FiringSince)
	}
}

// TestRefreshPair_GapDoesNotMatureStreak (GH-1043): a pair that fired once,
// then went unevaluated for hours (no VWAP, parse error, below quorum), must
// not publish on the single firing observation after the gap. Elapsed time
// across an unobserved interval is not evidence the divergence persisted.
func TestRefreshPair_GapDoesNotMatureStreak(t *testing.T) {
	var fired int
	svc, rdb, _ := newTestService(t, depeggedRefs(), divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) {
			fired++
		},
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	afterGap := t0.Add(3 * time.Hour)

	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, afterGap)
	got := readDivergence(t, rdb, pair)
	if got.WarningFired || fired != 0 {
		t.Fatalf("first firing refresh after a 3h unevaluated gap: WarningFired=%v hooks=%d, "+
			"want the warning held — one post-gap sample must not satisfy the persistence debounce",
			got.WarningFired, fired)
	}
	if !got.FiringSince.Equal(afterGap) {
		t.Errorf("FiringSince = %v, want the streak restarted at %v", got.FiringSince, afterGap)
	}

	_ = svc.RefreshPair(ctx, pair, 1.00, afterGap.Add(divergence.DefaultWarningPersistence+time.Minute))
	if !readDivergence(t, rdb, pair).WarningFired || fired != 1 {
		t.Errorf("divergence observed across two refreshes after the gap did not publish (hooks=%d)", fired)
	}
}

// TestRefreshPair_SlowCadenceStillWarns: an operator cadence longer than the
// debounce (divergence_min_interval_seconds = 1h) is not a gap; consecutive
// refreshes one interval apart must still mature the streak.
func TestRefreshPair_SlowCadenceStillWarns(t *testing.T) {
	svc, rdb, _ := newTestService(t, depeggedRefs(), divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		RefreshInterval:      time.Hour,
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(time.Hour))
	if !readDivergence(t, rdb, pair).WarningFired {
		t.Error("two consecutive firing refreshes at a 1h cadence did not publish: the gap reset blinded the warning")
	}
}

// TestRefreshPair_PublishedWarningSurvivesGap: once published, an
// evaluation gap must not flap the warning off and re-send the webhook.
func TestRefreshPair_PublishedWarningSurvivesGap(t *testing.T) {
	var fired int
	svc, rdb, _ := newTestService(t, depeggedRefs(), divergence.ServiceOptions{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) {
			fired++
		},
	})
	ctx := context.Background()
	pair := xlmUSD(t)
	t0 := time.Now()
	_ = svc.RefreshPair(ctx, pair, 1.00, t0)
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(divergence.DefaultWarningPersistence+time.Minute))
	_ = svc.RefreshPair(ctx, pair, 1.00, t0.Add(3*time.Hour))
	if !readDivergence(t, rdb, pair).WarningFired || fired != 1 {
		t.Errorf("published warning after a gap: WarningFired=%v hooks=%d, want true and 1",
			readDivergence(t, rdb, pair).WarningFired, fired)
	}
}
