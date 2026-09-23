package freeze_test

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
)

// fuzzPolicy builds a Policy from fuzz-sized knobs. Durations are whole
// minutes so the ladder arithmetic stays readable in a failing input; a
// zero knob exercises the WithDefaults sentinel.
func fuzzPolicy(hold, uncorrHold, ext, grace uint8, maxExt, buckets uint8, confMin, zMax float64) freeze.Policy {
	return freeze.Policy{
		InitialHold:               time.Duration(hold) * time.Minute,
		UncorroboratedInitialHold: time.Duration(uncorrHold) * time.Minute,
		Extension:                 time.Duration(ext) * time.Minute,
		MaxExtensions:             int(maxExt % 8),
		UnfreezeConfidenceMin:     confMin,
		UnfreezeZScoreMax:         zMax,
		UnfreezeBuckets:           int(buckets % 6),
		MarkerGrace:               time.Duration(grace) * time.Minute,
	}
}

// unfreezeHealthy is the ADR-0019 auto-unfreeze condition for one bucket, stated
// independently of the implementation: scored, not firing, a lens agrees
// with the candidate, confidence strictly above the floor and z strictly
// below the ceiling.
func unfreezeHealthy(p freeze.Policy, sig freeze.Signal) bool {
	return sig.Scored && !sig.Fires && sig.ReleaseCorroborated &&
		sig.Confidence > p.UnfreezeConfidenceMin && sig.ZScore < p.UnfreezeZScoreMax
}

func initialHoldOf(p freeze.Policy, corroborated bool) time.Duration {
	if corroborated {
		return p.InitialHold
	}
	return p.UncorroboratedInitialHold
}

