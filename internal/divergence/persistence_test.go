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
	if cached.WarningFired {
		t.Error("WarningFired = true on a single-refresh 10% gap: a fast-move VWAP-vs-spot lag " +
			"must not raise a false divergence warning before it has persisted past the debounce window")
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
	if readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("WarningFired = true on the first observation; the debounce must hold it for one window")
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
	if readDivergence(t, rdb, pair).WarningFired {
		t.Fatal("WarningFired = true on spike #1; must be held")
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
