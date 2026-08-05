package freeze_test

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// t0 is the fixed evaluation origin for the lifecycle tables.
var t0 = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// firing is a bucket that trips the ADR-0019 3-signal AND.
func firing(at time.Time) freeze.Signal {
	return freeze.Signal{Now: at, Fires: true, Scored: true, Confidence: 0.02, ZScore: 12}
}

// healthy is a bucket that meets the ADR-0019 auto-unfreeze
// condition: confidence strictly above 0.30 AND z strictly below 3.0.
func healthy(at time.Time) freeze.Signal {
	return freeze.Signal{Now: at, Scored: true, Confidence: 0.50, ZScore: 0.4}
}

// middling is neither: it does not fire, and it does not clear the
// auto-unfreeze bar either (z between the two thresholds). This is
// the band the pre-lifecycle code released on, because release was
// simply "the fire condition stopped holding".
func middling(at time.Time) freeze.Signal {
	return freeze.Signal{Now: at, Scored: true, Confidence: 0.50, ZScore: 4.2}
}

// TestPolicy_WithDefaults_MatchesADR0019 pins the shipped defaults to
// the ADR's own numbers. These are the values an operator inherits by
// leaving `[anomaly.phase2]` alone, so a silent drift here is a
// silent change to how long every frozen pair serves a stale price.
func TestPolicy_WithDefaults_MatchesADR0019(t *testing.T) {
	p := freeze.Policy{}.WithDefaults()

	if p.InitialHold != 30*time.Minute {
		t.Errorf("InitialHold = %v, want ADR-0019's 30m", p.InitialHold)
	}
	if p.Extension != 30*time.Minute {
		t.Errorf("Extension = %v, want ADR-0019's 30m", p.Extension)
	}
	if p.MaxExtensions != 4 {
		t.Errorf("MaxExtensions = %d, want ADR-0019's 4 (2h total)", p.MaxExtensions)
	}
	if p.UnfreezeConfidenceMin != 0.30 {
		t.Errorf("UnfreezeConfidenceMin = %v, want ADR-0019's 0.30", p.UnfreezeConfidenceMin)
	}
	if p.UnfreezeZScoreMax != 3.0 {
		t.Errorf("UnfreezeZScoreMax = %v, want ADR-0019's 3.0", p.UnfreezeZScoreMax)
	}
	if p.UnfreezeBuckets != 2 {
		t.Errorf("UnfreezeBuckets = %d, want ADR-0019's 2 consecutive buckets", p.UnfreezeBuckets)
	}
	// The documented deviation. Shorter than the ADR's flat 30m, and
	// deliberately longer than the longest default window that can
	// carry a spike, so a one-bucket spike can never be waited out.
	if p.UncorroboratedInitialHold != 10*time.Minute {
		t.Errorf("UncorroboratedInitialHold = %v, want 10m", p.UncorroboratedInitialHold)
	}
	if p.UncorroboratedInitialHold >= p.InitialHold {
		t.Error("the uncorroborated hold is not shorter than the corroborated one")
	}
	if p.MarkerGrace != cachekeys.FreezeTTL {
		t.Errorf("MarkerGrace = %v, want cachekeys.FreezeTTL %v", p.MarkerGrace, cachekeys.FreezeTTL)
	}
}

// TestPolicy_FireStartsTheHold — a fresh fire sets the hold from the
// corroboration signal and starts the ladder at zero.
func TestPolicy_FireStartsTheHold(t *testing.T) {
	p := freeze.Policy{}

	sig := firing(t0)
	sig.Corroborated = true
	out := p.Evaluate(freeze.State{}, sig)
	if !out.Frozen || out.Transition != freeze.TransitionFired {
		t.Fatalf("corroborated fire: Frozen=%v Transition=%q", out.Frozen, out.Transition)
	}
	if got := out.State.HoldUntil.Sub(t0); got != freeze.DefaultInitialHold {
		t.Errorf("corroborated hold = %v, want %v", got, freeze.DefaultInitialHold)
	}
	if out.State.FiredAt != t0 || out.State.ExtensionsUsed != 0 || out.State.Escalated {
		t.Errorf("unexpected fresh state: %+v", out.State)
	}
	if want := freeze.DefaultInitialHold + freeze.DefaultMarkerGrace; out.MarkerTTL != want {
		t.Errorf("MarkerTTL = %v, want hold+grace %v", out.MarkerTTL, want)
	}

	out = p.Evaluate(freeze.State{}, firing(t0)) // Corroborated false
	if got := out.State.HoldUntil.Sub(t0); got != freeze.DefaultUncorroboratedInitialHold {
		t.Errorf("uncorroborated hold = %v, want %v", got, freeze.DefaultUncorroboratedInitialHold)
	}
}