// FuzzPolicyEvaluate checks one lifecycle step against the ADR-0019
// contract for an arbitrary (policy, prior state, signal): release only on
// an earned streak after the initial hold, never out of escalation; the
// ladder only climbs at a scored expiry; the marker TTL always covers the
// remaining hold plus grace, and the persisted ladder is still live (by the
// durable authority's own predicate) for exactly that TTL.
func FuzzPolicyEvaluate(f *testing.F) {
	// hold, uncorr, ext, grace, maxExt, buckets, confMin, zMax,
	// active, firedAgoMin, holdLeftMin, extUsed, escalated, streak, corrPrev,
	// fires, scored, conf, z, corr, relCorr
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		false, int16(0), int16(0), uint8(0), false, uint8(0), false,
		true, true, 0.1, 6.0, true, false)
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		true, int16(30), int16(0), uint8(0), false, uint8(1), true,
		false, true, 0.9, 1.0, true, true)
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		true, int16(120), int16(-1), uint8(4), false, uint8(0), false,
		false, true, 0.2, 4.0, false, false)
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		true, int16(10), int16(-5), uint8(0), false, uint8(0), false,
		false, false, 0.0, 0.0, false, false)
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		true, int16(300), int16(10), uint8(4), true, uint8(2), true,
		false, true, 0.99, 0.1, true, true)
	// Boundaries: confidence exactly at the floor and z exactly at the
	// ceiling are NOT healthy (strict comparisons); a saturated streak
	// inside the initial hold stays saturated.
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		true, int16(40), int16(0), uint8(0), false, uint8(1), true,
		false, true, 0.30, 1.0, true, true)
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		true, int16(40), int16(0), uint8(0), false, uint8(1), true,
		false, true, 0.9, 3.0, true, true)
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), 0.0, 0.0,
		true, int16(5), int16(20), uint8(0), false, uint8(5), true,
		false, true, 0.9, 1.0, true, true)
	f.Fuzz(func(t *testing.T,
		hold, uncorrHold, ext, grace, maxExt, buckets uint8, confMin, zMax float64,
		active bool, firedAgoMin, holdLeftMin int16, extUsed uint8, escalated bool, streak uint8, corrPrev bool,
		fires, scored bool, conf, z float64, corr, relCorr bool,
	) {
		raw := fuzzPolicy(hold, uncorrHold, ext, grace, maxExt, buckets, confMin, zMax)
		p := raw.WithDefaults()
		now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		var prev freeze.State
		if active {
			prev = freeze.State{
				FiredAt:        now.Add(-time.Duration(firedAgoMin) * time.Minute),
				HoldUntil:      now.Add(time.Duration(holdLeftMin) * time.Minute),
				ExtensionsUsed: int(extUsed % 8),
				Escalated:      escalated,
				UnfreezeStreak: int(streak % 8),
				Corroborated:   corrPrev,
			}
		}
		sig := freeze.Signal{
			Now: now, Fires: fires, Scored: scored, Confidence: conf, ZScore: z,
			Corroborated: corr, ReleaseCorroborated: relCorr,
		}

		out := raw.Evaluate(prev, sig)
		if again := raw.Evaluate(prev, sig); again != out {
			t.Fatalf("Evaluate is not pure: %+v then %+v", out, again)
		}

		if !out.Frozen {
			if out.State != (freeze.State{}) || out.MarkerTTL != 0 {
				t.Fatalf("unfrozen outcome carries state %+v / ttl %v", out.State, out.MarkerTTL)
			}
		} else {
			remaining := max(out.State.HoldUntil.Sub(now), 0)
			if out.MarkerTTL != remaining+p.MarkerGrace {
				t.Fatalf("MarkerTTL %v, want remaining hold %v + grace %v", out.MarkerTTL, remaining, p.MarkerGrace)
			}
			if !out.State.Active() || out.State.HoldUntil.Before(now) {
				t.Fatalf("frozen outcome with inactive or lapsed state %+v", out.State)
			}
			// The marker and the durable ladder must agree on how long the
			// freeze outlives aggregator silence.
			if !freeze.LadderStillLive(out.State, p.MarkerGrace, now.Add(out.MarkerTTL)) {
				t.Fatalf("ladder already dead at marker expiry: %+v ttl=%v", out.State, out.MarkerTTL)
			}
			if freeze.LadderStillLive(out.State, p.MarkerGrace, now.Add(out.MarkerTTL+time.Nanosecond)) {
				t.Fatalf("ladder outlives its marker: %+v ttl=%v", out.State, out.MarkerTTL)
			}
		}

		if !prev.Active() {
			if !fires {
				if out.Frozen || out.Transition != freeze.TransitionNone {
					t.Fatalf("unfrozen pair with no fire: %+v", out)
				}
				return
			}
			want := freeze.State{FiredAt: now, HoldUntil: now.Add(initialHoldOf(p, corr)), Corroborated: corr}
			if out.Transition != freeze.TransitionFired || out.State != want {
				t.Fatalf("fire: got %+v, want fired with %+v", out, want)
			}
			return
		}

		minServed := !now.Before(prev.FiredAt.Add(initialHoldOf(p, prev.Corroborated)))
		wantStreak := 0
		if unfreezeHealthy(p, sig) {
			wantStreak = prev.UnfreezeStreak + 1
			if prev.UnfreezeStreak >= p.UnfreezeBuckets {
				wantStreak = prev.UnfreezeStreak
			}
		}
		wantRelease := !prev.Escalated && wantStreak >= p.UnfreezeBuckets && minServed
		if (out.Transition == freeze.TransitionReleased) != wantRelease || out.Frozen == wantRelease {
			t.Fatalf("release=%v (%+v), want %v: streak %d/%d minServed=%v escalated=%v",
				out.Transition == freeze.TransitionReleased, out, wantRelease, wantStreak, p.UnfreezeBuckets, minServed, prev.Escalated)
		}
		if wantRelease {
			return
		}

		st := out.State
		if st.FiredAt != prev.FiredAt || st.Corroborated != prev.Corroborated {
			t.Fatalf("fire identity rewritten: %+v -> %+v", prev, st)
		}
		if st.UnfreezeStreak != wantStreak {
			t.Fatalf("streak %d, want %d (prev %d, healthy=%v)", st.UnfreezeStreak, wantStreak, prev.UnfreezeStreak, unfreezeHealthy(p, sig))
		}
		if prev.Escalated && !st.Escalated {
			t.Fatal("escalation is sticky until an operator clears it")
		}
		expired := !now.Before(prev.HoldUntil)
		switch out.Transition {
		case freeze.TransitionHeld:
			if !prev.Escalated && (expired || st.HoldUntil != prev.HoldUntil) {
				t.Fatalf("held outside the hold or moved it: %+v -> %+v", prev, st)
			}
			if st.ExtensionsUsed != prev.ExtensionsUsed {
				t.Fatal("a hold consumed an extension")
			}
		case freeze.TransitionHeldUnscored:
			if !expired || scored || prev.Escalated || st.ExtensionsUsed != prev.ExtensionsUsed {
				t.Fatalf("held_unscored: expired=%v scored=%v ext %d->%d", expired, scored, prev.ExtensionsUsed, st.ExtensionsUsed)
			}
		case freeze.TransitionExtended:
			if !expired || !scored || prev.Escalated || st.ExtensionsUsed != prev.ExtensionsUsed+1 ||
				prev.ExtensionsUsed >= p.MaxExtensions {
				t.Fatalf("extended: expired=%v scored=%v ext %d->%d max %d", expired, scored, prev.ExtensionsUsed, st.ExtensionsUsed, p.MaxExtensions)
			}
		case freeze.TransitionEscalated:
			if !expired || !scored || prev.Escalated || !st.Escalated || prev.ExtensionsUsed < p.MaxExtensions {
				t.Fatalf("escalated: expired=%v scored=%v ext %d max %d", expired, scored, prev.ExtensionsUsed, p.MaxExtensions)
			}
		default:
			t.Fatalf("active freeze produced transition %q", out.Transition)
		}
		if out.Transition != freeze.TransitionHeld || prev.Escalated {
			if st.HoldUntil != now.Add(p.Extension) {
				t.Fatalf("%s: hold until %v, want now+extension", out.Transition, st.HoldUntil)
			}
		}
	})
}

// FuzzPolicyLadder drives a whole freeze through a fuzzed sequence of
// buckets on a monotone clock and checks the ladder-level guarantees no
// single step can: a release is always backed by UnfreezeBuckets
// consecutive healthy buckets and the served initial hold; the ladder
// never exceeds MaxExtensions and escalates only once it is spent; an
// escalated freeze never auto-releases; the hold never moves backwards.
func FuzzPolicyLadder(f *testing.F) {
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0), uint8(0), []byte{0x01, 0x80, 0x80, 0x9f, 0x9f})
	f.Add(uint8(2), uint8(3), uint8(1), uint8(4), uint8(3), []byte{0x01, 0x8e, 0x0e, 0x0e, 0x0e, 0x0e, 0x0e, 0xfe, 0xfe})
	f.Add(uint8(1), uint8(1), uint8(1), uint8(1), uint8(1), []byte{0x01, 0x42, 0x42, 0x42, 0x42, 0xfe, 0xfe, 0xfe})
	f.Fuzz(func(t *testing.T, hold, ext, grace, maxExt, buckets uint8, steps []byte) {
		raw := fuzzPolicy(hold, hold/2, ext, grace, maxExt, buckets, 0.3, 3.0)
		p := raw.WithDefaults()
		now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		var st freeze.State
		run := 0 // consecutive healthy buckets since the freeze fired
		for i, b := range steps {
			// bits: 0 fires, 1 scored, 2 relCorr, 3 corr, 4 healthy conf, 5-7 minutes advanced (x5)
			now = now.Add(time.Duration(b>>5) * 5 * time.Minute)
			sig := freeze.Signal{
				Now: now, Fires: b&1 != 0, Scored: b&2 != 0, ReleaseCorroborated: b&4 != 0,
				Corroborated: b&8 != 0, Confidence: 0.1, ZScore: 1,
			}
			if b&16 != 0 {
				sig.Confidence = 0.9
			}
			out := raw.Evaluate(st, sig)
			if st.Active() {
				if unfreezeHealthy(p, sig) {
					run++
				} else {
					run = 0
				}
			}
			switch {
			case out.Transition == freeze.TransitionReleased:
				if st.Escalated {
					t.Fatalf("step %d: escalated freeze auto-released", i)
				}
				if run < p.UnfreezeBuckets {
					t.Fatalf("step %d: released after %d healthy buckets, need %d", i, run, p.UnfreezeBuckets)
				}
				if now.Before(st.FiredAt.Add(initialHoldOf(p, st.Corroborated))) {
					t.Fatalf("step %d: released inside the initial hold", i)
				}
			case out.Frozen:
				n := out.State
				if n.ExtensionsUsed > p.MaxExtensions {
					t.Fatalf("step %d: %d extensions > max %d", i, n.ExtensionsUsed, p.MaxExtensions)
				}
				if n.Escalated && n.ExtensionsUsed != p.MaxExtensions {
					t.Fatalf("step %d: escalated with %d/%d extensions", i, n.ExtensionsUsed, p.MaxExtensions)
				}
				if st.Active() && n.HoldUntil.Before(st.HoldUntil) {
					t.Fatalf("step %d: hold moved backwards %v -> %v", i, st.HoldUntil, n.HoldUntil)
				}
				if st.Active() && n.ExtensionsUsed < st.ExtensionsUsed {
					t.Fatalf("step %d: ladder moved down", i)
				}
			}
			if !out.Frozen {
				run = 0
			}
			st = out.State
		}
	})
}

// FuzzLadderStillLive pins the durable-liveness predicate the rehydrate,
// the recovery worker and the marker merge all share: live exactly while
// now <= hold + grace, monotone in both the clock and the grace.
func FuzzLadderStillLive(f *testing.F) {
	f.Add(true, int64(0), int64(0), int64(0))
	f.Add(true, int64(60), int64(300), int64(360))
	f.Add(true, int64(60), int64(300), int64(361))
	f.Add(false, int64(60), int64(300), int64(0))
	f.Fuzz(func(t *testing.T, active bool, holdSec, graceSec, nowSec int64) {
		const bound = int64(1) << 32
		if holdSec < -bound || holdSec > bound || graceSec < 0 || graceSec > bound || nowSec < -bound || nowSec > bound {
			return
		}
		base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		var st freeze.State
		if active {
			st = freeze.State{FiredAt: base.Add(-time.Hour), HoldUntil: base.Add(time.Duration(holdSec) * time.Second)}
		}
		grace := time.Duration(graceSec) * time.Second
		now := base.Add(time.Duration(nowSec) * time.Second)
		got := freeze.LadderStillLive(st, grace, now)
		want := active && nowSec <= holdSec+graceSec
		if got != want {
			t.Fatalf("LadderStillLive(hold=%ds grace=%ds now=%ds active=%v) = %v, want %v",
				holdSec, graceSec, nowSec, active, got, want)
		}
		if got && !freeze.LadderStillLive(st, grace, now.Add(-time.Second)) {
			t.Fatal("live now but dead a second earlier")
		}
		if got && !freeze.LadderStillLive(st, grace+time.Second, now) {
			t.Fatal("live with grace g but dead with a longer grace")
		}
	})
}