// TestPolicy_NoFireNoFreeze — the quiet path must stay quiet.
func TestPolicy_NoFireNoFreeze(t *testing.T) {
	out := freeze.Policy{}.Evaluate(freeze.State{}, healthy(t0))
	if out.Frozen || out.Transition != freeze.TransitionNone {
		t.Errorf("healthy bucket on a clean pair: Frozen=%v Transition=%q", out.Frozen, out.Transition)
	}
	if out.State.Active() {
		t.Errorf("state activated with no fire: %+v", out.State)
	}
}

// TestPolicy_MiddlingBucketDoesNotRelease is the core regression in
// pure form: the band between the fire threshold and the unfreeze
// threshold (z ∈ [3, 5], confidence ∈ [0.30, 0.45]) is a HOLD band,
// not a release band. The pre-lifecycle code treated it as release,
// because release was `!fires`.
func TestPolicy_MiddlingBucketDoesNotRelease(t *testing.T) {
	p := freeze.Policy{}
	st := p.Evaluate(freeze.State{}, firing(t0)).State

	// Walk a long way past every hold expiry with middling buckets.
	now := t0
	for i := 0; i < 20; i++ {
		now = now.Add(5 * time.Minute)
		out := p.Evaluate(st, middling(now))
		if !out.Frozen {
			t.Fatalf("released on a middling bucket after %v (z=4.2 is above the "+
				"auto-unfreeze bar of 3.0)", now.Sub(t0))
		}
		if out.State.UnfreezeStreak != 0 {
			t.Fatalf("middling bucket earned an unfreeze streak: %+v", out.State)
		}
		st = out.State
	}
}

// TestPolicy_ReleaseNeedsTwoConsecutiveAndTheMinimumHold walks the
// three ways a release can be refused and the one way it is granted.
func TestPolicy_ReleaseNeedsTwoConsecutiveAndTheMinimumHold(t *testing.T) {
	p := freeze.Policy{}
	fired := p.Evaluate(freeze.State{}, firing(t0)).State
	minEnd := t0.Add(freeze.DefaultUncorroboratedInitialHold)

	t.Run("two healthy buckets inside the minimum hold do not release", func(t *testing.T) {
		st := fired
		for i := 1; i <= 4; i++ {
			at := t0.Add(time.Duration(i) * time.Minute)
			out := p.Evaluate(st, healthy(at))
			if !out.Frozen {
				t.Fatalf("released at %v, inside the %v minimum hold",
					at.Sub(t0), freeze.DefaultUncorroboratedInitialHold)
			}
			st = out.State
		}
		if st.UnfreezeStreak < 2 {
			t.Fatalf("streak did not accumulate inside the hold: %+v", st)
		}
	})

	t.Run("one healthy bucket past the minimum hold does not release", func(t *testing.T) {
		out := p.Evaluate(fired, healthy(minEnd.Add(time.Minute)))
		if !out.Frozen {
			t.Fatal("released on a streak of one")
		}
		if out.State.UnfreezeStreak != 1 {
			t.Errorf("UnfreezeStreak = %d, want 1", out.State.UnfreezeStreak)
		}
	})

	t.Run("a broken streak restarts the count", func(t *testing.T) {
		st := p.Evaluate(fired, healthy(minEnd.Add(time.Minute))).State
		st = p.Evaluate(st, middling(minEnd.Add(2*time.Minute))).State
		if st.UnfreezeStreak != 0 {
			t.Fatalf("streak survived a middling bucket: %+v", st)
		}
		out := p.Evaluate(st, healthy(minEnd.Add(3*time.Minute)))
		if !out.Frozen {
			t.Fatal("released on the first healthy bucket after a break — the ADR " +
				"wants two CONSECUTIVE")
		}
	})

	t.Run("two consecutive healthy buckets past the minimum hold release", func(t *testing.T) {
		st := p.Evaluate(fired, healthy(minEnd.Add(time.Minute))).State
		out := p.Evaluate(st, healthy(minEnd.Add(2*time.Minute)))
		if out.Frozen {
			t.Fatalf("still frozen: %+v", out.State)
		}
		if out.Transition != freeze.TransitionReleased {
			t.Errorf("Transition = %q, want %q", out.Transition, freeze.TransitionReleased)
		}
		if out.State.Active() {
			t.Errorf("released outcome carries an active state: %+v", out.State)
		}
	})
}

// TestPolicy_UnscoredBucketNeverEarnsRelease — a bucket the scorer
// could not score (no baseline, no previous VWAP, Phase 1 firing
// before the confidence step ran) is the ABSENCE of evidence. It must
// reset the streak, not credit it: the state right after an
// aggregator restart is exactly an unscored bucket, and crediting it
// would make "restart the aggregator twice" an unfreeze primitive.
func TestPolicy_UnscoredBucketNeverEarnsRelease(t *testing.T) {
	p := freeze.Policy{}
	st := p.Evaluate(freeze.State{}, firing(t0)).State
	past := t0.Add(freeze.DefaultUncorroboratedInitialHold + time.Minute)

	st = p.Evaluate(st, healthy(past)).State
	if st.UnfreezeStreak != 1 {
		t.Fatalf("setup: streak = %d", st.UnfreezeStreak)
	}
	// An unscored bucket arrives between the two healthy ones.
	unscored := freeze.Signal{Now: past.Add(time.Minute)}
	st = p.Evaluate(st, unscored).State
	if st.UnfreezeStreak != 0 {
		t.Errorf("unscored bucket left the streak at %d, want 0", st.UnfreezeStreak)
	}
	out := p.Evaluate(st, healthy(past.Add(2*time.Minute)))
	if !out.Frozen {
		t.Error("released after unscored + one healthy — an unmeasurable bucket " +
			"must not count toward recovery")
	}

	// Two unscored buckets in a row: still frozen, indefinitely.
	for i := 0; i < 5; i++ {
		out = p.Evaluate(out.State, freeze.Signal{Now: past.Add(time.Duration(10+i) * time.Minute)})
		if !out.Frozen {
			t.Fatalf("released on unscored buckets alone at iteration %d", i)
		}
	}
}

// TestPolicy_LadderExtendsFourTimesThenEscalates pins the ADR's
// "up to 4 extensions (2 hours total); after 4 extensions escalate to
// operator review (P1 alert); freeze stays active until manual
// unfreeze".
func TestPolicy_LadderExtendsFourTimesThenEscalates(t *testing.T) {
	p := freeze.Policy{}
	st := p.Evaluate(freeze.State{}, firing(t0)).State

	now := t0.Add(freeze.DefaultUncorroboratedInitialHold + time.Second)
	for i := 1; i <= freeze.DefaultMaxExtensions; i++ {
		out := p.Evaluate(st, middling(now))
		if out.Transition != freeze.TransitionExtended {
			t.Fatalf("expiry %d: Transition = %q, want %q", i, out.Transition, freeze.TransitionExtended)
		}
		if out.State.ExtensionsUsed != i {
			t.Fatalf("expiry %d: ExtensionsUsed = %d", i, out.State.ExtensionsUsed)
		}
		if out.State.Escalated {
			t.Fatalf("escalated early, at extension %d", i)
		}
		if got := out.State.HoldUntil.Sub(now); got != freeze.DefaultExtension {
			t.Errorf("expiry %d: new hold = %v, want %v", i, got, freeze.DefaultExtension)
		}
		st = out.State
		now = now.Add(freeze.DefaultExtension + time.Second)
	}

	// Total elapsed at the escalation point: the initial hold plus 4
	// extensions. For the ADR's corroborated 30m hold that is 2h30m;
	// the 4 × 30m ladder is the part the ADR fixes.
	out := p.Evaluate(st, middling(now))
	if out.Transition != freeze.TransitionEscalated {
		t.Fatalf("Transition = %q, want %q", out.Transition, freeze.TransitionEscalated)
	}
	if !out.Frozen || !out.State.Escalated {
		t.Fatalf("escalation must stay frozen: %+v", out)
	}
	if got := now.Sub(t0); got < 2*time.Hour {
		t.Errorf("escalated after only %v of freeze; ADR-0019's ladder is 2h of "+
			"extensions on top of the initial hold", got)
	}
}

// TestPolicy_EscalatedFreezeNeverAutoUnfreezes — ADR-0019 holds an
// escalated freeze "until manual unfreeze". Auto-unfreeze is
// suppressed on purpose: the ladder already spent two hours asking
// whether the pair had recovered, and a human has been paged.
func TestPolicy_EscalatedFreezeNeverAutoUnfreezes(t *testing.T) {
	p := freeze.Policy{}
	st := freeze.State{
		FiredAt:        t0,
		HoldUntil:      t0.Add(time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
	}

	now := t0.Add(3 * time.Hour)
	for i := 0; i < 50; i++ {
		now = now.Add(time.Minute)
		out := p.Evaluate(st, healthy(now))
		if !out.Frozen {
			t.Fatalf("escalated freeze auto-unfroze after %d healthy buckets", i+1)
		}
		if out.State.ExtensionsUsed != freeze.DefaultMaxExtensions {
			t.Errorf("escalated freeze kept climbing the ladder: %+v", out.State)
		}
		if out.MarkerTTL <= 0 {
			t.Errorf("escalated freeze wrote a non-positive marker TTL %v — the "+
				"marker would lapse and flags.frozen would clear itself", out.MarkerTTL)
		}
		st = out.State
	}
}

// TestPolicy_FiringBucketNeverEarnsAStreak — defence in depth against
// an operator configuring overlapping fire and unfreeze bands (e.g.
// raising confidence_max_freeze above the unfreeze bound). A bucket
// that is simultaneously "anomalous enough to freeze" and "healthy
// enough to release" must resolve as frozen.
func TestPolicy_FiringBucketNeverEarnsAStreak(t *testing.T) {
	p := freeze.Policy{UnfreezeZScoreMax: 20} // deliberately overlapping
	st := p.Evaluate(freeze.State{}, firing(t0)).State

	now := t0.Add(freeze.DefaultUncorroboratedInitialHold + time.Minute)
	for i := 0; i < 4; i++ {
		out := p.Evaluate(st, freeze.Signal{
			Now: now, Fires: true, Scored: true, Confidence: 0.9, ZScore: 12,
		})
		if !out.Frozen {
			t.Fatalf("a still-FIRING bucket released the freeze at iteration %d", i)
		}
		if out.State.UnfreezeStreak != 0 {
			t.Fatalf("a firing bucket earned an unfreeze streak: %+v", out.State)
		}
		st = out.State
		now = now.Add(time.Minute)
	}
}

// TestPolicy_MarkerTTLTracksRemainingHold — the TTL must shrink as
// the hold is consumed and never go negative, so a marker cannot
// outlive its hold by more than the grace.
func TestPolicy_MarkerTTLTracksRemainingHold(t *testing.T) {
	p := freeze.Policy{}
	st := p.Evaluate(freeze.State{}, firing(t0)).State

	for _, elapsed := range []time.Duration{time.Minute, 5 * time.Minute, 9 * time.Minute} {
		out := p.Evaluate(st, middling(t0.Add(elapsed)))
		want := freeze.DefaultUncorroboratedInitialHold - elapsed + freeze.DefaultMarkerGrace
		if out.MarkerTTL != want {
			t.Errorf("after %v: MarkerTTL = %v, want %v", elapsed, out.MarkerTTL, want)
		}
	}
}

// TestPolicy_OperatorTuning — every duration is operator-tunable, and
// a partial override keeps the ADR defaults for everything else.
func TestPolicy_OperatorTuning(t *testing.T) {
	p := freeze.Policy{
		InitialHold:   2 * time.Hour,
		MaxExtensions: 1,
	}
	sig := firing(t0)
	sig.Corroborated = true
	out := p.Evaluate(freeze.State{}, sig)
	if got := out.State.HoldUntil.Sub(t0); got != 2*time.Hour {
		t.Errorf("overridden InitialHold not honoured: %v", got)
	}

	// Unset fields still come from the ADR: extension is 30m.
	st := out.State
	now := t0.Add(2*time.Hour + time.Second)
	ext := p.Evaluate(st, middling(now))
	if ext.Transition != freeze.TransitionExtended {
		t.Fatalf("Transition = %q", ext.Transition)
	}
	if got := ext.State.HoldUntil.Sub(now); got != freeze.DefaultExtension {
		t.Errorf("Extension = %v, want the default %v", got, freeze.DefaultExtension)
	}
	// MaxExtensions=1 → the very next expiry escalates.
	esc := p.Evaluate(ext.State, middling(now.Add(freeze.DefaultExtension+time.Second)))
	if esc.Transition != freeze.TransitionEscalated {
		t.Errorf("Transition = %q, want %q with MaxExtensions=1", esc.Transition, freeze.TransitionEscalated)
	}
}

// TestPolicy_UnscoredExpiryDoesNotBurnTheLadder — the restart-
// rehydration trap (live occurrence: crypto:XLM/fiat:GBP 24h,
// 2026-08-05): during the ~30-minute post-restart confidence
// bootstrap every bucket is UNSCORED, so a rehydrated freeze could
// never accumulate an unfreeze streak while its expiries still burned
// extensions — it marched to ESCALATED without one scored
// evaluation. An unscored expiry now slides the hold WITHOUT
// consuming an extension; the ladder resumes when scoring does.
func TestPolicy_UnscoredExpiryDoesNotBurnTheLadder(t *testing.T) {
	p := freeze.Policy{}
	st := p.Evaluate(freeze.State{}, firing(t0)).State

	// Ten hold expiries in a row, all unscored (a bootstrap window far
	// longer than the whole pre-fix ladder): no extension consumed, no
	// escalation, still frozen throughout.
	now := t0.Add(freeze.DefaultUncorroboratedInitialHold + time.Second)
	for i := 1; i <= 10; i++ {
		sig := middling(now)
		sig.Scored = false
		out := p.Evaluate(st, sig)
		if out.Transition != freeze.TransitionHeldUnscored {
			t.Fatalf("unscored expiry %d: Transition = %q, want %q", i, out.Transition, freeze.TransitionHeldUnscored)
		}
		if out.State.ExtensionsUsed != 0 {
			t.Fatalf("unscored expiry %d consumed an extension: %d", i, out.State.ExtensionsUsed)
		}
		if out.State.Escalated {
			t.Fatalf("unscored expiry %d escalated", i)
		}
		if !out.Frozen {
			t.Fatalf("unscored expiry %d unfroze", i)
		}
		st = out.State
		now = now.Add(freeze.DefaultExtension + time.Second)
	}

	// Scoring returns: the ladder picks up from ZERO extensions —
	// a scored middling expiry consumes the FIRST extension.
	out := p.Evaluate(st, middling(now))
	if out.Transition != freeze.TransitionExtended || out.State.ExtensionsUsed != 1 {
		t.Fatalf("first scored expiry after bootstrap: %q ext=%d, want extended ext=1",
			out.Transition, out.State.ExtensionsUsed)
	}

	// And a recovered pair releases through the normal streak once
	// scored — the bootstrap window cost it nothing.
	st = out.State
	now = now.Add(time.Minute)
	for i := 0; i < freeze.DefaultUnfreezeBuckets; i++ {
		res := p.Evaluate(st, healthy(now))
		st = res.State
		if res.Transition == freeze.TransitionReleased {
			return // released — done
		}
		now = now.Add(time.Minute)
	}
	res := p.Evaluate(st, healthy(now))
	if res.Transition != freeze.TransitionReleased {
		t.Fatalf("post-bootstrap recovery did not release: %q %+v", res.Transition, res.State)
	}
}
